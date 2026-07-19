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

	"foci/internal/delegator"
	"foci/internal/log"
)

// handleEvent is the dispatcher callback. It switches on ev.Type,
// decodes Properties into the matching typed payload, and invokes the
// appropriate On* handler. Unknown events are logged at DEBUG and
// dropped — forward-compatible against new opencode event types.
func (b *Backend) handleEvent(ev rawEvent) {
	// Child session events are tagged with childCallID by the subscriber.
	// Route them to a dedicated handler that fires OnSubagentText without
	// touching the parent's turn state (turnText, turnTools, etc.).
	if ev.childCallID != "" {
		b.handleChildEvent(ev)
		return
	}
	log.NewComponentLogger(b.logComponent()).Debugf("handleEvent: %s", ev.Type)
	switch ev.Type {
	case EventMessagePartUpdated:
		var p eventMessagePartUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode message.part.updated: %v", err)
			return
		}
		b.onMessagePartUpdated(p.Part, p.Delta)

	case EventMessagePartDelta:
		var p eventMessagePartDelta
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode message.part.delta: %v", err)
			return
		}
		b.onMessagePartDelta(p)

	case EventMessageUpdated:
		var p eventMessageUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode message.updated: %v", err)
			return
		}
		b.onMessageUpdated(p.Info)

	case EventMessageRemoved:
		// Ignored — foci doesn't retract messages.

	case EventSessionIdle:
		var p eventSessionIdle
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode session.idle: %v", err)
			return
		}
		b.onSessionIdle(p.SessionID)

	case EventSessionStatus:
		var p eventSessionStatus
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode session.status: %v", err)
			return
		}
		b.onSessionStatus(p.SessionID, p.Status)

	case EventSessionCompacted:
		var p eventSessionCompacted
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode session.compacted: %v", err)
			return
		}
		b.onSessionCompacted(p.SessionID)

	case EventSessionError:
		var p eventSessionError
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode session.error: %v", err)
			return
		}
		b.onSessionError(p.SessionID, p.Error)

	case EventPermissionUpdated:
		var p eventPermissionUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode permission.updated: %v", err)
			return
		}
		b.onPermissionUpdated(p.Permission)

	case EventPermissionAsked:
		// opencode 1.2.x: properties IS the PermissionRequest (no nested
		// `.permission` wrapper, unlike the legacy permission.updated).
		var req PermissionRequest
		if err := json.Unmarshal(ev.Properties, &req); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode permission.asked: %v", err)
			return
		}
		b.onPermissionAsked(req)

	case EventPermissionReplied:
		var p eventPermissionReplied
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("handlers: decode permission.replied: %v", err)
			return
		}
		b.onPermissionReplied(p.SessionID, p.PermissionID, p.Response)

	case EventServerConnected:
		// Already logged by Server.route; no per-Backend action.

	default:
		log.NewComponentLogger(b.logComponent()).Debugf("handlers: unhandled event %s", ev.Type)
	}
}

// handleChildEvent processes events rerouted from child (subagent) sessions.
// Only completed text parts are surfaced — via OnSubagentText, keyed by the
// parent tool callID — so the app can display the subagent's output grouped
// with its OnSubagentStart/End. Everything else is dropped. Critically, this
// path never touches the parent's turn state (turnText, turnTools, etc.),
// mirroring ccstream's guard where ParentToolUseID != nil returns before any
// accumulation.
func (b *Backend) handleChildEvent(ev rawEvent) {
	switch ev.Type {
	case EventMessagePartUpdated:
		var p eventMessagePartUpdated
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			return
		}
		// Only surface completed text parts (time.end set). This matches
		// ccstream's per-assistant-message delivery: each text block fires
		// one OnSubagentText call with the full block text.
		if p.Part.Type != PartText || p.Part.Text == "" {
			return
		}
		if p.Part.Time == nil || p.Part.Time.End == 0 {
			return // streaming delta or incomplete — skip for now
		}
		if se := b.sessionEvents.Load(); se != nil && se.OnSubagentText != nil {
			se.OnSubagentText(ev.childCallID, p.Part.Text, 1) // opencode has no reactivation → run 1
		}
		// Keep typing indicator alive during subagent work.
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
	}
}

