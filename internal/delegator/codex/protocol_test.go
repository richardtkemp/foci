package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"foci/internal/delegator"
)

func TestDispatch_TurnStarted(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var typed bool
	b.typingFunc = func(on bool) { typed = on }

	b.dispatch([]byte(`{"method":"turn/started","params":{"turn":{"id":"tu_1","status":"inProgress"}}}`))
	if !typed {
		t.Error("typingFunc was not invoked with true for turn/started")
	}
}

func TestDispatch_TurnCompleted(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var got *delegator.TurnResult
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}
	b.turnMu.Unlock()

	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}}`))

	if got == nil {
		t.Fatal("OnTurnComplete was not fired for turn/completed")
	}
	b.turnMu.Lock()
	active := b.turnActive
	b.turnMu.Unlock()
	if active {
		t.Error("turnActive = true, want false after turn/completed")
	}
}

func TestDispatch_TokenUsageStashedAndDeliveredOnTurnComplete(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	b.dispatch([]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_1","tokenUsage":{"last":{"inputTokens":100,"outputTokens":50,"cachedInputTokens":20,"reasoningOutputTokens":5,"totalTokens":175},"modelContextWindow":128000}}}`))

	b.turnMu.Lock()
	stashed := b.stashedUsage
	b.turnMu.Unlock()
	// codex reports cachedInputTokens (20) as a SUBSET of inputTokens (100).
	// foci's downstream math is Anthropic-style additive, so InputTokens is
	// mapped as input-minus-cached (80) with CacheReadInputTokens=20, keeping
	// input+cacheRead == codex's reported input (100). See onTokenUsage.
	if stashed == nil || stashed.InputTokens != 80 || stashed.CacheReadInputTokens != 20 {
		t.Fatalf("stashedUsage not set correctly: %+v", stashed)
	}

	var got *delegator.TurnResult
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}
	b.turnMu.Unlock()

	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}}`))

	if got == nil || got.Usage == nil || got.Usage.InputTokens != 80 || got.Usage.CacheReadInputTokens != 20 {
		t.Fatalf("TurnResult.Usage not delivered from stash: %+v", got)
	}
}

func TestDispatch_ItemStartedCommandExecution(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var toolID, toolName string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnToolStart: func(id, name, input string) { toolID, toolName = id, name },
	})

	b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"it_cmd","command":"ls"}}}`))

	if toolID != "it_cmd" || toolName != "bash" {
		t.Errorf("OnToolStart = %q/%q, want it_cmd/bash", toolID, toolName)
	}
}

