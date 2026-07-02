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
	b.turnMu.Unlock()

	b.mu.Lock()
	b.lastUsage = nil
	b.mu.Unlock()

	// Seed activity timestamp so the idle reaper has an initial deadline
	// rather than polling indefinitely when no events arrive.
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
// Called from Inject's begin-turn path (SourceUser/Steer at idle); not part
// of the public Delegator surface.
func (b *Backend) sendToPane(_ context.Context, prompt string, turn *delegator.TurnEvents) error {
	b.beginTurn(turn)

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
// path when len(inj.Attachments) > 0.
func (b *Backend) sendToPaneWithAttachments(_ context.Context, prompt string, attachments []delegator.Attachment, turn *delegator.TurnEvents) error {
	b.beginTurn(turn)

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
// priority ("now" / "next" / "later"). Used by SourceSteer dispatch so
// the message dequeues ahead of any other queued items at CC's next
// mid-turn drain.
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
//	Steer    | in-flight  | SendUser at priority "now"; CC folds via mid-turn drain
//	Steer    | idle       | begin turn — degrades to User-idle
//	Compact  | any        | send slash command (fire-and-forget)
//	Pass     | any        | send slash command (fire-and-forget)
//
// All in-flight injections land inside CC's current run loop — folded
// into the running ask at a drain point, or (a "now" steer arriving
// mid-stream) answered in a fresh ask cycle of the same run. Either way
// the response belongs to the current foci turn, which completes at the
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
func (b *Backend) Inject(ctx context.Context, inj delegator.Inject) error {
	inFlight := b.IsTurnInFlight()
	b.logger().Debugf("Inject: source=%s text_bytes=%d attachments=%d in_flight=%v",
		inj.Source, len(inj.Text), len(inj.Attachments), inFlight)

	switch inj.Source {
	case delegator.SourceUser:
		if !inFlight {
			return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn)
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
			return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn)
		}
		// In-flight steer: SendUser at priority "now". CC dequeues "now"
		// ahead of "next"/"later"; depending on where the run is it either
		// folds the steer into the current ask (mid-tool arrival) or aborts
		// the ask and answers it in a fresh ask cycle (mid-stream arrival,
		// print.ts abort('interrupt')). Both stay inside the current run, so
		// no bookkeeping is needed here: the turn completes at the run's
		// idle regardless of how many ask cycles the steer minted.
		// "Stop right now" semantics live in /reset hard, not Steer.
		return b.sendUserMessagePriority(inj.Text, "now")

	case delegator.SourceCompact, delegator.SourcePass:
		// Slash commands. Fire-and-forget. The caller is responsible for
		// any synchronisation (e.g. compaction.go arms CompactionWaiter
		// before calling Inject).
		if inFlight {
			b.logger().Warnf("Inject(%s): called with turn in flight — slash command will queue behind active turn", inj.Source)
		}
		return b.sendUserMessage(inj.Text)
	}
	return fmt.Errorf("ccstream: Inject: unknown source %d", inj.Source)
}

// beginTurnWithText starts a new turn, dispatching to the attachments path
// when the inject carries them and to plain text otherwise. Internal to
// Inject — callers reach turn-start through Inject(SourceUser) at idle.
func (b *Backend) beginTurnWithText(ctx context.Context, text string, atts []delegator.Attachment, turn *delegator.TurnEvents) error {
	if len(atts) > 0 {
		return b.sendToPaneWithAttachments(ctx, text, atts, turn)
	}
	return b.sendToPane(ctx, text, turn)
}
