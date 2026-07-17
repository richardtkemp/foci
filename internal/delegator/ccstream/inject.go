package ccstream

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
)

// AttachSessionEvents installs the session-scoped delivery callbacks. Stored
// in atomic.Pointer so concurrent readers (text/tool emission paths) don't
// take turnMu. Idempotent — re-attachment replaces the previous events,
// which is useful in tests and in the AttachSessionEvents-per-Get pattern
// the agent layer uses.
func (b *Backend) AttachSessionEvents(events *delegator.SessionEvents) {
	b.sessionEvents.Store(events)
}

// beginTurn initialises all turn-related state for a new turn. turn carries
// the per-turn bookkeeping callbacks (OnTurnComplete, nudges); may be nil
// for fire-and-forget paths (slash commands, tests) that don't need
// completion signalling.
//
// A turn spans CC's whole run loop: everything CC drains into the run this
// message starts (steers, follow-ups, nudges) belongs to this turn, and the
// turn completes on CC's session_state_changed:idle (see onSessionIdle). A
// begin-turn therefore only happens at CC-idle — Inject routes in-flight
// messages to the fold path instead.
func (b *Backend) beginTurn(turn *delegator.TurnEvents) {
	b.turnMu.Lock()
	b.beginTurnLocked(turn)
	b.turnMu.Unlock()
	b.drainEdgeCallbacks()
	b.resetTurnScratch()
}

// tryBeginTurn begins a new turn only if none is in flight, returning
// ErrTurnInFlight otherwise. The idle check and the turn begin happen under
// one turnMu hold, so two racing SourceSystem injects cannot both begin and
// clobber each other's TurnEvents — exactly one wins; the loser waits for
// completion and retries.
func (b *Backend) tryBeginTurn(turn *delegator.TurnEvents) error {
	// Pending background work (a spawned subagent or run_in_background Bash not
	// yet completed) will chain an autonomous run that owns delivery, so a
	// SourceSystem turn must not begin during the pending window (spec §4).
	// Checked before turnMu — Pending() takes the tracker's own lock, and the
	// retry loop re-checks, so the TOCTOU gap is benign.
	if b.agents.Pending() > 0 {
		return delegator.ErrTurnInFlight
	}
	b.turnMu.Lock()
	if b.turnActive {
		b.turnMu.Unlock()
		return delegator.ErrTurnInFlight
	}
	// Grace after an autonomous run: CC often chains back-to-back autonomous
	// runs with a sub-second idle between them. Deferring here keeps a
	// SourceSystem inject (reflection) out of that gap so it can't steal the
	// next run into its silent sink (#1048).
	if !b.lastAutonomousEnd.IsZero() && time.Since(b.lastAutonomousEnd) < autonomousInjectGrace {
		// Reaching here means Pending()==0 and no active run (earlier returns),
		// so the pending-work gate would NOT have blocked this — the grace is the
		// sole guard. Log once per window (Phase 4 instrument-for-removal, #1048):
		// if production never shows this line, the grace is redundant and can go.
		if !b.lastAutonomousEnd.Equal(b.lastGraceLogEnd) {
			b.lastGraceLogEnd = b.lastAutonomousEnd
			b.logger().Infof("autonomous grace blocked a system inject the pending-work gate would not have (%.1fs since last run end) — #1048 grace instrumented for removal",
				time.Since(b.lastAutonomousEnd).Seconds())
		}
		b.turnMu.Unlock()
		return delegator.ErrTurnInFlight
	}
	b.beginTurnLocked(turn)
	b.turnMu.Unlock()
	b.drainEdgeCallbacks()
	b.resetTurnScratch()
	return nil
}

// AwaitingAutonomousRun reports whether a delivering autonomous run is active,
// pending (a spawned background task not yet completed), or imminently expected
// (within the post-run chain grace). The inbox consults this to hold system
// injects across the whole background-work window (spec §4). Pending() is read
// before turnMu to avoid nesting the tracker lock under turnMu.
func (b *Backend) AwaitingAutonomousRun() bool {
	if b.agents.Pending() > 0 {
		return true
	}
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	// An active autonomous run is now a first-class turn (turnActive), covered by
	// the normal in-flight gate; here we only report the pending/imminent window
	// (background work not yet begun as a turn, or the post-run chain grace).
	return !b.lastAutonomousEnd.IsZero() && time.Since(b.lastAutonomousEnd) < autonomousInjectGrace
}

