package askgw

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/question"
)

type capturedCallback struct {
	mu sync.Mutex
	cb func(data string)
}

func (c *capturedCallback) set(cb func(data string)) { c.mu.Lock(); c.cb = cb; c.mu.Unlock() }
func (c *capturedCallback) fire(data string) {
	c.mu.Lock()
	cb := c.cb
	c.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

func startTestServer(t *testing.T, present PresentFn, resolve ResolveSessionFn) (*Server, string) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "askgw-test.sock")
	uid := strconv.Itoa(os.Getuid())

	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		MaxFrameBytes:  1 << 20,
		DefaultTimeout: 0,
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, sockPath
}

func dialServer(t *testing.T, sockPath string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sendFrame(t *testing.T, conn net.Conn, frame any) {
	t.Helper()
	b, err := Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestE2E_AnsweredRoundTrip(t *testing.T) {
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(frameAgent string) (string, string) {
		return "agent1", "agent1/chat1"
	}

	srv, sockPath := startTestServer(t, present, resolve)
	_ = srv

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-1",
		Source:    "test",
		Questions: makeQuestions(),
	})

	ack := readFrame(t, r)
	if ack["type"] != TypeAck {
		t.Fatalf("expected ack, got %v", ack["type"])
	}

	time.Sleep(50 * time.Millisecond)
	cb.fire(question.OptionData(0))

	ans := readFrame(t, r)
	if ans["type"] != TypeAnswer {
		t.Fatalf("expected answer, got %v", ans["type"])
	}
	if ans["status"] != StatusAnswered {
		t.Errorf("status = %v, want %v", ans["status"], StatusAnswered)
	}
	answers, ok := ans["answers"].(map[string]any)
	if !ok {
		t.Fatal("missing answers map")
	}
	if answers["sudo"] != "Approve" {
		t.Errorf("answer = %v, want Approve", answers["sudo"])
	}
}

func TestE2E_UnavailableNoSession(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		t.Fatal("present should not be called")
		return "", false
	}
	resolve := func(frameAgent string) (string, string) { return "", "" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-2",
		Questions: makeQuestions(),
	})

	ans := readFrame(t, r)
	if ans["type"] != TypeAnswer {
		t.Fatalf("expected answer, got %v", ans["type"])
	}
	if ans["status"] != StatusUnavailable {
		t.Errorf("status = %v, want %v", ans["status"], StatusUnavailable)
	}
}

func TestE2E_PresentFails(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", false
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-3",
		Questions: makeQuestions(),
	})

	ans := readFrame(t, r)
	if ans["status"] != StatusUnavailable {
		t.Errorf("status = %v, want %v", ans["status"], StatusUnavailable)
	}
}

func TestE2E_Cancel(t *testing.T) {
	cancelCalled := false
	var cancelMu sync.Mutex
	cb := &capturedCallback{}

	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	cancelPrompt := func(msgID, finalText string) {
		cancelMu.Lock()
		cancelCalled = true
		cancelMu.Unlock()
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := filepath.Join(t.TempDir(), "askgw-cancel.sock")
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		Present:        present,
		CancelPrompt:   cancelPrompt,
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-cancel",
		Questions: makeQuestions(),
	})
	_ = readFrame(t, r) // ack

	time.Sleep(50 * time.Millisecond)

	sendFrame(t, conn, &CancelFrame{
		Protocol: ProtocolVersion,
		Type:     TypeCancel,
		ID:       "e2e-cancel",
		Reason:   "timeout",
	})

	time.Sleep(50 * time.Millisecond)

	cancelMu.Lock()
	cc := cancelCalled
	cancelMu.Unlock()
	if !cc {
		t.Fatal("cancelPrompt should have been called")
	}

	if cb != nil {
		_ = cb
	}
}

func TestE2E_DismissedViaCancelButton(t *testing.T) {
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-dismiss",
		Questions: makeQuestions(),
	})
	_ = readFrame(t, r) // ack

	time.Sleep(50 * time.Millisecond)
	cb.fire(question.CancelData)

	ans := readFrame(t, r)
	if ans["status"] != StatusDismissed {
		t.Errorf("status = %v, want %v", ans["status"], StatusDismissed)
	}
}

