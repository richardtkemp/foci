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
// freshTurn distinguishes a GENUINE new logical turn (true — from sendToPane /
// sendToPaneWithAttachments) from a #813 re-arm continuation of the current
// logical turn (false — from reArmForContinuation). On a fresh turn the
// shadow-turn signals (sawFirstResult / continuationExpected / foldPending) are
// reset to a clean slate; on a re-arm they MUST persist so the in-flight
// logical turn keeps recognising its mid-turn re-inits (sawFirstResult stays
// true) — resetting them would break the continuation tracking.
func (b *Backend) beginTurn(turn *delegator.TurnEvents, freshTurn bool) {
	b.turnMu.Lock()
	// Collision canary: a fresh turn starting while we were still awaiting a
	// folded steer's shadow reply means the #813 re-arm protection did NOT keep
	// this session marked in-flight — the shadow reply can be lost to this turn.
	// Should be unreachable post-fix; logged (outside the lock) if it fires.
	collision := b.awaitingShadow
	if freshTurn {
		// Clean slate for a genuine new logical turn (also clears any stale
		// signals a collision left behind).
		b.sawFirstResult = false
		b.continuationExpected = false
		b.foldPending = false
	}
	prevGen := b.turnGen
	var collisionAwaitedFor time.Duration
	var collisionHeldOutput, collisionHeldTextlen int
	if collision {
		b.awaitingShadow = false
		collisionAwaitedFor = time.Since(b.reArmAt).Round(time.Millisecond)
		// Capture the round-1 result now at risk: the still-pending shadow reply
		// for prevGen will be misattributed to this fresh turn and the stashed
		// round-1 content can be lost. Logged below as the collision casualty.
		if b.heldResult != nil {
			if b.heldResult.Usage != nil {
				collisionHeldOutput = b.heldResult.Usage.OutputTokens
			}
			collisionHeldTextlen = len(b.heldResult.Text)
		}
	}
	b.turnActive = true
	b.turnEvents = turn
	b.turnText.Reset()
	b.turnTools = 0
	b.turnResultCh = make(chan *ResultMessage, 1)
	b.turnGen++          // new turn instance; stale watchdog ticks for prior gens no-op
	b.completing = false // fresh turn is unclaimed
	b.turnMu.Unlock()

	if collision {
		b.logger().Extra("steer_shadow event=collision detail=new_turn_began_during_shadow_window prev_gen=%d awaited_for=%s held_output=%d held_textlen=%d",
			prevGen, collisionAwaitedFor, collisionHeldOutput, collisionHeldTextlen)
	}

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
	b.beginTurn(turn, true) // genuine new logical turn: reset shadow-turn signals

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
	b.beginTurn(turn, true) // genuine new logical turn: reset shadow-turn signals

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

// WaitForTurn blocks until the current turn completes (result message received).
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
// All in-flight injections rely on CC's mid-turn drain
// (claude-code/src/query.ts:1570-1589) to fold the message as an
// attachment into the current ask() — no separate ask/result cycle
// is produced for them. The model addresses the message in the same
// turn and the response reaches the original handler.
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
		// In-flight follow-up: SendUser at default priority. CC's mid-turn
		// drain at the next tool boundary (claude-code's
		// query.ts:1570-1589) folds the message as an attachment to the
		// current turn's tool_results — the model responds in the same
		// ask(), so the original turn's OnTurnComplete and the always-live
		// SessionEvents.OnText carry the response. inj.Turn is
		// intentionally ignored.
		//
		// NOTE(#813): deliberately NOT re-armed here — this is the opposite
		// decision from the SourceSteer branch below, and it is intentional.
		// A plain follow-up does NOT create a shadow turn: because it goes in
		// at default priority ("next"), CC drains it at the next tool boundary
		// INTO the running ask(), producing a SINGLE OnResult that delivers the
		// reply in-turn. A steer goes in at "now", which forces an immediate
		// drain that aborts the current ask() and spawns the reply as a second,
		// untracked result (the shadow turn) — that is the only path #813's
		// re-arm exists to protect.
		// Verified by log-mining (2026-06): every in-flight SourceUser inject
		// produced exactly one OnResult(had_turn_events=true, delivered=true);
		// no same-second output=0 abort, no second shadow result — across the
		// 257 in-flight follow-ups in the archives, vs the steer's confirmed
		// double-OnResult signature. (Phase 1's "zero follow-ups" was a sampling
		// artefact of one short window, not the real frequency.)
		// Re-arming here would be a REGRESSION: reArmForContinuation suppresses
		// completion to wait for a second OnResult that never arrives, so the
		// reply would sit idle until the ~45s watchdog fires — a visible delay
		// on a common path, for no benefit.
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
		// ahead of "next"/"later" at the next mid-turn drain, so the
		// steer message folds in before any other queued items without
		// aborting the current ask() and killing in-flight tool work.
		// "Stop right now" semantics live in /reset hard, not Steer.
		//
		// The stdin write makes CC emit an immediate result and then produce
		// the real reply as a SEPARATE result. Mark a pending fold so OnResult
		// re-arms the turn across that gap, keeping it in flight (refcount held,
		// delivering sink attached) — otherwise the reply runs as an untracked
		// shadow turn and can be lost to a colliding inject (#813).
		b.markFoldedInject()
		if err := b.sendUserMessagePriority(inj.Text, "now"); err != nil {
			b.unmarkFoldedInject()
			return err
		}
		return nil

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
