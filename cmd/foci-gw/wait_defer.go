package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/defersend"
	"foci/internal/log"
	"foci/internal/route"
	"foci/internal/timeutil"
)

var deferLog = log.NewComponentLogger("defersend")

// defaultWaitTimeout bounds how long a deferred send waits for its condition
// before it is delivered anyway (send-anyway-on-timeout).
const defaultWaitTimeout = 2 * time.Hour

// deferSweepInterval is how often the background sweep re-evaluates pending
// sends.
const deferSweepInterval = 10 * time.Second

// waitConds carries the four wait-until duration strings plus the timeout and
// the opt-out. An empty duration field means that condition is not requested.
type waitConds struct {
	warm         string
	cold         string
	userActive   string
	userInactive string
	timeout      string
	none         bool
}

func (wc waitConds) any() bool {
	return wc.warm != "" || wc.cold != "" || wc.userActive != "" || wc.userInactive != ""
}

// activityProbes builds the two "within duration?" closures shared by the
// one-shot if-gate and the wait evaluator. Both apply the in-flight
// short-circuit: a turn executing on the target counts as active. The SESSION
// probe additionally counts a turn that ENDED within the window as active
// (in.LastTurnEnd), so "cold" means CONTINUOUS dead time — the whole window
// with no turn running AND none recently finished. Without this a turn older
// than the window reads cold the instant inFlight drops, releasing a deferred
// send into the sub-window gap between back-to-back turns. The USER probe is
// deliberately NOT widened by LastTurnEnd: a turn ending is session activity,
// not necessarily a human interaction (it may be cron/agent/memory), so
// --if-user-*/--wait-user-* keep reading only genuine user touches.
func activityProbes(in activityGateInputs, isUserActive userActivityChecker, isSessionActive sessionActivityChecker) (userActiveWithin, sessionActiveWithin func(time.Duration) bool) {
	userActiveWithin = func(within time.Duration) bool {
		return in.InFlight || isUserActive(in.SessionBase, within)
	}
	sessionActiveWithin = func(within time.Duration) bool {
		if in.InFlight {
			return true
		}
		if !in.LastTurnEnd.IsZero() && time.Since(in.LastTurnEnd) <= within {
			return true
		}
		return isSessionActive(in.SessionBase, within)
	}
	return userActiveWithin, sessionActiveWithin
}

// waitSatisfied reports whether every requested wait condition currently holds.
// A wait condition is the mirror of its if-gate: --wait-warm holds once the
// session is warm, --wait-cold once it is cold, etc. Returns an error on a
// malformed duration.
func waitSatisfied(wc waitConds, in activityGateInputs, isUserActive userActivityChecker, isSessionActive sessionActivityChecker) (bool, error) {
	userActiveWithin, sessionActiveWithin := activityProbes(in, isUserActive, isSessionActive)

	conds := []struct {
		value       string
		label       string
		holds       func(time.Duration) bool
		wantActive  bool
		activeCheck func(time.Duration) bool
	}{
		{wc.warm, "wait_warm", sessionActiveWithin, true, sessionActiveWithin},
		{wc.cold, "wait_cold", sessionActiveWithin, false, sessionActiveWithin},
		{wc.userActive, "wait_user_active", userActiveWithin, true, userActiveWithin},
		{wc.userInactive, "wait_user_inactive", userActiveWithin, false, userActiveWithin},
	}
	for _, c := range conds {
		if c.value == "" {
			continue
		}
		dur, err := time.ParseDuration(c.value)
		if err != nil {
			return false, fmt.Errorf("bad %s duration: %w", c.label, err)
		}
		if c.activeCheck(dur) != c.wantActive {
			return false, nil
		}
	}
	return true, nil
}

// waitRequest is the wait-gate wire subset shared by the /send, /branch and
// /command bodies — the server mirror of waitFlags (cmd/foci/wait_flags.go).
// Declared once so the three handlers cannot drift on field names.
type waitRequest struct {
	WaitWarm       string `json:"wait_warm"`
	WaitCold       string `json:"wait_cold"`
	WaitUserActive string `json:"wait_user_active"`
	WaitUserInact  string `json:"wait_user_inactive"`
	WaitTimeout    string `json:"wait_timeout"`
	WaitNone       bool   `json:"wait_none"`
}

// conds converts the wire fields into the waitConds the evaluator takes.
func (w waitRequest) conds() waitConds {
	return waitConds{
		warm:         w.WaitWarm,
		cold:         w.WaitCold,
		userActive:   w.WaitUserActive,
		userInactive: w.WaitUserInact,
		timeout:      w.WaitTimeout,
		none:         w.WaitNone,
	}
}

