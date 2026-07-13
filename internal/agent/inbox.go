// Package agent — Inbox subsystem.
//
// The Inbox is the agent's per-session message intake. Each session key gets
// its own inbox struct (channel + steer buffer + in-flight flag + worker
// goroutine) lazy-spawned on first Enqueue. Different sessions run their
// turns in parallel; each session serialises its own turns.
//
// Architecture (Phase 6 of the injection refactor):
//
//   bot receives msg
//     → bot filters (require_mention, throttle, IsGroupChat)   [platform]
//     → bot resolves session key                               [platform]
//     → bot builds Envelope (with platform-supplied Driver)
//     → bot calls agent.Enqueue(env)                           [hand-off]
//     → agent routes per-session:                              [agent]
//         - in-flight + steer-eligible + CC backend → Inject(SourceSteer)
//         - in-flight + steer-eligible + API backend → AppendSteer
//         - otherwise → push to session channel
//     → per-session worker drains, batches, calls Driver.Drive [agent]
//
// The Driver interface lets the platform-specific renderer/tracker stay
// platform-side while the agent owns the queueing, batching, and routing.

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/relogin"
	"foci/internal/turn"
)

// SteerPreference is a sender's per-message routing choice, overriding the
// agent's steer_mode config for this envelope only. Platforms that offer a
// per-message affordance (the app's send options) set it; everything else
// leaves SteerDefault.
type SteerPreference int

const (
	// SteerDefault follows the agent's steer_mode config.
	SteerDefault SteerPreference = iota

	// SteerAlways steers mid-turn even when steer_mode is off.
	SteerAlways

	// SteerNever queues the message for a fresh turn after the in-flight
	// turn completes. An explicitly-queued message is a turn and nothing
	// else: it is also exempt from the conversational intercepts that would
	// otherwise consume it mid-turn — plan-cancel feedback (#858), pending
	// foci_ask answer capture (#884), and the backend question/elicitation
	// typed-answer intercepts.
	SteerNever
)

// Envelope is a session-resolved, platform-neutral message ready for the
// agent's per-session worker. Constructed by the bot after inbound filtering
// and session-key resolution; consumed by the worker to drive a turn.
type Envelope struct {
	SessionKey  string
	Text        string
	Attachments []platform.Attachment
	UserID      string
	Username    string
	SenderName  string
	ChatID      int64 // platform-side chat/channel ID; opaque to the agent, used by the Driver for renderer/tracker setup
	IsGroupChat bool
	ReceivedAt  time.Time

	// Steer is the sender's per-message steer/queue choice; SteerDefault
	// when the platform offers none. See SteerPreference.
	Steer SteerPreference

	// Original is the platform-specific raw message (e.g. *gotgbot.Message
	// for telegram, *discordgo.Message for discord). Opaque to the agent;
	// the Driver type-asserts to recover it for renderer/reply targeting.
	// Mirrors platform.QueuedMessage.Original.
	Original any

	// Driver is the platform-specific turn execution callback. The
	// session worker invokes Driver.Drive after batching. Set by the
	// bot at Enqueue time so the right platform driver runs the turn
	// (an agent serving telegram + discord has one Driver per platform;
	// each Envelope carries a reference to the Driver from its origin).
	Driver Driver

	// Inject, when non-nil, marks this as a system injection (keepalive,
	// reflection, compaction-resume, inter-session notify, tmux-watch, …)
	// rather than a platform message. The worker runs Inject.Run instead of
	// Driver.Drive, serialising it with this session's platform turns, and
	// (for non-control triggers) defers it while a foci_ask is pending.
	Inject *InjectMeta
}

// InjectMeta carries a system injection's execution closure and classification.
// Run is built where the platform wiring lives (cmd/foci-gw) and does the whole
// HandleMessage + delivery/relay; the worker only decides WHEN to call it, so no
// platform coupling leaks into the agent package.
type InjectMeta struct {
	Trigger string
	Run     func()
}

// controlInjectTriggers are exempt from ask-deferral: they resume/drive the agent
// and must not be held behind a pending ask (which can outlive a compaction).
var controlInjectTriggers = map[string]bool{
	"compaction-resume": true,
	"plan-command":      true,
}

// IsControlInjectTrigger reports whether an injection trigger is a control
// injection (exempt from ask-deferral).
func IsControlInjectTrigger(trigger string) bool { return controlInjectTriggers[trigger] }

// platformApp is the PlatformName reported by the native-app connection
// (internal/app.appConn.PlatformName). The app answers asks via interactive
// frames, so its typed messages bypass the typed-text ask-capture gates.
const platformApp = "app"

// platformName returns the originating platform ("telegram"/"discord"/"app")
// via the Driver's Connection, or "" when unavailable — a nil Driver or a
// Driver exposing no Connection (system-injected envelopes, non-interactive
// drivers, test stubs). "" never matches platformApp, so the default behaviour
// (capture typed answers) is preserved whenever the source can't be resolved.
func (e Envelope) platformName() string {
	if e.Driver == nil {
		return ""
	}
	conn := e.Driver.Connection()
	if conn == nil {
		return ""
	}
	return conn.PlatformName()
}

