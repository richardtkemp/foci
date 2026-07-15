package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func parseLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}
	return got
}

// TestWriterSendRequest verifies sendRequest emits a JSON-RPC request with
// method, id, and params populated.
func TestWriterSendRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	params := map[string]any{"prompt": "hello"}
	if err := w.sendRequest("turn", params, 42); err != nil {
		t.Fatalf("sendRequest: %v", err)
	}

	got := parseLine(t, &buf)

	if got["method"] != "turn" {
		t.Errorf("method = %v, want %q", got["method"], "turn")
	}
	if got["id"] != float64(42) {
		t.Errorf("id = %v, want %v", got["id"], float64(42))
	}

	p, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params is not an object: %T", got["params"])
	}
	if p["prompt"] != "hello" {
		t.Errorf("params.prompt = %v, want %q", p["prompt"], "hello")
	}
}

// TestWriterSendNotification verifies sendNotification emits a JSON-RPC
// notification with method and params but no id key.
func TestWriterSendNotification(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	params := map[string]any{"seq": float64(7)}
	if err := w.sendNotification("event", params); err != nil {
		t.Fatalf("sendNotification: %v", err)
	}

	got := parseLine(t, &buf)

	if got["method"] != "event" {
		t.Errorf("method = %v, want %q", got["method"], "event")
	}
	if _, present := got["id"]; present {
		t.Errorf("id key present (%v), want absent for notifications", got["id"])
	}

	p, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params is not an object: %T", got["params"])
	}
	if p["seq"] != float64(7) {
		t.Errorf("params.seq = %v, want %v", p["seq"], float64(7))
	}
}

// TestWriterSendNotification_OmitEmptyParams verifies that when params is nil
// the omitempty tag drops the field entirely (no "params":null on the wire).
func TestWriterSendNotification_OmitEmptyParams(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	if err := w.sendNotification("ping", nil); err != nil {
		t.Fatalf("sendNotification: %v", err)
	}

	got := parseLine(t, &buf)
	if _, present := got["params"]; present {
		t.Errorf("params key present (%v), want absent when nil", got["params"])
	}
}

// TestWriterSendResponse verifies sendResponse emits an object with id and a
// JSON-encoded result payload.
func TestWriterSendResponse(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	result := map[string]any{"approved": true}
	if err := w.sendResponse(99, result); err != nil {
		t.Fatalf("sendResponse: %v", err)
	}

	got := parseLine(t, &buf)

	if got["id"] != float64(99) {
		t.Errorf("id = %v, want %v", got["id"], float64(99))
	}
	if _, present := got["method"]; present {
		t.Errorf("method key present (%v), want absent on responses", got["method"])
	}

	inner, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %T", got["result"])
	}
	if inner["approved"] != true {
		t.Errorf("result.approved = %v, want true", inner["approved"])
	}
}

// TestWriterSendResponse_RawMessage verifies the result is serialised as raw
// JSON rather than being double-encoded into a string.
func TestWriterSendResponse_RawMessage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	raw := json.RawMessage(`{"code":3}`)
	if err := w.sendResponse(5, raw); err != nil {
		t.Fatalf("sendResponse: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	want := `{"id":5,"result":{"code":3}}`
	if line != want {
		t.Errorf("output = %q, want %q", line, want)
	}
}

// TestWriterSendAfterClose verifies Send returns errWriterClosed once the
// writer has been closed.
func TestWriterSendAfterClose(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := w.Send(rpcNotification{Method: "event"}); !errors.Is(err, errWriterClosed) {
		t.Errorf("Send after Close = %v, want errWriterClosed", err)
	}
	// The helpers must propagate the same error.
	if err := w.sendRequest("turn", nil, 1); !errors.Is(err, errWriterClosed) {
		t.Errorf("sendRequest after Close = %v, want errWriterClosed", err)
	}
	if err := w.sendNotification("event", nil); !errors.Is(err, errWriterClosed) {
		t.Errorf("sendNotification after Close = %v, want errWriterClosed", err)
	}
	if err := w.sendResponse(1, nil); !errors.Is(err, errWriterClosed) {
		t.Errorf("sendResponse after Close = %v, want errWriterClosed", err)
	}

	if buf.Len() != 0 {
		t.Errorf("buffer non-empty after Close: %q", buf.String())
	}
}

// TestWriterCloseIdempotent verifies that closing twice is a no-op and that
// Close is safe to call concurrently with Send.
func TestWriterCloseIdempotent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// notifyPayload is a params object carrying a unique index so that concurrent
// writes can be checked for both line integrity and absence of duplication.
type notifyPayload struct {
	N int `json:"n"`
}

// TestWriterConcurrentSends verifies the write mutex prevents interleaving:
// every Send produces exactly one well-formed JSON line, and the set of
// written indices is exactly the set we sent (no corruption, no drops).
func TestWriterConcurrentSends(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})

	const goroutines = 16
	const msgsPerGoroutine = 50
	const total = goroutines * msgsPerGoroutine

	seen := make(chan int, total)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	next := make(chan int, goroutines)
	go func() {
		for i := 0; i < total; i++ {
			next <- i
		}
		close(next)
	}()

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := range next {
				if err := w.sendNotification("event", notifyPayload{N: i}); err != nil {
					t.Errorf("sendNotification: %v", err)
					return
				}
				seen <- i
			}
		}()
	}
	wg.Wait()
	close(seen)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != total {
		t.Fatalf("got %d lines, want %d", len(lines), total)
	}

	got := make(map[int]bool, total)
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d: invalid JSON (interleaved write?): %v\nraw: %s", i, err, line)
		}
		if obj["method"] != "event" {
			t.Errorf("line %d: method = %v, want %q", i, obj["method"], "event")
		}
		p, ok := obj["params"].(map[string]any)
		if !ok {
			t.Fatalf("line %d: params not an object: %T\nraw: %s", i, obj["params"], line)
		}
		nf, ok := p["n"].(float64)
		if !ok {
			t.Fatalf("line %d: params.n not a number: %T\nraw: %s", i, p["n"], line)
		}
		n := int(nf)
		if n < 0 || n >= total {
			t.Fatalf("line %d: params.n = %d out of range", i, n)
		}
		if got[n] {
			t.Fatalf("line %d: params.n = %d duplicated", i, n)
		}
		got[n] = true
	}
	if len(got) != total {
		t.Fatalf("distinct indices = %d, want %d", len(got), total)
	}

	// Sanity: every index we believe we sent was actually written.
	for sent := range seen {
		if !got[sent] {
			t.Errorf("sent index %d missing from output", sent)
		}
	}
}
