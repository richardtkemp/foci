package askgw

import (
	"testing"
	"time"

	"foci/internal/question"
)

// newHTTPTestServer builds a bare *Server (no Unix listener — HTTPTransport
// never touches the socket side) wired to present/resolve fakes, mirroring
// the present/resolve fixtures server_test.go uses for the socket path.
func newHTTPTestServer(t *testing.T, present PresentFn, resolve ResolveSessionFn, timeout time.Duration) *Server {
	t.Helper()
	srv, err := NewServer(ServerDeps{
		SocketPath:     "", // unused: Start() is never called for these tests
		AllowedUIDs:    nil,
		MaxFrameBytes:  1 << 20,
		DefaultTimeout: timeout,
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

func askBody(id string) []byte {
	b, _ := Encode(&AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        id,
		Source:    "test",
		Questions: makeQuestions(),
	})
	return b
}

func TestHTTPSubmitAnswerRoundTrip(t *testing.T) {
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }

	srv := newHTTPTestServer(t, present, resolve, 0)
	tr := NewHTTPTransport(srv)

	id, ok, code, msg := tr.Submit(askBody("http-1"))
	if !ok {
		t.Fatalf("submit failed: code=%s msg=%s", code, msg)
	}
	if id != "http-1" {
		t.Fatalf("id = %q, want http-1", id)
	}

	// Poll while still pending: should return quickly with status "pending"
	// rather than blocking the full wait.
	af, found := tr.Poll(id, 50*time.Millisecond)
	if !found {
		t.Fatal("expected found=true for a submitted ask")
	}
	if af.Status != StatusPending {
		t.Errorf("status = %q, want %q", af.Status, StatusPending)
	}

	// Human answers concurrently with the next long-poll.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cb.fire(question.OptionData(0))
	}()

	af, found = tr.Poll(id, time.Second)
	if !found {
		t.Fatal("expected found=true")
	}
	if af.Status != StatusAnswered {
		t.Errorf("status = %q, want %q", af.Status, StatusAnswered)
	}
	if string(af.Answers["sudo"]) != `"Approve"` {
		t.Errorf("answers[sudo] = %s, want \"Approve\"", af.Answers["sudo"])
	}

	// The entry is consumed by the terminal Poll — a further poll 404s.
	if _, found := tr.Poll(id, 10*time.Millisecond); found {
		t.Error("expected found=false after the terminal answer was already collected")
	}
}

func TestHTTPPollUnknownID(t *testing.T) {
	srv := newHTTPTestServer(t, nil, nil, 0)
	tr := NewHTTPTransport(srv)
	if _, found := tr.Poll("never-submitted", 10*time.Millisecond); found {
		t.Error("expected found=false for an id that was never submitted")
	}
}

func TestHTTPSubmitDuplicateID(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	srv := newHTTPTestServer(t, present, resolve, 0)
	tr := NewHTTPTransport(srv)

	if _, ok, _, _ := tr.Submit(askBody("dup")); !ok {
		t.Fatal("first submit should succeed")
	}
	id, ok, code, _ := tr.Submit(askBody("dup"))
	if ok {
		t.Fatal("second submit with the same id should be rejected")
	}
	if code != "duplicate_id" {
		t.Errorf("code = %q, want duplicate_id", code)
	}
	if id != "dup" {
		t.Errorf("id = %q, want dup", id)
	}
}

func TestHTTPSubmitMalformed(t *testing.T) {
	srv := newHTTPTestServer(t, nil, nil, 0)
	tr := NewHTTPTransport(srv)

	// Empty questions slice fails AskFrame.Validate() inside handleAsk.
	body, _ := Encode(&AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "bad-1"})
	id, ok, code, msg := tr.Submit(body)
	if ok {
		t.Fatal("expected malformed ask to be rejected")
	}
	if code != "malformed" {
		t.Errorf("code = %q, want malformed", code)
	}
	if id != "bad-1" {
		t.Errorf("id = %q, want bad-1", id)
	}
	if msg == "" {
		t.Error("expected a validation error message")
	}
}

func TestHTTPSubmitWrongType(t *testing.T) {
	srv := newHTTPTestServer(t, nil, nil, 0)
	tr := NewHTTPTransport(srv)

	body, _ := Encode(&CancelFrame{Protocol: ProtocolVersion, Type: TypeCancel, ID: "c-1"})
	_, ok, code, _ := tr.Submit(body)
	if ok {
		t.Fatal("expected a non-ask frame type to be rejected")
	}
	if code != "unknown_type" {
		t.Errorf("code = %q, want unknown_type", code)
	}
}

func TestHTTPSubmitUnavailable(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		t.Fatal("present should not be called when no session resolves")
		return "", false
	}
	resolve := func(string) (string, string) { return "", "" }
	srv := newHTTPTestServer(t, present, resolve, 0)
	tr := NewHTTPTransport(srv)

	id, ok, _, _ := tr.Submit(askBody("unavail-1"))
	if !ok {
		t.Fatal("submit itself should still succeed (mirrors socket ack-then-unavailable-answer)")
	}
	af, found := tr.Poll(id, time.Second)
	if !found {
		t.Fatal("expected found=true")
	}
	if af.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", af.Status, StatusUnavailable)
	}
}

func TestHTTPTimeout(t *testing.T) {
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	srv := newHTTPTestServer(t, present, resolve, 30*time.Millisecond)
	tr := NewHTTPTransport(srv)

	id, ok, _, _ := tr.Submit(askBody("timeout-1"))
	if !ok {
		t.Fatal("submit failed")
	}
	af, found := tr.Poll(id, time.Second)
	if !found {
		t.Fatal("expected found=true")
	}
	if af.Status != StatusTimeout {
		t.Errorf("status = %q, want %q", af.Status, StatusTimeout)
	}
}

