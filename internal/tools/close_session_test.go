package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func runCloseSession(t *testing.T, tool *Tool, sessionKey string) string {
	t.Helper()
	return runCloseSessionArgs(t, tool, sessionKey, `{}`)
}

func runCloseSessionArgs(t *testing.T, tool *Tool, sessionKey, args string) string {
	t.Helper()
	ctx := context.Background()
	if sessionKey != "" {
		ctx = WithSessionKey(ctx, sessionKey)
	}
	res, err := tool.Execute(ctx, []byte(args))
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
	}, func(string, string) (int, error) {
		t.Error("reader called without mark_read")
		return 0, nil
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
	tool := NewCloseSessionTool(func(string, string) (int, error) { called = true; return 1, nil },
		func(string, string) (int, error) { called = true; return 1, nil })
	if out := runCloseSession(t, tool, ""); !strings.HasPrefix(out, "Error: no session key") {
		t.Errorf("result = %q", out)
	}
	if called {
		t.Error("closer called without a session key")
	}
}

// TestCloseSession_MarkRead (#2592): mark_read closes and then hands the
// reader the calling session, even when nothing was open (an already-closed
// chat can still hold the unread reply); each reader outcome is reported.
func TestCloseSession_MarkRead(t *testing.T) {
	type call struct{ agent, key string }
	var reads []call
	closed, marked, readErr := 1, 1, error(nil)
	tool := NewCloseSessionTool(func(string, string) (int, error) { return closed, nil },
		func(agentID, sessionKey string) (int, error) {
			reads = append(reads, call{agentID, sessionKey})
			return marked, readErr
		})

	out := runCloseSessionArgs(t, tool, "clutch/c42", `{"mark_read":true}`)
	if !strings.HasPrefix(out, "Closed this chat") || !strings.Contains(out, "marked read once this turn ends") {
		t.Errorf("open chat: result = %q", out)
	}
	if len(reads) != 1 || reads[0] != (call{"clutch", "clutch/c42"}) {
		t.Fatalf("reader calls = %+v, want one for clutch clutch/c42", reads)
	}

	closed = 0
	if out := runCloseSessionArgs(t, tool, "clutch/c42", `{"mark_read":true}`); !strings.HasPrefix(out, "Nothing to close") ||
		!strings.Contains(out, "marked read once this turn ends") {
		t.Errorf("already closed: result = %q", out)
	}
	if len(reads) != 2 {
		t.Errorf("reader calls = %d, want the already-closed chat marked too", len(reads))
	}

	marked = 0
	if out := runCloseSessionArgs(t, tool, "clutch/c42", `{"mark_read":true}`); !strings.Contains(out, "Nothing to mark read") {
		t.Errorf("no chat: result = %q", out)
	}

	readErr = errors.New("app provider not running")
	if out := runCloseSessionArgs(t, tool, "clutch/c42", `{"mark_read":true}`); !strings.Contains(out, "cannot mark it read: app provider not running") {
		t.Errorf("reader error: result = %q", out)
	}

	reads, closed = nil, 1
	if out := runCloseSessionArgs(t, tool, "clutch/c42", `{"mark_read":false}`); strings.Contains(out, "read") || len(reads) != 0 {
		t.Errorf("mark_read=false: result = %q, reader calls = %d; want the plain close", out, len(reads))
	}
}
