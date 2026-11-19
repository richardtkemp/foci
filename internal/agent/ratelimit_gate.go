package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"foci/internal/account"
	"foci/internal/procx"
	"foci/internal/ratelimit"
	"foci/internal/session"
)

// RateLimitGate suppresses non-user API work while an endpoint is rate-limited.
// Closed gates queue system work for replay; user turns may pass as recovery
// probes and release the gate early when they succeed.
type RateLimitGate struct {
	mu                sync.Mutex
	until             time.Time    // zero = not rate-limited
	queue             []QueuedItem // pending work to replay on open
	missingHintStreak int          // consecutive request limits without a trustworthy reset hint
}

// QueuedItem is a pending message queued while rate-limited.
type QueuedItem struct {
	SessionKey string
	Message    string
	Trigger    string // original trigger context (e.g. "user", "keepalive")
}

// RateLimitedError is returned when the gate blocks a message.
// Callers can type-assert to access the reset time and the endpoint whose
// gate closed — the endpoint that limited.
type RateLimitedError struct {
	Until    time.Time
	Endpoint string
}

func (e *RateLimitedError) Error() string {
	return rateLimitedText(e.Endpoint, e.Until)
}

// rateLimitedText is the one rendering of a closed gate, shared by the
// user-facing RateLimitedError and the scheduler-facing SessionRateLimited
// reason: it names the endpoint that limited when one is known, and keeps
// the endpoint-less form otherwise.
func rateLimitedText(endpoint string, until time.Time) string {
	if endpoint == "" {
		return fmt.Sprintf("rate limited (resets %s)", until.Format(time.Kitchen))
	}
	return fmt.Sprintf("rate limited on %s (resets %s)", endpoint, until.Format(time.Kitchen))
}

// Close marks the gate as rate-limited until the given time.
func (g *RateLimitGate) Close(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = until
}

// IsLimited returns true if the gate is currently closed (rate-limited).
func (g *RateLimitGate) IsLimited() (limited bool, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() || time.Now().After(g.until) {
		return false, time.Time{}
	}
	return true, g.until
}

// Enqueue adds a message to the queue for replay when the gate opens.
func (g *RateLimitGate) Enqueue(sessionKey, message, trigger string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queue = append(g.queue, QueuedItem{
		SessionKey: sessionKey,
		Message:    message,
		Trigger:    trigger,
	})
}

// CloseFor resolves a neutral rate-limit signal through the shared policy,
// closes the gate, and returns its absolute deadline.
func (g *RateLimitGate) CloseFor(signal ratelimit.Signal) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	resolved := ratelimit.Resolve(time.Now(), signal, g.missingHintStreak)
	g.until = resolved.Until
	g.missingHintStreak = resolved.MissingHintStreak
	return resolved.Until
}

// Open releases a closed gate immediately after a successful probe. Pending
// queued work is preserved for the next DrainQueue call. It returns whether
// the gate had a deadline to release.
func (g *RateLimitGate) Open() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	wasClosed := !g.until.IsZero()
	g.until = time.Time{}
	g.missingHintStreak = 0
	return wasClosed
}

// DrainQueue returns and clears the queue if the gate is now open.
// Returns nil if still rate-limited.
func (g *RateLimitGate) DrainQueue() []QueuedItem {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.until.IsZero() && time.Now().Before(g.until) {
		return nil // still limited
	}
	if len(g.queue) == 0 {
		return nil
	}
	items := g.queue
	g.queue = nil
	g.until = time.Time{}
	g.missingHintStreak = 0
	return items
}

// rateLimitGateKey names one of the agent's gates: the endpoint that limited
// and, for a delegated agent, the account it limited (#3186: a hard limit a
// backend reports is its account's, so each account of the pool has its own
// gate). account is "" for an API agent, whose limits are the endpoint's.
type rateLimitGateKey struct {
	endpoint string
	account  string
}

// String names the gate for a log line and a refusal.
func (k rateLimitGateKey) String() string { return k.endpoint }

