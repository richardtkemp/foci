package askgw

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"foci/internal/clock"
)

type mockWriter struct {
	mu     sync.Mutex
	frames [][]byte
}

func (m *mockWriter) WriteFrame(v any) error {
	b, err := Encode(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.frames = append(m.frames, b)
	m.mu.Unlock()
	return nil
}

func (m *mockWriter) Close() error { return nil }

func (m *mockWriter) lastFrame() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.frames) == 0 {
		return nil
	}
	return m.frames[len(m.frames)-1]
}

func (m *mockWriter) frameCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.frames)
}

func makeQuestions() []AskQuestion {
	return []AskQuestion{{
		Key:      "sudo",
		Header:   "Sudo",
		Question: "Run command?",
		Options:  []AskOption{{Label: "Approve"}, {Label: "Deny"}},
	}}
}

func TestRegistryAnswerIsolation(t *testing.T) {
	r := NewRegistry()
	conn1 := r.RegisterConn()
	conn2 := r.RegisterConn()
	w1 := &mockWriter{}
	w2 := &mockWriter{}

	_, err := r.Add(conn1, "same-id", "agent1", "agent1/chat1", w1, makeQuestions(), "msg1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.Add(conn2, "same-id", "agent2", "agent2/chat2", w2, makeQuestions(), "msg2", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	if r.Get(conn1, "same-id") == nil {
		t.Fatal("conn1 entry missing")
	}
	if r.Get(conn2, "same-id") == nil {
		t.Fatal("conn2 entry missing")
	}

	done := r.recordAnswer(conn1, "same-id", "sudo", singleAnswer("Approve"))
	if !done {
		t.Fatal("expected done=true for single question")
	}
	r.sendAnswer(conn1, "same-id")

	if r.Get(conn1, "same-id") != nil {
		t.Fatal("conn1 entry should be removed after answer")
	}
	if r.Get(conn2, "same-id") == nil {
		t.Fatal("conn2 entry should still exist (answer isolation)")
	}

	last := w1.lastFrame()
	if last == nil {
		t.Fatal("conn1 should have received answer frame")
	}
	var af AnswerFrame
	if err := json.Unmarshal(last, &af); err != nil {
		t.Fatal(err)
	}
	if af.Status != StatusAnswered {
		t.Errorf("status = %q, want answered", af.Status)
	}
}

func TestRegistryCancel(t *testing.T) {
	r := NewRegistry()
	connID := r.RegisterConn()
	w := &mockWriter{}
	cancelCalled := false

	_, err := r.Add(connID, "ask-1", "agent", "agent/chat", w, makeQuestions(), "msg-1", 0, func() {
		cancelCalled = true
	})
	if err != nil {
		t.Fatal(err)
	}

	ok := r.Cancel(connID, "ask-1")
	if !ok {
		t.Fatal("cancel should return true for existing entry")
	}
	if !cancelCalled {
		t.Fatal("cancelFn should have been called")
	}
	if r.Get(connID, "ask-1") != nil {
		t.Fatal("entry should be removed after cancel")
	}

	ok = r.Cancel(connID, "ask-1")
	if ok {
		t.Fatal("cancel should return false for non-existent entry")
	}
}

// TestRegistryTimeout proves an unanswered ask is auto-resolved once its
// timeout elapses. Driven by a *clock.Fake so the wait for the timeout is
// virtual (Advance fires it synchronously) rather than a real sleep racing
// the registry's internal timer — the fixed 50ms-timeout/150ms-sleep margin
// this replaced could flake under a loaded `go test -p=$(nproc)
// -parallel=16` run (#1513).
func TestRegistryTimeout(t *testing.T) {
	fc := clock.NewFake()
	r := NewRegistryWithClock(fc)
	connID := r.RegisterConn()
	w := &mockWriter{}

	_, err := r.Add(connID, "ask-1", "agent", "agent/chat", w, makeQuestions(), "msg-1", 50*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}

	fc.Advance(50 * time.Millisecond)

	if r.Get(connID, "ask-1") != nil {
		t.Fatal("entry should be removed after timeout")
	}

	last := w.lastFrame()
	if last == nil {
		t.Fatal("should have received timeout frame")
	}
	var af AnswerFrame
	if err := json.Unmarshal(last, &af); err != nil {
		t.Fatal(err)
	}
	if af.Status != StatusTimeout {
		t.Errorf("status = %q, want timeout", af.Status)
	}
}

func TestRegistryUnavailable(t *testing.T) {
	r := NewRegistry()
	w := &mockWriter{}
	r.ResolveUnavailable(w, "ask-1")

	last := w.lastFrame()
	if last == nil {
		t.Fatal("should have received unavailable frame")
	}
	var af AnswerFrame
	if err := json.Unmarshal(last, &af); err != nil {
		t.Fatal(err)
	}
	if af.Status != StatusUnavailable {
		t.Errorf("status = %q, want unavailable", af.Status)
	}
}

func TestRegistryUnregisterCleansUp(t *testing.T) {
	r := NewRegistry()
	connID := r.RegisterConn()
	w := &mockWriter{}
	cancelCalled := false

	_, _ = r.Add(connID, "ask-1", "agent", "agent/chat", w, makeQuestions(), "msg-1", 0, func() {
		cancelCalled = true
	})

	entries := r.UnregisterConn(connID)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !cancelCalled {
		t.Fatal("cancelFn should have been called on unregister")
	}
	if r.Get(connID, "ask-1") != nil {
		t.Fatal("entry should be gone after unregister")
	}
}

func TestRegistryMultiQuestionFlow(t *testing.T) {
	qs := []AskQuestion{
		{Key: "q1", Question: "First?", Options: []AskOption{{Label: "A"}, {Label: "B"}}},
		{Key: "q2", Question: "Second?", Options: []AskOption{{Label: "C"}, {Label: "D"}}},
	}
	r := NewRegistry()
	connID := r.RegisterConn()
	w := &mockWriter{}

	_, err := r.Add(connID, "ask-1", "agent", "agent/chat", w, qs, "msg-1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	done := r.recordAnswer(connID, "ask-1", "q1", singleAnswer("A"))
	if done {
		t.Fatal("should not be done after first question")
	}

	e := r.Get(connID, "ask-1")
	if e == nil || e.current != 1 {
		t.Fatal("entry should be at question index 1")
	}

	done = r.recordAnswer(connID, "ask-1", "q2", singleAnswer("C"))
	if !done {
		t.Fatal("should be done after second question")
	}
	r.sendAnswer(connID, "ask-1")

	if r.Get(connID, "ask-1") != nil {
		t.Fatal("entry should be removed")
	}

	last := w.lastFrame()
	var af AnswerFrame
	if err := json.Unmarshal(last, &af); err != nil {
		t.Fatal(err)
	}
	if len(af.Answers) != 2 {
		t.Fatalf("expected 2 answers, got %d", len(af.Answers))
	}
}