// Driver is the platform-side handle the per-session worker uses to set up
// rendering and lifecycle for a turn. Each platform bot supplies its own
// implementation (see telegram/discord packages).
//
// The agent worker is platform-agnostic: it batches Envelopes, calls
// Driver.Drive once per batch, and drains orphan steers/late extras as
// follow-up turns. The Driver owns everything platform-specific:
// renderer/tracker construction, sink wiring, cancellable turn context
// (for /stop), OnTurnEnd / OnTurnComplete lifecycle hooks, error
// sanitisation and logging.
type Driver interface {
	// WrapTurn invokes fn (which the agent supplies — typically a closure
	// over Agent.RunTurn) inside whatever platform-side lifecycle the
	// driver wants: typing-active flag, post-turn notification drain,
	// platform-specific OnTurnEnd / OnTurnComplete hooks, error
	// sanitisation, etc. The agent owns the cancellable turn ctx and
	// per-session cancellation; the platform owns its own ancillary
	// state.
	//
	// ctx is the per-turn cancellable context. Implementations may
	// consult ctx.Err() after fn returns to distinguish user cancellation
	// from genuine errors, or thread it into platform-side calls that
	// should cancel when the turn cancels.
	//
	// Implementations should:
	//   - flip any "turn active" indicator on entry and off via defer
	//   - run fn() to execute the turn
	//   - fire post-turn lifecycle hooks (drain notifications, OnTurnEnd,
	//     OnTurnComplete) in whatever order they prefer
	//   - return nil on user cancellation (ctx.Err() != nil after fn
	//     returned) since "Stopped." has already been delivered
	//   - return fn's error otherwise so the agent logs it
	WrapTurn(ctx context.Context, fn func() error) error

	// NewTurnSink constructs the per-turn rendering sink (renderer + tool
	// tracker + streaming sink) for a turn seeded by env. Returns the
	// sink plus a cleanup closure (typically renderer.Cleanup) that the
	// agent invokes via defer after the turn completes.
	//
	// Returning a nil sink signals that the platform can't render this
	// envelope — usually because env.Original isn't the platform's
	// expected message type (e.g. discord receiving a telegram envelope).
	// RunTurn skips silently in that case.
	NewTurnSink(env Envelope) (turnevent.Sink, func())

	// Connection exposes the platform's delivery interface. Used by the
	// agent for session-scoped sinks (late-delivery, notify flows),
	// platform-name discrimination in turn metadata, and cross-session
	// dispatch. Bots typically return themselves.
	Connection() platform.Connection
}

// SteerEntry is a buffered steer message together with the time it was
// received. Preserving receipt time lets orphaned steers (drained after a
// turn completes and rebuilt as follow-up turns) show accurate meta header
// timestamps.
type SteerEntry struct {
	Text       string
	ReceivedAt time.Time
}

// inboxChanSize is the per-session channel buffer. Matches the legacy
// MessageQueue default; over-quota sessions get drop+warn rather than
// blocking the receive path.
const inboxChanSize = 64

// compactionHoldPoll is how often the session worker re-checks IsCompacting
// while holding a dequeued envelope during a /compact turn (#856). Poll, not
// event-wait: clearCompacting has no broadcast and the IsCompacting latch
// self-heals, so a poll cannot miss-wake-wedge. Compaction is brief, so the
// added dispatch latency is bounded by one interval.
const compactionHoldPoll = 100 * time.Millisecond

// sessionInbox is one session's intake — channel, steer buffer, in-flight
// flag, worker goroutine lifecycle. Lazy-created in Agent.getOrCreateInbox
// on first Enqueue for that session key.
//
// Each session runs its own worker goroutine so different sessions on the
// same agent (e.g. multiple users in DMs) proceed in parallel. The worker
// is spawned exactly once via workerStarted (sync.Once) and lives until
// agent shutdown (idle GC of long-quiet sessions deferred to a future
// version — typical agents have O(10) sessions, each goroutine is ~8KB).
type sessionInbox struct {
	sk         string
	ch         chan Envelope
	steerMu    sync.Mutex
	steer      []SteerEntry
	turnActive atomic.Bool

	// injMu guards deferredInjects: proactive injections held while a foci_ask
	// was pending on this session, drained (re-enqueued) when the ask resolves.
	injMu           sync.Mutex
	deferredInjects []Envelope

	workerStarted sync.Once

	// cancelMu guards turnCancel. Set by the session worker before each
	// turn; cleared on turn return. Agent.CancelSession reads under the
	// mutex to fire /stop with race safety against the worker's turn
	// boundaries.
	cancelMu   sync.Mutex
	turnCancel context.CancelFunc

	log *log.ComponentLogger
}

func newSessionInbox(sk string, lg *log.ComponentLogger) *sessionInbox {
	return &sessionInbox{
		sk:  sk,
		ch:  make(chan Envelope, inboxChanSize),
		log: lg,
	}
}

func (inb *sessionInbox) appendSteer(text string, receivedAt time.Time) {
	inb.steerMu.Lock()
	inb.steer = append(inb.steer, SteerEntry{Text: text, ReceivedAt: receivedAt})
	inb.steerMu.Unlock()
}

func (inb *sessionInbox) drainSteer() []SteerEntry {
	inb.steerMu.Lock()
	defer inb.steerMu.Unlock()
	if len(inb.steer) == 0 {
		return nil
	}
	out := inb.steer
	inb.steer = nil
	return out
}

// drainSteerTexts is the Steerer-shaped variant: returns text-only entries
// for use by turn.RunTurn's tool-boundary drain. Receipt timestamps are
// discarded — mid-turn paste doesn't render a meta header.
func (inb *sessionInbox) drainSteerTexts() []string {
	entries := inb.drainSteer()
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Text
	}
	return out
}

// drainAvailable non-blockingly drains all immediately available envelopes
// from this session's channel. Used by the worker to batch consecutive
// messages into one turn.
func (inb *sessionInbox) drainAvailable() []Envelope {
	var out []Envelope
	for {
		select {
		case env := <-inb.ch:
			out = append(out, env)
		default:
			return out
		}
	}
}

// --- Agent integration ---

// SetInboxBackend installs a custom backend resolver. Production code does
// not need this — Agent.Enqueue resolves backends via DelegatedManager
// directly. Tests use this to inject mock delegators without constructing
// a full DelegatedManager.
func (a *Agent) SetInboxBackend(fn func(ctx context.Context, sk string) (delegator.Delegator, error)) {
	a.inboxBackend = fn
}

// SetInboxSteerMode toggles urgent-steer dispatch. When true, mid-turn
// text-only messages route via Backend.ImmediateInject(SourceSteer) (CC-backend) or
// the steer buffer (API-backend). When false, all messages queue to the
// next turn (matches legacy steer_mode=false). Set during agent setup
// from config; default false.
func (a *Agent) SetInboxSteerMode(v bool) {
	a.inboxSteerMode = v
}

