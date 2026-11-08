package codex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/delegator"
	"foci/internal/delegator/sessionenv"
)

// onThreadStarted records the transcript path Codex associates with a thread.
func (b *Backend) onThreadStarted(params *threadStartedParams) {
	if params.Thread.ID == "" {
		return
	}
	// Batch-only app-server instances and ephemeral multiplexed threads must
	// never become the backend's interactive root. The root is established
	// only by Start's explicit session mapping.
	if b.startOpts.BatchOnly {
		return
	}
	b.mu.Lock()
	root := b.threadID == "" || b.threadID == params.Thread.ID
	if root {
		b.threadID = params.Thread.ID
		if params.Thread.Path != "" {
			b.sessionFilePath = params.Thread.Path
		}
		if params.Thread.Status.Type != "" {
			b.threadStatus = params.Thread.Status
		}
	}
	b.mu.Unlock()
	if root && b.startOpts.SessionKey != "" {
		b.registerThread(b.startOpts.SessionKey, params.Thread.ID)
	}
}

func (b *Backend) onThreadStatusChanged(params *threadStatusChangedParams) {
	if params.Status.Type == "" {
		return
	}
	b.mu.Lock()
	b.threadStatus = params.Status
	b.mu.Unlock()
}

// onTurnStarted signals the typing indicator.
func (b *Backend) onTurnStarted() {
	b.itemMu.Lock()
	if b.itemCache != nil {
		b.itemCache = make(map[string]itemEnvelope)
	}
	b.itemMu.Unlock()
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
}

// onTurnCompleted finalises the turn.
func (b *Backend) onTurnCompleted(params *turnCompletedParams) {
	// Read turnText under turnMu: onItemCompleted writes them under
	// turnMu from the reader goroutine and completeTurn Resets turnText under
	// turnMu from the agent goroutine (e.g. a turn/start >30s timeout firing
	// while the turn is still running) — reading the strings.Builder without
	// the lock is a real data race.
	b.mu.Lock()
	model := b.model
	threadName := b.threadName
	b.mu.Unlock()
	model = qualifiedModel(model)

	// A failed turn's error (e.g. "Your workspace is out of credits") must
	// reach the user, not just the log. Deliver it the way ccstream delivers
	// CC's synthetic API-error message: as assistant text — streamed via
	// OnText and part of the turn's final text — so the chat shows why the
	// turn produced nothing. A clean completion clears the status-block error.
	failMsg := ""
	if params.Turn.Status == "failed" {
		failMsg = "unknown error"
		if params.Turn.Error != nil && strings.TrimSpace(params.Turn.Error.Message) != "" {
			failMsg = strings.TrimSpace(params.Turn.Error.Message)
		}
		b.logWarnf("turn failed: %s", failMsg)
	}
	b.mu.Lock()
	switch params.Turn.Status {
	case "failed":
		b.lastTurnError = failMsg
	case "completed":
		b.lastTurnError = ""
	}
	b.mu.Unlock()

	errText := ""
	if failMsg != "" {
		errText = turnFailedText(failMsg)
	}

	b.turnMu.Lock()
	if errText != "" {
		if b.turnText.Len() > 0 {
			b.turnText.WriteString("\n\n")
		}
		b.turnText.WriteString(errText)
	}
	usage := b.turnUsageLocked()
	text := b.turnText.String()
	b.turnMu.Unlock()

	if errText != "" {
		if se := b.sessionEvents.Load(); se != nil && se.OnText != nil {
			se.OnText(errText)
		}
	}
	result := &delegator.TurnResult{
		Text:       text,
		Usage:      usage,
		Model:      model,
		ThreadName: threadName,
	}
	b.completeTurn(result)
}

