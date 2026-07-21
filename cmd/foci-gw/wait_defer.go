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

// enqueueDeferredSend persists a not-yet-satisfiable send and writes the
// "deferred" receipt. A deferred send is inherently async — the caller's
// connection cannot be held until the condition holds — so --sync callers get
// this receipt now and the reply (if any) is delivered to the session later.
func enqueueDeferredSend(w http.ResponseWriter, d httpHandlerDeps, agentID, sessionKey, text, policy, model string, wc waitConds, rcpt route.Receipt) {
	if d.deferStore == nil {
		http.Error(w, "deferred sends unavailable", http.StatusServiceUnavailable)
		return
	}
	timeout, err := resolveWaitTimeout(wc.timeout)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := timeutil.Now()
	id, err := d.deferStore.Enqueue(defersend.Record{
		AgentID: agentID, SessionKey: sessionKey, Text: text, Policy: policy, Model: model,
		WaitWarm: wc.warm, WaitCold: wc.cold, WaitUserActive: wc.userActive, WaitUserInactive: wc.userInactive,
		CreatedAt: now, DeadlineAt: now.Add(timeout),
	})
	if err != nil {
		deferLog.Errorf("enqueue deferred send: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	deferLog.Infof("deferred send %d queued (agent=%s session=%s deadline=%s)", id, agentID, sessionKey, timeutil.Format(now.Add(timeout)))
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

// deferSweeper periodically re-evaluates pending deferred sends and delivers
// each one whose wait condition now holds, or whose deadline has passed
// (send-anyway). It runs until ctx is cancelled.
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

// sweep delivers every ready pending send once. A send is ready when its wait
// condition holds now or its deadline has passed.
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
			deferLog.Warnf("dropping deferred send %d: agent %q gone", r.ID, r.AgentID)
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
			deferLog.Errorf("dropping deferred send %d: %v", r.ID, err)
			_ = s.store.Delete(r.ID)
			continue
		}
		// A rate-limited endpoint withholds delivery unconditionally (#1417) —
		// unlike an activity condition, there is no "send anyway" escape hatch
		// on deadline: firing into a live rate limit is a guaranteed-fail API
		// call that only extends the backoff further. The record just stays
		// queued (still persisted, still restart-surviving) until the endpoint
		// gate reopens; this same 10s sweep tick is the drain, delivering one
		// record at a time in FIFO order.
		if limited, reason := inst.ag.SessionRateLimited(r.SessionKey); limited {
			deferLog.Debugf("deferred send %d withheld: %s", r.ID, reason)
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
		deferLog.Infof("delivering deferred send %d (agent=%s session=%s): %s", r.ID, r.AgentID, r.SessionKey, reason)
		s.deliver(inst, r)
		_ = s.store.Delete(r.ID)
	}
}

// deliver injects a deferred send onto the target session's inbox — the same
// buffered, queued delivery asyncDispatch uses, minus the HTTP receipt (the
// caller is long gone; a deferred send is fire-and-forget).
func (s *deferSweeper) deliver(inst *agentInstance, r defersend.Record) {
	if r.Model != "" {
		if err := applyModelOverride(inst, r.SessionKey, r.Model, s.deps.cfg.Models); err != nil {
			deferLog.Warnf("deferred model override %q: %v", r.Model, err)
		}
	}
	app.DeliverExternalPrompt(r.SessionKey, r.Text)
	sendCtx := agent.WithTrigger(s.deps.ctx, "user")
	deliverBufferedQueued(inst, s.deps.connMgr, sendCtx, r.SessionKey, r.Text, "defersend", false, route.Policy(r.Policy))
}
