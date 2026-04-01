package ccstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// mockHandler records dispatched messages for assertion.
type mockHandler struct {
	assistants   []*AssistantMessage
	results      []*ResultMessage
	permissions  []*PermissionRequest
	systems      []string // subtypes
	errors       []error
	controlResps []json.RawMessage
	cancelReqs   []string
	toolProgress []*ToolProgressMessage
	streamEvents []json.RawMessage
}

func (h *mockHandler) OnAssistant(msg *AssistantMessage)          { h.assistants = append(h.assistants, msg) }
func (h *mockHandler) OnResult(msg *ResultMessage)                { h.results = append(h.results, msg) }
func (h *mockHandler) OnPermissionRequest(msg *PermissionRequest) { h.permissions = append(h.permissions, msg) }
func (h *mockHandler) OnControlResponse(raw json.RawMessage)      { h.controlResps = append(h.controlResps, raw) }
func (h *mockHandler) OnControlCancelRequest(reqID string)        { h.cancelReqs = append(h.cancelReqs, reqID) }
func (h *mockHandler) OnToolProgress(msg *ToolProgressMessage)    { h.toolProgress = append(h.toolProgress, msg) }
func (h *mockHandler) OnStreamEvent(raw json.RawMessage)          { h.streamEvents = append(h.streamEvents, raw) }
func (h *mockHandler) OnSystem(subtype string, _ json.RawMessage) { h.systems = append(h.systems, subtype) }
func (h *mockHandler) OnError(err error)                          { h.errors = append(h.errors, err) }

func TestReaderDispatchAssistant(t *testing.T) {
	t.Parallel()

	line := `{"type":"assistant","message":{"id":"msg_01","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"claude-sonnet-4-20250514","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}},"session_id":"sess-1"}` + "\n"

	h := &mockHandler{}
	r := NewReader(strings.NewReader(line), h)
	r.Run(context.Background())

	if len(h.assistants) != 1 {
		t.Fatalf("got %d assistant messages, want 1", len(h.assistants))
	}
	msg := h.assistants[0]
	if msg.Message.Model != "claude-sonnet-4-20250514" {
		t.Errorf("model = %q, want %q", msg.Message.Model, "claude-sonnet-4-20250514")
	}
	if len(msg.Message.Content) != 1 || msg.Message.Content[0].Text != "Hello!" {
		t.Errorf("content = %+v, want single text block with 'Hello!'", msg.Message.Content)
	}
}

