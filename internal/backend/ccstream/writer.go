package ccstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// errWriterClosed is returned by all Send methods after Close has been called.
var errWriterClosed = errors.New("ccstream: writer is closed")

// interruptSeq is an atomic counter used to generate unique request IDs for
// interrupt control requests. Unique per process lifetime, which is sufficient
// since request IDs only need to be unique within a session.
var interruptSeq atomic.Int64

// Writer serialises NDJSON messages to Claude Code's stdin pipe.
// All writes are serialised by a mutex — no interleaving of JSON lines.
// Thread-safe for concurrent callers.
type Writer struct {
	mu     sync.Mutex
	w      io.WriteCloser
	enc    *json.Encoder
	closed bool
}

// NewWriter creates a Writer wrapping the given stdin pipe.
// The json.Encoder writes directly to w with no HTML escaping and no
// indentation — one compact JSON object per line.
func NewWriter(w io.WriteCloser) *Writer {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "")
	return &Writer{
		w:   w,
		enc: enc,
	}
}

// Send marshals msg as a single JSON line to the pipe.
// Returns an error if the writer is closed or the write fails.
func (wr *Writer) Send(msg interface{}) error {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	if wr.closed {
		return errWriterClosed
	}
	return wr.enc.Encode(msg)
}

// SendUser sends a user-typed message to Claude Code.
func (wr *Writer) SendUser(content string) error {
	return wr.Send(NewUserMessage(content))
}

// SendControl sends a control request to Claude Code with the given request ID
// and request payload. The payload is first marshalled to json.RawMessage so
// the outer envelope carries it as an embedded JSON object.
func (wr *Writer) SendControl(reqID string, request interface{}) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("ccstream: marshal control request body: %w", err)
	}
	return wr.Send(ControlRequest{
		Type:      "control_request",
		RequestID: reqID,
		Request:   json.RawMessage(raw),
	})
}

// SendControlResponse sends a control response back to Claude Code, answering
// a previously received control request (e.g. permission prompts).
func (wr *Writer) SendControlResponse(reqID string, response interface{}) error {
	return wr.Send(NewControlResponse(reqID, response))
}

// SendKeepAlive sends a keep-alive heartbeat to Claude Code.
func (wr *Writer) SendKeepAlive() error {
	return wr.Send(KeepAlive{
		Type: "keep_alive",
	})
}

// SendInterrupt sends an interrupt control request to Claude Code with an
// auto-generated unique request ID.
func (wr *Writer) SendInterrupt() error {
	seq := interruptSeq.Add(1)
	reqID := fmt.Sprintf("interrupt-%d", seq)
	return wr.SendControl(reqID, InterruptRequest{
		Subtype: "interrupt",
	})
}

// Close marks the writer as closed and closes the underlying pipe, sending
// EOF to Claude Code. Idempotent — subsequent calls return nil.
func (wr *Writer) Close() error {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	if wr.closed {
		return nil
	}
	wr.closed = true
	return wr.w.Close()
}
