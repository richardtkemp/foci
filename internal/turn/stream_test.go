package turn

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockTransport records SendInitial/EditStream calls for testing.
type mockTransport struct {
	mu        sync.Mutex
	sendCalls []string
	editCalls []transportEditCall
	sendMsgID string
	sendErr   error
	editErr   error
}

type transportEditCall struct {
	msgID string
	text  string
}

func (m *mockTransport) SendInitial(text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCalls = append(m.sendCalls, text)
	if m.sendErr != nil {
		return "", m.sendErr
	}
	return m.sendMsgID, nil
}

func (m *mockTransport) EditStream(msgID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.editCalls = append(m.editCalls, transportEditCall{msgID, text})
	return m.editErr
}

func TestStreamWriter_NonLive_BufferOnly(t *testing.T) {
	// Verifies that when live=false, deltas are buffered but no messages are sent.
	transport := &mockTransport{sendMsgID: "1"}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 1000, false)

	sw.OnDelta("hello ")
	sw.OnDelta("world")

	msgID := sw.Finish()
	if msgID != "" {
		t.Errorf("msgID = %q, want empty (non-live)", msgID)
	}
	if got := sw.Content(); got != "hello world" {
		t.Errorf("content = %q, want %q", got, "hello world")
	}
	if len(transport.sendCalls) != 0 {
		t.Errorf("sendCalls = %d, want 0", len(transport.sendCalls))
	}
}

func TestStreamWriter_Live_SendsInitial(t *testing.T) {
	// Verifies that the first delta in live mode triggers SendInitial.
	transport := &mockTransport{sendMsgID: "42"}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 1000, true)

	sw.OnDelta("hello")

	msgID := sw.Finish()
	if msgID != "42" {
		t.Errorf("msgID = %q, want %q", msgID, "42")
	}
	if len(transport.sendCalls) != 1 {
		t.Fatalf("sendCalls = %d, want 1", len(transport.sendCalls))
	}
	if transport.sendCalls[0] != "hello" {
		t.Errorf("sendInitial text = %q, want %q", transport.sendCalls[0], "hello")
	}
}

func TestStreamWriter_Truncation(t *testing.T) {
	// Verifies that buffer contents are truncated at the maxChars boundary.
	transport := &mockTransport{sendMsgID: "1"}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 10, true)

	sw.OnDelta(strings.Repeat("x", 15))
	sw.Finish()

	if len(transport.sendCalls) == 0 {
		t.Fatal("expected at least one send")
	}
	sent := transport.sendCalls[0]
	if !strings.HasSuffix(sent, "...") {
		t.Errorf("truncated text should end with ..., got %q", sent)
	}
	// 10 chars + "..." = 13
	if len(sent) != 13 {
		t.Errorf("truncated len = %d, want 13", len(sent))
	}
}

func TestStreamWriter_FullContent_NotTruncated(t *testing.T) {
	// Verifies that Content() returns the full buffer even after truncation during send.
	transport := &mockTransport{sendMsgID: "1"}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 10, true)

	sw.OnDelta(strings.Repeat("x", 15))
	sw.Finish()

	if got := sw.Content(); len(got) != 15 {
		t.Errorf("content len = %d, want 15 (full buffer, not truncated)", len(got))
	}
}

func TestStreamWriter_Finish_Idempotent(t *testing.T) {
	// Verifies that Finish is safe to call multiple times.
	transport := &mockTransport{sendMsgID: "99"}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 1000, true)

	sw.OnDelta("test")
	id1 := sw.Finish()
	id2 := sw.Finish()

	if id1 != "99" {
		t.Errorf("first finish = %q, want %q", id1, "99")
	}
	if id2 != "99" {
		t.Errorf("second finish = %q, want %q", id2, "99")
	}
}

func TestStreamWriter_SendError_NoMsgID(t *testing.T) {
	// Verifies that if SendInitial fails, the stream writer has no message ID.
	transport := &mockTransport{sendErr: fmt.Errorf("API error")}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 1000, true)

	sw.OnDelta("test")
	msgID := sw.Finish()

	if msgID != "" {
		t.Errorf("msgID = %q, want empty (send failed)", msgID)
	}
}

func TestStreamWriter_DeltaAfterFinish_Ignored(t *testing.T) {
	// Verifies that deltas after Finish are silently ignored.
	transport := &mockTransport{sendMsgID: "1"}
	sw := NewStreamWriter(transport, 50*time.Millisecond, 1000, true)

	sw.OnDelta("before")
	sw.Finish()
	sw.OnDelta("after")

	if got := sw.Content(); got != "before" {
		t.Errorf("content = %q, want %q", got, "before")
	}
}

func TestStreamWriter_EditLoop_FiresOnDirty(t *testing.T) {
	// Verifies that the edit loop fires edits when the buffer is dirty.
	transport := &mockTransport{sendMsgID: "1"}
	sw := NewStreamWriter(transport, 10*time.Millisecond, 1000, true)

	sw.OnDelta("first")
	// Wait for edit loop to fire at least once.
	time.Sleep(50 * time.Millisecond)
	sw.OnDelta(" second")
	time.Sleep(50 * time.Millisecond)
	sw.Finish()

	transport.mu.Lock()
	edits := len(transport.editCalls)
	transport.mu.Unlock()
	if edits == 0 {
		t.Error("expected at least one edit from the edit loop")
	}
}