// StartInbox initialises the inbox subsystem. ctx is the parent context
// for all per-session worker goroutines — workers exit on its
// cancellation. Idempotent: subsequent calls are no-ops.
//
// Must be called before Enqueue; otherwise envelopes will be buffered in
// orphaned inboxes that never spawn workers (defensive: routing still
// works, the buffer just doesn't drain until StartInbox runs).
func (a *Agent) StartInbox(ctx context.Context) {
	a.inboxesMu.Lock()
	defer a.inboxesMu.Unlock()
	if a.inboxStarted {
		return
	}
	a.inboxCtx = ctx
	if a.inboxes == nil {
		a.inboxes = make(map[string]*sessionInbox)
	}
	a.inboxStarted = true
	// Spawn workers for any inboxes that were created before
	// StartInbox (defensive: tests sometimes Enqueue before Start).
	for _, inb := range a.inboxes {
		inb := inb
		inb.workerStarted.Do(func() {
			go a.sessionWorker(a.inboxCtx, inb)
		})
	}
}

// Enqueue accepts a fully-resolved envelope and routes it to the right
// per-session inbox. The session inbox + worker are lazy-created on first
// call for a given session key.
//
// Enqueue is THE entry point for input to an agent session — the "when" half
// of the pair it forms with delegator.ImmediateInject (the "how": the raw
// write to the backend, which Enqueue's routing and the turn transport call
// once dispatch is safe). Anything that wants a session to process input —
// platform messages, system injections, HTTP sends — comes through here so it
// serialises with in-flight turns, defers behind pending asks, and holds
// through compaction. Only the routing below (urgent steer) and the turn
// transport reach ImmediateInject directly.
//
// Routing:
//
//	in-flight + text-only + CC backend     → Backend.ImmediateInject(SourceSteer)
//	in-flight + text-only + API backend    → AppendSteer (drained at tool boundary)
//	otherwise (idle, or has attachments)   → push to session's channel
//
// Returns true when the envelope was accepted — queued, dispatched, or
// intentionally consumed (steer, plan-cancel, ask answer, re-login code) —
// and false when it was dropped (empty session key, full queue, failed
// dispatch, re-login gate). Fire-and-forget callers may ignore the result;
// callers that wait on the envelope's effect (EnqueueInjectWait) must not.
//
// Drops envelopes with empty session keys (logged) — caller is expected
// to resolve the session key before calling Enqueue.
func (a *Agent) Enqueue(env Envelope) bool {
	if env.SessionKey == "" {
		a.logger().Warnf("inbox: enqueue with empty session key, dropping (text=%dB)", len(env.Text))
		return false
	}

	// CC re-login gate (#843). A 401 on the shared OAuth credential pauses every
	// delegated agent while an automated re-login runs. DelegatedManager != nil
	// is the cheap "this is a delegated (CC) agent" test — it avoids spinning up
	// a backend just to classify. While the gate is active these messages are
	// dropped, except the one capture window where the triggering agent's next
	// message is the pasted-back login code.
	if a.DelegatedManager != nil && relogin.G.Active() {
		if relogin.G.ShouldCapture(a.AgentID) {
			relogin.G.SubmitCode(env.Text)
			a.logger().Infof("inbox: captured CC login code sk=%s", env.SessionKey)
			return true
		}
		a.logger().Warnf("inbox: dropping message during CC re-login sk=%s (%dB)", env.SessionKey, len(env.Text))
		return false
	}

	inb := a.getOrCreateInbox(env.SessionKey)

	// Injections skip all user-message routing (plan-cancel, ask-capture, steer)
	// — they are system turns, not user input. Queue straight to the worker.
	if env.Inject != nil {
		select {
		case inb.ch <- env:
			return true
		default:
			a.logger().Warnf("inbox: queue full for sk=%s, dropping injection trigger=%s", env.SessionKey, env.Inject.Trigger)
			return false
		}
	}

	isActive := inb.turnActive.Load()
	// Compaction hold (#856): a /compact turn is rewriting CC's transcript.
	// Steering or folding a message now makes CC absorb the raw text into the
	// compaction turn (it arrives unframed — no [meta] header). Route to the
	// channel instead; the worker dispatches it as a clean fresh turn once
	// compaction completes.
	compacting := a.IsCompacting(env.SessionKey)

	// Plan-cancel-by-message (#858). A pending ExitPlanMode permission blocks the
	// session: CC waits for Allow/Deny and ignores stdin until it answers. UNLIKE
	// normal permissions — which keep queuing behind WaitForPermission — a typed
	// message during plan approval is revision feedback: deny the plan with the
	// text so CC revises and re-presents, and edit the buttons away (handled by
	// the prompt's cancel listener). Runs before steer/queue routing so it catches
	// the message whether steer mode is on (would otherwise hit ignored stdin) or
	// off (would otherwise queue). Attachments / empty text fall through.
	//
	// SteerNever carve-out: a message the sender explicitly marked "queue" is a
	// turn, not feedback — it must not be consumed here. It queues below and
	// waits for the plan flow (and turn) to resolve.
	if isActive && env.Steer != SteerNever && env.Text != "" && len(env.Attachments) == 0 {
		if be, err := a.resolveSessionBackend(a.inboxCtx, env.SessionKey); err == nil && be != nil {
			if pr, ok := be.(delegator.PlanResponder); ok {
				if reqID := pr.HasPendingPlanPermission(); reqID != "" {
					if err := pr.CancelPlanWithFeedback(reqID, env.Text); err != nil {
						a.logger().Warnf("inbox: plan-cancel-by-message failed sk=%s reqID=%s: %v (falling through)", env.SessionKey, reqID, err)
					} else {
						a.logger().Infof("inbox: plan approval cancelled by follow-up message sk=%s reqID=%s", env.SessionKey, reqID)
						return true
					}
				}
			}
		}
	}

	// Pending-ask-by-message (#884). When a foci_ask is pending (and not /paused)
	// for this session, a typed reply is an ANSWER to it — even while a turn is in
	// flight. Without this, an active turn makes the message steer-eligible below
	// and it folds into the running turn (SourceSteer) instead of answering the
	// ask: the answer-capture in run_turn.go only fires on the IDLE path of a NEW
	// turn, so a pending ask would otherwise lose to ANY in-flight turn (e.g. the
	// user multitasking with the agent while a quiz ask waits). Runs before steer
	// routing, mirroring plan-cancel above; /pause is the escape hatch to steer
	// mid-ask. Idle messages fall through to the channel → run_turn's own capture
	// (gated on isActive here so we don't double-handle the idle case).
	//
	// Platform carve-out: the native app delivers ask-answers out-of-band via
	// interactive-form frames (InteractiveResponse → handleBatchResponse), so a
	// typed message there is ALWAYS meant for the agent, never an answer. Skip
	// typed-text capture for app-sourced turns so quizzes don't swallow ordinary
	// app messages (telegram/discord still capture, since their only "Other"
	// free-text answer channel IS a typed reply).
	//
	// SteerNever carve-out: an explicitly-queued message is a turn, never an
	// answer — same rationale as the plan-cancel carve-out above.
	if isActive && env.Steer != SteerNever && env.Text != "" && len(env.Attachments) == 0 && env.platformName() != platformApp &&
		a.AskRouter != nil && a.AskRouter.PendingForSession != nil && a.AskRouter.HandleResponse != nil {
		if reqID := a.AskRouter.PendingForSession(env.SessionKey); reqID != "" &&
			!(a.AskRouter.IsPaused != nil && a.AskRouter.IsPaused(env.SessionKey)) {
			if answer := strings.TrimSpace(env.Text); answer != "" {
				a.logger().Debugf("inbox: routing mid-turn typed reply to pending ask sk=%s req=%s", env.SessionKey, reqID)
				a.AskRouter.HandleResponse(reqID, answer)
				return true
			}
		}
	}

	// Per-message preference beats the agent's steer_mode config: SteerAlways
	// steers even with the config off, SteerNever queues even with it on. The
	// compaction hold and text-only/attachment constraints still apply — a
	// SteerAlways message can't be folded into a compaction transcript or
	// carry attachments mid-turn.
	steerMode := a.inboxSteerMode
	switch env.Steer {
	case SteerAlways:
		steerMode = true
	case SteerNever:
		steerMode = false
	}
	steerEligible := steerMode && isActive && !compacting && env.Text != "" && len(env.Attachments) == 0

	if steerEligible {
		be, err := a.resolveSessionBackend(a.inboxCtx, env.SessionKey)
		if err != nil {
			a.logger().Warnf("inbox: backend lookup failed sk=%s: %v (falling back to buffer)", env.SessionKey, err)
			inb.appendSteer(env.Text, env.ReceivedAt)
			return true
		}
		if be != nil {
			err := be.ImmediateInject(a.inboxCtx, delegator.Inject{
				Source: delegator.SourceSteer,
				Text:   env.Text,
			})
			switch {
			case err == nil:
				a.logger().Debugf("inbox: urgent dispatch sk=%s sent %dB", env.SessionKey, len(env.Text))
				return true
			case errors.Is(err, delegator.ErrTurnNotInFlight):
				// The turn finished between the turnActive check above and the
				// inject landing. Fall through to the normal idle path so the
				// message starts a properly-tracked turn instead of an untracked
				// one inside the backend.
				a.logger().Debugf("inbox: steer raced turn completion sk=%s, re-routing to idle path", env.SessionKey)
			default:
				// Steering is an optimisation, never a place to lose input: a
				// failed dispatch (dead stdin, protocol error) falls through
				// to the channel push below, so the message runs as a fresh
				// turn once the worker gets to it instead of being dropped.
				a.logger().Warnf("inbox: urgent dispatch sk=%s failed: %v — queueing for a fresh turn instead", env.SessionKey, err)
			}
		} else {
			// API-mode fallback: buffer for next tool-boundary drain.
			inb.appendSteer(env.Text, env.ReceivedAt)
			a.logger().Debugf("inbox: buffered steer sk=%s %dB", env.SessionKey, len(env.Text))
			return true
		}
	}

	// Push to this session's channel; drop with warning if full.
	select {
	case inb.ch <- env:
		return true
	default:
		a.logger().Warnf("inbox: queue full for sk=%s, dropping message (%dB)", env.SessionKey, len(env.Text))
		return false
	}
}

