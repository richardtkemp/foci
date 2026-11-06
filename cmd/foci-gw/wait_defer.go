package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/command"
	"foci/internal/defersend"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/timeutil"
)

var deferLog = log.NewComponentLogger("defersend")

// defaultWaitTimeout is the deadline a deferred request gets when the caller
// gives no wait_timeout (Dick, 2026-10-10: 24 h). At the deadline a send or a
// branch whose condition never held is delivered anyway; a COMMAND is dropped
// with a notice instead (#2220), so a deferred `/reset` never fires
// mid-conversation just because time ran out. A stored record keeps the
// deadline it was enqueued with.
const defaultWaitTimeout = 24 * time.Hour

// deferMaxAttempts bounds the delivery attempts of a record whose delivery
// fails TRANSIENTLY (a full inbox): it stays queued for a later sweep, and the
// third failure drops it with a notice (#2220). A permanent failure drops at
// once.
const deferMaxAttempts = 3

var (
	// errDeferUnknownCommand: a deferred command's name is not registered
	// (the registry changed during the wait, or a record predates the
	// enqueue check). Permanent.
	errDeferUnknownCommand = errors.New("unknown command")
	// errDeferNotExecutable: a deferred command is registered but has no
	// Execute function. Permanent.
	errDeferNotExecutable = errors.New("command not executable")
)

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

