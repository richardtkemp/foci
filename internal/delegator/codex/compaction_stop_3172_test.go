package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// compactionStub answers thread/compact/start the way codex app-server does
// (codex-rs app-server tests/suite/v2/compaction.rs): an empty response, with
// the compaction then running as its own turn. turn/started for that turn is
// sent before the reply, the order that tests compactSent being set before
// the request goes out.
func compactionStub(b **Backend, turnID string) func(string, json.RawMessage, int64) (json.RawMessage, error) {
	return func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method != "thread/compact/start" {
			return nil, rpcErrorReply{code: -32601, message: "unexpected method " + method}
		}
		(*b).dispatchLocal([]byte(fmt.Sprintf(`{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":%q}}}`, turnID)))
		return json.RawMessage(`{}`), nil
	}
}

func compactionItemStarted(turnID string) []byte {
	return []byte(fmt.Sprintf(`{"method":"item/started","params":{"threadId":"thread-1","turnId":%q,"item":{"type":"contextCompaction","id":"cc_1"}}}`, turnID))
}

func compactionItemCompleted(turnID string) []byte {
	return []byte(fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":%q,"item":{"type":"contextCompaction","id":"cc_1"}}}`, turnID))
}

func turnCompletedLine(turnID, status, errMsg string) []byte {
	errPart := ""
	if errMsg != "" {
		errPart = fmt.Sprintf(`,"error":{"message":%q}`, errMsg)
	}
	return []byte(fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":%q,"status":%q%s}}}`, turnID, status, errPart))
}

// startCompaction arms the wait and sends thread/compact/start, as
// agent.runDelegatedCompact does, then starts WaitForCompaction in the
// background with a deadline far past the test's own patience.
func startCompaction(t *testing.T, turnID string) (*Backend, <-chan error) {
	t.Helper()
	var b *Backend
	b = setupMockBackend(t, compactionStub(&b, turnID))
	b.ArmCompactionWait()
	if err := b.triggerCompaction(); err != nil {
		t.Fatalf("triggerCompaction: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- b.WaitForCompaction(ctx) }()
	return b, done
}

func waitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForCompaction still blocked 5s after the compaction turn ended (before #3172 it waited out the 20m compaction timeout)")
		return nil
	}
}

// TestCompactionTurnInterruptedReleasesWait is the #3172 regression: /stop
// interrupted codex's compaction turn (turn_aborted, wire turn/completed
// status "interrupted"), no contextCompaction item completed, and foci kept
// waiting the full 20 minutes while it held the user's messages.
func TestCompactionTurnInterruptedReleasesWait(t *testing.T) {
	t.Parallel()
	b, done := startCompaction(t, "tu_compact")
	b.dispatchLocal(compactionItemStarted("tu_compact"))
	b.dispatchLocal(turnCompletedLine("tu_compact", "interrupted", ""))

	if err := waitResult(t, done); !errors.Is(err, delegator.ErrCompactionStopped) {
		t.Fatalf("WaitForCompaction = %v, want ErrCompactionStopped", err)
	}
}

// TestCompactionTurnInterruptedBeforeItemReleasesWait: a /stop that lands
// before codex reports the contextCompaction item still releases the wait;
// the turn is known from its turn/started.
func TestCompactionTurnInterruptedBeforeItemReleasesWait(t *testing.T) {
	t.Parallel()
	b, done := startCompaction(t, "tu_compact")
	b.dispatchLocal(turnCompletedLine("tu_compact", "interrupted", ""))

	if err := waitResult(t, done); !errors.Is(err, delegator.ErrCompactionStopped) {
		t.Fatalf("WaitForCompaction = %v, want ErrCompactionStopped", err)
	}
}

// TestCompactionTurnFailedReleasesWait: a failed compaction turn ends the
// wait with codex's error, not a timeout.
func TestCompactionTurnFailedReleasesWait(t *testing.T) {
	t.Parallel()
	b, done := startCompaction(t, "tu_compact")
	b.dispatchLocal(compactionItemStarted("tu_compact"))
	b.dispatchLocal(turnCompletedLine("tu_compact", "failed", "Your workspace is out of credits"))

	err := waitResult(t, done)
	if err == nil || errors.Is(err, delegator.ErrCompactionStopped) || !strings.Contains(err.Error(), "out of credits") {
		t.Fatalf("WaitForCompaction = %v, want a failure carrying codex's message", err)
	}
}

// TestCompactionCompletedStillSucceeds: the normal order (item/completed,
// then turn/completed "completed") is a success; the turn's end after the
// item must not overwrite it.
func TestCompactionCompletedStillSucceeds(t *testing.T) {
	t.Parallel()
	b, done := startCompaction(t, "tu_compact")
	b.dispatchLocal(compactionItemStarted("tu_compact"))
	b.dispatchLocal(compactionItemCompleted("tu_compact"))
	b.dispatchLocal(turnCompletedLine("tu_compact", "completed", ""))

	if err := waitResult(t, done); err != nil {
		t.Fatalf("WaitForCompaction = %v, want nil", err)
	}
}

// TestCompactionWaitIgnoresOtherTurnsEnd: a turn/completed for a turn that
// is not the compaction's does not release the wait.
func TestCompactionWaitIgnoresOtherTurnsEnd(t *testing.T) {
	t.Parallel()
	b, done := startCompaction(t, "tu_compact")
	b.dispatchLocal(turnCompletedLine("tu_other", "interrupted", ""))

	select {
	case err := <-done:
		t.Fatalf("WaitForCompaction returned %v on another turn's end, want still waiting", err)
	case <-time.After(100 * time.Millisecond):
	}
	b.dispatchLocal(compactionItemCompleted("tu_compact"))
	if err := waitResult(t, done); err != nil {
		t.Fatalf("WaitForCompaction = %v, want nil", err)
	}
}

// TestCompactionWaitReleasedWhenConnectionEnds: with the app-server gone no
// compaction item can arrive, so the wait ends at once with an error.
func TestCompactionWaitReleasedWhenConnectionEnds(t *testing.T) {
	t.Parallel()
	b, done := startCompaction(t, "tu_compact")
	b.onReaderStopped(errors.New("EOF"))

	err := waitResult(t, done)
	if err == nil || errors.Is(err, delegator.ErrCompactionStopped) {
		t.Fatalf("WaitForCompaction = %v, want a connection error", err)
	}
}