// ---------------------------------------------------------------------------
// message.part.updated — text deltas, tool lifecycle, reasoning
// ---------------------------------------------------------------------------

func (b *Backend) onMessagePartUpdated(part Part, delta string) {
	// Skip synthetic parts (server-injected UI banners foci doesn't
	// want surfaced as model text).
	if part.Synthetic {
		return
	}

	// Record the part's type so subsequent message.part.delta events
	// (which carry no type, only a partID) can route to the right
	// streaming handler. Guarded by turnMu — beginTurn resets the map
	// on a different goroutine. Released before the type switch below,
	// whose handlers take turnMu themselves (handleTextPart/handleToolPart).
	if part.ID != "" {
		b.turnMu.Lock()
		if b.partTypes == nil {
			// Events can arrive before the first beginTurn (which normally
			// allocates this map) — e.g. a freshly-created session emitting
			// message.part.updated ahead of foci's own turn bookkeeping.
			b.partTypes = make(map[string]string)
		}
		b.partTypes[part.ID] = part.Type
		b.turnMu.Unlock()
	}

	switch part.Type {
	case PartText:
		b.handleTextPart(part, delta)

	case PartReasoning:
		b.handleReasoningPart(part, delta)

	case PartTool:
		b.handleToolPart(part)

	case PartCompaction:
		b.handleCompactionPart()

	default:
		// step-start, step-finish, snapshot, patch, agent, retry, file —
		// not surfaced by foci for v1. Logged at DEBUG for observability.
		log.NewComponentLogger(b.logComponent()).Debugf("handlers: part type %s ignored", part.Type)
	}
}

// onMessagePartDelta routes an incremental content delta (message.part.delta)
// to the streaming handler for the part's type. opencode streams both text and
// reasoning content via these events, separate from the part.updated
// open/close lifecycle. The payload carries no part type — only a partID — so
// the type is resolved from partTypes, which the preceding message.part.updated
// open event populated. Deltas for unknown parts (arrived before the open, or
// synthetic parts we don't track) are dropped at DEBUG.
func (b *Backend) onMessagePartDelta(p eventMessagePartDelta) {
	if p.Delta == "" || p.PartID == "" {
		return
	}
	b.turnMu.Lock()
	partType := b.partTypes[p.PartID]
	b.turnMu.Unlock()
	if partType == "" {
		log.NewComponentLogger(b.logComponent()).Debugf("handlers: part.delta for untracked part %s", p.PartID)
		return
	}
	switch partType {
	case PartText:
		// Part{} has no Time/Text, so handleTextPart fires only the
		// streaming-delta path; the completion path stays bound to the
		// terminal message.part.updated (time.end set).
		b.handleTextPart(Part{}, p.Delta)
	case PartReasoning:
		b.handleReasoningPart(Part{}, p.Delta)
	default:
		// tool, file, etc. — not streamed via deltas.
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
		if b.seenTextParts == nil {
			// See partTypes nil-guard in onMessagePartUpdated: events can
			// arrive before the first beginTurn allocates this map.
			b.seenTextParts = make(map[string]bool)
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

// handleReasoningPart fires OnThinkingDelta for a reasoning fragment. It is
// driven by message.part.delta events (streamed, incremental) — the part is
// ignored because the delta event carries only the fragment, not the part.
// The terminal message.part.updated (reasoning complete) carries no delta and
// is a no-op here; streaming is entirely delta-driven.
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
			if b.seenToolCalls == nil {
				// See partTypes nil-guard in onMessagePartUpdated: events can
				// arrive before the first beginTurn allocates this map.
				b.seenToolCalls = make(map[string]bool)
			}
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
		if part.Tool == taskTool {
			desc := delegator.ExtractAgentDescription(json.RawMessage(inputJSON))
			b.agents.Add(part.CallID, desc)
			if se := b.sessionEvents.Load(); se != nil && se.OnSubagentStart != nil {
				se.OnSubagentStart(part.CallID, desc, "", 1)
			}
		}

	case ToolStateCompleted:
		if se := b.sessionEvents.Load(); se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(part.CallID, part.Tool, part.State.Output, false)
		}
		if part.Tool == taskTool {
			b.agents.Remove(part.CallID)
			if se := b.sessionEvents.Load(); se != nil && se.OnSubagentEnd != nil {
				se.OnSubagentEnd(part.CallID, 1)
			}
		}

	case ToolStateError:
		if se := b.sessionEvents.Load(); se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(part.CallID, part.Tool, part.State.Error, true)
		}
		if part.Tool == taskTool {
			b.agents.Remove(part.CallID)
			if se := b.sessionEvents.Load(); se != nil && se.OnSubagentEnd != nil {
				se.OnSubagentEnd(part.CallID, 1)
			}
		}
	}
}