// EnqueueInjectWait enqueues a system injection on sessionKey's inbox and
// blocks until its run closure has completed (or ctx ends). This is the
// synchronous counterpart of enqueueing an Envelope with Inject set: the
// injection serialises with the session's platform turns and defers behind a
// pending foci_ask, so system input never steers an in-flight turn — while
// the caller still gets "the turn has finished" semantics (HTTP /send --sync,
// reflection/keepalive passes that must complete before their scheduler
// continues).
//
// Must NOT be called from the session's own inbox worker (an Inject.Run
// closure or a platform Driver turn): the worker cannot dequeue the new
// envelope while blocked here, so the wait would deadlock. Worker-context
// and nested-turn callers (e.g. the pre-compaction memory hook, which runs
// inside the outer turn's post-turn phase) call HandleMessage directly.
//
// Returns ctx.Err() if ctx ends first; the injection still runs when the
// worker reaches it (Run closures are not cancellable once queued).
func (a *Agent) EnqueueInjectWait(ctx context.Context, sessionKey, trigger string, run func()) error {
	done := make(chan struct{})
	accepted := a.Enqueue(Envelope{
		SessionKey: sessionKey,
		Inject: &InjectMeta{Trigger: trigger, Run: func() {
			defer close(done)
			run()
		}},
	})
	if !accepted {
		return fmt.Errorf("inbox rejected injection for session %s (trigger=%s)", sessionKey, trigger)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resolveSessionBackend returns the delegated backend for sk, or nil for
// API-mode sessions. Test seam: SetInboxBackend overrides this; production
// uses DelegatedManager directly.
func (a *Agent) resolveSessionBackend(ctx context.Context, sk string) (delegator.Delegator, error) {
	if a.inboxBackend != nil {
		return a.inboxBackend(ctx, sk)
	}
	if a.DelegatedManager != nil {
		return a.DelegatedManager.Get(ctx, sk)
	}
	return nil, nil
}

// injectGatePollInterval is the backstop tick for the inject gate's wait loop.
// The adoption edge broadcasts via InFlightWaitCh, but a pending-background-work
// transition (a tracked subagent/Bash completing) has no channel, so the wait
// polls backendAwaitingAutonomousRun at this cadence (ALARP: a poll beats new
// callback plumbing for a rare, bounded wait). Not configurable — a fixed 1s
// tick on an already-blocked worker.
const injectGatePollInterval = 1 * time.Second

// backendAwaitingAutonomousRun is the nil-safe wrapper over
// DelegatedManager.BackendAwaitingAutonomousRun: false for API agents (no
// DelegatedManager) and for idle/non-tracking backends. Used by the inbox
// inject gate to hold system injects across the background-work window (spec §4).
func (a *Agent) backendAwaitingAutonomousRun(sk string) bool {
	if a.DelegatedManager == nil {
		return false
	}
	return a.DelegatedManager.BackendAwaitingAutonomousRun(sk)
}

// waitInjectGate blocks until sk has no delivering work active or pending — an
// adopted autonomous run (IsInFlightDelivering) or the backend's whole
// background-work window (backendAwaitingAutonomousRun, spec §4). Running an
// inject during that window rebinds the shared session sink to the inject's
// NopSink and swallows the run's output (#1068). Returns false if ctx is
// cancelled while waiting (the worker should stop), true when the gate is open.
// The adoption edge broadcasts via InFlightWaitCh; a pending→run→clear
// transition has no channel, so it also polls at injectGatePollInterval. Applied
// at every runInject site — the dequeue path and the post-batch heldInjects loop.
func (a *Agent) waitInjectGate(ctx context.Context, sk string) bool {
	gated := func() bool {
		return a.IsInFlightDelivering(sk) || a.backendAwaitingAutonomousRun(sk)
	}
	if gated() {
		log.Extra("inbox", "gate_wait sk=%s reason=autonomous_or_pending — holding injection until the run and any pending background work clear (#1070/spec§4)", sk)
	}
	for gated() {
		wait := a.InFlightWaitCh(sk)
		select {
		case <-ctx.Done():
			return false
		case <-wait:
			// Adoption edge fired — re-check.
		case <-time.After(injectGatePollInterval):
			// Pending-work transitions have no broadcast — poll.
		}
	}
	return true
}

// InboxTurnActive reports whether the given session has a turn in flight,
// according to the per-session inbox flag. Returns false for unknown
// sessions. Used by tests and diagnostics.
func (a *Agent) InboxTurnActive(sk string) bool {
	inb := a.lookupInbox(sk)
	if inb == nil {
		return false
	}
	return inb.turnActive.Load()
}

// InboxHasPendingInput reports whether the session has user input waiting —
// either queued envelopes on the channel or buffered steer text not yet
// drained. Used by the #845 compaction-resume nudge to avoid self-injecting
// when the user already queued a follow-up (which drives continuation itself).
//
// Best-effort: a message racing in just after this check is harmless — it
// simply queues behind the resume nudge and runs next.
func (a *Agent) InboxHasPendingInput(sk string) bool {
	inb := a.lookupInbox(sk)
	if inb == nil {
		return false
	}
	if len(inb.ch) > 0 {
		return true
	}
	inb.steerMu.Lock()
	defer inb.steerMu.Unlock()
	return len(inb.steer) > 0
}

// DrainInboxSteerTexts returns and clears the API-mode mid-turn buffer for
// the given session. Used as the Steerer for that session's RunTurn call.
// Returns nil for unknown sessions.
func (a *Agent) DrainInboxSteerTexts(sk string) []string {
	inb := a.lookupInbox(sk)
	if inb == nil {
		return nil
	}
	return inb.drainSteerTexts()
}

func (a *Agent) lookupInbox(sk string) *sessionInbox {
	a.inboxesMu.Lock()
	defer a.inboxesMu.Unlock()
	return a.inboxes[sk]
}

func (a *Agent) getOrCreateInbox(sk string) *sessionInbox {
	a.inboxesMu.Lock()
	defer a.inboxesMu.Unlock()
	if a.inboxes == nil {
		a.inboxes = make(map[string]*sessionInbox)
	}
	if inb, ok := a.inboxes[sk]; ok {
		return inb
	}
	inb := newSessionInbox(sk, a.logger())
	a.inboxes[sk] = inb
	if a.inboxStarted {
		inb.workerStarted.Do(func() {
			go a.sessionWorker(a.inboxCtx, inb)
		})
	}
	return inb
}

// sessionWorker drains one session's channel, batches incoming envelopes,
// drives the turn via the platform Driver, then drains orphan steers and
// late-arriving messages as follow-up turns. One goroutine per session.
//
// The worker exits on inboxCtx cancellation (agent shutdown). turnActive
// is flipped around each Driver.Drive call so Enqueue's routing decisions
// see "in-flight" while a turn is running and "idle" between turns.
func (a *Agent) sessionWorker(ctx context.Context, inb *sessionInbox) {
	a.logger().Debugf("inbox: session worker started sk=%s", inb.sk)
	defer func() {
		a.logger().Debugf("inbox: session worker exiting sk=%s", inb.sk)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-inb.ch:
			// System injection: run it serialised with this session's platform
			// turns (the worker is idle between turns here) rather than in a
			// detached goroutine that races them.
			if env.Inject != nil {
				// Hold the injection while a delivering autonomous run is live
				// or its background-work window is open (#1070/spec §4). The
				// worker reaching here while such work is live is exactly the
				// autonomous case (a platform turn occupies the worker
				// synchronously below), never a normal platform turn.
				if !a.waitInjectGate(ctx, env.SessionKey) {
					return
				}
				a.runInject(inb, env)
				continue
			}
			// Sink-delivery gate (TODO #767): if a turn is currently in
			// flight on this session base AND its sink does NOT deliver to
			// a user-facing platform (reflection, keepalive, compaction-
			// memory, session-end-memory — all of which dispatch via
			// handleDelegatedBranch with no sink on ctx), folding this
			// envelope into that turn via the existing RunInference inject
			// path would discard the response. Wait for the non-delivering
			// turn to clear before dispatching a fresh turn whose own sink
			// reaches the user.
			//
			// Each query is independent — the combination "in flight AND
			// NOT delivering = wait" is visible at the call site rather
			// than baked into a single overloaded predicate. While we wait,
			// further envelopes accumulate in inb.ch (buffered) and will
			// be batched together via drainAvailable once the gate opens.
			// In-flight tracking keys directly by session key. A facet
			// envelope gates on the facet's OWN in-flight turn, not the
			// parent root's (a facet runs on its own backend; coupling it to
			// root would be the #719 bug). Root-injected reflection/memory
			// turns run under the root key, so a root envelope still sees
			// them.
			ifk := env.SessionKey
			if a.IsTurnInFlight(env.SessionKey) && !a.IsInFlightDelivering(env.SessionKey) {
				log.Extra("inbox", "gate_wait sk=%s ifk=%s reason=in_flight_non_delivering — holding fresh turn until non-delivering turn clears (#767)", env.SessionKey, ifk)
			}
			for a.IsTurnInFlight(env.SessionKey) && !a.IsInFlightDelivering(env.SessionKey) {
				wait := a.InFlightWaitCh(env.SessionKey)
				select {
				case <-ctx.Done():
					return
				case <-wait:
					// State changed — re-check the predicate.
				}
			}
			// Compaction hold (#856). While a /compact turn is in flight,
			// dispatching a fresh turn writes CC's stdin mid-compaction and the
			// text folds into the compaction transcript unframed. Hold until it
			// clears, then dispatch as a clean turn — further arrivals accumulate
			// in inb.ch and batch via drainAvailable once the gate opens (same as
			// the #767 gate above). Poll rather than event-wait: clearCompacting
			// has no broadcast and the IsCompacting latch self-heals (5-min
			// expiry), so a poll cannot miss-wake-wedge. Compaction is rare and
			// brief; in the common auto path the worker is already past compaction
			// by the time it dequeues, so this never spins.
			for a.IsCompacting(env.SessionKey) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(compactionHoldPoll):
				}
			}
			// turnActive flips true only after the transport has written
			// the primary message to the backend, not the moment we
			// dequeue (TODO #777). This closes the reorder race where a
			// fast follow-up Enqueue could match the steer predicate and
			// reach ccstream's stdin via ImmediateInject(SourceSteer) before the
			// primary's own Inject call completed inside RunInference,
			// stripping the [meta] header off the displaced message. See
			// clutch/docs/inbox-steer-reorder-bug.md.
			//
			// One sync.Once spans the entire driveAndDrainOrphans call so
			// follow-up turns inside the orphan-drain loop don't reset the
			// flag — once the backend has primary, steering stays open for
			// the duration of this batch's processing.
			var once sync.Once
			workerCtx := WithOnPrimaryWritten(ctx, func() {
				once.Do(func() { inb.turnActive.Store(true) })
			})
			// Batch consecutive platform messages into this turn, but hold back
			// any injections drained alongside them — they run individually after
			// the turn (an inject has no Driver and must not enter driveAndDrainOrphans).
			batch := []Envelope{env}
			var heldInjects []Envelope
			for _, d := range inb.drainAvailable() {
				if d.Inject != nil {
					heldInjects = append(heldInjects, d)
				} else {
					batch = append(batch, d)
				}
			}
			steerer := turnevent.SteererFunc(inb.drainSteerTexts)
			heldInjects = append(heldInjects, a.driveAndDrainOrphans(workerCtx, inb, batch, steerer, env)...)
			inb.turnActive.Store(false)
			// The just-finished platform turn may have spawned background work
			// (a subagent / run_in_background Bash) whose autonomous run now owns
			// delivery — so each held inject passes the same gate as the dequeue
			// path, not a direct runInject (the Phase 3 bypass fix).
			for _, inj := range heldInjects {
				if !a.waitInjectGate(ctx, inj.SessionKey) {
					return
				}
				a.runInject(inb, inj)
			}
		}
	}
}