func TestE2E_BadProtocolRejected(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "a", "a/c" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  "askgw/99",
		Type:      TypeAsk,
		ID:        "bad",
		Questions: makeQuestions(),
	})

	err := readFrame(t, r)
	if err["type"] != TypeError {
		t.Fatalf("expected error, got %v", err["type"])
	}
	if err["code"] != "bad_protocol" {
		t.Errorf("code = %v, want bad_protocol", err["code"])
	}
}

func TestE2E_MultiQuestion(t *testing.T) {
	var callbacks []func(string)
	var cbMu sync.Mutex

	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cbMu.Lock()
		callbacks = append(callbacks, onResponse)
		cbMu.Unlock()
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	qs := []AskQuestion{
		{Key: "q1", Header: "First", Question: "Pick one:", Options: []AskOption{{Label: "A"}, {Label: "B"}}},
		{Key: "q2", Header: "Second", Question: "Pick again:", Options: []AskOption{{Label: "C"}, {Label: "D"}}},
	}

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-multi",
		Questions: qs,
	})
	_ = readFrame(t, r) // ack

	time.Sleep(50 * time.Millisecond)
	cbMu.Lock()
	firstCB := callbacks[0]
	cbMu.Unlock()
	firstCB(question.OptionData(0)) // answer "A"

	time.Sleep(50 * time.Millisecond)
	cbMu.Lock()
	if len(callbacks) < 2 {
		t.Fatalf("expected 2 callbacks, got %d", len(callbacks))
	}
	secondCB := callbacks[1]
	cbMu.Unlock()
	secondCB(question.OptionData(1)) // answer "D"

	ans := readFrame(t, r)
	if ans["status"] != StatusAnswered {
		t.Fatalf("status = %v, want answered", ans["status"])
	}
	answers := ans["answers"].(map[string]any)
	if answers["q1"] != "A" {
		t.Errorf("q1 = %v, want A", answers["q1"])
	}
	if answers["q2"] != "D" {
		t.Errorf("q2 = %v, want D", answers["q2"])
	}
}

func TestE2E_ConnectionDropCancelsPending(t *testing.T) {
	cancelCalled := false
	var cancelMu sync.Mutex
	cb := &capturedCallback{}

	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	cancelPrompt := func(msgID, finalText string) {
		cancelMu.Lock()
		cancelCalled = true
		cancelMu.Unlock()
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := filepath.Join(t.TempDir(), "askgw-drop.sock")
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		Present:        present,
		CancelPrompt:   cancelPrompt,
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-drop",
		Questions: makeQuestions(),
	})
	_ = readFrame(t, r) // ack

	time.Sleep(50 * time.Millisecond)

	conn.Close()

	time.Sleep(100 * time.Millisecond)

	cancelMu.Lock()
	cc := cancelCalled
	cancelMu.Unlock()
	if !cc {
		t.Fatal("cancelPrompt should fire when connection drops")
	}
}

func TestE2E_RejectBadUID(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}
	sockPath := filepath.Join(t.TempDir(), "askgw-baduid.sock")
	srv, err := NewServer(ServerDeps{
		SocketPath:  sockPath,
		AllowedUIDs: []string{"99999"},
		Present: func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
			return "", true
		},
		CancelPrompt:   func(string, string) {},
		ResolveSession: func(string) (string, string) { return "a", "a/c" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Server rejects the peer UID and closes the connection. The write may
	// succeed (buffered) or fail (broken pipe) — either way, the read should
	// get nothing back.
	b, _ := Encode(&AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "should-fail",
		Questions: makeQuestions(),
	})
	_, _ = conn.Write(b)

	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected connection rejection, got data: %s", buf[:n])
	}
}

func TestE2E_AckSentOnSuccess(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-ack",
		Questions: makeQuestions(),
	})

	frame := readFrame(t, r)
	if frame["type"] != TypeAck {
		t.Errorf("expected ack, got %v", frame["type"])
	}
	if frame["id"] != "e2e-ack" {
		t.Errorf("id = %v, want e2e-ack", frame["id"])
	}
}

