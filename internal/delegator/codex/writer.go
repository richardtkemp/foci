package codex

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// errWriterClosed is returned by all Send methods after Close.
var errWriterClosed = errors.New("codex: writer is closed")

// Writer serialises JSON-RPC messages to the app-server's stdin pipe.
// All writes are serialised by a mutex — no interleaving of JSON lines.
type Writer struct {
	mu     sync.Mutex
	w      io.WriteCloser
	enc    *json.Encoder
	closed atomic.Bool
}

// NewWriter creates a Writer wrapping the given stdin pipe.
func NewWriter(w io.WriteCloser) *Writer {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "")
	return &Writer{w: w, enc: enc}
}

// Send marshals msg as a single JSON line to the pipe.
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

// sendRequest sends a JSON-RPC request and returns its ID.
func (wr *Writer) sendRequest(method string, params interface{}, id int64) error {
	return wr.Send(rpcRequest{Method: method, ID: id, Params: params})
}

// sendNotification sends a JSON-RPC notification (no ID, no response).
func (wr *Writer) sendNotification(method string, params interface{}) error {
	return wr.Send(rpcNotification{Method: method, Params: params})
}

// sendResponse sends a JSON-RPC response to a server-initiated request
// (e.g. approval decisions).
func (wr *Writer) sendResponse(id int64, result interface{}) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return wr.Send(struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
	}{ID: id, Result: raw})
}

// Close marks the writer as closed and closes the underlying pipe.
func (wr *Writer) Close() error {
	if wr.closed.Swap(true) {
		return nil
	}
	return wr.w.Close()
}
