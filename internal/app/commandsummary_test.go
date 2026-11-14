package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/turnevent"
)

// commandSummaryFixture is a hub with a stub fast model, one bound chat and a
// conn backed by fa. The delay before the description is asked for is cut to
// a few ms for the test.
func commandSummaryFixture(t *testing.T, reply string, fa *fakeAgent) (*openRouterStub, *convBinding, turnevent.Sink) {
	t.Helper()
	old := commandSummaryDelay
	commandSummaryDelay = 5 * time.Millisecond
	t.Cleanup(func() { commandSummaryDelay = old })
	stub := &openRouterStub{reply: reply}
	h := newTestHub()
	h.pinSum = testSummariser(stub.serve(t).URL, nil)
	b := h.ensureBinding(nil, "arnix", "conv-1")
	b.attach(deviceClient(h, "phone"))
	conn := &appConn{hub: h, agentID: "arnix", agentRef: fa}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	t.Cleanup(cleanup)
	return stub, b, sink
}

// waitDetail polls the activity detail until it is want, failing at a deadline.
func waitDetail(t *testing.T, b *convBinding, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b.info().ActivityDetail == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("activity detail = %q, want %q", b.info().ActivityDetail, want)
}

// A codex shell call (raw command, no description) shows the shortened
// command at once, then the fast model's description of it while it runs
// (#3118, Q48). The command goes to the model with the six-word prompt.
func TestAppSink_UndescribedCommandGetsFastModelDescription(t *testing.T) {
	stub, b, sink := commandSummaryFixture(t, "find markdown files", &fakeAgent{})
	ctx := context.Background()
	sink.Emit(ctx, turnevent.TurnStart{})
	sink.Emit(ctx, turnevent.ToolCall{Name: "bash", Args: json.RawMessage("find . -name '*.md'\n| head")})
	if got := b.info().ActivityDetail; got != "find . -name '*.md'" {
		t.Fatalf("detail at tool start = %q, want the command's first line", got)
	}
	waitDetail(t, b, "find markdown files")
	if b.info().Activity != "tool" {
		t.Errorf("activity = %q, want tool", b.info().Activity)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d model calls, want 1", len(reqs))
	}
	msgs, _ := reqs[0]["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	user, _ := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "SIX WORDS") {
		t.Errorf("system prompt = %q", sys)
	}
	if !strings.Contains(user, "find . -name '*.md'") {
		t.Errorf("model input = %q, want the command", user)
	}
}

// A CC Bash call with its own description is unchanged: no model call.
func TestAppSink_DescribedCommandNoModelCall(t *testing.T) {
	stub, b, sink := commandSummaryFixture(t, "should not be used", &fakeAgent{})
	ctx := context.Background()
	sink.Emit(ctx, turnevent.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"ls","description":"List files"}`)})
	time.Sleep(50 * time.Millisecond)
	if got := b.info().ActivityDetail; got != "List files" {
		t.Errorf("detail = %q, want the call's description", got)
	}
	if n := len(stub.requests()); n != 0 {
		t.Errorf("%d model calls, want 0", n)
	}
}

// A command that ends before the delay costs no model call, and a late
// description never relabels whatever runs next.
func TestAppSink_CommandSummaryOnlyForRunningCall(t *testing.T) {
	stub, b, sink := commandSummaryFixture(t, "list files", &fakeAgent{})
	ctx := context.Background()
	commandSummaryDelay = 30 * time.Millisecond
	sink.Emit(ctx, turnevent.ToolCall{Name: "bash", Args: json.RawMessage("ls")})
	sink.Emit(ctx, turnevent.ToolResult{Name: "bash"})
	sink.Emit(ctx, turnevent.ToolCall{Name: "Read", Args: json.RawMessage(`{"file_path":"/x"}`)})
	time.Sleep(100 * time.Millisecond)
	if n := len(stub.requests()); n != 0 {
		t.Errorf("%d model calls for a finished command, want 0", n)
	}
	if got := b.info().ActivityDetail; got != "Read" {
		t.Errorf("detail = %q, want the next tool's label", got)
	}
}

// setTurnToolDetail relabels only the call it was given: after a later tool
// call starts, the earlier call's description is dropped.
func TestSetTurnToolDetail_StaleSeqIgnored(t *testing.T) {
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{fakeClient(): {}}}
	first := b.setTurnTool("tool", "ls", "ls", true)
	if !b.turnToolRunning(first) {
		t.Fatal("first call should be running")
	}
	second := b.setTurnTool("tool", "git status", "git status", true)
	b.setTurnToolDetail(first, "list files")
	if got := b.info().ActivityDetail; got != "git status" {
		t.Errorf("detail = %q after a stale relabel, want the running call's", got)
	}
	b.setTurnToolDetail(second, "show repo status")
	if got := b.info().ActivityDetail; got != "show repo status" {
		t.Errorf("detail = %q, want the new label", got)
	}
	b.setTurnActivity("thinking")
	if b.turnToolRunning(second) {
		t.Error("call should not be running after the activity moved on")
	}
}

// Q47: a codex agent's gap after a tool result reads "thinking"; any other
// backend's stays "warming". Turn start is "warming" for both.
func TestAppSink_ToolResultActivityByBackend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codex bool
		want  string
	}{
		{"codex thinks", true, "thinking"},
		{"others warm up", false, "warming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, b, sink := commandSummaryFixture(t, "x", &fakeAgent{thinksAfterTool: tc.codex})
			ctx := context.Background()
			sink.Emit(ctx, turnevent.TurnStart{})
			if got := b.info().Activity; got != "warming" {
				t.Fatalf("activity at turn start = %q, want warming", got)
			}
			sink.Emit(ctx, turnevent.ToolCall{Name: "Read", Args: json.RawMessage(`{"file_path":"/x"}`)})
			sink.Emit(ctx, turnevent.ToolResult{Name: "Read"})
			if got := b.info().Activity; got != tc.want {
				t.Errorf("activity after tool result = %q, want %q", got, tc.want)
			}
		})
	}
}