// DrainRateLimitQueue checks if any rate limit gates have opened, and replays
// queued messages through HandleMessage. Called from keepalive tick.
func (a *Agent) DrainRateLimitQueue(ctx context.Context) {
	// Expire usage-hold windows past their reset (#2491), so a hold lifts at
	// its reset time even when no usage event arrives.
	for _, acct := range a.Accounts().Accounts() {
		acct.Hold()
	}
	a.rateLimitGatesMu.RLock()
	gates := make(map[rateLimitGateKey]*RateLimitGate, len(a.rateLimitGates))
	for key, gate := range a.rateLimitGates {
		gates[key] = gate
	}
	a.rateLimitGatesMu.RUnlock()

	// Drain each gate's queue
	for key, gate := range gates {
		endpoint := key.endpoint
		items := gate.DrainQueue()
		if len(items) == 0 {
			continue
		}

		a.logger().Infof("rate limit lifted for %s, replaying %d queued items", endpoint, len(items))
		for i, item := range items {
			itemCtx := WithTrigger(ctx, item.Trigger)
			if err := a.HandleMessage(itemCtx, item.SessionKey, []string{item.Message}, nil); err != nil {
				a.logger().Errorf("replay queued message session=%s trigger=%s: %v", item.SessionKey, item.Trigger, err)
				// If we hit another rate limit on THIS endpoint, stop replaying its queue
				var rlErr *RateLimitedError
				if errors.As(err, &rlErr) {
					a.logger().Infof("rate limited again during replay on %s, %d items re-queued", endpoint, len(items)-i)
					break // stop replaying THIS endpoint, but continue with others
				}
				continue
			}
			a.logger().Debugf("replayed message session=%s trigger=%s", item.SessionKey, item.Trigger)
		}
	}
}

// getOrCreateRateLimitGate returns the gate of endpoint for a session with
// no account (an API agent's), creating it if it doesn't exist yet.
// Thread-safe.
func (a *Agent) getOrCreateRateLimitGate(endpoint string) *RateLimitGate {
	return a.rateLimitGate(endpoint, nil)
}

// sessionRateLimitGate returns the gate sessionKey's turns answer to: its
// endpoint's, for its account.
func (a *Agent) sessionRateLimitGate(sessionKey string) (*RateLimitGate, string) {
	endpoint := a.resolveEndpoint(sessionKey)
	return a.rateLimitGate(endpoint, a.AccountFor(sessionKey)), endpoint
}

// rateLimitGate returns the gate of endpoint ("" = the agent's default) for
// acct (nil = no account), creating it if it doesn't exist yet. Thread-safe.
func (a *Agent) rateLimitGate(endpoint string, acct *account.Account) *RateLimitGate {
	if endpoint == "" {
		endpoint = a.Endpoint
	}
	key := rateLimitGateKey{endpoint: endpoint, account: acct.Key()}

	// Fast path: read lock
	a.rateLimitGatesMu.RLock()
	if gate, ok := a.rateLimitGates[key]; ok {
		a.rateLimitGatesMu.RUnlock()
		return gate
	}
	a.rateLimitGatesMu.RUnlock()

	// Slow path: write lock
	a.rateLimitGatesMu.Lock()
	defer a.rateLimitGatesMu.Unlock()

	// Double-check (another goroutine might have created it)
	if gate, ok := a.rateLimitGates[key]; ok {
		return gate
	}

	// Lazy init map
	if a.rateLimitGates == nil {
		a.rateLimitGates = make(map[rateLimitGateKey]*RateLimitGate)
	}

	// Create new gate, reopening at a deadline a previous process persisted
	// (#2026) — otherwise every scheduler fires into a live cap after a
	// restart until one of them trips it again. A past deadline is harmless:
	// IsLimited treats it as open.
	gate := &RateLimitGate{}
	if until, ok := a.loadRateLimitDeadline(key); ok {
		gate.until = until
	}
	a.rateLimitGates[key] = gate
	return gate
}

// rateLimitDeadline is the persisted close-deadline of one gate. Only the
// deadline survives a restart; the replay queue and the missing-hint backoff
// streak are process-scoped.
func (a *Agent) rateLimitDeadline(key rateLimitGateKey) session.PersistedTime {
	name := "ratelimit_until:" + key.endpoint
	if key.account != "" {
		name += "@" + key.account
	}
	return a.SessionIndex.PersistedTime(a.AgentID, name)
}