// runInject executes a system injection's closure inside the worker goroutine,
// recovering from panics so a bad injection can't take down the session worker.
// A proactive (non-control) injection arriving while a foci_ask is pending is
// deferred instead — buffered and re-enqueued when the ask resolves (via
// DrainDeferredInjects) — so it can't race the pending ask.
func (a *Agent) runInject(inb *sessionInbox, env Envelope) {
	if !IsControlInjectTrigger(env.Inject.Trigger) && a.askPending(env.SessionKey) {
		inb.injMu.Lock()
		inb.deferredInjects = append(inb.deferredInjects, env)
		inb.injMu.Unlock()
		a.logger().Debugf("inbox: deferred injection sk=%s trigger=%s (ask pending)", inb.sk, env.Inject.Trigger)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			a.logger().Errorf("inbox: inject panic sk=%s trigger=%s: %v", inb.sk, env.Inject.Trigger, r)
		}
	}()
	if env.Inject.Run != nil {
		env.Inject.Run()
	}
}

// askPending reports whether an unpaused foci_ask is awaiting an answer on sk.
func (a *Agent) askPending(sk string) bool {
	r := a.AskRouter
	if r == nil || r.PendingForSession == nil {
		return false
	}
	if r.PendingForSession(sk) == "" {
		return false
	}
	return !(r.IsPaused != nil && r.IsPaused(sk))
}

