package voice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Proves the per-connection turn semaphore bounds concurrency: with capacity 1,
// a second acquire fails until the first slot is released — this is what stops a
// frame flood from spawning unbounded goroutines.
func TestConnAcquireTurn_BoundsConcurrency(t *testing.T) {
	c := &conn{turnSem: make(chan struct{}, 1)}

	if !c.acquireTurn() {
		t.Fatal("first acquire should succeed")
	}
	if c.acquireTurn() {
		t.Fatal("second acquire should fail while the slot is held")
	}
	c.releaseTurn()
	if !c.acquireTurn() {
		t.Fatal("acquire should succeed again after release")
	}
}

// Proves a nil semaphore (cap disabled) never blocks — back-compat for callers
// that don't set MaxConcurrentTurns.
func TestConnAcquireTurn_NilSemUnbounded(t *testing.T) {
	c := &conn{}
	for i := 0; i < 100; i++ {
		if !c.acquireTurn() {
			t.Fatalf("nil-sem acquire %d should always succeed", i)
		}
		c.releaseTurn()
	}
}

// Proves STT caps the response body via io.LimitReader: an oversized upstream
// reply is truncated to MaxResponse rather than read whole into memory.
func TestOpenAISTT_LimitsResponseBody(t *testing.T) {
	big := strings.Repeat("a", 10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	stt := &OpenAISTT{
		Endpoint: srv.URL,
		HTTP:     HTTPOpts{Timeout: 5 * time.Second, MaxResponse: 100},
	}
	out, err := stt.Transcribe(context.Background(), []byte("audio"), "a.wav")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if len(out) > 100 {
		t.Errorf("response not capped: got %d bytes, want <= 100", len(out))
	}
}

// Proves HTTPOpts falls back to package defaults when zero-valued, so direct
// struct construction stays safe (non-zero timeout, non-zero read cap).
func TestHTTPOpts_Defaults(t *testing.T) {
	var o HTTPOpts
	if got := o.client().Timeout; got != defaultHTTPTimeout {
		t.Errorf("default timeout = %v, want %v", got, defaultHTTPTimeout)
	}
	if got := o.maxResponse(); got != defaultMaxResponseBytes {
		t.Errorf("default maxResponse = %d, want %d", got, defaultMaxResponseBytes)
	}
}
