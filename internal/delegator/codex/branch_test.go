package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/log"
)

// setupMockBackend returns a Backend whose writer is backed by an in-process
// pipe and a reader goroutine that plays the role of the codex app-server:
// for every JSON-RPC request written by the Backend, handler is invoked with
// (method, params, id). If handler returns a non-nil result, it is delivered
// to the pendingRPC channel for that id (a successful server response). If
// handler returns an error, nil is delivered instead, which sendAndWait
// interprets as "request cancelled (process exited)" — the path a real
// server-side error or dropped connection takes.
//
// handler may be nil to reply to every request with an empty {} result.
func setupMockBackend(t *testing.T, handler func(method string, params json.RawMessage, id int64) (json.RawMessage, error)) *Backend {
	t.Helper()
	b := &Backend{}
	b.lg = log.NewComponentLogger("test")
	b.pendingRPC = make(map[int64]chan json.RawMessage)

	pr, pw := io.Pipe()
	b.writer = NewWriter(pw)

	go func() {
		dec := json.NewDecoder(pr)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return // pipe closed / EOF
			}
			var env struct {
				Method string          `json:"method,omitempty"`
				ID     *int64          `json:"id,omitempty"`
				Params json.RawMessage `json:"params,omitempty"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Errorf("mock app-server: unparseable request: %v", err)
				return
			}
			if env.ID == nil {
				continue // notification; no response expected
			}
			id := *env.ID

			var result json.RawMessage
			var err error
			if handler != nil {
				result, err = handler(env.Method, env.Params, id)
			}
			if err != nil {
				result = nil // triggers "request cancelled" in sendAndWait
			} else if result == nil {
				result = json.RawMessage("{}")
			}

			b.rpcMu.Lock()
			ch, ok := b.pendingRPC[id]
			b.rpcMu.Unlock()
			if !ok {
				t.Errorf("mock app-server: no pending channel for id %d", id)
				continue
			}
			ch <- result
		}
	}()

	t.Cleanup(func() { _ = pw.Close() })
	return b
}

// TestForkSession_Success verifies ForkSession sends a thread/fork request
// carrying the parent thread id and surfaces the new thread id from the
// server's response.
func TestForkSession_Success(t *testing.T) {
	const parentID = "parent-thread-123"
	const newID = "new-thread-456"

	var gotMethod, gotThreadID string
	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		gotMethod = method
		var p struct {
			ThreadID   string `json:"threadId"`
			LastTurnID string `json:"lastTurnId,omitempty"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			t.Errorf("parse thread/fork params: %v", err)
		}
		gotThreadID = p.ThreadID
		return json.RawMessage(`{"thread":{"id":"` + newID + `"}}`), nil
	})

	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{
		ParentSessionID: parentID,
	})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if res.SessionID != newID {
		t.Errorf("SessionID = %q, want %q", res.SessionID, newID)
	}
	if gotMethod != "thread/fork" {
		t.Errorf("sent method %q, want thread/fork", gotMethod)
	}
	if gotThreadID != parentID {
		t.Errorf("sent threadId %q, want %q", gotThreadID, parentID)
	}
}

// TestForkSession_NotStarted verifies ForkSession errors before any I/O when
// the backend has no active app-server connection (writer is nil).
func TestForkSession_NotStarted(t *testing.T) {
	b := &Backend{lg: log.NewComponentLogger("test")}
	_, err := b.ForkSession(context.Background(), delegator.ForkRequest{
		ParentSessionID: "any",
	})
	if err == nil {
		t.Fatal("expected error when writer is nil, got nil")
	}
	if !strings.Contains(err.Error(), "not started") {
		t.Errorf("expected 'not started' error, got %v", err)
	}
}

// TestForkSession_TruncateUnsupported verifies TruncateAfter > 0 is rejected
// without contacting the server.
func TestForkSession_TruncateUnsupported(t *testing.T) {
	handlerCalled := false
	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		handlerCalled = true
		return json.RawMessage(`{}`), nil
	})

	_, err := b.ForkSession(context.Background(), delegator.ForkRequest{
		ParentSessionID: "parent",
		TruncateAfter:   5,
	})
	if err == nil {
		t.Fatal("expected error for TruncateAfter > 0, got nil")
	}
	if !strings.Contains(err.Error(), "truncate") {
		t.Errorf("expected truncate-related error, got %v", err)
	}
	if handlerCalled {
		t.Error("app-server handler must not be called for an unsupported truncate")
	}
}

// TestCleanupSession_Success verifies CleanupSession sends a thread/delete
// request carrying the target session id.
func TestCleanupSession_Success(t *testing.T) {
	const targetID = "thread-to-delete"

	var gotMethod, gotThreadID string
	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		gotMethod = method
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			t.Errorf("parse thread/delete params: %v", err)
		}
		gotThreadID = p.ThreadID
		return json.RawMessage(`{}`), nil
	})

	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
		SessionID: targetID,
	}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if gotMethod != "thread/delete" {
		t.Errorf("sent method %q, want thread/delete", gotMethod)
	}
	if gotThreadID != targetID {
		t.Errorf("sent threadId %q, want %q", gotThreadID, targetID)
	}
}

// TestCleanupSession_NonExistentNotError verifies that a failing thread/delete
// (e.g. the thread is already gone) is swallowed: CleanupSession is best-effort
// and must return nil so callers can tear down unconditionally.
func TestCleanupSession_NonExistentNotError(t *testing.T) {
	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		// Simulate a server-side "no such thread" failure: returning an
		// error causes the mock to deliver nil, which sendAndWait reports
		// as "request cancelled (process exited)".
		return nil, errors.New("thread not found")
	})

	err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
		SessionID: "no-such-thread",
	})
	if err != nil {
		t.Errorf("cleanup of non-existent thread should be nil, got %v", err)
	}
}