// present reports whether an active wait is requested: at least one gate
// flag is set and the opt-out is not. The exact negation of the "no wait"
// early-out in deferUnmetWait — one definition for both, and for
// handleBranch's #2301 enqueue gate.
func (wc waitConds) present() bool {
	return !wc.none && wc.any()
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
	if code, msg := refuseDeferred(d, rec); code != 0 {
		deferLog.Infof("refused deferred %s at enqueue (agent=%s session=%s): %d %s",
			rec.EffectiveKind(), rec.AgentID, rec.SessionKey, code, deferRefusalLogText(rec, msg))
		http.Error(w, msg, code)
		return
	}
	rec.WaitWarm = wc.warm
	rec.WaitCold = wc.cold
	rec.WaitUserActive = wc.userActive
	rec.WaitUserInactive = wc.userInactive
	rec.CreatedAt = timeutil.Now()
	rec.DeadlineAt = rec.CreatedAt.Add(timeout)
	id, replaced, err := d.deferStore.Enqueue(rec)
	if err != nil {
		deferLog.Errorf("enqueue deferred %s: %v", rec.EffectiveKind(), err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	deferLog.Infof("deferred %s %d queued (agent=%s session=%s deadline=%s)",
		rec.EffectiveKind(), id, rec.AgentID, rec.SessionKey, timeutil.Format(rec.DeadlineAt))
	if len(replaced) > 0 {
		deferLog.Infof("deferred %s %d replaces identical pending %v", rec.EffectiveKind(), id, replaced)
	}
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

// refuseDeferred is the enqueue-time check of a request about to be stored
// (#2220): it refuses, with the status and text the immediate path would
// answer, a request whose delivery could only fail — so the caller learns now
// instead of getting a 202 for a record the sweep would silently drop. code 0
// means accept. A command must name a registered, executable command (404
// "unknown command", handleCommand's answer); a send or branch carrying a
// model must resolve it exactly as the immediate path does (400, the
// errBranchBadModel text). Delivery checks again: the registry or config can
// change during the wait.
func refuseDeferred(d httpHandlerDeps, rec defersend.Record) (int, string) {
	inst, ok := d.agents.Get(rec.AgentID)
	if !ok {
		return 0, ""
	}
	switch rec.EffectiveKind() {
	case defersend.KindCommand:
		if err := checkDeferredCommand(inst, rec); err != nil {
			return http.StatusNotFound, "unknown command"
		}
	default:
		if rec.Model != "" {
			if err := checkModelOverride(inst, rec.SessionKey, rec.Model, d.cfg.Models); err != nil {
				return http.StatusBadRequest, fmt.Errorf("%w: %w", errBranchBadModel, err).Error()
			}
		}
	}
	return 0, ""
}

// deferRefusalLogText is msg for the log line — except for a command, whose
// refusal is logged as its name only (never its arguments, #2259).
func deferRefusalLogText(rec defersend.Record, msg string) string {
	if rec.EffectiveKind() != defersend.KindCommand {
		return msg
	}
	return msg + ": /" + command.RequestFromText(rec.Text, rec.SessionKey, "", 0).Name
}

// checkDeferredCommand reports whether a command record names a registered
// command with an Execute function — the lookup Registry.Dispatch makes, on
// the name command.RequestFromText parses, the same parse dispatch uses.
func checkDeferredCommand(inst *agentInstance, rec defersend.Record) error {
	if inst.cmds == nil {
		return errDeferUnknownCommand
	}
	cmd := inst.cmds.Get(command.RequestFromText(rec.Text, rec.SessionKey, "", 0).Name)
	if cmd == nil {
		return errDeferUnknownCommand
	}
	if cmd.Execute == nil {
		return errDeferNotExecutable
	}
	return nil
}

// deferUnmetWait is the wait gate shared by /send, /branch and /command: with
// no wait condition (or wait_none) it reports false and the caller proceeds;
// with a condition that does not hold NOW it enqueues the request for the
// sweep and writes the 202 deferred receipt (true = handled, stop). A
// malformed duration answers 400. A nil defer store degrades to proceeding —
// the graceful degradation /send has always had: a missing queue must not
// break every request.
func deferUnmetWait(w http.ResponseWriter, d httpHandlerDeps, in activityGateInputs, wc waitConds, rec defersend.Record, rcpt route.Receipt) bool {
	if !wc.present() {
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
// holds. At the deadline a send or branch is delivered anyway; a command is
// dropped with a notice (#2220). A transient delivery failure keeps the
// record for a later sweep, up to deferMaxAttempts; a permanent one drops it
// at once with a notice. It runs until ctx is cancelled.
type deferSweeper struct {
	store           *defersend.Store
	deps            httpHandlerDeps
	isUserActive    userActivityChecker
	isSessionActive sessionActivityChecker
	// deliverFn replaces deliver when set — a test seam for driving the
	// retry/drop classification with a chosen failure. Production leaves it
	// nil.
	deliverFn func(*agentInstance, defersend.Record) error
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
// passed; a command that reaches its deadline with its condition unmet is
// dropped, not delivered.
func (s *deferSweeper) sweep() {
	records, err := s.store.All()
	if err != nil {
		deferLog.Errorf("sweep: list pending: %v", err)
		return
	}
	now := timeutil.Now()
	for _, r := range records {
		inst, ok := s.deps.agents.Get(r.AgentID)
		if !ok {
			// No agent, so no session chat to notify: log only.
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
			s.drop(r, "bad wait condition", err)
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
		if !activityOK && r.EffectiveKind() == defersend.KindCommand {
			// #2220 (Dick, 2026-10-10): a command is never delivered just
			// because time ran out — `foci command --wait-cold 55m /reset`
			// must not reset mid-conversation at its deadline.
			s.drop(r, "deadline reached and the wait condition never held", nil)
			continue
		}
		reason := "condition met"
		if !activityOK {
			reason = "deadline reached — sending anyway"
		}
		deferLog.Infof("delivering deferred %s %d (agent=%s session=%s): %s", r.EffectiveKind(), r.ID, r.AgentID, r.SessionKey, reason)
		deliver := s.deliver
		if s.deliverFn != nil {
			deliver = s.deliverFn
		}
		if err := deliver(inst, r); err != nil {
			s.failed(r, err)
			continue
		}
		_ = s.store.Delete(r.ID)
	}
}

// failed handles one failed delivery attempt: a transient failure keeps the
// record for a later sweep until its deferMaxAttempts-th failure (the count
// is persisted, so it survives a restart); a permanent failure, or the last
// transient one, drops it with a notice.
func (s *deferSweeper) failed(r defersend.Record, err error) {
	transient, class := classifyDeferFailure(err)
	if transient {
		n, cerr := s.store.RecordFailedAttempt(r.ID)
		if cerr != nil {
			deferLog.Errorf("deferred %s %d: record failed attempt: %v", r.EffectiveKind(), r.ID, cerr)
		} else if n < deferMaxAttempts {
			deferLog.Warnf("deferred %s %d delivery failed (attempt %d of %d, will retry; agent=%s session=%s): %v",
				r.EffectiveKind(), r.ID, n, deferMaxAttempts, r.AgentID, r.SessionKey, err)
			return
		}
		class = fmt.Sprintf("%s, %d attempts", class, deferMaxAttempts)
	}
	s.drop(r, class, err)
}

// classifyDeferFailure sorts a delivery error into transient (worth another
// sweep: the target's inbox was full) or permanent, and names its reason
// class — the short phrase the drop notice carries. The class never includes
// request text or arguments.
func classifyDeferFailure(err error) (transient bool, class string) {
	switch {
	case errors.Is(err, errBranchInboxFull):
		return true, "session inbox full"
	case errors.Is(err, errBranchBadModel):
		return false, "bad model"
	case errors.Is(err, errBranchUnbranchableParent):
		return false, "backend cannot branch this session"
	case errors.Is(err, errDeferUnknownCommand):
		return false, "unknown command"
	case errors.Is(err, errDeferNotExecutable):
		return false, "command not executable"
	default:
		return false, "delivery failed"
	}
}

// drop removes a record that will never be delivered, logs it at WARN and
// sends the drop notice to the target session's chat. cause (may be nil) is
// logged, never sent: for a command it is built from the name only.
func (s *deferSweeper) drop(r defersend.Record, class string, cause error) {
	if cause != nil {
		deferLog.Warnf("dropping deferred %s %d (agent=%s session=%s): %s: %v", r.EffectiveKind(), r.ID, r.AgentID, r.SessionKey, class, cause)
	} else {
		deferLog.Warnf("dropping deferred %s %d (agent=%s session=%s): %s", r.EffectiveKind(), r.ID, r.AgentID, r.SessionKey, class)
	}
	_ = s.store.Delete(r.ID)
	s.notifyDropped(r, class)
}

// deferDropNotice is the drop notice text: the kind, the record id and the
// reason class — never the request text or arguments, which can carry
// secrets (#2259).
func deferDropNotice(r defersend.Record, class string) string {
	return fmt.Sprintf("⚠️ Deferred %s #%d dropped: %s.", r.EffectiveKind(), r.ID, class)
}

// notifyDropped sends the drop notice to the target session's OWN chat
// (route.PolicyStrict: never the agent's primary), and nothing when that
// session has no live connection. For a branch the target is the parent.
func (s *deferSweeper) notifyDropped(r defersend.Record, class string) {
	conn, _ := route.ConnFor(s.deps.connMgr, r.AgentID, r.SessionKey, route.PolicyStrict)
	if conn == nil {
		return
	}
	if err := conn.SendToSession(r.SessionKey, deferDropNotice(r, class)); err != nil {
		deferLog.Warnf("deferred %s %d: drop notice not sent: %v", r.EffectiveKind(), r.ID, err)
	}
}

// deliver hands one ready record to its kind's delivery path and returns the
// failure, if any, for the sweep to classify (classifyDeferFailure).
func (s *deferSweeper) deliver(inst *agentInstance, r defersend.Record) error {
	switch r.EffectiveKind() {
	case defersend.KindBranch:
		return s.deliverBranch(inst, r)
	case defersend.KindCommand:
		return s.deliverCommand(inst, r)
	default:
		return s.deliverSend(inst, r)
	}
}

// deliverSend injects a deferred send onto the target session's inbox — the
// same buffered, queued delivery asyncDispatch uses, minus the HTTP receipt
// (the caller is long gone; a deferred send is fire-and-forget). A
// choice-carrying record (Steer, #1318) routes as a user envelope instead so
// its mode takes effect at delivery — except policy=broadcast, where the mode
// is inert exactly as on the immediate path (steerChoiceRoutes). A model that
// no longer resolves is a permanent failure (the immediate path refuses it
// with 400), and nothing is delivered; a full inbox is transient.
func (s *deferSweeper) deliverSend(inst *agentInstance, r defersend.Record) error {
	if r.Model != "" {
		if err := applyModelOverride(inst, r.SessionKey, r.Model, s.deps.cfg.Models); err != nil {
			return fmt.Errorf("%w: %w", errBranchBadModel, err)
		}
	}
	app.DeliverExternalPrompt(r.SessionKey, r.Text)
	if pref := platform.SteerPreferenceFromWire(r.Steer); steerChoiceRoutes(pref, route.Policy(r.Policy)) {
		// Human is deliberately false, mirroring the branch ruling below
		// (#1130): whoever declared themselves human at enqueue time may be
		// gone by delivery — a deferred request always delivers as automated.
		if !enqueueSteerChoice(inst, s.deps, r.SessionKey, r.Text, pref, false, route.Policy(r.Policy)) {
			return errBranchInboxFull
		}
		return nil
	}
	sendCtx := agent.WithTrigger(s.deps.ctx, "user")
	if !deliverBufferedQueued(inst, s.deps.connMgr, sendCtx, r.SessionKey, r.Text, "defersend", false, route.Policy(r.Policy)) {
		return errBranchInboxFull
	}
	return nil
}

// deliverBranch runs a deferred branch at delivery time: the fork happens NOW
// from r.SessionKey (the parent's state at delivery, not at enqueue), the
// turn is async with trigger "branch", and no receipt exists (the HTTP caller
// is long gone). A delivery failure is returned for the sweep to classify: a
// full inbox is retried (bounded), while a bad model at delivery time, a fork
// error or a named parent on a backend that cannot branch (#2284's refusal,
// from the same shared runBranchTurn) drops the record with a notice — a
// branch has not started yet, and silently forking on the wrong model is
// worse than a loud drop. The main/named
// oracle here is the plain default key (defaultSessionKey, no create hook):
// the record carries no rung (#2284 R1: no deferred-record change), so a
// deferred empty-selector branch whose default was CreateDefault-minted
// (#1859) is refused at delivery rather than sent — an acceptable corner for
// a backend that cannot branch. The human flag is deliberately false (#1130):
// it is not part of the deferrable record, and whoever declared themselves
// human at enqueue time may be gone by delivery — a deferred request always
// delivers as automated.
func (s *deferSweeper) deliverBranch(inst *agentInstance, r defersend.Record) error {
	parentIsDefault := r.SessionKey == defaultSessionKey(s.deps, inst.id)
	_, err := runBranchTurn(s.deps, inst, r.SessionKey, parentIsDefault, route.Receipt{SessionKey: r.SessionKey}, branchTurnOptions{
		Text: r.Text, Model: r.Model, NoCompact: r.NoCompact, NoResetHook: r.NoResetHook, Persistent: r.Persistent, Silent: r.Silent,
	}, false, false)
	return err
}

// deliverCommand dispatches a deferred command through the agent's command
// registry — the same dispatch handleCommand uses, DocPath send included.
// A name that is no longer registered, or a registered command without an
// Execute function, is a permanent failure (checked BEFORE dispatch, the
// lookup refuseDeferred made at enqueue: the registry can change during the
// wait). The delivery is logged with metadata only (id, agent, session,
// kind, text length); the result text is never logged, because it can carry
// secrets such as a pairing key (#2259). The !ok warning likewise logs the
// command name only, never its arguments: the name comes from the same
// command.RequestFromText the dispatch parsed, so it is exactly the name the
// registry looked up.
func (s *deferSweeper) deliverCommand(inst *agentInstance, r defersend.Record) error {
	name := command.RequestFromText(r.Text, r.SessionKey, "", 0).Name
	err := checkDeferredCommand(inst, r)
	if errors.Is(err, errDeferUnknownCommand) {
		deferLog.Warnf("deferred command %d unknown — no such registered command (agent=%s session=%s): /%s",
			r.ID, r.AgentID, r.SessionKey, name)
		return err
	}
	var result command.Response
	ok := err == nil
	if ok {
		result, ok = dispatchAgentCommand(s.deps, inst, s.deps.ctx, r.SessionKey, r.Text)
	}
	if !ok {
		deferLog.Warnf("deferred command %d not executable — registered command has no Execute (agent=%s session=%s): /%s",
			r.ID, r.AgentID, r.SessionKey, name)
		return errDeferNotExecutable
	}
	deferLog.Infof("deferred command %d delivered (agent=%s session=%s kind=%s text_len=%d)",
		r.ID, r.AgentID, r.SessionKey, r.EffectiveKind(), len(result.DisplayText()))
	return nil
}