// onItemStarted maps a Codex item/started notification to SessionEvents.
func (b *Backend) onItemStarted(params *itemStartedParams) {
	var item itemEnvelope
	if err := json.Unmarshal(params.Item, &item); err != nil {
		b.logWarnf("dropping malformed item in item/started: %v", err)
		return
	}

	// Stash by ID for approval-request correlation.
	b.itemMu.Lock()
	if b.itemCache != nil {
		b.itemCache[item.ID] = item
	}
	b.itemMu.Unlock()

	se := b.sessionEvents.Load()
	switch item.Type {
	// Tools — feed OnToolStart so the activity indicator shows what's running.
	case "commandExecution":
		if se != nil && se.OnToolStart != nil {
			// Unwrapped: codex reports the argv it built from the tool input,
			// which for a bash call is foci's session-env wrapper. Agents and
			// users must see the command they actually asked for.
			se.OnToolStart(item.ID, "bash", sessionenv.UnwrapDisplayCommand(item.Command))
		}
	case "fileChange":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "edit", summarizePaths(item.Changes))
		}
	case "mcpToolCall":
		if se != nil && se.OnToolStart != nil {
			name := "mcp:" + item.Server + "." + item.Tool
			se.OnToolStart(item.ID, name, truncateArgs(item.Arguments))
		}
	case "dynamicToolCall":
		if se != nil && se.OnToolStart != nil {
			name := item.Tool
			if item.Namespace != "" {
				name = item.Namespace + "." + item.Tool
			}
			se.OnToolStart(item.ID, name, truncateArgs(item.Arguments))
		}
	case "webSearch":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "web_search", item.Query)
		}
	case "imageGeneration":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "image_gen", "")
		}
	case "contextCompaction":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, delegator.CompactionToolName, "")
		}
	case "sleep":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, sleepToolName, sleepDetail(item.DurationMs))
		}
	default:
		if nonToolItemTypes[item.Type] {
			return
		}
		b.noteUnmappedItemType(item.Type)
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, item.Type, "")
		}
	}
}

// sleepToolName is the name foci shows for codex's "sleep" item: the tool
// the model called (clock.sleep), which is also the item's kind in the
// rollout file ({"type":"Extension","kind":"clock.sleep"}). On the
// app-server wire the same item arrives as {"type":"sleep","durationMs":N}
// on both item/started and item/completed (live probe, codex 0.167.0, #2955).
const sleepToolName = "clock.sleep"

