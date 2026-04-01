package ccstream

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

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

func TestWriterSendUserWithPriority(t *testing.T) {
	// Verifies that SendUserWithPriority sets the priority field on the wire
	// and that the default SendUser omits it.
	t.Parallel()

	tests := []struct {
		name     string
		priority string
		wantKey  bool   // whether "priority" key should be present in JSON
		wantVal  string // expected value if present
	}{
		{"now interrupts", PriorityNow, true, "now"},
		{"next queues", PriorityNext, true, "next"},
		{"later defers", PriorityLater, true, "later"},
		{"empty omits field", "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			w := NewWriter(nopWriteCloser{&buf})

			var err error
			if tt.priority != "" {
				err = w.SendUserWithPriority("test", tt.priority)
			} else {
				err = w.SendUser("test")
			}
			if err != nil {
				t.Fatalf("send: %v", err)
			}

			line := strings.TrimSpace(buf.String())
			var got map[string]any
			if err := json.Unmarshal([]byte(line), &got); err != nil {
				t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
			}

			val, present := got["priority"]
			if tt.wantKey && !present {
				t.Errorf("priority key missing, want %q", tt.wantVal)
			} else if !tt.wantKey && present {
				t.Errorf("priority key present (%v), want absent", val)
			} else if tt.wantKey && val != tt.wantVal {
				t.Errorf("priority = %v, want %q", val, tt.wantVal)
			}
		})
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

	allow := NewPermissionAllow("toolu_01ABC", "user_temporary")
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