// DrainDeferredInjects re-enqueues any injections that were deferred while an ask
// was pending on sk, so they run now that it has resolved. Called from the
// ask-resolve hook. Safe on a nil/unknown session.
func (a *Agent) DrainDeferredInjects(sk string) {
	a.inboxesMu.Lock()
	inb := a.inboxes[sk]
	a.inboxesMu.Unlock()
	if inb == nil {
		return
	}
	inb.injMu.Lock()
	held := inb.deferredInjects
	inb.deferredInjects = nil
	inb.injMu.Unlock()
	for _, env := range held {
		a.Enqueue(env)
	}
}

// SetTurnObserver installs a callback fired before each driver.WrapTurn
// invocation, with the batch being driven. Test-only — production wires
// nil. Replaces the recordingDriver.Drive batch-capture pattern after
// TODO #746 Stage C moved batch ownership into the agent.
func (a *Agent) SetTurnObserver(fn func(sk string, batch []Envelope)) {
	a.turnObserver = fn
}

// SetTurnLifecycleHooks wires the session-lifecycle callbacks fired at the
// turn boundary in HandleMessage — onTurnComplete after the turn's completion
// event, onTurnEnd last. Fired for every backend turn regardless of origin
// (platform or system injection), which is why they live on the Agent rather
// than the platform Driver's WrapTurn (injections bypass WrapTurn). Either may
// be nil. Wired once at gateway setup.
func (a *Agent) SetTurnLifecycleHooks(onComplete, onEnd func()) {
	a.onTurnComplete = onComplete
	a.onTurnEnd = onEnd
}