// sleepDetail renders a sleep item's duration as the tool's detail ("45s").
func sleepDetail(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

// nonToolItemTypes are the codex ThreadItem types that are not tool calls
// (conversation content, markers, or subagent activity foci has disabled).
// Every other type without its own case shows as a tool named by its type,
// so a tool codex adds later is visible instead of reading as "thinking"
// (#2955). Taken from the codex 0.166.0 generated schema (ThreadItem oneOf).
var nonToolItemTypes = map[string]bool{
	"userMessage":         true,
	"agentMessage":        true,
	"reasoning":           true,
	"plan":                true,
	"hookPrompt":          true,
	"functionCallOutput":  true,
	"subAgentActivity":    true,
	"collabAgentToolCall": true,
	"enteredReviewMode":   true,
	"exitedReviewMode":    true,
}

// loggedItemTypes holds the unmapped item types already logged, so each new
// type is logged once per process rather than once per item.
var loggedItemTypes sync.Map

// noteUnmappedItemType logs, once per type, that an item type with no
// mapping was shown as a generic tool.
func (b *Backend) noteUnmappedItemType(typ string) {
	if _, seen := loggedItemTypes.LoadOrStore(typ, true); !seen {
		b.logInfof("unmapped item type %q shown as a tool by its type name", typ)
	}
}

// onItemCompleted maps a Codex item/completed notification to SessionEvents.
func (b *Backend) onItemCompleted(params *itemCompletedParams) {
	var item itemEnvelope
	if err := json.Unmarshal(params.Item, &item); err != nil {
		b.logWarnf("dropping malformed item in item/completed: %v", err)
		return
	}
	// Mark the cached entry finished, so inFlightCall never answers a call
	// that already has its result (#2617).
	b.itemMu.Lock()
	c, started := b.itemCache[item.ID]
	if started {
		c.Status = "completed"
		b.itemCache[item.ID] = c
	}
	b.itemMu.Unlock()

	se := b.sessionEvents.Load()
	switch item.Type {
	case "agentMessage":
		// Only accumulate into the turn result when the phase isn't
		// "commentary" (mid-turn narration ahead of a tool call) — live
		// verified against codex app-server 0.144.5 (generate-json-schema +
		// a live turn/start->turn/steer->turn/completed probe) that
		// agentMessage items carry phase "commentary" or "final_answer".
		// A missing/empty phase (older codex, or a provider that doesn't
		// emit it — the schema's own doc calls this out) keeps the
		// pre-existing behaviour: accumulate. Commentary still reaches the
		// live view via OnText below, just excluded from the final text.
		text := item.Text
		if item.Phase != "commentary" {
			b.turnMu.Lock()
			b.turnText.WriteString(text)
			b.turnMu.Unlock()
		}
		if se != nil && se.OnText != nil {
			se.OnText(text)
		}

	case "commandExecution":
		if se != nil && se.OnToolEnd != nil {
			isError := item.Status == "failed"
			se.OnToolEnd(item.ID, "bash", "", isError)
		}

	case "fileChange":
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "edit", "", item.Status == "failed")
		}

	case "mcpToolCall":
		if se != nil && se.OnToolEnd != nil {
			name := "mcp:" + item.Server + "." + item.Tool
			se.OnToolEnd(item.ID, name, "", item.Status == "failed")
		}

	case "dynamicToolCall":
		if se != nil && se.OnToolEnd != nil {
			name := item.Tool
			if item.Namespace != "" {
				name = item.Namespace + "." + item.Tool
			}
			se.OnToolEnd(item.ID, name, "", !itemSuccess(item))
		}

	case "webSearch":
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "web_search", "", false)
		}

	case "imageGeneration":
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "image_gen", "", item.Status == "failed")
		}

	case "reasoning":
		if se != nil && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(item.Text)
		}

	case "contextCompaction":
		// Shown as a tool under delegator.CompactionToolName, which the agent
		// layer's tool count skips: compaction is internal bookkeeping, not a
		// call the model made. It is booked as a compaction turn (#2825), so
		// the ledger no longer reports the pre-compaction fill as current.
		b.bookCompaction(time.Now().UTC())
		b.compactMu.Lock()
		if b.compactDoneCh != nil {
			close(b.compactDoneCh)
			b.compactDoneCh = nil
		}
		b.compactMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, delegator.CompactionToolName, "", false)
		}

	case "sleep":
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, sleepToolName, "", item.Status == "failed")
		}

	default:
		// Ends only a generic tool onItemStarted showed: an unmapped item
		// that arrives on item/completed alone had no running period, and an
		// end with no start would be a stray event.
		if !started || nonToolItemTypes[item.Type] {
			return
		}
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, item.Type, "", item.Status == "failed")
		}
	}
}

// itemSuccess returns true if a dynamic tool call succeeded.
func itemSuccess(item itemEnvelope) bool {
	if item.Status != "" {
		return item.Status == "completed" || item.Status == "success"
	}
	return true
}

// maxSummarizedPaths caps how many file paths summarizePaths joins into a
// single string, so a large patch (hundreds of changed files) can't blow up
// the activity indicator or, more importantly, the fileChange approval
// prompt text sent to the user's chat.
const maxSummarizedPaths = 10

// summarizePaths extracts a comma-separated list of file paths from a
// fileChange item's changes array for the activity indicator and approval
// prompt. Bounded to maxSummarizedPaths entries — an unbounded join of a
// large changeset previously produced unbounded approval text.
func summarizePaths(changes []fileChangeEntry) string {
	if len(changes) == 0 {
		return ""
	}
	shown := changes
	truncated := 0
	if len(changes) > maxSummarizedPaths {
		shown = changes[:maxSummarizedPaths]
		truncated = len(changes) - maxSummarizedPaths
	}
	parts := make([]string, 0, len(shown))
	for _, c := range shown {
		parts = append(parts, c.Path)
	}
	out := strings.Join(parts, ", ")
	if truncated > 0 {
		out += fmt.Sprintf(" (+%d more)", truncated)
	}
	return out
}

// maxTruncateArgsLen caps truncateArgs' output length in bytes.
const maxTruncateArgsLen = 200