func TestHTTPCancel(t *testing.T) {
	cb := &capturedCallback{}
	cancelled := make(chan struct{})
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	srv, err := NewServer(ServerDeps{
		MaxFrameBytes:  1 << 20,
		Present:        present,
		CancelPrompt:   func(string, string) { close(cancelled) },
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	tr := NewHTTPTransport(srv)

	id, ok, _, _ := tr.Submit(askBody("cancel-1"))
	if !ok {
		t.Fatal("submit failed")
	}

	// An in-flight Poll unblocks with StatusCancelled rather than waiting
	// out the (unset, i.e. infinite) timeout.
	done := make(chan AnswerFrame, 1)
	go func() {
		af, _ := tr.Poll(id, time.Second)
		done <- af
	}()
	time.Sleep(20 * time.Millisecond)

	if !tr.Cancel(id) {
		t.Fatal("cancel should succeed for a pending ask")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("expected the chat prompt cancelFn to fire")
	}
	select {
	case af := <-done:
		if af.Status != StatusCancelled {
			t.Errorf("status = %q, want %q", af.Status, StatusCancelled)
		}
	case <-time.After(time.Second):
		t.Fatal("expected the in-flight Poll to unblock")
	}

	// A second cancel is a no-op (already gone).
	if tr.Cancel(id) {
		t.Error("expected the second cancel to report false (already withdrawn)")
	}
}

func TestHTTPSweepAbandoned(t *testing.T) {
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	srv := newHTTPTestServer(t, present, resolve, 0)
	tr := NewHTTPTransport(srv)

	id, ok, _, _ := tr.Submit(askBody("orphan-1"))
	if !ok {
		t.Fatal("submit failed")
	}
	cb.fire(question.OptionData(0))
	// Give the answer callback's goroutine time to land before sweeping —
	// onAnswer runs synchronously off the callback in this fake, but be
	// generous to avoid a flake if that ever changes.
	time.Sleep(20 * time.Millisecond)

	// Nobody ever polls: sweep with a zero staleness threshold should evict
	// the already-resolved, never-collected entry immediately.
	tr.sweepAbandoned(0)

	if _, found := tr.Poll(id, 10*time.Millisecond); found {
		t.Error("expected the abandoned entry to have been swept")
	}
}

// notifyBody mirrors askBody for a NotifyFrame — used by
// Server.HandleNotifyFrame, the entry point cmd/foci-gw/askgw_http.go's
// POST /askgw/notify handler calls (it has no connWriter/connID of its own,
// unlike the socket transport, so it can't go through handleFrame).
func notifyBody(id string, exitCode int) []byte {
	b, _ := Encode(&NotifyFrame{
		Protocol: ProtocolVersion,
		Type:     TypeNotify,
		ID:       id,
		ExitCode: &exitCode,
	})
	return b
}

func TestHTTPNotifyEditsAnsweredAsk(t *testing.T) {
	cb := &capturedCallback{}
	nr := &notifyRecorder{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "platform-msg-http", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }

	srv, err := NewServer(ServerDeps{
		SocketPath:     "",
		AllowedUIDs:    nil,
		MaxFrameBytes:  1 << 20,
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
		EditMessage:    nr.edit,
		NotifyFallback: nr.fallback,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	tr := NewHTTPTransport(srv)

	id, ok, _, _ := tr.Submit(askBody("http-notify-1"))
	if !ok {
		t.Fatal("submit failed")
	}
	cb.fire(question.OptionData(0))
	af, found := tr.Poll(id, time.Second)
	if !found || af.Status != StatusAnswered {
		t.Fatalf("poll = (found=%v, status=%v), want answered", found, af.Status)
	}

	// The HTTP notify endpoint calls srv.HandleNotifyFrame directly — it
	// never touches HTTPTransport (notify is fire-and-forget, no id/poll
	// bookkeeping of its own).
	gotID, ok, code, msg := srv.HandleNotifyFrame(notifyBody(id, 0))
	if !ok {
		t.Fatalf("HandleNotifyFrame failed: code=%s msg=%s", code, msg)
	}
	if gotID != id {
		t.Errorf("returned id = %q, want %q", gotID, id)
	}

	calls, _, fellBack := nr.snapshot()
	if fellBack {
		t.Fatal("expected the edit path, not the standalone fallback")
	}
	if len(calls) != 1 || calls[0].msgID != "platform-msg-http" {
		t.Fatalf("edit calls = %+v, want one call against platform-msg-http", calls)
	}
}

func TestHTTPNotifyRejectsMalformedEnvelope(t *testing.T) {
	srv := newHTTPTestServer(t, func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}, func(string) (string, string) { return "agent1", "agent1/chat1" }, 0)

	_, ok, code, _ := srv.HandleNotifyFrame([]byte(`{not json`))
	if ok {
		t.Fatal("expected malformed JSON to be rejected")
	}
	if code != "malformed" {
		t.Errorf("code = %q, want %q", code, "malformed")
	}

	_, ok, code, _ = srv.HandleNotifyFrame(askBody("wrong-type"))
	if ok {
		t.Fatal("expected a non-notify frame type to be rejected")
	}
	if code != "unknown_type" {
		t.Errorf("code = %q, want %q", code, "unknown_type")
	}
}
