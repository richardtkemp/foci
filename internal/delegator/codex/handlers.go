package codex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// Subagent runs are deliberately NOT reset here. A child outlives its
	// parent's turn in both directions — it can still be working when the
	// parent's turn ends, and still working when the next one starts — so
	// clearing at a turn boundary orphaned a live child's run. Runs end on the
	// child's own turn/completed (see handleSubagentNotification).
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
}

// onTurnCompleted finalises the turn.
func (b *Backend) onTurnCompleted(params *turnCompletedParams) {
	// Subagent runs are NOT ended here (#1588). #1324 used this boundary
	// because SubAgentActivityKind has no terminal variant, so nothing seemed
	// to end a run — but a child has its own thread and codex streams that
	// thread's turn/completed, which is the real signal
	// (handleSubagentNotification). A live probe showed a parent's turn
	// completing with its child still working, so ending runs here reported
	// children as finished while they were going.
	//
	// Read turnText/turnTools under turnMu: onItemCompleted writes them under
	// turnMu from the reader goroutine and completeTurn Resets turnText under
	// turnMu from the agent goroutine (e.g. a turn/start >30s timeout firing
	// while the turn is still running) — reading the strings.Builder without
	// the lock is a real data race.
	b.turnMu.Lock()
	usage := b.stashedUsage
	text := b.turnText.String()
	tools := b.turnTools
	b.turnMu.Unlock()

	b.mu.Lock()
	model := b.model
	threadName := b.threadName
	b.mu.Unlock()
	if model != "" {
		model = "codex/" + model
	}
	result := &delegator.TurnResult{
		Text:       text,
		ToolCalls:  tools,
		Usage:      usage,
		Model:      model,
		ThreadName: threadName,
	}
	if params.Turn.Status == "failed" && params.Turn.Error != nil {
		b.logWarnf("turn failed: %s", params.Turn.Error.Message)
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
			se.OnToolStart(item.ID, "compact", "")
		}
	// subAgentActivity is the authoritative child-thread lifecycle. Its item ID
	// is transient; the agentThreadId is stable across collab follow-ups.
	// Handled on BOTH notifications — see openSubagentRun.
	case "subAgentActivity":
		b.openSubagentRun(&item, se)
	case "collabAgentToolCall":
		b.logUnhandledCollabItem("item/started", params.Item)
	}
}

// handleSubagentNotification consumes every notification belonging to a
// subagent's child thread. Nothing here may reach the process owner: the
// thread lookup in dispatch is the whole ownership decision, exactly as it is
// for batch threads, so the default is to swallow rather than fall through.
//
// The child's own turn/completed is what ends a run. afe20cd0 concluded no
// completion signal existed, reasoning from SubAgentActivityKind being
// {started, interacted, interrupted} with no terminal variant — true of that
// enum, but the child has its own THREAD, and codex streams that thread's full
// turn lifecycle. Ending runs at the parent-turn boundary instead reported
// children as finished while they were still working (#1588); the child's own
// turn ending is the honest signal, and it arrives on the wire already.
func (b *Backend) handleSubagentNotification(threadID, method string, params []byte) {
	se := b.sessionEvents.Load()
	switch method {
	case "item/completed":
		// Whole messages only. Deltas are consumed silently below: the
		// completed item carries the same text in one piece, and the subagent
		// panel renders messages, not a token stream.
		var p itemCompletedParams
		if json.Unmarshal(params, &p) != nil {
			return
		}
		var item itemEnvelope
		if json.Unmarshal(p.Item, &item) != nil {
			return
		}
		if item.Type != "agentMessage" || item.Text == "" {
			return
		}
		if run := b.subagents.current(threadID); run != nil && se != nil && se.OnSubagentText != nil {
			se.OnSubagentText(run.groupKey, item.Text, run.runIndex)
		}
	case "turn/completed":
		if run := b.subagents.stop(threadID); run != nil && se != nil && se.OnSubagentEnd != nil {
			se.OnSubagentEnd(run.groupKey, run.runIndex)
		}
	default:
		// Consumed, not ignored: the event is the child's and the owner must
		// not see it. Logged so an event type worth extracting from is
		// visible rather than silently swallowed.
		b.logDebugf("subagent thread %s: consuming %s", threadID, method)
	}
}

// logUnhandledCollabItem reports a collabAgentToolCall item verbatim instead of
// interpreting it.
//
// foci previously drove the whole subagent UI from these — spawnAgent opened a
// run, sendInput/resumeAgent emitted prompts, closeAgent ended it. That was
// written from codex's type definitions, never from observed traffic, and live
// probing shows codex does not send them: across codex 0.144.5 and 0.145.0,
// with collab mode genuinely enabled (`collab = true`) and the model
// demonstrably calling collaboration.spawn_agent / send_message /
// interrupt_agent, the wire carried ONLY subAgentActivity items. Zero
// collabAgentToolCall items appeared in the notification stream OR in either
// thread's history. openai/codex#31300 canonicalised these for the *v1*
// collaboration tools; current codex uses MultiAgentV2, which reports through
// subAgentActivity (see openSubagentRun, the live path).
//
// So the old handling was an elaborate interpretation of a payload nobody had
// ever seen. Rather than keep guessing — or delete the case and be blind if it
// returns — this logs the raw item at WARN. WARN is deliberate: it is the
// level that reaches an operator through log.SetWarnHook and the
// notify.inject_chat_warnings path, so the first real specimen surfaces
// immediately, with its full payload, instead of being silently consumed by
// handling built on assumptions.
func (b *Backend) logUnhandledCollabItem(method string, raw json.RawMessage) {
	b.logWarnf("unhandled collabAgentToolCall on %s — foci has never observed one of these; "+
		"please capture this payload and see openSubagentRun for the live path: %s", method, string(raw))
}