func TestDispatch_ItemCompleted(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var texts []string
	var toolEnds int
	var think string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnText:          func(s string) { texts = append(texts, s) },
		OnToolEnd:       func(id, name, output string, isError bool) { toolEnds++ },
		OnThinkingDelta: func(s string) { think = s },
	})

	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"agentMessage","id":"it_1","text":"hello"}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"it_2","status":"completed","command":"ls"}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"it_3","status":"failed","command":"false"}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"reasoning","id":"it_4","text":"pondering"}}}`))

	if len(texts) != 1 || texts[0] != "hello" {
		t.Errorf("OnText = %v, want [hello]", texts)
	}
	if toolEnds != 2 {
		t.Errorf("OnToolEnd calls = %d, want 2", toolEnds)
	}
	if think != "pondering" {
		t.Errorf("OnThinkingDelta = %q, want %q", think, "pondering")
	}

	b.turnMu.Lock()
	tools := b.turnTools
	text := b.turnText.String()
	b.turnMu.Unlock()
	if tools != 2 {
		t.Errorf("turnTools = %d, want 2", tools)
	}
	if text != "hello" {
		t.Errorf("turnText = %q, want %q", text, "hello")
	}
}

// TestDispatch_AgentMessage_CommentaryExcludedFromTurnText is the red/green
// regression for #1329 item 6: codex's own generate-json-schema (v0.144.5)
// documents agentMessage's `phase` field as "commentary" | "final_answer",
// and a live turn/start -> turn/steer -> turn/completed probe confirmed the
// running app-server actually emits it (a b3d41c78 attempt to act on this
// was reverted minutes later, with no live check either way — the problem it
// described was left unfixed with no TODO, per #1329's own description).
// Only "final_answer" (or an unphased item, for backward compat with a
// provider/older codex that doesn't emit phase) should reach the delivered
// turn result; "commentary" narration must not pollute it, though it still
// reaches the live view via OnText.
func TestDispatch_AgentMessage_CommentaryExcludedFromTurnText(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var texts []string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnText: func(s string) { texts = append(texts, s) },
	})

	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"agentMessage","id":"m1","text":"Running the requested command now.","phase":"commentary"}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"agentMessage","id":"m2","text":"Done — here is the answer.","phase":"final_answer"}}}`))
	// Unphased item (older codex / a provider that doesn't emit phase):
	// backward-compat path, still accumulates.
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"agentMessage","id":"m3","text":" Also unphased."}}}`))

	b.turnMu.Lock()
	text := b.turnText.String()
	b.turnMu.Unlock()

	want := "Done — here is the answer. Also unphased."
	if text != want {
		t.Errorf("turnText = %q, want %q (commentary must be excluded)", text, want)
	}
	if len(texts) != 3 {
		t.Errorf("OnText calls = %d, want 3 (commentary still reaches the live view)", len(texts))
	}
}

// TestDispatch_AgentMessageDelta_DoesNotDoubleTurnText is the red/green
// regression for the double-accumulation bug found while live-verifying
// phase semantics for item 6: onAgentMessageDelta used to ALSO write into
// turnText, and a completed agentMessage item's full text (written by
// onItemCompleted) is exactly the concatenation of its own deltas — not
// additional content (verified live) — so every message's contribution to
// the delivered turn result was silently doubled.
func TestDispatch_AgentMessageDelta_DoesNotDoubleTurnText(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"th_1","turnId":"tu_1","itemId":"m1","delta":"Hello, "}}`))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"th_1","turnId":"tu_1","itemId":"m1","delta":"world."}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"agentMessage","id":"m1","text":"Hello, world.","phase":"final_answer"}}}`))

	b.turnMu.Lock()
	text := b.turnText.String()
	b.turnMu.Unlock()

	if text != "Hello, world." {
		t.Errorf("turnText = %q, want %q (deltas must not double-count the completed item's text)", text, "Hello, world.")
	}
}

// TestDispatch_CollabAgentToolCall_DeterministicOrderAndCounted is the
// red/green regression for #1329 item 4: AgentsStates is a Go map, so
// ranging it directly delivers multi-agent collab messages in random
// iteration order — this pins sorted-by-key (deterministic) delivery.
// It also pins the ToolCalls fix: collabAgentToolCall previously never
// incremented turnTools (undercounting a real subagent spawn) while
// contextCompaction — internal bookkeeping, not a user tool call — did
// (overcounting); collab should count, compaction should not.
func TestDispatch_CollabAgentToolCall_DeterministicOrderAndCounted(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var order []string
	var ended bool
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnSubagentText: func(groupKey, text string, runIndex int) { order = append(order, text) },
		OnSubagentEnd:  func(groupKey string, runIndex int) { ended = true },
	})

	// zzz/aaa/mmm keys chosen so map iteration order is virtually certain to
	// differ from sorted order at least once across repeated runs — sorting
	// makes the assertion below deterministic regardless.
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"collabAgentToolCall","id":"c1","agentsStates":{"zzz":{"message":"third"},"aaa":{"message":"first"},"mmm":{"message":"second"}}}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"contextCompaction","id":"c2"}}}`))

	want := []string{"first", "second", "third"}
	if len(order) != len(want) {
		t.Fatalf("OnSubagentText order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("OnSubagentText order = %v, want %v (must be sorted by agent id)", order, want)
		}
	}
	if !ended {
		t.Error("OnSubagentEnd was not fired")
	}

	b.turnMu.Lock()
	tools := b.turnTools
	b.turnMu.Unlock()
	if tools != 1 {
		t.Errorf("turnTools = %d, want 1 (collab counted, compaction not)", tools)
	}
}