// enqueueDeferred persists a not-yet-satisfiable request and writes the
// "deferred" receipt (HTTP 202) — the same envelope regardless of kind. A
// deferred request is inherently async — the caller's connection cannot be
// held until the condition holds — so --sync callers get this receipt now and
// the reply (if any) is delivered later, by the sweep.
func enqueueDeferred(w http.ResponseWriter, d httpHandlerDeps, rec defersend.Record, wc waitConds, rcpt route.Receipt) {
	if d.deferStore == nil {
		http.Error(w, "deferred sends unavailable", http.StatusServiceUnavailable)
		return
	}
	timeout, err := resolveWaitTimeout(wc.timeout)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec.WaitWarm = wc.warm
	rec.WaitCold = wc.cold
	rec.WaitUserActive = wc.userActive
	rec.WaitUserInactive = wc.userInactive
	rec.CreatedAt = timeutil.Now()
	rec.DeadlineAt = rec.CreatedAt.Add(timeout)
	id, err := d.deferStore.Enqueue(rec)
	if err != nil {
		deferLog.Errorf("enqueue deferred %s: %v", rec.EffectiveKind(), err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	deferLog.Infof("deferred %s %d queued (agent=%s session=%s deadline=%s)",
		rec.EffectiveKind(), id, rec.AgentID, rec.SessionKey, timeutil.Format(rec.DeadlineAt))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "deferred",
		"deferred_id":  id,
		"target":       rcpt.Target,
		"session":      rcpt.SessionKey,
		"resolved_via": string(rcpt.Via),
	})
}

// deferUnmetWait is the wait gate shared by /send, /branch and /command: with
// no wait condition (or wait_none) it reports false and the caller proceeds;
// with a condition that does not hold NOW it enqueues the request for the
// sweep and writes the 202 deferred receipt (true = handled, stop). A
// malformed duration answers 400. A nil defer store degrades to proceeding —
// the graceful degradation /send has always had: a missing queue must not
// break every request.
func deferUnmetWait(w http.ResponseWriter, d httpHandlerDeps, in activityGateInputs, wc waitConds, rec defersend.Record, rcpt route.Receipt) bool {
	if wc.none || !wc.any() {
		return false
	}
	isUserActive, isSessionActive := buildActivityCheckers(d)
	satisfied, err := waitSatisfied(wc, in, isUserActive, isSessionActive)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	if satisfied {
		return false
	}
	if d.deferStore == nil {
		deferLog.Warnf("wait unmet but defer store unavailable — running now (agent=%s session=%s)", in.AgentID, in.SessionBase)
		return false
	}
	enqueueDeferred(w, d, rec, wc, rcpt)
	return true
}

// resolveWaitTimeout parses the timeout string, defaulting to defaultWaitTimeout.
func resolveWaitTimeout(s string) (time.Duration, error) {
	if s == "" {
		return defaultWaitTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad wait_timeout duration: %w", err)
	}
	return d, nil
}

// deferSweeper periodically re-evaluates pending deferred requests — sends,
// branches and commands — and delivers each one whose wait condition now
// holds, or whose deadline has passed (deliver-anyway). It runs until ctx is
// cancelled.
type deferSweeper struct {
	store           *defersend.Store
	deps            httpHandlerDeps
	isUserActive    userActivityChecker
	isSessionActive sessionActivityChecker
}