// SetOnCacheExpiry wires the callback fired whenever a session's cache warmth
// changes (see the onCacheExpiry field). Wired once at gateway setup to
// app.SetCacheExpiry so the client's warmth indicator tracks the DB value.
func (a *Agent) SetOnCacheExpiry(fn func(sessionKey string, expiryMs int64)) {
	a.onCacheExpiry = fn
}

// CancelSession cancels the in-flight turn for sk, if any. Used by /stop
// (and any other consumer that needs per-session cancellation precision).
// No-op if the session has no inbox or no turn is currently in flight.
//
// Replaces the old per-bot cancelTurn() which was a single field for all
// sessions on a shared bot — TODO #743's per-session /stop precision.
func (a *Agent) CancelSession(sk string) bool {
	a.inboxesMu.Lock()
	inb, ok := a.inboxes[sk]
	a.inboxesMu.Unlock()
	if !ok {
		return false
	}
	inb.cancelMu.Lock()
	cancel := inb.turnCancel
	inb.cancelMu.Unlock()
	if cancel == nil {
		return false
	}
	a.logger().Infof("CancelSession sk=%s firing turn cancel", sk)
	cancel()
	return true
}

// sessionRouter returns sk's delivery router, building it once (shared across
// every turn on the session and bound into SessionEvents once at backend
// acquisition — #1068 Phase 1). Its fallback (resolvingLateSink) resolves the
// delivering connection at Emit time, so the router is correct for a session
// that connects, disconnects, or reconnects after it was built — no Driver in
// scope, no rebuild.
func (a *Agent) sessionRouter(sk string) *sessionRouter {
	a.routersMu.Lock()
	defer a.routersMu.Unlock()
	return a.sessionRouterLocked(sk)
}

// sessionRouterLocked is sessionRouter's body; callers already holding
// routersMu (AttachDelivery) use it to build the router and SessionEvents in
// one critical section.
func (a *Agent) sessionRouterLocked(sk string) *sessionRouter {
	if r := a.routers[sk]; r != nil {
		return r
	}
	if a.routers == nil {
		a.routers = make(map[string]*sessionRouter)
	}
	r := newSessionRouter(resolvingLateSink{a: a, sk: sk})
	a.routers[sk] = r
	return r
}

// AttachDelivery binds sk's session-scoped delivery callbacks to be. Built once
// per session key (around sk's router) and cached, so a respawned backend
// re-attaches the same closures — the attach happens at backend acquisition
// (setBackendCallbacks), never per turn. Delivery flows through the router;
// thinking deltas also accumulate into a session-scoped buffer that DrainThinking
// empties per turn. Replaces the per-RunInference attach that bound to the ctx
// sink and poisoned concurrent autonomous runs (#1068 Phase 1).
func (a *Agent) AttachDelivery(be delegator.Delegator, sk string) {
	a.routersMu.Lock()
	se := a.sessionEvents[sk]
	if se == nil {
		if a.sessionEvents == nil {
			a.sessionEvents = make(map[string]*delegator.SessionEvents)
		}
		if a.thinkingBufs == nil {
			a.thinkingBufs = make(map[string]*strings.Builder)
		}
		router := a.sessionRouterLocked(sk)
		buf := &strings.Builder{}
		a.thinkingBufs[sk] = buf
		se = &delegator.SessionEvents{
			OnText: func(text string) {
				router.Emit(context.Background(), turnevent.TextBlock{Text: text, Phase: turnevent.PhaseIntermediate})
			},
			OnSubagentStart: func(groupKey, label string) {
				router.Emit(context.Background(), turnevent.SubagentStart{GroupKey: groupKey, Label: label})
			},
			OnSubagentText: func(groupKey, text string) {
				router.Emit(context.Background(), turnevent.SubagentText{GroupKey: groupKey, Text: text})
			},
			OnSubagentEnd: func(groupKey string) {
				router.Emit(context.Background(), turnevent.SubagentEnd{GroupKey: groupKey})
			},
			OnTextDelta: func(delta string) {
				router.Emit(context.Background(), turnevent.TextDelta{Delta: delta})
			},
			OnThinkingDelta: func(delta string) {
				router.Emit(context.Background(), turnevent.ThinkingDelta{Delta: delta})
				buf.WriteString(delta)
			},
			OnToolStart: func(id, name, input string) {
				router.Emit(context.Background(), turnevent.ToolCall{ID: id, Name: name, Args: []byte(input)})
			},
			OnToolEnd: func(id, name, output string, isError bool) {
				router.Emit(context.Background(), turnevent.ToolResult{ID: id, Name: name, Output: output, IsError: isError})
			},
		}
		a.sessionEvents[sk] = se
	}
	a.routersMu.Unlock()
	be.AttachSessionEvents(se)
}

// DrainThinking returns and clears sk's accumulated thinking text (the thinking
// deltas streamed since the last drain). Called at turn completion to log the
// turn's thinking to the conversation DB. Safe without extra synchronisation on
// the buffer: the backend dispatches thinking deltas and OnTurnComplete on the
// same stream-reader goroutine, so writes and this drain never overlap.
func (a *Agent) DrainThinking(sk string) string {
	a.routersMu.Lock()
	defer a.routersMu.Unlock()
	buf := a.thinkingBufs[sk]
	if buf == nil {
		return ""
	}
	s := buf.String()
	buf.Reset()
	return s
}