func TestE2E_DuplicateAskID(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	_, sockPath := startTestServer(t, present, resolve)
	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	ask := &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "dup",
		Questions: makeQuestions(),
	}
	sendFrame(t, conn, ask)
	_ = readFrame(t, r) // ack

	sendFrame(t, conn, ask)
	err := readFrame(t, r)
	if err["type"] != TypeError {
		t.Fatalf("expected error for duplicate, got %v", err["type"])
	}
}

func TestE2E_GatewayTimeout(t *testing.T) {
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := filepath.Join(t.TempDir(), "askgw-timeout.sock")
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		DefaultTimeout: 100 * time.Millisecond,
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        "e2e-timeout",
		Questions: makeQuestions(),
	})
	_ = readFrame(t, r) // ack

	ans := readFrame(t, r)
	if ans["status"] != StatusTimeout {
		t.Errorf("status = %v, want %v", ans["status"], StatusTimeout)
	}
}

func TestE2E_FrameTimeoutOverride(t *testing.T) {
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := filepath.Join(t.TempDir(), "askgw-fto.sock")
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		DefaultTimeout: 10 * time.Second,
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)

	sendFrame(t, conn, &AskFrame{
		Protocol:       ProtocolVersion,
		Type:           TypeAsk,
		ID:             "e2e-fto",
		TimeoutSeconds: 0.1,
		Questions:      makeQuestions(),
	})
	_ = readFrame(t, r) // ack

	start := time.Now()
	ans := readFrame(t, r)
	elapsed := time.Since(start)

	if ans["status"] != StatusTimeout {
		t.Errorf("status = %v, want %v", ans["status"], StatusTimeout)
	}
	if elapsed > 2*time.Second {
		t.Errorf("frame timeout should fire ~100ms, took %v", elapsed)
	}
}

// editCall/notifyCall record invocations of a test's EditMessage/
// NotifyFallback fakes, guarded by a mutex since the socket conn's frame
// loop runs on its own goroutine.
type notifyRecorder struct {
	mu         sync.Mutex
	editCalls  []editCall
	notifyText string
	notifyGot  bool
}

type editCall struct {
	agentID, sessionKey, msgID, text string
}

func (nr *notifyRecorder) edit(agentID, sessionKey, msgID, text string) bool {
	nr.mu.Lock()
	defer nr.mu.Unlock()
	nr.editCalls = append(nr.editCalls, editCall{agentID, sessionKey, msgID, text})
	return true
}

func (nr *notifyRecorder) editFails(agentID, sessionKey, msgID, text string) bool {
	nr.mu.Lock()
	defer nr.mu.Unlock()
	nr.editCalls = append(nr.editCalls, editCall{agentID, sessionKey, msgID, text})
	return false
}

func (nr *notifyRecorder) fallback(agentID, sessionKey, text string) {
	nr.mu.Lock()
	defer nr.mu.Unlock()
	nr.notifyGot = true
	nr.notifyText = text
}

func (nr *notifyRecorder) snapshot() ([]editCall, string, bool) {
	nr.mu.Lock()
	defer nr.mu.Unlock()
	return append([]editCall(nil), nr.editCalls...), nr.notifyText, nr.notifyGot
}

// answerFirstQuestion drives an ask through to a StatusAnswered outcome,
// returning once the answer frame has been read — the point at which
// Registry.recordAnswered has run and a subsequent notify frame has
// somewhere to render.
func answerFirstQuestion(t *testing.T, conn net.Conn, r *bufio.Reader, cb *capturedCallback, askID string) {
	t.Helper()
	sendFrame(t, conn, &AskFrame{
		Protocol:  ProtocolVersion,
		Type:      TypeAsk,
		ID:        askID,
		Questions: makeQuestions(),
	})
	_ = readFrame(t, r) // ack
	time.Sleep(50 * time.Millisecond)
	cb.fire(question.OptionData(0))
	ans := readFrame(t, r)
	if ans["status"] != StatusAnswered {
		t.Fatalf("status = %v, want %v", ans["status"], StatusAnswered)
	}
}

