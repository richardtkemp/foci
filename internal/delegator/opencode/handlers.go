// handlers.go — SSE event → SessionEvents / TurnEvents dispatch. This is
// the heart of the opencode backend: it translates opencode's SSE events
// into the same callback shape ccstream produces, so the agent layer's
// turn_delegated.go can drive both backends identically.
//
// handleEvent is wired as the Backend's dispatch handler in Start (via
// SetDispatchHandler). The dispatcher goroutine drains b.events
// and calls handleEvent serially — so all On* methods run on a single
// goroutine per Backend, no internal locking needed for the call itself
// (though they do take turnMu / mu for shared-state access).

package opencode

import (
	"context"
	"encoding/json"
	"strings"

	"foci/internal/delegator"
	"foci/internal/log"
)

// handleEvent is the dispatcher callback. It switches on ev.Type,
// decodes Properties into the matching typed payload, and invokes the
// appropriate On* handler. Unknown events are logged at DEBUG and
// dropped — forward-compatible against new opencode event types.
func (b *Backend) handleEvent(ev rawEvent) {
	log.Debugf(b.logComponent(), "handleEvent: %s", ev.Type)
	switch ev.Type {
	case EventMessagePartUpdated:
		var p eventMessagePartUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode message.part.updated: %v", err)
			return
		}
		b.onMessagePartUpdated(p.Part, p.Delta)

	case EventMessageUpdated:
		var p eventMessageUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode message.updated: %v", err)
			return
		}
		b.onMessageUpdated(p.Info)

	case EventMessageRemoved:
		// Ignored — foci doesn't retract messages.

	case EventSessionIdle:
		var p eventSessionIdle
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode session.idle: %v", err)
			return
		}
		b.onSessionIdle(p.SessionID)

	case EventSessionStatus:
		var p eventSessionStatus
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode session.status: %v", err)
			return
		}
		b.onSessionStatus(p.SessionID, p.Status)

	case EventSessionCompacted:
		var p eventSessionCompacted
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode session.compacted: %v", err)
			return
		}
		b.onSessionCompacted(p.SessionID)

	case EventSessionError:
		var p eventSessionError
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode session.error: %v", err)
			return
		}
		b.onSessionError(p.SessionID, p.Error)

	case EventPermissionUpdated:
		var p eventPermissionUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode permission.updated: %v", err)
			return
		}
		b.onPermissionUpdated(p.Permission)

	case EventPermissionAsked:
		// opencode 1.2.x: properties IS the PermissionRequest (no nested
		// `.permission` wrapper, unlike the legacy permission.updated).
		var req PermissionRequest
		if err := json.Unmarshal(ev.Properties, &req); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode permission.asked: %v", err)
			return
		}
		b.onPermissionAsked(req)

	case EventPermissionReplied:
		var p eventPermissionReplied
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.Warnf(b.logComponent(), "handlers: decode permission.replied: %v", err)
			return
		}
		b.onPermissionReplied(p.SessionID, p.PermissionID, p.Response)

	case EventServerConnected:
		// Already logged by Server.route; no per-Backend action.

	default:
		log.Debugf(b.logComponent(), "handlers: unhandled event %s", ev.Type)
	}
}

// ---------------------------------------------------------------------------
// message.part.updated — text deltas, tool lifecycle, reasoning, subtasks
// ---------------------------------------------------------------------------

func (b *Backend) onMessagePartUpdated(part Part, delta string) {
	// Skip synthetic parts (server-injected UI banners foci doesn't
	// want surfaced as model text).
	if part.Synthetic {
		return
	}

	switch part.Type {
	case PartText:
		b.handleTextPart(part, delta)

	case PartReasoning:
		b.handleReasoningPart(part, delta)

	case PartTool:
		b.handleToolPart(part)

	case PartSubtask:
		b.handleSubtaskPart(part)

	case PartCompaction:
		b.handleCompactionPart()

	default:
		// step-start, step-finish, snapshot, patch, agent, retry, file —
		// not surfaced by foci for v1. Logged at DEBUG for observability.
		log.Debugf(b.logComponent(), "handlers: part type %s ignored", part.Type)
	}
}