// resolvingLateSink is the session router's fallback — where events go when no
// per-turn sink is registered (an autonomous run, or text arriving after a turn
// cleared, the rearm-counter scenario from TODO #745). It resolves the current
// delivering connection at Emit time via Agent.ResolveLateConn (route.ConnFor);
// a connection-less session logs the orphaned final text rather than silently
// dropping it (#1038).
type resolvingLateSink struct {
	a  *Agent
	sk string
}

func (s resolvingLateSink) Emit(ctx context.Context, ev turnevent.Event) {
	var conn platform.Connection
	if s.a.ResolveLateConn != nil {
		conn = s.a.ResolveLateConn(s.sk)
	}
	if conn == nil {
		if tc, ok := ev.(turnevent.TurnComplete); ok && strings.TrimSpace(tc.FinalText) != "" {
			s.a.logger().Warnf("late-delivery: no delivering connection for sk=%s — discarded a %d-char reply (it reached no chat)", s.sk, len(tc.FinalText))
		}
		return
	}
	logFn := func(trigger string, err error) {
		s.a.logger().Warnf("late-delivery send failed sk=%s trigger=%s: %v", s.sk, trigger, err)
	}
	// Best-effort conversation-DB logging; no per-turn metadata at this scope.
	newLoggingSink(
		turn.NewSessionSink(conn, s.sk, "late-delivery", turn.WithSessionSinkErrorHandler(logFn)),
		s.a, 0, &TurnMetadata{}, s.sk,
	).Emit(ctx, ev)
}

func (s resolvingLateSink) DeliversToPlatform() bool {
	return s.a.ResolveLateConn != nil && s.a.ResolveLateConn(s.sk) != nil
}

// driveAndDrainOrphans runs a single batched turn plus the orphan/extras
// drain loop. Split out so the worker stays readable. After the primary
// turn completes, any orphan steers (text the user sent during the turn
// that was buffered for tool-boundary drain but never drained because the
// turn was text-only) plus any late arrivals on the channel get
// re-dispatched as follow-up turns until both drains are empty.
//
// Injection envelopes drained alongside late arrivals are returned to the
// caller (the session worker) to run after the batch completes — folding one
// into a follow-up platform batch would silently drop it (its Run closure
// would never fire, and an inject carries no Text for the turn to deliver).
// Mirrors the worker's own held-back filtering at batch time.
func (a *Agent) driveAndDrainOrphans(ctx context.Context, inb *sessionInbox, batch []Envelope, steerer turnevent.Steerer, seed Envelope) []Envelope {
	driver := seed.Driver
	if driver == nil {
		a.logger().Warnf("inbox: no driver on envelope sk=%s, dropping batch (%d msgs)", inb.sk, len(batch))
		return nil
	}
	router := a.sessionRouter(inb.sk)
	a.driveOnce(ctx, inb, batch, steerer, router, driver)
	var heldInjects []Envelope
	for {
		orphans := inb.drainSteer()
		var extras []Envelope
		for _, d := range inb.drainAvailable() {
			if d.Inject != nil {
				heldInjects = append(heldInjects, d)
			} else {
				extras = append(extras, d)
			}
		}
		if len(orphans) == 0 && len(extras) == 0 {
			return heldInjects
		}
		followUp := buildFollowUp(seed, orphans, extras)
		a.logger().Infof("inbox: follow-up sk=%s orphans=%d extras=%d", inb.sk, len(orphans), len(extras))
		a.driveOnce(ctx, inb, followUp, steerer, router, driver)
	}
}

// driveOnce wraps a single turn invocation with a cancellable ctx whose
// cancel func is registered with the inbox so Agent.CancelSession can
// fire it (the per-session /stop path). The cancel is cleared on return
// so post-turn /stop calls become no-ops rather than racing with the
// next turn's setup.
//
// driver.WrapTurn provides the platform-side lifecycle envelope (typing
// indicators, notification drain, OnTurnEnd / OnTurnComplete hooks);
// Agent.RunTurn does the actual turn execution.
func (a *Agent) driveOnce(ctx context.Context, inb *sessionInbox, batch []Envelope, steerer turnevent.Steerer, router *sessionRouter, driver Driver) {
	turnCtx, cancel := context.WithCancel(ctx)
	inb.cancelMu.Lock()
	inb.turnCancel = cancel
	inb.cancelMu.Unlock()
	defer func() {
		inb.cancelMu.Lock()
		inb.turnCancel = nil
		inb.cancelMu.Unlock()
		cancel()
	}()
	if a.turnObserver != nil {
		a.turnObserver(inb.sk, batch)
	}
	err := driver.WrapTurn(turnCtx, func() error {
		return a.RunTurn(turnCtx, inb.sk, batch, steerer, router, driver)
	})
	if err != nil {
		a.logger().Errorf("inbox: driver error sk=%s: %v", inb.sk, err)
	}
}

// buildFollowUp constructs a follow-up batch from orphan steers + late
// extras. Orphan steers inherit the seed envelope's metadata (UserID,
// Username, etc.) because they were buffered fragments of "the same
// conversation"; their receipt timestamps are preserved so meta headers
// render with the original send time, not the drain time. Extras retain
// their own metadata since they're standalone messages that just arrived.
func buildFollowUp(seed Envelope, orphans []SteerEntry, extras []Envelope) []Envelope {
	if len(orphans) == 0 && len(extras) == 0 {
		return nil
	}
	out := make([]Envelope, 0, len(orphans)+len(extras))
	for _, s := range orphans {
		out = append(out, Envelope{
			SessionKey:  seed.SessionKey,
			Text:        s.Text,
			UserID:      seed.UserID,
			Username:    seed.Username,
			SenderName:  seed.SenderName,
			ChatID:      seed.ChatID,
			IsGroupChat: seed.IsGroupChat,
			ReceivedAt:  s.ReceivedAt,
			Original:    seed.Original,
			Driver:      seed.Driver,
		})
	}
	out = append(out, extras...)
	return out
}