// loadRateLimitDeadline reads key's persisted deadline. A delegated agent's
// deadline stored before gates were per account (under the endpoint alone)
// is moved to the first account that asks, so a gate closed at the upgrade
// stays closed (#3186).
func (a *Agent) loadRateLimitDeadline(key rateLimitGateKey) (time.Time, bool) {
	if until, ok := a.rateLimitDeadline(key).Load(); ok || key.account == "" {
		return until, ok
	}
	old := a.rateLimitDeadline(rateLimitGateKey{endpoint: key.endpoint})
	until, ok := old.Load()
	if !ok {
		return time.Time{}, false
	}
	if err := a.rateLimitDeadline(key).Save(until); err == nil {
		_ = old.Save(time.Time{})
	}
	return until, true
}

// persistRateLimitDeadline saves (or, for zero, clears) a gate's deadline.
func (a *Agent) persistRateLimitDeadline(key rateLimitGateKey, until time.Time) {
	if err := a.rateLimitDeadline(key).Save(until); err != nil {
		a.logger().Warnf("persist rate limit deadline (%s): %v", key.endpoint, err)
	}
}

// resolveEndpoint returns the endpoint a session's turns run against: the
// endpoint leg of the session's effective model tuple (see SessionModelTuple)
// — the owning session's override, else the agent default. An inheriting
// child gates on the endpoint its traffic actually uses (the root's), not the
// agent default.
func (a *Agent) resolveEndpoint(sessionKey string) string {
	return a.SessionModelTuple(sessionKey).Endpoint
}

// SessionRateLimited reports whether the given session may NOT fire because its
// endpoint's rate-limit gate is closed, the account is on a usage hold, a
// Claude Code login is pending (reloginBlocksBackground), or the session key
// is empty. This is
// the shared gate every periodic scheduler consults — it does NOT run the
// can_run_background script (that is background-work-only). Returns (true,
// reason) when blocked, (false, "") when clear.
func (a *Agent) SessionRateLimited(sessionKey string) (limited bool, reason string) {
	if sessionKey == "" {
		return true, "no session key"
	}
	gate, endpoint := a.sessionRateLimitGate(sessionKey)
	if lim, until := gate.IsLimited(); lim {
		return true, rateLimitedText(endpoint, until)
	}
	// The account-wide usage hold (#2491): no periodic work but keepalive
	// during a hold, nothing during a stop.
	if blocked, reason := a.UsageHoldBlocks(sessionKey, false); blocked {
		return true, reason
	}
	if blocked, reason := a.reloginBlocksBackground(sessionKey); blocked {
		return true, reason
	}
	return false, ""
}

// reloginBlocksBackground reports whether a Claude Code login is owed on
// sessionKey's account, for an agent that uses it (#3006): an attempt is running, or one failed and
// nothing has worked since. Background work then does not run, keepalive
// included: on the dead credential each run would only 401 (#2384 pauses
// injections the same way; periodic work forks its own backend and passed
// that pause). It resumes when the login completes. Agents on other backends
// do not use the login (UsesCCLogin).
func (a *Agent) reloginBlocksBackground(sessionKey string) (bool, string) {
	if !a.UsesCCLogin() {
		return false, ""
	}
	if g := a.loginGate(sessionKey); g.Active() || g.Pending() {
		return true, "Claude Code login pending (send /login)"
	}
	return false, ""
}

// KeepaliveRateLimited is SessionRateLimited for keepalive: the endpoint gate,
// a usage STOP and a pending login block it; a usage HOLD does not —
// keepalives are what a hold keeps running (#2491).
func (a *Agent) KeepaliveRateLimited(sessionKey string) (limited bool, reason string) {
	if sessionKey == "" {
		return true, "no session key"
	}
	gate, endpoint := a.sessionRateLimitGate(sessionKey)
	if lim, until := gate.IsLimited(); lim {
		return true, rateLimitedText(endpoint, until)
	}
	if blocked, reason := a.UsageHoldBlocks(sessionKey, true); blocked {
		return true, reason
	}
	return a.reloginBlocksBackground(sessionKey)
}