// handleTextPart processes text part updates. Two paths:
//   - delta != "" → fire OnTextDelta for streaming display.
//   - part.Time.End != 0 (part complete) → fire OnText with the full
//     text, accumulate into turnText. Tracked via seenTextParts to
//     avoid double-firing if opencode re-sends.
func (b *Backend) handleTextPart(part Part, delta string) {
	if part.Ignored {
		return
	}

	// Streaming delta.
	if delta != "" {
		if se := b.sessionEvents.Load(); se != nil && se.OnTextDelta != nil {
			se.OnTextDelta(delta)
		}
	}

	// Complete text (Time.End set).
	if part.Time != nil && part.Time.End != 0 && part.Text != "" {
		b.turnMu.Lock()
		if b.seenTextParts[part.ID] {
			b.turnMu.Unlock()
			return // already fired OnText for this part
		}
		b.seenTextParts[part.ID] = true

		// Accumulate into turnText. Multiple text parts in one turn
		// (e.g., narration → tool call → narration) are separated by
		// a blank line, matching ccstream's cross-message separation.
		if b.turnText.Len() > 0 {
			b.turnText.WriteString("\n\n")
		}
		b.turnText.WriteString(part.Text)
		b.turnMu.Unlock()

		if se := b.sessionEvents.Load(); se != nil && se.OnText != nil {
			se.OnText(part.Text)
		}
	}
}

// handleReasoningPart fires OnThinkingDelta for reasoning deltas. opencode
// sends reasoning as complete blocks (no incremental delta), so we fire
// on the full text.
func (b *Backend) handleReasoningPart(_ Part, delta string) {
	if delta != "" {
		if se := b.sessionEvents.Load(); se != nil && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(delta)
		}
	}
}

// handleToolPart dispatches tool lifecycle events based on state.status.
// Tool starts are deduped via seenToolCalls (opencode may re-emit
// "running" for the same callID across partial updates).
func (b *Backend) handleToolPart(part Part) {
	if part.State == nil {
		return
	}

	inputJSON := ""
	if len(part.State.Input) > 0 {
		inputJSON = string(part.State.Input)
	}

	switch part.State.Status {
	case ToolStateRunning:
		b.turnMu.Lock()
		seen := b.seenToolCalls[part.CallID]
		if !seen {
			b.seenToolCalls[part.CallID] = true
			b.turnTools++
		}
		b.turnMu.Unlock()
		if seen {
			return // already fired OnToolStart for this callID
		}
		if se := b.sessionEvents.Load(); se != nil && se.OnToolStart != nil {
			se.OnToolStart(part.CallID, part.Tool, inputJSON)
		}

	case ToolStateCompleted:
		if se := b.sessionEvents.Load(); se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(part.CallID, part.Tool, part.State.Output, false)
		}

	case ToolStateError:
		if se := b.sessionEvents.Load(); se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(part.CallID, part.Tool, part.State.Error, true)
		}
	}
}

// handleSubtaskPart surfaces subtask descriptions as blockquoted
// OnSubagentText — mirrors ccstream's SubagentSurfacesBlockquotedText.
// Full subtask streaming is future work; v1 surfaces only the
// description so the user can see what's happening.
func (b *Backend) handleSubtaskPart(part Part) {
	if part.Description == "" {
		return
	}
	quoted := "> " + strings.ReplaceAll(part.Description, "\n", "\n> ")
	if se := b.sessionEvents.Load(); se != nil && se.OnSubagentText != nil {
		se.OnSubagentText(part.ID, quoted)
	}
}

// handleCompactionPart fires when a compaction part arrives — arms
// the compaction waiter so WaitForCompaction unblocks.
func (b *Backend) handleCompactionPart() {
	b.turnMu.Lock()
	if b.compactDoneCh != nil {
		select {
		case <-b.compactDoneCh: // already closed
		default:
			close(b.compactDoneCh)
		}
	}
	b.turnMu.Unlock()
}

// ---------------------------------------------------------------------------
// message.updated — model / usage / error extraction
// ---------------------------------------------------------------------------

