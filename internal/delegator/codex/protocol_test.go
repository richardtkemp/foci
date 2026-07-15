package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

func TestDispatch_TurnStarted(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var typed bool
	b.typingFunc = func(on bool) { typed = on }

	b.dispatch([]byte(`{"method":"turn/started"}`))
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

	b.dispatch([]byte(`{"method":"turn/completed","threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}`))

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

	b.dispatch([]byte(`{"method":"thread/tokenUsage/updated","threadId":"th_1","turnId":"tu_1","tokenUsage":{"total":{"inputTokens":100,"outputTokens":50,"cachedInputTokens":20,"reasoningOutputTokens":5,"totalTokens":175},"modelContextWindow":128000}}`))

	b.turnMu.Lock()
	stashed := b.stashedUsage
	b.turnMu.Unlock()
	if stashed == nil || stashed.InputTokens != 100 {
		t.Fatalf("stashedUsage not set correctly: %+v", stashed)
	}

	var got *delegator.TurnResult
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}
	b.turnMu.Unlock()

	b.dispatch([]byte(`{"method":"turn/completed","threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}`))

	if got == nil || got.Usage == nil || got.Usage.InputTokens != 100 {
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

	b.dispatch([]byte(`{"method":"item/started","threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"it_cmd","command":"ls"}}`))

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

	b.dispatch([]byte(`{"method":"item/completed","threadId":"th_1","turnId":"tu_1","item":{"type":"agentMessage","id":"it_1","text":"hello"}}`))
	b.dispatch([]byte(`{"method":"item/completed","threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"it_2","status":"completed","command":"ls"}}`))
	b.dispatch([]byte(`{"method":"item/completed","threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"it_3","status":"failed","command":"false"}}`))
	b.dispatch([]byte(`{"method":"item/completed","threadId":"th_1","turnId":"tu_1","item":{"type":"reasoning","id":"it_4","text":"pondering"}}`))

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

func TestDispatch_StreamingDeltas(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var textDelta, thinkDelta string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnTextDelta:      func(s string) { textDelta += s },
		OnThinkingDelta:  func(s string) { thinkDelta += s },
	})

	b.dispatch([]byte(`{"method":"item/agentMessage/delta","threadId":"th_1","turnId":"tu_1","delta":"Hel"}`))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","threadId":"th_1","turnId":"tu_1","delta":"lo"}`))
	b.dispatch([]byte(`{"method":"item/reasoning/textDelta","threadId":"th_1","turnId":"tu_1","itemId":"it_3","delta":"step"}`))
	b.dispatch([]byte(`{"method":"item/reasoning/summaryTextDelta","threadId":"th_1","turnId":"tu_1","itemId":"it_4","delta":"sum"}`))

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

	b.dispatch([]byte(`{"method":"configWarning","summary":"model not found","details":"falling back","path":"/etc/codex.toml"}`))
	b.dispatch([]byte(`{"method":"warning","threadId":"th_1","message":"rate limited"}`))

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

	b.dispatch([]byte(`{"method":"item/commandExecution/requestApproval","id":42,"itemId":"it_cmd","threadId":"th_1","turnId":"tu_1","command":"rm -rf /","reason":"dangerous"}`))

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

	b.dispatch([]byte(`{"method":"item/fileChange/requestApproval","id":43,"itemId":"it_fc","threadId":"th_1","turnId":"tu_1","reason":"edit config"}`))

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

	b.dispatch([]byte(`{"method":"item/permissions/requestApproval","id":44,"itemId":"it_perm"}`))

	if !cleared {
		t.Error("onPromptsCleared not fired after auto-deny")
	}
}

func TestDispatch_ServerRequestResolvedClearsPrompts(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	var cleared bool
	b.onPromptsCleared = func() { cleared = true }

	b.dispatch([]byte(`{"method":"serverRequest/resolved","threadId":"th_1","requestId":"req_9"}`))

	if !cleared {
		t.Error("onPromptsCleared not fired for serverRequest/resolved")
	}
}

func TestDispatch_ResponseDeliveredToPendingRPC(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	ch := make(chan json.RawMessage, 1)
	b.pendingRPC[7] = ch

	b.dispatch([]byte(`{"id":7,"result":{"thread":{"id":"th_x"}}}`))

	select {
	case res := <-ch:
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
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","threadId":"t1","delta":`))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","threadId":"t1","turnId":"tu1","delta":"ok"}`))

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