// truncateArgs returns a truncated copy of raw JSON arguments for display.
// Truncates at a rune boundary, not a raw byte offset: raw JSON arguments
// can contain multibyte UTF-8 (non-ASCII string values), and slicing at a
// fixed byte offset can split a rune in half, producing invalid UTF-8 for
// the activity indicator.
func truncateArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) <= maxTruncateArgsLen {
		return s
	}
	cut := maxTruncateArgsLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// onAgentMessageDelta delivers a streaming text delta for live display only.
//
// This does NOT accumulate into turnText. It used to (WriteString on every
// delta), and onItemCompleted's "agentMessage" case ALSO writes the
// completed item's full text into turnText — live-verified (codex 0.144.5)
// that a completed agentMessage's `text` is exactly the concatenation of
// its own deltas, not additional content, so every message's contribution
// to TurnResult.Text (and therefore the delivered final answer) was
// silently doubled. Found while live-verifying phase semantics for #1329
// item 6; distinct from but entangled with that fix (phase-filtering the
// doubled text would have just doubled the filtered result too).
//
// Trade-off: if the reader stops mid-message (process death / disconnect
// before item/completed fires), the interrupted-turn fallback text
// (onReaderStopped) now loses that last partial message instead of
// contributing a partial-but-doubled string — an acceptable cost in a rare
// path for correctness on every normal turn.
func (b *Backend) onAgentMessageDelta(params *agentMessageDeltaParams) {
	delta := params.Delta
	// A lost thread's streamed message is marked once, on its first delta,
	// matching the marked completed text (lostMarked).
	if b.lostThread != "" && b.lostDeltaThread != b.lostThread {
		delta = b.lostMarked(delta)
		b.lostDeltaThread = b.lostThread
	}
	se := b.sessionEvents.Load()
	if se != nil && se.OnTextDelta != nil {
		se.OnTextDelta(delta)
	}
}

// onTokenUsage handles one API cycle's usage. codex sends this notification
// once per cycle (see tokenUsageParams). The cycle is booked in the cost
// ledger as one call (bookCycle), and its counts are kept as the latest
// cycle's context fill (stashedUsage), which GetContextWindow and compaction
// read — never summed (#1855).
func (b *Backend) onTokenUsage(params *tokenUsageParams) {
	// codex/OpenAI token semantics differ from Anthropic's: cachedInputTokens
	// is a SUBSET of inputTokens (live-verified against codex 0.144.5 rollout
	// token_count entries: input_tokens=14550 with cached_input_tokens=8960,
	// and total_tokens == input_tokens + output_tokens — the cached count is
	// already included in inputTokens, not additive). foci's downstream
	// context-fullness (internal/compaction/compact.go: input + cacheRead +
	// cacheWrite) and cost math are Anthropic-style additive, so we subtract
	// the cached portion out of InputTokens here. Reporting it in both fields
	// otherwise double-counts the cache: context occupancy inflates (premature
	// auto-compaction) and cost double-charges the cached tokens.
	b.checkTokenUsage(params)
	// Each notification is one API cycle: one call (#2111 P2).
	b.bookCycle(params)
	last := params.TokenUsage.Last
	inputTokens := last.InputTokens - last.CachedInputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	u := &delegator.TurnUsage{
		InputTokens:              inputTokens,
		OutputTokens:             last.OutputTokens,
		CacheReadInputTokens:     last.CachedInputTokens,
		CacheCreationInputTokens: last.CacheWriteInputTokens,
	}
	b.turnMu.Lock()
	if params.TurnID != "" && params.TurnID != b.usageTurnID {
		b.resetTurnUsageLocked()
		b.usageTurnID = params.TurnID
	}
	b.stashedUsage = u
	turn := b.turnEvents
	b.turnMu.Unlock()
	// One API cycle is the agent layer's cache-warmth evidence (#2509).
	turn.ReportModelUsage(*u)

	if params.TokenUsage.ModelContextWindow > 0 {
		b.mu.Lock()
		b.contextWindow = params.TokenUsage.ModelContextWindow
		b.mu.Unlock()
	}
}

// resetTurnUsageLocked clears the per-turn usage state. Caller holds turnMu.
func (b *Backend) resetTurnUsageLocked() {
	b.stashedUsage = nil
	b.usageTurnID = ""
}