func (b *Backend) onMessageUpdated(msg Message) {
	if msg.Role != "assistant" {
		return
	}

	// Store model + provider (guard against empty — partial/streaming
	// messages may omit them; we don't want to overwrite a prior turn's
	// values with empty strings). modelID/providerID arrive as a pair and
	// are reused by sendSummarize to drive /summarize compaction.
	if msg.ModelID != "" {
		b.mu.Lock()
		b.lastModel = msg.ModelID
		if msg.ProviderID != "" {
			b.lastProvider = msg.ProviderID
		}
		b.mu.Unlock()
	}

	// Store usage (input/output/cache). opencode provides per-message
	// token counts directly — no ccstream ModelUsage correction needed.
	if msg.Tokens != nil {
		b.mu.Lock()
		b.lastUsage = &TokenUsage{
			InputTokens:              msg.Tokens.Input,
			OutputTokens:             msg.Tokens.Output,
			CacheReadInputTokens:     msg.Tokens.Cache.Read,
			CacheCreationInputTokens: msg.Tokens.Cache.Write,
		}
		b.mu.Unlock()
	}

	// Error handling. ProviderAuthError fires onAuthFailure (authfail.go
	// wires the callback); MessageAbortedError is expected on /reset
	// hard; other errors are logged.
	if msg.Error != nil {
		b.handleMessageError(msg.Error)
	}

	// Note: we do NOT fire OnTurnComplete here even if msg.finish is
	// set — we wait for session.idle so any straggler part.updated
	// events arrive first (mirrors ccstream's "wait for OnResult, not
	// for the last assistant message" invariant).
}

// handleMessageError dispatches on msg.error.name.
func (b *Backend) handleMessageError(err *MessageError) {
	component := b.logComponent()
	switch err.Name {
	case ErrProviderAuth:
		var data ProviderAuthErrorData
		if json.Unmarshal(err.Data, &data) == nil {
			log.Warnf(component, "auth failure: %s", data.Message)
			if b.server != nil {
				b.server.fanOutAuthFailure(data.Message)
			} else {
				b.fireAuthFailure(data.Message)
			}
		} else {
			log.Warnf(component, "auth failure (unparsable data)")
		}
	case ErrMessageAborted:
		log.Debugf(component, "message aborted (expected on /reset hard)")
	default:
		log.Warnf(component, "message error: %s", err.Name)
	}
}

// ---------------------------------------------------------------------------
// session.idle — turn completion + steerBuf flush
// ---------------------------------------------------------------------------

func (b *Backend) onSessionIdle(sessionID string) {
	// Guard: only our session. Per-session routing means this should
	// always match, but the check is cheap insurance against a routing
	// bug.
	if sessionID != b.sessionID {
		log.Debugf(b.logComponent(), "onSessionIdle: ignored (session=%s, ours=%s)", sessionID, b.sessionID)
		return
	}

	log.Debugf(b.logComponent(), "onSessionIdle: building turn result")

	// Snapshot accumulated turn state.
	b.turnMu.Lock()
	text := b.turnText.String()
	tools := b.turnTools
	turn := b.turnEvents
	ch := b.turnResultCh
	b.turnMu.Unlock()

	b.mu.Lock()
	model := b.lastModel
	usage := b.lastUsage
	b.mu.Unlock()

	// Build TurnResult.
	result := &delegator.TurnResult{
		Text:      text,
		ToolCalls: tools,
		Model:     model,
	}
	if usage != nil {
		result.Usage = &delegator.TurnUsage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
		}
	}

	// Pre-answer nudge gate. If the caller provided a
	// PreAnswerNudgeFunc and it returns non-empty text, re-begin the
	// turn with the follow-up instead of completing. Mirrors
	// ccstream's two-round logic.
	if turn != nil && turn.PreAnswerNudgeFunc != nil {
		if followUp := turn.PreAnswerNudgeFunc(result); followUp != "" {
			log.Debugf(b.logComponent(), "onSessionIdle: pre-answer nudge fired, re-sending")
			b.beginTurn(turn) // reuse same TurnEvents
			_ = b.sendPrompt(context.Background(), followUp, nil)
			return // don't complete — wait for the next session.idle
		}
	}

	// Complete the turn.
	b.turnMu.Lock()
	b.turnEvents = nil
	b.turnActive = false
	b.turnMu.Unlock()

	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}

	// Signal WaitForTurn.
	if ch != nil {
		select {
		case ch <- &ResultMessage{Subtype: "success", Result: text}:
		default:
		}
	}

	// Typing indicator off.
	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	// Flush steerBuf — any user/steer messages that arrived during the
	// turn are sent as a follow-up. The follow-up turn uses a nil
	// TurnEvents for v1 (no OnTurnComplete; text arrives via
	// SessionEvents.OnText which is enough for the user to see the
	// response).
	if err := b.flushSteerBuf(context.Background(), func() *delegator.TurnEvents {
		return nil
	}); err != nil {
		log.Warnf(b.logComponent(), "onSessionIdle: flushSteerBuf: %v", err)
	}
}

