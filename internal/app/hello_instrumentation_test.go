package app

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	flog "foci/internal/log"
)

// captureWarns collects only THIS feature's warnings for the duration of a test.
//
// The filter is load-bearing, not tidiness: the warn hook is process-global, and
// clearing it makes subsequent warnings accumulate in the log package's replay
// buffer, which is then flushed into the NEXT hook that gets installed. So an
// unfiltered capture in one test receives a pile of unrelated warnings emitted
// by other tests in the package — green when run alone, failing under the full
// suite (observed 2026-08-15).
func captureWarns(t *testing.T) func() []string {
	t.Helper()
	var (
		mu   sync.Mutex
		msgs []string
	)
	flog.SetWarnHook(func(_ flog.Level, component, msg string) {
		if !strings.Contains(msg, "#1713") {
			return
		}
		mu.Lock()
		msgs = append(msgs, component+": "+msg)
		mu.Unlock()
	})
	t.Cleanup(func() { flog.SetWarnHook(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), msgs...)
	}
}

func helloLessClient(h *Hub) *wsClient { return &wsClient{hub: h} }

// The point of the change (#1713): a RUN of sockets closing without completing
// the handshake must become visible above DEBUG. A 3-hour outage previously
// produced zero such lines.
func TestNoteSocketClosed_WarnsOnConsecutiveHelloLess(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	for i := 0; i < helloLessWarnAt-1; i++ {
		h.noteSocketClosed(helloLessClient(h), 200*time.Millisecond)
	}
	if got := len(warns()); got != 0 {
		t.Fatalf("warned after %d hello-less sockets (%v), want none before %d",
			helloLessWarnAt-1, warns(), helloLessWarnAt)
	}

	h.noteSocketClosed(helloLessClient(h), 200*time.Millisecond)
	got := warns()
	if len(got) != 1 {
		t.Fatalf("warns after %d hello-less sockets = %d (%v), want 1", helloLessWarnAt, len(got), got)
	}
	if !strings.Contains(got[0], "without completing the handshake") {
		t.Errorf("warning does not describe the failure: %q", got[0])
	}
	if !strings.Contains(got[0], "#1713") {
		t.Errorf("warning does not reference the ticket: %q", got[0])
	}
}

// A socket that DID complete the handshake must not count, and must reset the
// run — otherwise a healthy app that occasionally loses a socket would slowly
// accumulate toward a false warning.
func TestNoteSocketClosed_HelloSeenNeitherCountsNorWarns(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	for i := 0; i < helloLessWarnAt*3; i++ {
		c := helloLessClient(h)
		c.helloSeen = true
		h.noteSocketClosed(c, time.Second)
	}
	if got := warns(); len(got) != 0 {
		t.Fatalf("warned on healthy sockets: %v", got)
	}
	if h.helloLessRun != 0 {
		t.Errorf("helloLessRun = %d after only healthy sockets, want 0", h.helloLessRun)
	}
}

// noteHelloSeen is what dispatchInbound calls on a real hello; it must clear a
// partial run so intermittent failures never sum to a warning.
func TestNoteHelloSeen_ResetsPartialRun(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	for i := 0; i < helloLessWarnAt-1; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	h.noteHelloSeen() // a successful handshake lands here
	for i := 0; i < helloLessWarnAt-1; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	if got := warns(); len(got) != 0 {
		t.Fatalf("run was not reset by a successful hello: %v", got)
	}
}

// A sustained outage must keep warning rather than warning once and going
// quiet — otherwise a 3-hour outage is one line, easily missed.
func TestNoteSocketClosed_KeepsWarningWhileOutageContinues(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	for i := 0; i < helloLessWarnAt*3; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	if got := len(warns()); got != 3 {
		t.Errorf("warns over %d consecutive failures = %d, want 3", helloLessWarnAt*3, got)
	}
}

// The hello line must record the RESUME COUNT. That single number is what
// distinguishes "socket died before hello" (no line at all) from "hello arrived
// asking to resume nothing" (line with resume=0) — the ambiguity that blocked
// the #1713 diagnosis entirely.
//
// This drives the REAL dispatchInbound hello path with a wire frame. Asserting
// against a format string retyped in the test would exercise no production code
// and pass even if the log line were deleted.
func TestHelloLog_RecordsResumeCount_ViaDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, wire, wantResume string
	}{
		{
			name:       "resume points present",
			wire:       `{"t":"hello","id":"h1","d":{"client":{"deviceId":"dev-abc"},"pushToken":"SECRET-TOKEN","resume":[{"conversationId":"c1","ack":7},{"conversationId":"c2","ack":3}]}}`,
			wantResume: "resume=2",
		},
		{
			// The case the log exists to make visible: the client DID complete
			// the handshake but asked to resume nothing, which produces no
			// replayTo and was previously indistinguishable from a dead socket.
			name:       "hello asking to resume nothing",
			wire:       `{"t":"hello","id":"h2","d":{"client":{"deviceId":"dev-abc"}}}`,
			wantResume: "resume=0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			flog.SetOutput(&buf)
			flog.SetLevel(flog.DEBUG)
			// Restore os.Stderr, the package default (log.go:112) — NOT nil.
			// A nil writer SIGSEGVs the next log write and took down unrelated
			// tests in this package (observed 2026-08-15).
			t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

			h := newTestHub()
			c := &wsClient{hub: h, send: make(chan []byte, 64), done: make(chan struct{})}
			h.dispatchInbound(c, []byte(tc.wire))

			out := buf.String()
			if !strings.Contains(out, tc.wantResume) {
				t.Errorf("hello line lacks %q — the #1713 discriminator:\n%s", tc.wantResume, out)
			}
			if !strings.Contains(out, "device=dev-abc") {
				t.Errorf("hello line lacks the device id:\n%s", out)
			}
			// The push token is a credential: presence may be logged, never the value.
			if strings.Contains(out, "SECRET-TOKEN") {
				t.Fatalf("push token VALUE leaked into the log:\n%s", out)
			}
			// The socket must also be marked, or noteSocketClosed would count
			// this completed handshake as a failure.
			c.mu.Lock()
			seen := c.helloSeen
			c.mu.Unlock()
			if !seen {
				t.Error("helloSeen not set by a real hello — healthy sockets would count toward the warning")
			}
		})
	}
}
