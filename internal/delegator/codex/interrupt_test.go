package codex

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// interruptStub answers turn/interrupt the way codex app-server 0.167.0 does
// (live probe on the work Mac, 2026-11-14, #3117): turn/interrupt is a
// REQUEST (ClientRequest.json) whose params require threadId AND turnId. A
// request without turnId is rejected with -32600 "missing field `turnId`"; a
// notification never reaches the handler at all, which is how codex treated
// foci's old shape — it did nothing.
type interruptStub struct {
	mu     sync.Mutex
	calls  int
	params []map[string]any
}

func (s *interruptStub) handle(method string, params json.RawMessage, _ int64) (json.RawMessage, error) {
	if method != "turn/interrupt" {
		return nil, rpcErrorReply{code: -32601, message: "unexpected method " + method}
	}
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	s.calls++
	s.params = append(s.params, p)
	s.mu.Unlock()
	if p["turnId"] == nil || p["turnId"] == "" {
		return nil, rpcErrorReply{code: -32600, message: "Invalid request: missing field `turnId`"}
	}
	if p["threadId"] == nil || p["threadId"] == "" {
		return nil, rpcErrorReply{code: -32600, message: "Invalid request: missing field `threadId`"}
	}
	return json.RawMessage(`{}`), nil
}

func (s *interruptStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestInterrupt_SendsRequestWithTurnID is the #3117 regression: /stop on a
// codex session did nothing because Interrupt sent turn/interrupt as a
// notification carrying only threadId.
func TestInterrupt_SendsRequestWithTurnID(t *testing.T) {
	t.Parallel()
	stub := &interruptStub{}
	b := setupMockBackend(t, stub.handle)
	b.turnMu.Lock()
	b.turnID = "turn-running"
	b.turnMu.Unlock()

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt = %v, want nil", err)
	}
	if stub.count() != 1 {
		t.Fatalf("turn/interrupt requests answered by codex = %d, want 1 (a notification never gets an answer and codex ignores it)", stub.count())
	}
	p := stub.params[0]
	if p["threadId"] != "thread-1" || p["turnId"] != "turn-running" {
		t.Errorf("turn/interrupt params = %v, want threadId=thread-1 turnId=turn-running", p)
	}
}

// TestInterrupt_NoRunningTurnIsNoOp: with no running turn nothing is sent and
// /stop is not failed. Live: codex never answers a turn/interrupt sent while
// the thread is idle (it parks it), so sending one would only hang.
func TestInterrupt_NoRunningTurnIsNoOp(t *testing.T) {
	t.Parallel()
	stub := &interruptStub{}
	b := setupMockBackend(t, stub.handle)

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt with no turn = %v, want nil", err)
	}
	time.Sleep(50 * time.Millisecond)
	if stub.count() != 0 {
		t.Errorf("Interrupt with no running turn sent %d request(s), want 0", stub.count())
	}
}

// TestInterrupt_ErrorReturned: a codex rejection while the turn is still the
// running one is returned to the caller (and logged at WARN).
func TestInterrupt_ErrorReturned(t *testing.T) {
	t.Parallel()
	b := setupMockBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		return nil, rpcErrorReply{code: -32000, message: "internal error"}
	})
	b.turnMu.Lock()
	b.turnID = "turn-running"
	b.turnMu.Unlock()

	if err := b.Interrupt(context.Background()); err == nil {
		t.Fatal("Interrupt = nil, want the codex error")
	}
}

// TestInterrupt_RaceWithTurnEndIsNoOp: the turn ended (turnId cleared or
// replaced) while the request was in flight. codex's rejection is then about
// a turn that is already gone, so /stop must not fail.
func TestInterrupt_RaceWithTurnEndIsNoOp(t *testing.T) {
	t.Parallel()
	var b *Backend
	b = setupMockBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		b.turnMu.Lock()
		b.turnID = ""
		b.turnMu.Unlock()
		return nil, rpcErrorReply{code: -32600, message: "expected active turn id turn-running but found turn-next"}
	})
	b.turnMu.Lock()
	b.turnID = "turn-running"
	b.turnMu.Unlock()

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt raced with turn end = %v, want nil", err)
	}
}

// TestTurnStartedRecordsTurnID: a turn foci did not start with turn/start
// (codex-initiated) is still stoppable, because turn/started carries its id.
func TestTurnStartedRecordsTurnID(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.dispatch([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-auto","status":"inProgress"}}}`))
	b.turnMu.Lock()
	got := b.turnID
	b.turnMu.Unlock()
	if got != "turn-auto" {
		t.Errorf("turnID after turn/started = %q, want turn-auto", got)
	}
}