func TestE2E_NotifyEditsAnsweredMessage(t *testing.T) {
	cb := &capturedCallback{}
	nr := &notifyRecorder{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "platform-msg-1", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := shortSockPath(t)
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
		EditMessage:    nr.edit,
		NotifyFallback: nr.fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)
	answerFirstQuestion(t, conn, r, cb, "notify-edit-1")

	exitCode := 0
	sendFrame(t, conn, &NotifyFrame{
		Protocol: ProtocolVersion,
		Type:     TypeNotify,
		ID:       "notify-edit-1",
		ExitCode: &exitCode,
	})
	time.Sleep(50 * time.Millisecond)

	calls, _, fellBack := nr.snapshot()
	if fellBack {
		t.Fatal("expected the edit path to be used, not the standalone fallback")
	}
	if len(calls) != 1 {
		t.Fatalf("edit calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.agentID != "agent1" || got.sessionKey != "agent1/chat1" || got.msgID != "platform-msg-1" {
		t.Errorf("edit call = %+v, want agent1/agent1-chat1/platform-msg-1", got)
	}
	if !strings.Contains(got.text, "✅") || !strings.Contains(got.text, "exit 0") {
		t.Errorf("edit text = %q, want a checkmark and \"exit 0\"", got.text)
	}
}

// shortSockPath returns a short unix-socket path. t.TempDir() embeds the (long)
// test name, which under a long TMPDIR — e.g. the land gate's /tmp/fgw/test-<pid>/
// — overflows sun_path's 108-byte limit and fails Start() with "bind: invalid
// argument". os.MkdirTemp with a short prefix keeps the path well under the cap.
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestE2E_NotifyFallsBackWhenNoPlatformMsgID(t *testing.T) {
	cb := &capturedCallback{}
	nr := &notifyRecorder{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true // e.g. the plain-text fallback send — no platform msgID to edit
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := shortSockPath(t)
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
		EditMessage:    nr.edit,
		NotifyFallback: nr.fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)
	answerFirstQuestion(t, conn, r, cb, "notify-fallback-1")

	exitCode := 1
	sendFrame(t, conn, &NotifyFrame{
		Protocol: ProtocolVersion,
		Type:     TypeNotify,
		ID:       "notify-fallback-1",
		ExitCode: &exitCode,
		Message:  "custom failure detail",
	})
	time.Sleep(50 * time.Millisecond)

	calls, text, fellBack := nr.snapshot()
	if len(calls) != 0 {
		t.Fatalf("edit calls = %d, want 0 (no platform msgID to edit)", len(calls))
	}
	if !fellBack {
		t.Fatal("expected the standalone-message fallback to fire")
	}
	if !strings.Contains(text, "❌") || !strings.Contains(text, "exit 1") || !strings.Contains(text, "custom failure detail") {
		t.Errorf("fallback text = %q, want a cross, \"exit 1\", and the message", text)
	}
}

func TestE2E_NotifyForUnknownIDIsDropped(t *testing.T) {
	nr := &notifyRecorder{}
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(frameAgent string) (string, string) { return "agent1", "agent1/chat1" }

	sockPath := shortSockPath(t)
	uid := strconv.Itoa(os.Getuid())
	srv, err := NewServer(ServerDeps{
		SocketPath:     sockPath,
		AllowedUIDs:    []string{uid},
		Present:        present,
		CancelPrompt:   func(string, string) {},
		ResolveSession: resolve,
		EditMessage:    nr.edit,
		NotifyFallback: nr.fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	conn := dialServer(t, sockPath)
	sendFrame(t, conn, &NotifyFrame{
		Protocol: ProtocolVersion,
		Type:     TypeNotify,
		ID:       "never-asked",
	})
	time.Sleep(50 * time.Millisecond)

	_, _, fellBack := nr.snapshot()
	if fellBack {
		t.Fatal("notify for an unknown id should be dropped, not delivered")
	}
}