// TestTruncateArgs_UTF8SafeBoundary is the red/green regression for #1329
// item 3: truncateArgs used to slice raw JSON at a fixed byte offset
// (s[:200]), which can split a multibyte UTF-8 rune in half and produce
// invalid UTF-8 for the activity indicator. This pins a boundary placed
// mid-rune: a run of 2-byte runes ("é", 0xC3 0xA9) padded so byte 200 lands
// inside one.
func TestTruncateArgs_UTF8SafeBoundary(t *testing.T) {
	t.Parallel()

	// 99 ASCII bytes + repeated "é" (2 bytes each) pushes the 200-byte cut
	// point into the middle of one of the multibyte runes.
	raw := json.RawMessage(`"` + strings.Repeat("a", 99) + strings.Repeat("é", 60) + `"`)

	got := truncateArgs(raw)

	if !utf8.ValidString(got) {
		t.Fatalf("truncateArgs produced invalid UTF-8: %q", got)
	}
}

// TestSummarizePaths_Bounded is the red/green regression for #1329 item 3:
// summarizePaths joined an unbounded list of changed file paths into the
// fileChange approval prompt text — a large patch (hundreds of files) could
// blow up that prompt. Output must be capped with a visible "+N more".
func TestSummarizePaths_Bounded(t *testing.T) {
	t.Parallel()

	changes := make([]fileChangeEntry, 0, 500)
	for i := 0; i < 500; i++ {
		changes = append(changes, fileChangeEntry{Path: strings.Repeat("x", 20)})
	}

	got := summarizePaths(changes)

	if len(got) > 2000 {
		t.Errorf("summarizePaths output len = %d, want bounded (<2000)", len(got))
	}
	if !strings.Contains(got, "more") {
		t.Errorf("summarizePaths = %q, want a truncation marker", got)
	}
}

func TestDispatch_StreamingDeltas(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var textDelta, thinkDelta string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnTextDelta:     func(s string) { textDelta += s },
		OnThinkingDelta: func(s string) { thinkDelta += s },
	})

	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"th_1","turnId":"tu_1","delta":"Hel"}}`))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"th_1","turnId":"tu_1","delta":"lo"}}`))
	b.dispatch([]byte(`{"method":"item/reasoning/textDelta","params":{"threadId":"th_1","turnId":"tu_1","itemId":"it_3","delta":"step"}}`))
	b.dispatch([]byte(`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"th_1","turnId":"tu_1","itemId":"it_4","delta":"sum","summaryIndex":2}}`))

	if textDelta != "Hello" {
		t.Errorf("textDelta = %q, want %q", textDelta, "Hello")
	}
	if thinkDelta != "stepsum" {
		t.Errorf("thinkDelta = %q, want %q", thinkDelta, "stepsum")
	}
}

func TestDispatch_Warnings(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var warnings []string
	b.onWarning = func(detail string) { warnings = append(warnings, detail) }

	b.dispatch([]byte(`{"method":"configWarning","params":{"summary":"model not found","details":"falling back","path":"/etc/codex.toml"}}`))
	b.dispatch([]byte(`{"method":"warning","params":{"threadId":"th_1","message":"rate limited"}}`))

	if len(warnings) != 2 {
		t.Fatalf("warnings = %d, want 2", len(warnings))
	}
	if want := "model not found: falling back (/etc/codex.toml)"; warnings[0] != want {
		t.Errorf("configWarning = %q, want %q", warnings[0], want)
	}
	if warnings[1] != "rate limited" {
		t.Errorf("runtime warning = %q, want %q", warnings[1], "rate limited")
	}
}

func TestDispatch_CommandApproval(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var reqID string
	var choices []delegator.PromptChoice
	b.permPromptFn = func(requestID, txt, summary, attachmentPath string, ch []delegator.PromptChoice) {
		reqID = requestID
		choices = ch
	}

	b.dispatch([]byte(`{"method":"item/commandExecution/requestApproval","id":42,"params":{"itemId":"it_cmd","threadId":"th_1","turnId":"tu_1","command":"rm -rf /","reason":"dangerous"}}`))

	if reqID != "it_cmd" {
		t.Errorf("requestID = %q, want it_cmd", reqID)
	}
	if len(choices) != 2 {
		t.Errorf("choices = %d, want 2", len(choices))
	}
	b.permMu.Lock()
	pending := b.pendingPerms[42]
	b.permMu.Unlock()
	if pending == nil || pending.command != "rm -rf /" {
		t.Errorf("pending approval not tracked correctly: %+v", pending)
	}
}

