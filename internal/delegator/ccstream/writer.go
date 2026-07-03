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
	closed atomic.Bool
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
	if wr.closed.Load() {
		return errWriterClosed
	}
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if wr.closed.Load() {
		return errWriterClosed
	}
	return wr.enc.Encode(msg)
}

// trySend is like Send but never blocks waiting for the write mutex: if a write
// is already in flight (e.g. wedged on a full pipe), it returns false instead of
// queueing behind it. Used by the shutdown path so Close is always reached.
func (wr *Writer) trySend(msg interface{}) bool {
	if wr.closed.Load() || !wr.mu.TryLock() {
		return false
	}
	defer wr.mu.Unlock()
	if wr.closed.Load() {
		return false
	}
	return wr.enc.Encode(msg) == nil
}

// SendUser sends a user-typed message to Claude Code. CC enqueues with the
// default priority "next" and folds the message into the current ask() at
// the next mid-turn drain (claude-code's query.ts:1570-1589) — there is
// no separate ask/result cycle for in-flight injections. SendUserPriority
// sets an explicit queue priority; foci currently only sends "next".
func (wr *Writer) SendUser(content string) error {
	return wr.Send(NewUserMessage(content))
}

// SendUserPriority is like SendUser but explicitly sets the queue priority.
// Valid values: "now" | "next" | "later". Used by SourceSteer dispatch.
// "now" additionally makes CC abort the in-flight ask (print.ts / REPL.tsx
// abort('interrupt')); steers don't use it today — see
// Backend.sendUserMessagePriority for the NYI gating it belongs behind.
// where the message must dequeue ahead of any other queued items at the
// next mid-turn drain.
func (wr *Writer) SendUserPriority(content, priority string) error {
	return wr.Send(NewUserMessagePriority(content, priority))
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
	msg, err := interruptControl()
	if err != nil {
		return err
	}
	return wr.Send(msg)
}

// TrySendInterrupt sends an interrupt without blocking. If a write is already
// in flight (the pipe is wedged), it returns false rather than waiting — the
// caller then proceeds to Close, which evicts the wedged write. (P2-4.)
func (wr *Writer) TrySendInterrupt() bool {
	msg, err := interruptControl()
	if err != nil {
		return false
	}
	return wr.trySend(msg)
}

// interruptControl builds an interrupt control-request envelope with a unique
// request ID.
func interruptControl() (ControlRequest, error) {
	raw, err := json.Marshal(InterruptRequest{Subtype: "interrupt"})
	if err != nil {
		return ControlRequest{}, fmt.Errorf("ccstream: marshal interrupt: %w", err)
	}
	seq := interruptSeq.Add(1)
	return ControlRequest{
		Type:      "control_request",
		RequestID: fmt.Sprintf("interrupt-%d", seq),
		Request:   json.RawMessage(raw),
	}, nil
}

// Close marks the writer as closed and closes the underlying pipe, sending EOF
// to Claude Code. It deliberately does NOT take wr.mu: a Send wedged writing to
// a full/hung pipe holds the mutex, and waiting for it would stall shutdown
// forever (P2-4). Closing the underlying fd evicts any such blocked write
// (it returns EBADF/EPIPE), unblocking the stuck Send. os.File methods are
// safe for concurrent Close vs. Write, so this is race-free. Idempotent.
func (wr *Writer) Close() error {
	if wr.closed.Swap(true) {
		return nil
	}
	return wr.w.Close()
}
