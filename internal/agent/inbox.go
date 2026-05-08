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
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"
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
	// Implementations should:
	//   - flip any "turn active" indicator on entry and off via defer
	//   - run fn() to execute the turn
	//   - fire post-turn lifecycle hooks (drain notifications, OnTurnEnd,
	//     OnTurnComplete) in whatever order they prefer
	//   - return nil on user cancellation (ctx.Err() != nil after fn
	//     returned) since "Stopped." has already been delivered
	//   - return fn's error otherwise so the agent logs it
	WrapTurn(fn func() error) error

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

	workerStarted sync.Once

	// router is the session-scoped delivery dispatcher (see
	// session_router.go). Lazy-built on the first Drive call by
	// sessionRouterFor — needs the Driver in scope to construct the
	// late-delivery fallback. After construction the field stays alive
	// for the session; only the per-turn sink registered on it changes
	// per turn. Read/written exclusively from the per-session worker
	// goroutine, so a plain pointer is safe.
	router *sessionRouter

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
// text-only messages route via Backend.Inject(SourceSteer) (CC-backend) or
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
// Routing:
//
//	in-flight + text-only + CC backend     → Backend.Inject(SourceSteer)
//	in-flight + text-only + API backend    → AppendSteer (drained at tool boundary)
//	otherwise (idle, or has attachments)   → push to session's channel
//
// Drops envelopes with empty session keys (logged) — caller is expected
// to resolve the session key before calling Enqueue.
func (a *Agent) Enqueue(env Envelope) {
	if env.SessionKey == "" {
		if a.Log != nil {
			a.Log.Warnf("inbox: enqueue with empty session key, dropping (text=%dB)", len(env.Text))
		}
		return
	}
	inb := a.getOrCreateInbox(env.SessionKey)

	isActive := inb.turnActive.Load()
	steerEligible := a.inboxSteerMode && isActive && env.Text != "" && len(env.Attachments) == 0

	if steerEligible {
		be, err := a.resolveSessionBackend(a.inboxCtx, env.SessionKey)
		if err != nil {
			if a.Log != nil {
				a.Log.Warnf("inbox: backend lookup failed sk=%s: %v (falling back to buffer)", env.SessionKey, err)
			}
			inb.appendSteer(env.Text, env.ReceivedAt)
			return
		}
		if be != nil {
			if err := be.Inject(a.inboxCtx, delegator.Inject{
				Source: delegator.SourceSteer,
				Text:   env.Text,
			}); err != nil {
				if a.Log != nil {
					a.Log.Warnf("inbox: urgent dispatch sk=%s failed: %v", env.SessionKey, err)
				}
				return
			}
			if a.Log != nil {
				a.Log.Debugf("inbox: urgent dispatch sk=%s sent %dB", env.SessionKey, len(env.Text))
			}
			return
		}
		// API-mode fallback: buffer for next tool-boundary drain.
		inb.appendSteer(env.Text, env.ReceivedAt)
		if a.Log != nil {
			a.Log.Debugf("inbox: buffered steer sk=%s %dB", env.SessionKey, len(env.Text))
		}
		return
	}

	// Push to this session's channel; drop with warning if full.
	select {
	case inb.ch <- env:
	default:
		if a.Log != nil {
			a.Log.Warnf("inbox: queue full for sk=%s, dropping message (%dB)", env.SessionKey, len(env.Text))
		}
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
	inb := newSessionInbox(sk, a.Log)
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
	if a.Log != nil {
		a.Log.Debugf("inbox: session worker started sk=%s", inb.sk)
	}
	defer func() {
		if a.Log != nil {
			a.Log.Debugf("inbox: session worker exiting sk=%s", inb.sk)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-inb.ch:
			inb.turnActive.Store(true)
			batch := append([]Envelope{env}, inb.drainAvailable()...)
			steerer := turnevent.SteererFunc(inb.drainSteerTexts)
			a.driveAndDrainOrphans(ctx, inb, batch, steerer, env)
			inb.turnActive.Store(false)
		}
	}
}

// SetTurnObserver installs a callback fired before each driver.WrapTurn
// invocation, with the batch being driven. Test-only — production wires
// nil. Replaces the recordingDriver.Drive batch-capture pattern after
// TODO #746 Stage C moved batch ownership into the agent.
func (a *Agent) SetTurnObserver(fn func(sk string, batch []Envelope)) {
	a.turnObserver = fn
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
	if a.Log != nil {
		a.Log.Infof("CancelSession sk=%s firing turn cancel", sk)
	}
	cancel()
	return true
}

// sessionRouterFor returns inb's sessionRouter, lazy-constructing it on
// first call. The fallback sink is built agent-side from the driver's
// platform.Connection — see lateDeliverySink. Called only from the
// per-session worker goroutine, so the read-then-init is single-threaded
// and safe without locking.
func (a *Agent) sessionRouterFor(inb *sessionInbox, driver Driver) *sessionRouter {
	if inb.router != nil {
		return inb.router
	}
	inb.router = newSessionRouter(a.lateDeliverySink(inb.sk, driver))
	return inb.router
}

// lateDeliverySink builds the SessionRouter's fallback sink for sk, the
// destination for events that arrive when no per-turn sink is registered
// (the rearm-counter scenario from TODO #745). Uses the driver's
// platform.Connection if exposed; returns nil otherwise so the router
// falls back to NopSink (appropriate for non-interactive drivers).
//
// Replaces the per-driver NewLateDeliverySink method (TODO #746 Stage D)
// — sink construction is platform-agnostic, so it belongs in the agent.
func (a *Agent) lateDeliverySink(sk string, driver Driver) turnevent.Sink {
	if driver == nil {
		return nil
	}
	conn := driver.Connection()
	if conn == nil {
		return nil
	}
	logFn := func(trigger string, err error) {
		if a.Log != nil {
			a.Log.Warnf("late-delivery send failed sk=%s trigger=%s: %v", sk, trigger, err)
		}
	}
	// Conversation-DB logging for late-delivered text. No per-turn
	// metadata available at this scope (the fallback fires when no per-
	// turn sink is registered) — log with empty user fields, session
	// key only. This is best-effort — late delivery is rare and the
	// session/text fields are the load-bearing identifiers.
	return newLoggingSink(
		turn.NewSessionSink(conn, sk, "late-delivery", turn.WithSessionSinkErrorHandler(logFn)),
		a, 0, &TurnMetadata{}, sk,
	)
}

// driveAndDrainOrphans runs a single batched turn plus the orphan/extras
// drain loop. Split out so the worker stays readable. After the primary
// turn completes, any orphan steers (text the user sent during the turn
// that was buffered for tool-boundary drain but never drained because the
// turn was text-only) plus any late arrivals on the channel get
// re-dispatched as follow-up turns until both drains are empty.
func (a *Agent) driveAndDrainOrphans(ctx context.Context, inb *sessionInbox, batch []Envelope, steerer turnevent.Steerer, seed Envelope) {
	driver := seed.Driver
	if driver == nil {
		if a.Log != nil {
			a.Log.Warnf("inbox: no driver on envelope sk=%s, dropping batch (%d msgs)", inb.sk, len(batch))
		}
		return
	}
	router := a.sessionRouterFor(inb, driver)
	a.driveOnce(ctx, inb, batch, steerer, router, driver)
	for {
		orphans := inb.drainSteer()
		extras := inb.drainAvailable()
		if len(orphans) == 0 && len(extras) == 0 {
			return
		}
		followUp := buildFollowUp(seed, orphans, extras)
		if a.Log != nil {
			a.Log.Infof("inbox: follow-up sk=%s orphans=%d extras=%d", inb.sk, len(orphans), len(extras))
		}
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
	err := driver.WrapTurn(func() error {
		return a.RunTurn(turnCtx, inb.sk, batch, steerer, router, driver)
	})
	if err != nil && a.Log != nil {
		a.Log.Errorf("inbox: driver error sk=%s: %v", inb.sk, err)
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