// turnUsageLocked is the completed turn's usage: the final cycle's context
// fill. The turn's spend is already in the ledger, one call per cycle, and
// the agent reads the turn's cost from there. Caller holds turnMu.
func (b *Backend) turnUsageLocked() *delegator.TurnUsage {
	if b.stashedUsage == nil {
		return nil
	}
	u := *b.stashedUsage
	return &u
}

// onServerRequestResolved handles a codex-side resolution of a pending
// approval (abort / steer / timeout). It deletes the matching pendingPerms
// entry keyed by the resolved requestId, then fires onPromptsCleared if no
// more pending approvals remain.
//
// Previously the requestId was ignored, so a codex-side resolution left a
// stale pendingPerms entry: onPromptsCleared never fired (leaving a dead
// approval button in the chat), and a later user click could respond to an
// already-resolved id. The requestId is the id of the resolved server request
// — the same JSON-RPC id foci stored as the pendingPerms key when it received
// the requestApproval (verified against codex 0.144.5's app-server schema:
// ServerRequestResolvedNotification.requestId is a RequestId = string|int64,
// the id of the request being resolved).
func (b *Backend) onServerRequestResolved(params *serverRequestResolvedParams) {
	rpcID, ok := coerceRPCID(params.RequestID)
	b.permMu.Lock()
	if ok {
		delete(b.pendingPerms, rpcID)
	}
	isEmpty := len(b.pendingPerms) == 0
	b.permMu.Unlock()
	if isEmpty && b.onPromptsCleared != nil {
		b.onPromptsCleared()
	}
}

// coerceRPCID converts a JSON-RPC RequestId (string | int64, decoded into an
// interface{} as float64 for numbers) into the int64 key foci uses for
// pendingPerms / pendingRPC. Returns ok=false if the value can't be
// interpreted as an integer id.
func coerceRPCID(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

// onReasoningDelta delivers streaming raw reasoning text.
func (b *Backend) onReasoningDelta(params *reasoningDeltaParams) {
	se := b.sessionEvents.Load()
	if se != nil && se.OnThinkingDelta != nil {
		se.OnThinkingDelta(params.Delta)
	}
}

// onReasoningSummaryDelta delivers streaming reasoning summary text.
func (b *Backend) onReasoningSummaryDelta(params *reasoningSummaryDeltaParams) {
	se := b.sessionEvents.Load()
	if se != nil && se.OnThinkingDelta != nil {
		se.OnThinkingDelta(params.Delta)
	}
}

// onConfigWarning logs recoverable configuration problems surfaced by the
// app-server.
//
// Logging is the ENTIRE delivery mechanism, deliberately. These are operator
// diagnostics — emitted at initialize before any thread exists, carrying a
// file:line:col and an instruction to edit a config file — so there is no
// thread they belong to and no agent that could act on one. WARN (not Info)
// is what makes them visible: log.SetWarnHook fires only at WARN/ERROR, and
// it is the single path behind notify.inject_chat_warnings and
// notify.inject_agent_warnings. Anything wanting these subscribes there
// rather than to a codex-specific callback.
//
// Severity is not incidental: codex logs these at ERROR itself, and
// "Invalid configuration; using defaults." means one typo silently swapped
// the agent's model and sandbox for defaults.
func (b *Backend) onConfigWarning(params *configWarningParams) {
	msg := params.Summary
	if params.Details != "" {
		msg += ": " + params.Details
	}
	if params.Path != "" {
		msg += " (" + params.Path + ")"
	}
	b.logWarnf("config warning: %s", msg)
}

// completeTurn fires the OnTurnComplete callback and clears turn state.
func (b *Backend) completeTurn(result *delegator.TurnResult) {
	b.turnMu.Lock()
	turn := b.turnEvents
	b.turnEvents = nil
	b.turnActive = false
	b.turnID = ""
	ch := b.turnResultCh
	b.turnResultCh = nil
	b.turnText.Reset()
	b.resetTurnUsageLocked()
	b.turnMu.Unlock()

	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}
	if ch != nil {
		select {
		case ch <- result:
		default:
		}
	}
}

// turnFailedText is the chat text for a codex turn that failed with msg —
// the counterpart of the "API Error: …" line CC writes for a failed call.
func turnFailedText(msg string) string {
	return "❌ Codex turn failed: " + msg
}