// handleCompactionPart fires when a compaction part arrives (part.type ==
// "compaction") — the real "compaction started" signal. opencode inserts
// this part at summarize initiation, before the LLM begins streaming the
// summary (~2.5s ahead of the first reasoning token, measured on 1.17.11).
// We close compactStartCh so WaitForCompactionStart unblocks at true
// initiation. compactDoneCh stays open here — it is closed only by
// onSessionCompacted (the session.compacted completion event).
func (b *Backend) handleCompactionPart() {
	b.turnMu.Lock()
	if b.compactStartCh != nil {
		select {
		case <-b.compactStartCh: // already closed
		default:
			close(b.compactStartCh)
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
			log.NewComponentLogger(component).Warnf("auth failure: %s", data.Message)
			if b.server != nil {
				b.server.fanOutAuthFailure(data.Message)
			} else {
				b.fireAuthFailure(data.Message)
			}
		} else {
			log.NewComponentLogger(component).Warnf("auth failure (unparsable data)")
		}
	case ErrMessageAborted:
		log.NewComponentLogger(component).Debugf("message aborted (expected on /reset hard)")
	case ErrAPI:
		var data ApiErrorData
		if json.Unmarshal(err.Data, &data) == nil {
			log.NewComponentLogger(component).Warnf("API error: %s (status=%d, retryable=%v)", data.Message, data.StatusCode, data.IsRetryable)
		} else {
			log.NewComponentLogger(component).Warnf("API error: %s (unparsable data)", err.Name)
		}
	default:
		log.NewComponentLogger(component).Warnf("message error: %s", err.Name)
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
		log.NewComponentLogger(b.logComponent()).Debugf("onSessionIdle: ignored (session=%s, ours=%s)", sessionID, b.sessionID)
		return
	}

	log.NewComponentLogger(b.logComponent()).Debugf("onSessionIdle: building turn result")

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
			log.NewComponentLogger(b.logComponent()).Debugf("onSessionIdle: pre-answer nudge fired, re-sending")
			b.beginTurn(turn) // reuse same TurnEvents
			_ = b.sendPrompt(context.Background(), followUp, nil, b.systemPrompt)
			return // don't complete — wait for the next session.idle
		}
	}

	// Complete the turn.
	b.turnMu.Lock()
	b.turnEvents = nil
	b.turnActive = false
	wasAborting := b.aborting
	b.turnMu.Unlock()

	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}

	// Watchdog: a turn completing with neither text nor tools likely means a
	// stray abort idle was mis-attributed to it — the residual race the abort
	// drain guards against. turn==nil means the turn was already completed
	// (e.g. the aborted turn 1 via failInFlightTurn), so this only fires for a
	// real active turn that produced nothing. Skip during an abort drain.
	if !wasAborting && turn != nil && result.Text == "" && result.ToolCalls == 0 {
		log.NewComponentLogger(b.logComponent()).Warnf("onSessionIdle: turn completed with no text/tools — possible stray abort idle mis-attributed to steered turn")
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

	// Abort-drain: a mid-turn steer aborted the active turn. Count the abort's
	// burst idles (empirically 2 on opencode 1.17.11) and flush the buffered
	// steer once the burst settles. The backstop timer (inject.go) covers a
	// missing/slow burst. Whichever fires first calls abortDrainComplete; the
	// other is a no-op via the aborting flag.
	b.turnMu.Lock()
	if b.aborting {
		b.abortIdlesSeen++
		done := b.abortIdlesSeen >= 2
		n := b.abortIdlesSeen
		b.turnMu.Unlock()
		if done {
			b.abortDrainComplete()
		} else {
			log.NewComponentLogger(b.logComponent()).Debugf("onSessionIdle: abort drain idle %d/2", n)
		}
		return
	}
	b.turnMu.Unlock()

	// Flush steerBuf — any user/steer messages that arrived during the
	// turn are sent as a follow-up. The follow-up turn uses a nil
	// TurnEvents for v1 (no OnTurnComplete; text arrives via
	// SessionEvents.OnText which is enough for the user to see the
	// response).
	if err := b.flushSteerBuf(context.Background(), func() *delegator.TurnEvents {
		return nil
	}); err != nil {
		log.NewComponentLogger(b.logComponent()).Warnf("onSessionIdle: flushSteerBuf: %v", err)
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
		if b.handleRateLimitRetry(status) {
			return
		}
		log.NewComponentLogger(b.logComponent()).Debugf("session retrying (attempt %d): %s", status.Attempt, status.Message)
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
		log.NewComponentLogger(component).Debugf("onSessionError: session=%s", sessionID)
	}
	switch err.Name {
	case ErrProviderAuth:
		var data ProviderAuthErrorData
		if json.Unmarshal(err.Data, &data) == nil {
			log.NewComponentLogger(component).Warnf("session error (auth): %s: %s", data.ProviderID, data.Message)
			if b.server != nil {
				b.server.fanOutAuthFailure(data.Message)
			} else {
				b.fireAuthFailure(data.Message)
			}
		} else {
			log.NewComponentLogger(component).Warnf("session error (auth): %s", err.Name)
		}
	case ErrMessageAborted:
		log.NewComponentLogger(component).Debugf("session error (aborted — expected on /reset hard)")
	case ErrAPI:
		var data ApiErrorData
		if json.Unmarshal(err.Data, &data) == nil {
			log.NewComponentLogger(component).Warnf("session error (API): %s (status=%d, retryable=%v)", data.Message, data.StatusCode, data.IsRetryable)
			// Inject the error message into the turn text so failInFlightTurn
			// delivers it to the user instead of the generic "ended unexpectedly"
			// fallback. Only when no text accumulated — don't clobber partial output.
			b.turnMu.Lock()
			if b.turnText.Len() == 0 && data.Message != "" {
				b.turnText.WriteString("⚠️ " + data.Message)
			}
			b.turnMu.Unlock()
		} else {
			log.NewComponentLogger(component).Warnf("session error (API): %s (unparsable data)", err.Name)
		}
	default:
		log.NewComponentLogger(component).Warnf("session error: %s", err.Name)
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
	tools := b.turnTools
	wasAborting := b.aborting
	b.turnEvents = nil
	b.turnActive = false
	b.turnResultCh = nil
	b.turnMu.Unlock()

	// Watchdog (error half): an active turn dying with no text/tools outside
	// an abort drain is notable — for a steered turn it could indicate a
	// stray abort event mis-attributed. The turn-1 abort itself (aborting=true)
	// legitimately completes with partial/no output, so is suppressed here.
	// MessageAbortedError and the locally-detected rate-limit completion are
	// deliberate POST /abort paths, never unexpected session deaths, so their
	// empty results are also suppressed.
	expectedEmpty := reason == ErrMessageAborted || reason == rateLimitTurnEnd
	if !wasAborting && !expectedEmpty && turn != nil && text == "" && tools == 0 {
		log.NewComponentLogger(b.logComponent()).Warnf("failInFlightTurn: active turn ended with no text/tools on %s — possible premature error on steered turn", reason)
	}

	if text == "" && !expectedEmpty {
		// Deliberate aborts (steer, /reset hard, or rate-limit cancellation) are
		// not unexpected session ends, so the scary fallback is misleading.
		// Deliver whatever partial text accumulated (possibly empty) and
		// complete silently; a steer's follow-up turn follows.
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
	log.NewComponentLogger(b.logComponent()).Debugf("failInFlightTurn: completed in-flight turn after %s", reason)
}