// beginTurnLocked initialises per-turn state. Caller must hold turnMu.
func (b *Backend) beginTurnLocked(turn *delegator.TurnEvents) {
	b.turnActive = true
	b.turnEvents = turn
	b.turnText.Reset()
	b.turnTools = 0
	b.turnResultCh = make(chan *ResultMessage, 1)
	b.stashedResult = nil
	b.stashedResultMsg = nil
	b.turnOutputTokens = 0
	b.turnCalls = 0
	b.redispatchInFlight = false
}

// AdoptRunningTurn opens a first-class foci turn around a CC run that CC started
// itself (autonomous — no foci send). Mirrors sendToPane minus the SendUser:
// begins the turn (turnActive + turnAutonomous, fresh accumulators) so the
// in-flight run's events, completion, accounting, meta, and nudges flow through
// the normal turn path (#1261). Returns false without adopting if a turn is
// already active — a foci-initiated turn raced this open and owns the run — so
// the caller can unwind the sink/turn-events it prepared. Called by the agent's
// openAutonomousTurn from the running-edge callback, off turnMu.
func (b *Backend) AdoptRunningTurn(turn *delegator.TurnEvents) bool {
	b.turnMu.Lock()
	if b.turnActive {
		b.turnMu.Unlock()
		return false
	}
	b.beginTurnLocked(turn)
	b.turnAutonomous = true
	b.turnMu.Unlock()
	b.resetTurnScratch()
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
	return true
}

// resetTurnScratch clears the non-turnMu turn-start state: cached usage
// (guarded by b.mu) and the activity timestamp seed so the idle reaper has
// an initial deadline rather than polling indefinitely when no events arrive.
func (b *Backend) resetTurnScratch() {
	b.mu.Lock()
	b.lastUsage = nil
	b.mu.Unlock()

	b.touchActivity()
}

// cancelTurn reverses beginTurn on send failure.
func (b *Backend) cancelTurn() {
	b.turnMu.Lock()
	b.turnActive = false
	b.turnEvents = nil
	b.turnMu.Unlock()
}

// sendToPane is the internal begin-turn primitive: starts a fresh turn
// with the given bookkeeping callbacks and sends a plain text user message.
// Called from Inject's begin-turn path (SourceUser/Steer/System at idle);
// not part of the public Delegator surface. exclusive selects tryBeginTurn
// (SourceSystem: fail with ErrTurnInFlight rather than clobber a turn that
// began in a race window) over the unconditional beginTurn.
func (b *Backend) sendToPane(_ context.Context, prompt string, turn *delegator.TurnEvents, exclusive bool) error {
	if exclusive {
		if err := b.tryBeginTurn(turn); err != nil {
			return err
		}
	} else {
		b.beginTurn(turn)
	}

	if b.typingFunc != nil {
		b.typingFunc(true)
	}

	b.logger().Debugf("sendToPane: calling writer.SendUser (%d bytes)", len(prompt))
	sendStart := time.Now()
	if err := b.writer.SendUser(prompt); err != nil {
		b.cancelTurn()
		return fmt.Errorf("ccstream: send user message: %w", err)
	}
	if elapsed := time.Since(sendStart); elapsed > 5*time.Second {
		b.logger().Warnf("sendToPane: writer.SendUser took %s (slow — possible mutex contention or blocked stdin)", elapsed.Round(time.Millisecond))
	} else {
		b.logger().Debugf("sendToPane: writer.SendUser returned in %s", elapsed.Round(time.Millisecond))
	}

	return nil
}

// sendToPaneWithAttachments is the internal begin-turn primitive for
// prompts that carry images/documents. Builds structured content blocks
// (text first, then each attachment as image/document) and sends a single
// user message containing all of them. Called from Inject's begin-turn
// path when len(inj.Attachments) > 0. exclusive as in sendToPane.
func (b *Backend) sendToPaneWithAttachments(_ context.Context, prompt string, attachments []delegator.Attachment, turn *delegator.TurnEvents, exclusive bool) error {
	if exclusive {
		if err := b.tryBeginTurn(turn); err != nil {
			return err
		}
	} else {
		b.beginTurn(turn)
	}

	if b.typingFunc != nil {
		b.typingFunc(true)
	}

	// Build content blocks: text first, then attachments.
	var blocks []ContentBlock
	if prompt != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: prompt})
	}
	for _, att := range attachments {
		blockType := attachmentBlockType(att.MimeType)
		blocks = append(blocks, ContentBlock{
			Type: blockType,
			Source: &ContentBlockSource{
				Type:     "base64",
				MimeType: att.MimeType,
				Data:     base64.StdEncoding.EncodeToString(att.Data),
			},
		})
	}

	b.logger().Debugf("sendToPaneWithAttachments: calling writer.Send (%d blocks)", len(blocks))
	sendStart := time.Now()
	if err := b.writer.Send(NewUserMessageBlocks(blocks)); err != nil {
		b.cancelTurn()
		return fmt.Errorf("ccstream: send user message with attachments: %w", err)
	}
	if elapsed := time.Since(sendStart); elapsed > 5*time.Second {
		b.logger().Warnf("sendToPaneWithAttachments: writer.Send took %s (slow)", elapsed.Round(time.Millisecond))
	} else {
		b.logger().Debugf("sendToPaneWithAttachments: writer.Send returned in %s", elapsed.Round(time.Millisecond))
	}

	return nil
}