// ---------------------------------------------------------------------------
// session.status — typing indicator
// ---------------------------------------------------------------------------

func (b *Backend) onSessionStatus(sessionID string, status SessionStatus) {
	if sessionID != b.sessionID {
		return
	}
	switch status.Type {
	case StatusBusy:
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
	case StatusIdle:
		// session.idle handles completion; this is belt-and-suspenders.
	case StatusRetry:
		log.Debugf(b.logComponent(), "session retrying (attempt %d): %s", status.Attempt, status.Message)
	}
}

// ---------------------------------------------------------------------------
// session.compacted — compaction waiter
// ---------------------------------------------------------------------------

func (b *Backend) onSessionCompacted(sessionID string) {
	if sessionID != b.sessionID {
		return
	}
	// Fire onCompactionDone with 0 — we don't know the pre-compaction
	// token count from this event. A future iteration can fetch it via
	// GET /session/:id/message?limit=1 if the platform layer needs it.
	b.mu.Lock()
	fn := b.onCompactionDone
	b.mu.Unlock()
	if fn != nil {
		fn(0)
	}
	// Close compactDoneCh so WaitForCompaction unblocks.
	b.turnMu.Lock()
	if b.compactDoneCh != nil {
		select {
		case <-b.compactDoneCh:
		default:
			close(b.compactDoneCh)
		}
	}
	b.turnMu.Unlock()
}

// ---------------------------------------------------------------------------
// session.error — auth failure / abort / generic
// ---------------------------------------------------------------------------

func (b *Backend) onSessionError(sessionID string, err *MessageError) {
	if err == nil {
		return
	}
	component := b.logComponent()
	if sessionID != "" {
		log.Debugf(component, "onSessionError: session=%s", sessionID)
	}
	switch err.Name {
	case ErrProviderAuth:
		var data ProviderAuthErrorData
		if json.Unmarshal(err.Data, &data) == nil {
			log.Warnf(component, "session error (auth): %s: %s", data.ProviderID, data.Message)
			if b.server != nil {
				b.server.fanOutAuthFailure(data.Message)
			} else {
				b.fireAuthFailure(data.Message)
			}
		} else {
			log.Warnf(component, "session error (auth): %s", err.Name)
		}
	case ErrMessageAborted:
		log.Debugf(component, "session error (aborted — expected on /reset hard)")
	default:
		log.Warnf(component, "session error: %s", err.Name)
	}

	// End any in-flight turn. A session error (including the synthetic one
	// finalizeExit raises when the opencode subprocess dies) means no
	// session.idle is coming, so without this the Backend's turnActive stays
	// true forever: foci then routes every new message as a steer into the dead
	// turn (never starting a fresh turn → never respawning the server), while the
	// agent layer shows no in-flight turn (so /stop reports nothing to cancel).
	// Completing the turn here re-couples the two markers and lets the session
	// recover (#arnix-perm).
	b.failInFlightTurn(err.Name)
}

// failInFlightTurn force-completes the current turn after an abnormal end
// (session error / subprocess death). It mirrors onSessionIdle's completion but
// delivers the accumulated text (or the error name) rather than waiting for a
// session.idle that will never arrive. No-op if no turn is active.
func (b *Backend) failInFlightTurn(reason string) {
	b.turnMu.Lock()
	if !b.turnActive {
		b.turnMu.Unlock()
		return
	}
	turn := b.turnEvents
	ch := b.turnResultCh
	text := b.turnText.String()
	b.turnEvents = nil
	b.turnActive = false
	b.turnResultCh = nil
	b.turnMu.Unlock()

	if text == "" {
		text = "⚠️ opencode session ended unexpectedly (" + reason + ")"
	}
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(&delegator.TurnResult{Text: text})
	}
	if ch != nil {
		select {
		case ch <- &ResultMessage{Subtype: "error", Result: text}:
		default:
		}
	}
	if b.typingFunc != nil {
		b.typingFunc(false)
	}
	log.Debugf(b.logComponent(), "failInFlightTurn: completed in-flight turn after %s", reason)
}