func TestDispatch_FileChangeApproval(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var reqID string
	b.permPromptFn = func(requestID, txt, summary, attachmentPath string, ch []delegator.PromptChoice) {
		reqID = requestID
	}

	b.dispatch([]byte(`{"method":"item/fileChange/requestApproval","id":43,"params":{"itemId":"it_fc","threadId":"th_1","turnId":"tu_1","reason":"edit config"}}`))

	if reqID != "it_fc" {
		t.Errorf("requestID = %q, want it_fc", reqID)
	}
	b.permMu.Lock()
	pending := b.pendingPerms[43]
	b.permMu.Unlock()
	if pending == nil {
		t.Fatal("pending approval not tracked")
	}
}

func TestDispatch_PermissionApprovalAutoDeny(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var cleared bool
	b.onPromptsCleared = func() { cleared = true }

	b.dispatch([]byte(`{"method":"item/permissions/requestApproval","id":44,"params":{"itemId":"it_perm"}}`))

	if !cleared {
		t.Error("onPromptsCleared not fired after auto-deny")
	}
}

func TestDispatch_ServerRequestResolvedClearsPrompts(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var cleared bool
	b.onPromptsCleared = func() { cleared = true }

	b.dispatch([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"th_1","requestId":"req_9"}}`))

	if !cleared {
		t.Error("onPromptsCleared not fired for serverRequest/resolved")
	}
}

func TestDispatch_ResponseDeliveredToPendingRPC(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	ch := make(chan rpcReply, 1)
	b.pendingRPC[7] = ch

	b.dispatch([]byte(`{"id":7,"result":{"thread":{"id":"th_x"}}}`))

	select {
	case reply := <-ch:
		res := reply.result
		var tr threadResult
		if err := json.Unmarshal(res, &tr); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if tr.Thread.ID != "th_x" {
			t.Errorf("result.thread.id = %q, want th_x", tr.Thread.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("response was not delivered to pendingRPC[7]")
	}

	b.rpcMu.Lock()
	_, present := b.pendingRPC[7]
	b.rpcMu.Unlock()
	if present {
		t.Error("pendingRPC[7] should be removed after delivery")
	}
}

func TestDispatch_ErrorResponseSurfacesAsError(t *testing.T) {
	// A JSON-RPC error response must reach the caller as a real error, not be
	// dropped so sendAndWait reports the misleading "process exited" (the error
	// field was previously never read).
	t.Parallel()
	b := newTestBackend(t)

	ch := make(chan rpcReply, 1)
	b.pendingRPC[9] = ch

	b.dispatch([]byte(`{"id":9,"error":{"code":-32000,"message":"bad model"}}`))

	select {
	case reply := <-ch:
		if reply.err == nil {
			t.Fatalf("expected an error, got result %s", reply.result)
		}
		if !strings.Contains(reply.err.Error(), "bad model") || !strings.Contains(reply.err.Error(), "-32000") {
			t.Errorf("error should carry code+message, got %v", reply.err)
		}
	case <-time.After(time.Second):
		t.Fatal("error response was not delivered")
	}
}

func TestDispatch_ResponseNoPending(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	b.dispatch([]byte(`{"id":999,"result":{"answer":"late"}}`))

	b.rpcMu.Lock()
	n := len(b.pendingRPC)
	b.rpcMu.Unlock()
	if n != 0 {
		t.Errorf("pendingRPC = %d entries, want 0", n)
	}
}

func TestDispatch_MalformedJSONDropped(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var deltas []string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnTextDelta: func(s string) { deltas = append(deltas, s) },
	})

	b.dispatch([]byte("this is not json at all"))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"t1","delta":`))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"tu1","delta":"ok"}}`))

	if got, want := strings.Join(deltas, ""), "ok"; got != want {
		t.Errorf("deltas = %q, want %q (malformed lines should be dropped)", got, want)
	}
}

func TestDispatch_UnrecognisedShape(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	b.dispatch([]byte(`{"foo":"bar","baz":[1,2,3]}`))

	if b.IsTurnInFlight() {
		t.Error("turn should not be in flight")
	}
}