func TestReaderDispatchResult(t *testing.T) {
	t.Parallel()

	line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":500,"duration_api_ms":400,"num_turns":1,"result":"Done.","total_cost_usd":0.001,"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}` + "\n"

	h := &mockHandler{}
	r := NewReader(strings.NewReader(line), h)
	r.Run(context.Background())

	if len(h.results) != 1 {
		t.Fatalf("got %d result messages, want 1", len(h.results))
	}
	if h.results[0].Subtype != "success" {
		t.Errorf("subtype = %q, want %q", h.results[0].Subtype, "success")
	}
	if h.results[0].Result != "Done." {
		t.Errorf("result = %q, want %q", h.results[0].Result, "Done.")
	}
}

func TestReaderDispatchPermission(t *testing.T) {
	t.Parallel()

	line := `{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"t1","description":"Run a command"}}` + "\n"

	h := &mockHandler{}
	r := NewReader(strings.NewReader(line), h)
	r.Run(context.Background())

	if len(h.permissions) != 1 {
		t.Fatalf("got %d permission requests, want 1", len(h.permissions))
	}
	if h.permissions[0].Request.ToolName != "Bash" {
		t.Errorf("tool_name = %q, want %q", h.permissions[0].Request.ToolName, "Bash")
	}
}

func TestReaderDispatchSystem(t *testing.T) {
	t.Parallel()

	line := `{"type":"system","subtype":"init","claude_code_version":"1.0","cwd":"/tmp","model":"claude-sonnet-4-20250514","permissionMode":"default","tools":["Bash"]}` + "\n"

	h := &mockHandler{}
	r := NewReader(strings.NewReader(line), h)
	r.Run(context.Background())

	if len(h.systems) != 1 {
		t.Fatalf("got %d system messages, want 1", len(h.systems))
	}
	if h.systems[0] != "init" {
		t.Errorf("subtype = %q, want %q", h.systems[0], "init")
	}
}

func TestReaderUnknownType(t *testing.T) {
	t.Parallel()

	line := `{"type":"unknown_future_type","data":"something"}` + "\n"

	h := &mockHandler{}
	r := NewReader(strings.NewReader(line), h)
	r.Run(context.Background())

	// No handler should have been called.
	if len(h.assistants) != 0 {
		t.Errorf("unexpected assistant dispatch")
	}
	if len(h.results) != 0 {
		t.Errorf("unexpected result dispatch")
	}
	if len(h.permissions) != 0 {
		t.Errorf("unexpected permission dispatch")
	}
	if len(h.systems) != 0 {
		t.Errorf("unexpected system dispatch")
	}
	if len(h.errors) != 0 {
		t.Errorf("unexpected error: %v", h.errors)
	}
}

func TestReaderMalformedJSON(t *testing.T) {
	t.Parallel()

	// First line is malformed, second is valid.
	input := "this is not json\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"duration_api_ms":1,"num_turns":1,"result":"ok","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}` + "\n"

	h := &mockHandler{}
	r := NewReader(strings.NewReader(input), h)
	r.Run(context.Background())

	if len(h.errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(h.errors))
	}
	// The valid line should still dispatch.
	if len(h.results) != 1 {
		t.Fatalf("got %d results, want 1 (reader should continue after malformed JSON)", len(h.results))
	}
}

func TestReaderEOF(t *testing.T) {
	t.Parallel()

	h := &mockHandler{}
	r := NewReader(strings.NewReader(""), h)
	r.Run(context.Background())

	// Should return without error.
	if len(h.errors) != 0 {
		t.Errorf("unexpected errors: %v", h.errors)
	}
}

func TestReaderContextCancel(t *testing.T) {
	t.Parallel()

	// Use a reader that blocks (never returns data). We use a pipe so the
	// scanner blocks on Read. Cancel the context to unblock.
	ctx, cancel := context.WithCancel(context.Background())

	// Create a reader that blocks forever by using a channel-based approach.
	// We'll use a pipe: the write end is never written to, so the read blocks.
	pr, pw := newTestPipe()
	defer pw.Close()

	h := &mockHandler{}
	rd := NewReader(pr, h)

	done := make(chan struct{})
	go func() {
		rd.Run(ctx)
		close(done)
	}()

	// Cancel context.
	cancel()

	// Run should return promptly.
	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation within 2 seconds")
	}
}

// newTestPipe returns a pipe where the read side checks for context cancellation
// via Close. We use io.Pipe which blocks reads until data is available or the
// writer is closed.
func newTestPipe() (*readCloserWithCancel, *writeCloserForPipe) {
	pr, pw := newCancellablePipe()
	return pr, pw
}

// cancellablePipe is a simple pipe where closing the write end unblocks the reader.
type readCloserWithCancel struct {
	ch     chan []byte
	closed chan struct{}
	buf    []byte
}

type writeCloserForPipe struct {
	closed chan struct{}
}

func newCancellablePipe() (*readCloserWithCancel, *writeCloserForPipe) {
	closed := make(chan struct{})
	return &readCloserWithCancel{closed: closed}, &writeCloserForPipe{closed: closed}
}

func (r *readCloserWithCancel) Read(p []byte) (int, error) {
	// Block until closed.
	<-r.closed
	return 0, context.Canceled
}

func (w *writeCloserForPipe) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *writeCloserForPipe) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return nil
}
