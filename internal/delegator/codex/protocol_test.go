package codex

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"foci/internal/delegator"
	"foci/internal/log"
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
// collabAgentToolCall is no longer interpreted — see logUnhandledCollabItem.
// foci has never observed one on the wire (two codex versions, collab mode on,
// collaboration.* tools demonstrably used), so the old handling was an
// interpretation of a payload nobody had seen. This pins the replacement
// contract: the raw item reaches the log at WARN so the first real specimen is
// captured, subagent state is left completely alone, and the item still counts
// as a tool call.
func TestDispatch_CollabAgentToolCall_IsLoggedNotInterpreted(t *testing.T) {
	b := newTestBackend(t)
	b.subagents = newSubagentTracker()

	var touched []string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnSubagentStart: func(g, l, p string, r int) { touched = append(touched, "start") },
		OnSubagentText:  func(g, text string, r int) { touched = append(touched, "text") },
		OnSubagentPrompt: func(g, p string, r int) { touched = append(touched, "prompt") },
		OnSubagentEnd:   func(g string, r int) { touched = append(touched, "end") },
	})

	var mu sync.Mutex
	var logged []string
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		if level.String() == "WARN" {
			logged = append(logged, msg)
		}
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	const raw = `{"type":"collabAgentToolCall","id":"c1","tool":"spawnAgent","receiverThreadIds":["agent-9"],"agentsStates":{"zzz":{"message":"third"},"aaa":{"message":"first"}}}`
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":` + raw + `}}`))

	if len(touched) != 0 {
		t.Errorf("collab item drove subagent callbacks %v — it must not, the payload is unverified", touched)
	}

	// The WHOLE payload must survive to the log: a specimen is only useful if
	// nothing was dropped on the way, so assert distinctive fields from every
	// part of it rather than that some warning fired.
	var found string
	mu.Lock()
	for _, m := range logged {
		if strings.Contains(m, "collabAgentToolCall") {
			found = m
		}
	}
	mu.Unlock()
	if found == "" {
		t.Fatal("no WARN logged for a collabAgentToolCall — the first real specimen would vanish silently")
	}
	for _, frag := range []string{`"tool":"spawnAgent"`, `"receiverThreadIds":["agent-9"]`, `"message":"third"`} {
		if !strings.Contains(found, frag) {
			t.Errorf("logged payload is missing %s — a truncated specimen cannot be acted on:\n%s", frag, found)
		}
	}

	b.turnMu.Lock()
	tools := b.turnTools
	b.turnMu.Unlock()
	if tools != 1 {
		t.Errorf("turnTools = %d, want 1 — accounting is independent of what the payload means", tools)
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

// Codex warnings are operator diagnostics, not conversation. Both observed
// shapes ("Invalid configuration; using defaults." with a file:line:col, and
// the untrusted-project notice) are emitted at initialize BEFORE any thread
// exists, are addressed to a human at a terminal, and are logged by codex
// itself at ERROR. So foci's only job is to log them at a level that is
// actually monitored — WARN, not Info.
//
// That level is load-bearing, not cosmetic: log.SetWarnHook fires only for
// WARN/ERROR, and it is the single mechanism feeding notify.inject_chat_warnings
// (chat) and notify.inject_agent_warnings (agent context). Logging at Info
// makes a warning invisible to BOTH, which is why this asserts through the
// real hook rather than through a backend-local callback — the hook is the
// production path, and a backend-local assertion would prove nothing about
// whether the operator ever sees it.
//
// Not parallel: log.SetWarnHook is process-global. Entries are filtered by
// content so a concurrent test's own warnings cannot pollute the assertion.
func TestDispatch_WarningsAreLoggedAtWarnLevel(t *testing.T) {
	b := newTestBackend(t)

	type entry struct {
		level string
		msg   string
	}
	var mu sync.Mutex
	var got []entry
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, entry{level.String(), msg})
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b.dispatch([]byte(`{"method":"configWarning","params":{"summary":"Invalid configuration; using defaults.","details":"/home/foci/.codex/config.toml:1:6: key with no value, expected ` + "`=`" + `","path":"/home/foci/.codex/config.toml"}}`))
	b.dispatch([]byte(`{"method":"warning","params":{"threadId":"th_1","message":"rate limited"}}`))

	find := func(substr string) *entry {
		mu.Lock()
		defer mu.Unlock()
		for i := range got {
			if strings.Contains(got[i].msg, substr) {
				return &got[i]
			}
		}
		return nil
	}

	e := find("Invalid configuration; using defaults.")
	if e == nil {
		t.Fatal("configWarning never reached log.SetWarnHook — logged below WARN, so notify.inject_chat_warnings cannot deliver it and a silently-defaulted codex config is invisible")
	}
	// The whole formatted diagnostic must survive: summary alone omits the
	// file:line:col that makes it actionable.
	if want := "/home/foci/.codex/config.toml:1:6"; !strings.Contains(e.msg, want) {
		t.Errorf("configWarning msg = %q, missing location %q", e.msg, want)
	}
	if e.level != "WARN" {
		t.Errorf("configWarning level = %q, want %q", e.level, "WARN")
	}

	if e := find("rate limited"); e == nil {
		t.Error("runtime warning never reached log.SetWarnHook — logged below WARN")
	} else if e.level != "WARN" {
		t.Errorf("runtime warning level = %q, want %q", e.level, "WARN")
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
