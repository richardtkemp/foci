package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func runCloseSession(t *testing.T, tool *Tool, sessionKey string) string {
	t.Helper()
	ctx := context.Background()
	if sessionKey != "" {
		ctx = WithSessionKey(ctx, sessionKey)
	}
	res, err := tool.Execute(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text
}

// TestCloseSession_ClosesCallingSession: the tool hands the closer the
// calling session's agent and full key, and reports each outcome distinctly.
func TestCloseSession_ClosesCallingSession(t *testing.T) {
	type call struct{ agent, key string }
	var calls []call
	n, closeErr := 1, error(nil)
	tool := NewCloseSessionTool(func(agentID, sessionKey string) (int, error) {
		calls = append(calls, call{agentID, sessionKey})
		return n, closeErr
	})
	if !tool.ExecExport {
		t.Error("close_session must be exec-exported (foci_close_session on CC)")
	}

	if out := runCloseSession(t, tool, "clutch/c42"); !strings.HasPrefix(out, "Closed this chat") {
		t.Errorf("open chat: result = %q", out)
	}
	if len(calls) != 1 || calls[0] != (call{"clutch", "clutch/c42"}) {
		t.Fatalf("closer calls = %+v, want one for clutch clutch/c42", calls)
	}

	n = 0
	if out := runCloseSession(t, tool, "clutch/c42"); !strings.HasPrefix(out, "Nothing to close") {
		t.Errorf("not open: result = %q", out)
	}

	n, closeErr = 0, errors.New("app provider not running")
	if out := runCloseSession(t, tool, "clutch/c42"); !strings.Contains(out, "app provider not running") {
		t.Errorf("closer error: result = %q", out)
	}
}

// TestCloseSession_NoSessionKey: without a session on the context the tool
// refuses and never calls the closer.
func TestCloseSession_NoSessionKey(t *testing.T) {
	called := false
	tool := NewCloseSessionTool(func(string, string) (int, error) { called = true; return 1, nil })
	if out := runCloseSession(t, tool, ""); !strings.HasPrefix(out, "Error: no session key") {
		t.Errorf("result = %q", out)
	}
	if called {
		t.Error("closer called without a session key")
	}
}