// attachmentBlockType returns the CC content block type for a MIME type.
func attachmentBlockType(mimeType string) string {
	if strings.HasPrefix(mimeType, "image/") {
		return "image"
	}
	return "document"
}

// WaitForTurn blocks until the current turn completes (session idle observed,
// or the legacy complete-on-result fallback / process-exit cleanup fired).
// Returns immediately if no turn is in progress.
func (b *Backend) WaitForTurn(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.turnResultCh
	b.turnMu.Unlock()

	if ch == nil {
		return nil
	}

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IsTurnInFlight reports whether a turn callback is registered but hasn't
// fired yet.
func (b *Backend) IsTurnInFlight() bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnActive
}

// sendUserMessage is the internal primitive that writes a user-role
// message to CC at the default priority ("next"). For mid-turn injections
// (follow-up SourceUser, post-tool nudges, slash commands), CC's mid-turn
// drain at the next tool boundary folds the message into the current
// ask() — there is no separate ask/result cycle to wait for.
func (b *Backend) sendUserMessage(text string) error {
	return b.writer.SendUser(text)
}

// sendUserMessagePriority writes a user-role message at the given queue
// priority. CC's classes ("now" > "next" > "later", messageQueueManager.ts):
// "now" additionally aborts the in-flight ask (abort('interrupt')) so it is
// answered immediately in a fresh ask cycle; "next" folds at the next
// mid-turn drain (tool boundary); "later" sits out the run entirely (CC uses
// it for its own background task notifications).
//
// SourceSteer currently sends "next" — same fold point as a follow-up, but
// dequeued through the explicit-priority path so the intent (and this seam)
// stays visible. Using "now" for steers is deliberately NOT wired: it should
// be gated on per-message steer tagging or an aggressive-steer config mode
// (both NYI) — interrupting mid-generation is too disruptive to be every
// steer's default, and "stop right now" already has /reset hard.
func (b *Backend) sendUserMessagePriority(text, priority string) error {
	return b.writer.SendUserPriority(text, priority)
}