// engageRateLimit is the single gate-closing path for API and delegated
// backends. The source supplies a neutral signal; the endpoint gate applies
// shared reset/fallback policy. notify controls whether proactive hooks fire —
// user API turns already receive the returned RateLimitedError directly.
func (a *Agent) engageRateLimit(endpoint string, acct *account.Account, signal ratelimit.Signal, notify bool) time.Time {
	if endpoint == "" {
		endpoint = a.Endpoint
	}
	gate := a.rateLimitGate(endpoint, acct)
	wasLimited, prev := gate.IsLimited()
	until := gate.CloseFor(signal)
	a.persistRateLimitDeadline(rateLimitGateKey{endpoint: endpoint, account: acct.Key()}, until)
	// A backend repeats a rejection while it retries (CC's rate_limit_event,
	// #2488): re-closing to the same deadline is not news, so it neither
	// re-logs at INFO nor re-notifies.
	if wasLimited && prev.Equal(until) {
		a.logger().Debugf("rate limit gate (%s) still closed until %s (kind=%s)", endpoint, until.Format(time.Kitchen), signal.Kind)
		return until
	}
	a.logger().Infof("rate limit gate (%s) closed until %s (kind=%s)", endpoint, until.Format(time.Kitchen), signal.Kind)
	if notify {
		for _, fn := range a.RateLimitFunc {
			fn(until)
		}
	}
	return until
}

// EngageRateLimit engages acct's default-endpoint gate for a signal from a
// delegated backend running on acct, and fires the standard notification
// hooks. User-triggered delegated turns still run through their coding agent
// and may probe the limit.
func (a *Agent) EngageRateLimit(acct *account.Account, signal ratelimit.Signal) {
	a.engageRateLimit(a.Endpoint, acct, signal, true)
}

// releaseRateLimit opens an endpoint gate after a successful API response.
// Queued system work remains queued until the normal drain tick replays it.
func (a *Agent) releaseRateLimit(endpoint string) {
	if endpoint == "" {
		endpoint = a.Endpoint
	}
	gate := a.getOrCreateRateLimitGate(endpoint)
	if gate.Open() {
		a.persistRateLimitDeadline(rateLimitGateKey{endpoint: endpoint}, time.Time{})
		a.logger().Infof("rate limit gate (%s) released early after successful probe", endpoint)
	}
}

// CanFireBackgroundOperation checks if a background operation can run on the given session.
// Returns false if:
//   - The session key is empty
//   - The rate limit gate for the session's endpoint is closed (SessionRateLimited)
//   - The configured can_run_background executable exits non-zero
//
// This is the FULL gate (rate limit + can_run_background). Only the
// background-work scheduler and the memory hooks (compaction / session-end)
// use it; the other periodic schedulers use SessionRateLimited alone, so the
// can_run_background script never gates keepalive/reflection/consolidation/reset.
func (a *Agent) CanFireBackgroundOperation(ctx context.Context, sessionKey string) (bool, string) {
	if limited, reason := a.SessionRateLimited(sessionKey); limited {
		return false, reason
	}

	// user-provided background gate. Exit 0 = allowed, non-zero = skip.
	if a.canRunBackground() != "" && !a.runCanRunBackground(ctx, sessionKey, a.resolveEndpoint(sessionKey)) {
		return false, "can_run_background declined"
	}

	return true, ""
}

// runCanRunBackground runs the configured can_run_background executable and
// reports whether background work is permitted. A failure to execute the
// command (not found, not executable) is treated as permitted so a broken
// script never wedges all background work.
func (a *Agent) runCanRunBackground(ctx context.Context, sessionKey, endpoint string) bool {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// procx.Spawn strips the foci-secrets group from the child and puts it in
	// its own process group.
	// Operator: can_run_background is an OPERATOR-AUTHORED script whose whole
	// job is to consult the operator's own tools. Trusted would silently strip
	// them. See docs/WIRING.md on the operator-hook sub-case.
	cmd := procx.Spawn(cctx, procx.Operator, a.canRunBackground())
	cmd.Env = append(procx.Env(procx.Operator),
		"FOCI_SESSION_KEY="+sessionKey,
		"FOCI_AGENT_ID="+a.AgentID,
		"FOCI_ENDPOINT="+endpoint,
	)

	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false // explicit non-zero exit = decline
	}
	a.logger().Warnf("can_run_background %q failed to run: %v", a.canRunBackground(), err)
	return true
}
