package ccstream

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWriterCloseUnblocksWedgedSend proves Close returns promptly even when a
// Send is wedged writing to a full pipe — Close closes the underlying fd
// without waiting on the write mutex, which evicts the blocked write. Before
// the fix Close took wr.mu and blocked forever behind the stuck Send, stalling
// the whole shutdown ladder. (P2-4.)
func TestWriterCloseUnblocksWedgedSend(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	wr := NewWriter(w)

	// Never drain r, so the pipe buffer (~64 KiB) fills and Send blocks holding
	// wr.mu. A >1 MiB payload guarantees the write blocks.
	sendErr := make(chan error, 1)
	go func() { sendErr <- wr.SendUser(strings.Repeat("x", 1<<20)) }()
	time.Sleep(50 * time.Millisecond) // let the Send block on the full pipe

	closed := make(chan error, 1)
	go func() { closed <- wr.Close() }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind a wedged Send")
	}

	// Closing the fd must unblock the stuck Send (write returns an error).
	select {
	case <-sendErr:
	case <-time.After(2 * time.Second):
		t.Fatal("wedged Send did not unblock after Close")
	}
}

// nopWriteCloser wraps an io.Writer to satisfy io.WriteCloser.
type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func TestWriterSendUser(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	if err := w.SendUser("hello"); err != nil {
		t.Fatalf("SendUser: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}

	if got["type"] != "user" {
		t.Errorf("type = %v, want %q", got["type"], "user")
	}

	message, ok := got["message"].(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %T", got["message"])
	}
	if message["role"] != "user" {
		t.Errorf("message.role = %v, want %q", message["role"], "user")
	}
	if message["content"] != "hello" {
		t.Errorf("message.content = %v, want %q", message["content"], "hello")
	}
}

func TestWriterSendUser_NoPriorityField(t *testing.T) {
	// Verifies SendUser writes a plain user message with no priority field.
	// Post-Phase 5 the priority field is gone — interrupt semantics are
	// expressed via SendInterrupt, not via priority="now". Guards against
	// regression that would re-introduce the field on the wire.
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})
	if err := w.SendUser("test"); err != nil {
		t.Fatalf("SendUser: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}

	if val, present := got["priority"]; present {
		t.Errorf("priority key present (%v), want absent post-Phase-5", val)
	}
	if got["type"] != "user" {
		t.Errorf("type = %v, want %q", got["type"], "user")
	}
}

func TestWriterSendKeepAlive(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	if err := w.SendKeepAlive(); err != nil {
		t.Fatalf("SendKeepAlive: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	if line != `{"type":"keep_alive"}` {
		t.Errorf("output = %q, want %q", line, `{"type":"keep_alive"}`)
	}
}

func TestWriterSendInterrupt(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	if err := w.SendInterrupt(); err != nil {
		t.Fatalf("SendInterrupt: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}

	if got["type"] != "control_request" {
		t.Errorf("type = %v, want %q", got["type"], "control_request")
	}

	request, ok := got["request"].(map[string]any)
	if !ok {
		t.Fatalf("request is not an object: %T", got["request"])
	}
	if request["subtype"] != "interrupt" {
		t.Errorf("request.subtype = %v, want %q", request["subtype"], "interrupt")
	}
}

func TestWriterSendControl(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	initReq := InitializeRequest{
		Subtype:      "initialize",
		SystemPrompt: "You are a helpful assistant.",
	}
	if err := w.SendControl("req-42", initReq); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}

	if got["type"] != "control_request" {
		t.Errorf("type = %v, want %q", got["type"], "control_request")
	}
	if got["request_id"] != "req-42" {
		t.Errorf("request_id = %v, want %q", got["request_id"], "req-42")
	}

	request, ok := got["request"].(map[string]any)
	if !ok {
		t.Fatalf("request is not an object: %T", got["request"])
	}
	if request["subtype"] != "initialize" {
		t.Errorf("request.subtype = %v, want %q", request["subtype"], "initialize")
	}
	if request["systemPrompt"] != "You are a helpful assistant." {
		t.Errorf("request.systemPrompt = %v, want %q", request["systemPrompt"], "You are a helpful assistant.")
	}
}

func TestWriterConcurrentSends(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	const goroutines = 10
	const msgsPerGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < msgsPerGoroutine; j++ {
				if err := w.SendKeepAlive(); err != nil {
					t.Errorf("SendKeepAlive: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != goroutines*msgsPerGoroutine {
		t.Fatalf("got %d lines, want %d", len(lines), goroutines*msgsPerGoroutine)
	}

	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line %d: invalid JSON: %v\nraw: %s", i, err, line)
		}
	}
}

func TestWriterCloseIdempotent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	// First close should succeed.
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Second close should not panic and return nil (idempotent).
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Send after Close should return an error.
	if err := w.SendKeepAlive(); err == nil {
		t.Error("SendKeepAlive after Close returned nil, want error")
	}
}

func TestWriterSendControlResponse(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	allow := &PermissionAllow{
		Behavior:               "allow",
		UpdatedInput:           json.RawMessage(`{}`),
		ToolUseID:              "toolu_01ABC",
		DecisionClassification: "user_temporary",
	}
	if err := w.SendControlResponse("req-77", allow); err != nil {
		t.Fatalf("SendControlResponse: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}

	if got["type"] != "control_response" {
		t.Errorf("type = %v, want %q", got["type"], "control_response")
	}

	response, ok := got["response"].(map[string]any)
	if !ok {
		t.Fatalf("response is not an object: %T", got["response"])
	}
	if response["subtype"] != "success" {
		t.Errorf("response.subtype = %v, want %q", response["subtype"], "success")
	}
	if response["request_id"] != "req-77" {
		t.Errorf("response.request_id = %v, want %q", response["request_id"], "req-77")
	}

	inner, ok := response["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.response is not an object: %T", response["response"])
	}
	if inner["behavior"] != "allow" {
		t.Errorf("behavior = %v, want %q", inner["behavior"], "allow")
	}
	if inner["toolUseID"] != "toolu_01ABC" {
		t.Errorf("toolUseID = %v, want %q", inner["toolUseID"], "toolu_01ABC")
	}
	if inner["decisionClassification"] != "user_temporary" {
		t.Errorf("decisionClassification = %v, want %q", inner["decisionClassification"], "user_temporary")
	}
}