// openSubagentRun opens a child's run for a subAgentActivity item with
// kind=started, and is a no-op for anything else.
//
// Called from BOTH onItemStarted and onItemCompleted on purpose. Codex 0.145.0
// delivers the entire subAgentActivity lifecycle — started, interacted,
// interrupted — on item/completed; item/started carries only agentMessage,
// reasoning and userMessage (verified against a live app-server over a
// spawn -> message -> close sequence). Watching item/started alone therefore
// opened no run at all: no poll, no OnSubagentStart, and the later stop() had
// nothing to end, leaving the subagent display inert along with everything
// layered on it. Rather than move the handler and re-break on the next
// protocol shift, both notifications feed this: start() is idempotent for an
// already-active child, so a release that emits on item/started, on
// item/completed, or on both behaves identically.
func (b *Backend) openSubagentRun(item *itemEnvelope, se *delegator.SessionEvents) {
	if item.Kind != "started" || b.subagents == nil || item.AgentThreadID == "" {
		return
	}
	run, created := b.subagents.start(item.AgentThreadID, item.ID, item.AgentPath)
	if !created || se == nil || se.OnSubagentStart == nil {
		return
	}
	// The prompt argument is empty because subAgentActivity does not carry one:
	// the observed payload is {type, id, kind, agentThreadId, agentPath} and
	// nothing else. The prompt used to come from collabAgentToolCall.prompt,
	// which foci no longer interprets (see logUnhandledCollabItem) — and which
	// codex has never actually been observed to send, so that prompt was never
	// really populated either. agentPath is the only descriptive field
	// available, and it serves as the label.
	se.OnSubagentStart(run.groupKey, item.AgentPath, "", run.runIndex)
}

// onItemCompleted maps a Codex item/completed notification to SessionEvents.
func (b *Backend) onItemCompleted(params *itemCompletedParams) {
	var item itemEnvelope
	if err := json.Unmarshal(params.Item, &item); err != nil {
		b.logWarnf("dropping malformed item in item/completed: %v", err)
		return
	}

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
		if item.Phase != "commentary" {
			b.turnMu.Lock()
			b.turnText.WriteString(item.Text)
			b.turnMu.Unlock()
		}
		if se != nil && se.OnText != nil {
			se.OnText(item.Text)
		}

	case "commandExecution":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			isError := item.Status == "failed"
			se.OnToolEnd(item.ID, "bash", "", isError)
		}

	case "fileChange":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "edit", "", item.Status == "failed")
		}

	case "mcpToolCall":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			name := "mcp:" + item.Server + "." + item.Tool
			se.OnToolEnd(item.ID, name, "", item.Status == "failed")
		}

	case "dynamicToolCall":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			name := item.Tool
			if item.Namespace != "" {
				name = item.Namespace + "." + item.Tool
			}
			se.OnToolEnd(item.ID, name, "", !itemSuccess(item))
		}

	case "webSearch":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "web_search", "", false)
		}

	case "imageGeneration":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "image_gen", "", item.Status == "failed")
		}

	case "reasoning":
		if se != nil && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(item.Text)
		}

	case "contextCompaction":
		// Not counted in turnTools: compaction is internal bookkeeping, not
		// a user-facing tool call — counting it here skewed
		// TurnResult.ToolCalls high while collabAgentToolCall (a real
		// subagent spawn, below) skewed it low by never counting at all.
		b.compactMu.Lock()
		if b.compactDoneCh != nil {
			close(b.compactDoneCh)
			b.compactDoneCh = nil
		}
		b.compactMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "compact", "", false)
		}

	case "subAgentActivity":
		// Codex 0.145.0 delivers the WHOLE lifecycle here, including
		// kind=started — see openSubagentRun.
		b.openSubagentRun(&item, se)
		if (item.Kind == "interrupted" || item.Kind == "interacted") && b.subagents != nil {
			if run := b.subagents.stop(item.AgentThreadID); run != nil && se != nil && se.OnSubagentEnd != nil {
				se.OnSubagentEnd(run.groupKey, run.runIndex)
			}
		}

	case "collabAgentToolCall":
		b.logUnhandledCollabItem("item/completed", params.Item)
		// Still counted as a tool call, like every other item type above.
		// That accounting is independent of what the item MEANS — if one ever
		// arrives, undercounting TurnResult.ToolCalls would be wrong whatever
		// we later decide the payload represents.
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
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
	se := b.sessionEvents.Load()
	if se != nil && se.OnTextDelta != nil {
		se.OnTextDelta(params.Delta)
	}
}

// onTokenUsage stashes the latest usage for the current turn. Delivered
// in TurnResult.Usage when the turn completes.
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
	inputTokens := params.TokenUsage.Last.InputTokens - params.TokenUsage.Last.CachedInputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	u := &delegator.TurnUsage{
		InputTokens:          inputTokens,
		OutputTokens:         params.TokenUsage.Last.OutputTokens,
		CacheReadInputTokens: params.TokenUsage.Last.CachedInputTokens,
	}
	b.turnMu.Lock()
	b.stashedUsage = u
	b.turnMu.Unlock()

	if params.TokenUsage.ModelContextWindow > 0 {
		b.mu.Lock()
		b.contextWindow = params.TokenUsage.ModelContextWindow
		b.mu.Unlock()
	}
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
	b.turnTools = 0
	b.stashedUsage = nil
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