// Inject is the canonical entry point for delivering a user-role event to
// CC. It subsumes SendToPane / SendToPaneWithAttachments / SendCommand —
// the routing decision (begin turn vs queue follow-up vs interrupt+queue
// vs slash command) lives in one place rather than being scattered across
// callsites.
//
// Routing matrix:
//
//	Source   | Turn state | Action
//	---------|------------|--------------------------------------------
//	User     | idle       | begin turn (with attachments if provided)
//	User     | in-flight  | SendUser at default priority; CC folds via mid-turn drain
//	Steer    | in-flight  | SendUser at priority "next"; CC folds via mid-turn drain
//	Steer    | idle       | begin turn — degrades to User-idle
//	System   | idle       | begin turn, atomically (tryBeginTurn)
//	System   | in-flight  | ErrTurnInFlight — never folds; caller waits + retries
//	Compact  | any        | send slash command (fire-and-forget)
//	Pass     | any        | send slash command (fire-and-forget)
//
// All in-flight injections land inside CC's current run loop — folded
// into the running ask at the next drain point (tool boundary). The
// response belongs to the current foci turn, which completes at the
// run's session_state_changed:idle (see onSessionIdle).
//
// inj.Turn is required for SourceUser/Steer at idle (a fresh turn needs an
// OnTurnComplete sink). Ignored for in-flight injections (the existing
// TurnEvents persists) and for slash commands. Delivery (text, tool events)
// flows through the SessionEvents installed via AttachSessionEvents — not
// inj.Turn.
//
// inj.Attachments are honored only when beginning a new turn; ignored
// otherwise. They become structured content blocks alongside the text.
func (b *Backend) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	inFlight := b.IsTurnInFlight()
	b.logger().Debugf("Inject: source=%s text_bytes=%d attachments=%d in_flight=%v",
		inj.Source, len(inj.Text), len(inj.Attachments), inFlight)

	switch inj.Source {
	case delegator.SourceUser:
		if !inFlight {
			return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn, false)
		}
		// In-flight follow-up: SendUser at default priority ("next"). CC
		// drains it into the running turn (at the next tool boundary, or as
		// a fresh ask cycle in the same run) — either way it stays inside
		// the current run, so the reply belongs to the current foci turn and
		// the turn completes at that run's idle. inj.Turn is intentionally
		// ignored.
		return b.sendUserMessage(inj.Text)

	case delegator.SourceSteer:
		if !inFlight {
			// Steer at idle — the turn this steer meant to interrupt finished
			// in the window between the inbox's turnActive check and this
			// dispatch. The inbox builds steers with no inj.Turn, so beginning
			// a turn here would run it untracked (nil TurnEvents → no
			// OnTurnComplete, lost usage/compaction). Decline and let the caller
			// re-route through the normal idle path, which supplies a Turn.
			// If a steer *does* carry its own Turn, it is safe to begin.
			if inj.Turn == nil {
				b.logger().Debugf("Inject(Steer): no turn in flight and no inj.Turn, returning ErrTurnNotInFlight for re-route")
				return delegator.ErrTurnNotInFlight
			}
			b.logger().Debugf("Inject(Steer): no turn in flight, beginning tracked turn from inj.Turn")
			return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn, false)
		}
		// In-flight steer: SendUser at priority "next" — CC folds it into
		// the running ask at the next mid-turn drain (tool boundary),
		// matching CC's own class for user input. It stays inside the
		// current run, so no bookkeeping is needed here: the turn completes
		// at the run's idle. Priority "now" (abort the in-flight ask, answer
		// immediately) is deliberately not used — see sendUserMessagePriority
		// for the NYI gating it should live behind. "Stop right now"
		// semantics live in /reset hard, not Steer.
		return b.sendUserMessagePriority(inj.Text, "next")

	case delegator.SourceSystem:
		// System-initiated text (foci send, cron, notifications, error and
		// restart injections) never folds into a running turn — only real
		// user input may steer. tryBeginTurn makes the idle check and turn
		// begin atomic; when a turn is in flight the caller receives
		// ErrTurnInFlight, waits for completion, and retries.
		return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn, true)

	case delegator.SourceCompact:
		// The /compact slash command. Fire-and-forget in the sense that no
		// OnTurnComplete delivers a reply to the user — but the run it starts
		// must still claim turnActive before CC transitions to "running", or
		// that transition looks identical to a spontaneous CC-initiated run
		// and the running-edge autonomous-adoption path (onAutonomousOpen)
		// adopts it as a first-class turn (#1266). That adoption
		// unconditionally Registers its own sink on the session router,
		// clobbering the registration the CALLER's still-in-flight turn owns
		// (compaction runs synchronously inside that turn's post-turn hook,
		// see agent.RunCompaction) — so when the caller's own deferred
		// TurnComplete eventually fires (after compaction finishes), the
		// router has already been cleared by the adopted run and the event
		// falls through to the late-delivery fallback, re-sending the
		// already-delivered pre-compaction reply as a duplicate message.
		// beginTurn(nil) claims turnActive with no TurnEvents — no
		// bookkeeping or delivery is needed for compaction's own output —
		// and the existing nil-turn path in completeTurn/onSessionIdle
		// clears it normally once CC's compaction idle fires. Skipped when
		// already in flight: that path exists for CC's own queue to
		// serialise the slash command behind the active turn, and
		// beginTurn(nil) would wrongly reset ITS bookkeeping.
		if inFlight {
			b.logger().Warnf("Inject(%s): called with turn in flight — slash command will queue behind active turn", inj.Source)
			return b.sendUserMessage(inj.Text)
		}
		b.beginTurn(nil)
		return b.sendUserMessage(inj.Text)

	case delegator.SourcePass:
		// Other slash commands (/context, /model, etc). Fire-and-forget:
		// response (if any) flows through the agent's normal stream events.
		if inFlight {
			b.logger().Warnf("Inject(%s): called with turn in flight — slash command will queue behind active turn", inj.Source)
		}
		return b.sendUserMessage(inj.Text)
	}
	return fmt.Errorf("ccstream: Inject: unknown source %d", inj.Source)
}

// beginTurnWithText starts a new turn, dispatching to the attachments path
// when the inject carries them and to plain text otherwise. Internal to
// Inject — callers reach turn-start through Inject(SourceUser/System) at
// idle. exclusive selects the atomic begin-if-idle path (SourceSystem).
func (b *Backend) beginTurnWithText(ctx context.Context, text string, atts []delegator.Attachment, turn *delegator.TurnEvents, exclusive bool) error {
	if len(atts) > 0 {
		return b.sendToPaneWithAttachments(ctx, text, atts, turn, exclusive)
	}
	return b.sendToPane(ctx, text, turn, exclusive)
}