func (s *deferSweeper) run(ctx context.Context) {
	t := time.NewTicker(deferSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// sweep delivers every ready pending request once, regardless of kind. A
// record is ready when its wait condition holds now or its deadline has
// passed.
func (s *deferSweeper) sweep() {
	records, err := s.store.All()
	if err != nil {
		deferLog.Errorf("sweep: list pending: %v", err)
		return
	}
	now := timeutil.Now()
	for _, r := range records {
		inst, ok := s.deps.agents[r.AgentID]
		if !ok {
			deferLog.Warnf("dropping deferred %s %d: agent %q gone", r.EffectiveKind(), r.ID, r.AgentID)
			_ = s.store.Delete(r.ID)
			continue
		}
		in := activityGateInputs{
			AgentID:     r.AgentID,
			SessionBase: r.SessionKey,
			InFlight:    inst.ag.IsTurnInFlight(r.SessionKey),
			LastTurnEnd: inst.ag.LastTurnEnd(r.SessionKey),
		}
		wc := waitConds{warm: r.WaitWarm, cold: r.WaitCold, userActive: r.WaitUserActive, userInactive: r.WaitUserInactive}
		activityOK, err := waitSatisfied(wc, in, s.isUserActive, s.isSessionActive)
		if err != nil {
			deferLog.Errorf("dropping deferred %s %d: %v", r.EffectiveKind(), r.ID, err)
			_ = s.store.Delete(r.ID)
			continue
		}
		// A rate-limited endpoint withholds delivery unconditionally (#1417) —
		// whatever the kind — unlike an activity condition, there is no
		// "send anyway" escape hatch on deadline: firing into a live rate
		// limit is a guaranteed-fail API call that only extends the backoff
		// further. The record just stays queued (still persisted, still
		// restart-surviving) until the endpoint gate reopens; this same 10s
		// sweep tick is the drain, delivering one record at a time in FIFO
		// order.
		if limited, reason := inst.ag.SessionRateLimited(r.SessionKey); limited {
			deferLog.Debugf("deferred %s %d withheld: %s", r.EffectiveKind(), r.ID, reason)
			continue
		}
		timedOut := !r.DeadlineAt.IsZero() && now.After(r.DeadlineAt)
		if !activityOK && !timedOut {
			continue
		}
		reason := "condition met"
		if !activityOK {
			reason = "deadline reached — sending anyway"
		}
		deferLog.Infof("delivering deferred %s %d (agent=%s session=%s): %s", r.EffectiveKind(), r.ID, r.AgentID, r.SessionKey, reason)
		s.deliver(inst, r)
		_ = s.store.Delete(r.ID)
	}
}

// deliver hands one ready record to its kind's delivery path. Errors are
// logged by the path itself; the sweep deletes the record after this single
// attempt either way (a deferred request is never retried forever).
func (s *deferSweeper) deliver(inst *agentInstance, r defersend.Record) {
	switch r.EffectiveKind() {
	case defersend.KindBranch:
		s.deliverBranch(inst, r)
	case defersend.KindCommand:
		s.deliverCommand(inst, r)
	default:
		s.deliverSend(inst, r)
	}
}

// deliverSend injects a deferred send onto the target session's inbox — the
// same buffered, queued delivery asyncDispatch uses, minus the HTTP receipt
// (the caller is long gone; a deferred send is fire-and-forget).
func (s *deferSweeper) deliverSend(inst *agentInstance, r defersend.Record) {
	if r.Model != "" {
		if err := applyModelOverride(inst, r.SessionKey, r.Model, s.deps.cfg.Models); err != nil {
			deferLog.Warnf("deferred model override %q: %v", r.Model, err)
		}
	}
	app.DeliverExternalPrompt(r.SessionKey, r.Text)
	sendCtx := agent.WithTrigger(s.deps.ctx, "user")
	deliverBufferedQueued(inst, s.deps.connMgr, sendCtx, r.SessionKey, r.Text, "defersend", false, route.Policy(r.Policy))
}

// deliverBranch runs a deferred branch at delivery time: the fork happens NOW
// from r.SessionKey (the parent's state at delivery, not at enqueue), the
// turn is async with trigger "branch", and no receipt exists (the HTTP caller
// is long gone). A delivery failure (bad model at delivery time, fork error,
// full inbox) is logged and the record dropped after that one attempt — a
// branch has not started yet, and silently forking on the wrong model is
// worse than a loud drop.
func (s *deferSweeper) deliverBranch(inst *agentInstance, r defersend.Record) {
	_, err := runBranchTurn(s.deps, inst, r.SessionKey, route.Receipt{SessionKey: r.SessionKey}, branchTurnOptions{
		Text: r.Text, Model: r.Model, NoCompact: r.NoCompact, NoResetHook: r.NoResetHook, Silent: r.Silent,
	}, false)
	if err != nil {
		deferLog.Warnf("deferred branch %d delivery failed (agent=%s parent=%s): %v", r.ID, r.AgentID, r.SessionKey, err)
	}
}

// deliverCommand dispatches a deferred command through the agent's command
// registry — the same dispatch handleCommand uses, DocPath send included. A
// command-not-found is logged; the result text is logged at INFO (there is no
// HTTP caller to return it to).
func (s *deferSweeper) deliverCommand(inst *agentInstance, r defersend.Record) {
	result, ok := dispatchAgentCommand(s.deps, inst, s.deps.ctx, r.SessionKey, r.Text)
	if !ok {
		deferLog.Warnf("deferred command %d not found (agent=%s session=%s): %s", r.ID, r.AgentID, r.SessionKey, r.Text)
		return
	}
	deferLog.Infof("deferred command %d delivered (agent=%s session=%s): %s", r.ID, r.AgentID, r.SessionKey, result.Text)
}
