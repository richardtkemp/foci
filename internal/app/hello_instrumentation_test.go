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

func helloLessClient(h *Hub) *wsClient { return helloLessClientFor(h, "dev-1") }

func helloLessClientFor(h *Hub, device string) *wsClient {
	return &wsClient{hub: h, deviceID: device}
}

// withWarnClock installs a hand-wound limiter so the escalating repeat schedule
// can be asserted by advancing time rather than by sleeping through a 30-minute
// cap. Returns the clock.
func withWarnClock(t *testing.T, h *Hub) *warnClock {
	t.Helper()
	clk := &warnClock{t: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	h.helloMu.Lock()
	h.helloWarns = flog.NewWarnLimiterWithClock(helloWarnBase, helloWarnMax, clk.Now)
	h.helloMu.Unlock()
	return clk
}

type warnClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *warnClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *warnClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// backdateHello makes device look like it last shook hands d ago, which is what
// takes a run of hello-less sockets past the quiet period into outage territory.
func backdateHello(h *Hub, device string, d time.Duration) {
	h.helloMu.Lock()
	h.helloStateLocked(device).lastHello = time.Now().Add(-d)
	h.helloMu.Unlock()
}

func helloRun(h *Hub, device string) int {
	h.helloMu.Lock()
	defer h.helloMu.Unlock()
	return h.helloStateLocked(device).run
}

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
	if got := helloRun(h, "dev-1"); got != 0 {
		t.Errorf("hello-less run = %d after only healthy sockets, want 0", got)
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
	h.noteHelloSeen("dev-1") // a successful handshake lands here
	for i := 0; i < helloLessWarnAt-1; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	if got := warns(); len(got) != 0 {
		t.Fatalf("run was not reset by a successful hello: %v", got)
	}
}

// A sustained outage must keep warning rather than warning once and going
// quiet — but on a schedule set by TIME, not by how hard the client retries.
// The old contract warned every 5th failed connect, so the line count scaled
// with the retry rate: the real 2026-08-17 outage produced 31 WARN lines in
// three hours, 82% of every warning in the log.
func TestNoteSocketClosed_RepeatsOnAnEscalatingSchedule(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()
	clk := withWarnClock(t, h)

	// A burst of failures inside one window is ONE line, however many arrive.
	for i := 0; i < helloLessWarnAt*10; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	if got := warns(); len(got) != 1 {
		t.Fatalf("warns for %d failures inside one window = %d (%v), want 1",
			helloLessWarnAt*10, len(got), got)
	}

	// The window then doubles: base, 2*base, 4*base.
	for i, window := range []time.Duration{helloWarnBase, 2 * helloWarnBase, 4 * helloWarnBase} {
		clk.Advance(window - time.Second)
		h.noteSocketClosed(helloLessClient(h), time.Second)
		if got := len(warns()); got != i+1 {
			t.Fatalf("warned early at step %d (window %s): warns = %d, want %d", i, window, got, i+1)
		}
		clk.Advance(time.Second)
		h.noteSocketClosed(helloLessClient(h), time.Second)
		if got := len(warns()); got != i+2 {
			t.Fatalf("did not warn after the %s window elapsed: warns = %d, want %d", window, got, i+2)
		}
	}
}

// Throttling is only safe if a reader can tell that lines were dropped —
// otherwise a gap is ambiguous between "healthy" and "the logger ate it".
func TestNoteSocketClosed_ReportsSuppressedCount(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()
	clk := withWarnClock(t, h)

	for i := 0; i < helloLessWarnAt*3; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	clk.Advance(helloWarnBase)
	h.noteSocketClosed(helloLessClient(h), time.Second)

	got := warns()
	if len(got) != 2 {
		t.Fatalf("warns = %d (%v), want 2", len(got), got)
	}
	if !strings.Contains(got[1], "further failed connects") {
		t.Errorf("repeat line does not say how many lines were suppressed: %q", got[1])
	}
}

// The closing half of the alarm. Without it the end of an outage is invisible —
// during the 2026-08-17 investigation each recovery had to be INFERRED from a
// counter resetting in some later line.
func TestNoteHelloSeen_AnnouncesRecoveryAfterAWarnedOutage(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()
	withWarnClock(t, h)

	for i := 0; i < helloLessWarnAt; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	if len(warns()) != 1 {
		t.Fatalf("setup: want exactly 1 outage warning, got %v", warns())
	}

	h.noteHelloSeen("dev-1")
	got := warns()
	if len(got) != 2 {
		t.Fatalf("warns = %d (%v), want an outage line and a recovery line", len(got), got)
	}
	if !strings.Contains(got[1], "RECOVERED") || !strings.Contains(got[1], "dev-1") {
		t.Errorf("recovery line does not announce recovery for the device: %q", got[1])
	}
}

// ...but a handshake that ends a run which never warned must stay silent, or
// ordinary supersession churn would emit "recovered" lines for a fleet that was
// never reported broken.
func TestNoteHelloSeen_SilentWhenNoOutageWasReported(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()
	withWarnClock(t, h)

	for i := 0; i < helloLessWarnAt-1; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	h.noteHelloSeen("dev-1")
	if got := warns(); len(got) != 0 {
		t.Errorf("announced recovery from an outage that was never warned about: %v", got)
	}
}

// The property that keeps an INTERMITTENT fault loud: recovery re-arms the
// alarm, so the next episode warns immediately instead of inheriting the
// silence the previous one earned. Without this, a flapping client would be
// muted progressively as it got worse — the failure mode a throttle must not
// introduce.
func TestNoteHelloSeen_RecoveryReArmsImmediateWarning(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()
	withWarnClock(t, h) // clock never advances: any second warning must come from the re-arm

	for i := 0; i < helloLessWarnAt; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	h.noteHelloSeen("dev-1") // episode 1 ends
	before := len(warns())

	// The device is healthy for a while, then breaks again. Backdating past the
	// quiet period is what makes the new run an outage rather than churn — that
	// gate is separate from the throttle and must keep doing its own job.
	backdateHello(h, "dev-1", helloQuietPeriod+time.Minute)
	for i := 0; i < helloLessWarnAt; i++ {
		h.noteSocketClosed(helloLessClient(h), time.Second)
	}
	got := warns()
	if len(got) != before+1 {
		t.Fatalf("second episode produced %d new lines, want 1 — a new fault must warn at once", len(got)-before)
	}
	if !strings.Contains(got[len(got)-1], "consecutive app connects") {
		t.Errorf("last line is not a fresh outage warning: %q", got[len(got)-1])
	}
}

// Handshake health is PER DEVICE. The fleet-wide version lied in both
// directions on 2026-08-17: a healthy phone's handshakes reset the global run
// while the Mac was dead throughout, and the warning named no device at all, so
// it read as a fleet outage when half the fleet was fine.
func TestNoteSocketClosed_PerDeviceIndependence(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()
	withWarnClock(t, h)

	// The broken device warns.
	for i := 0; i < helloLessWarnAt; i++ {
		h.noteSocketClosed(helloLessClientFor(h, "mac"), time.Second)
	}
	got := warns()
	if len(got) != 1 || !strings.Contains(got[0], "mac") {
		t.Fatalf("want one warning naming the broken device, got %v", got)
	}

	// A DIFFERENT device's successful handshake must not clear it...
	h.noteHelloSeen("phone")
	if n := len(warns()); n != 1 {
		t.Fatalf("a healthy peer device changed the broken device's state: %v", warns())
	}
	if run := helloRun(h, "mac"); run != helloLessWarnAt {
		t.Errorf("broken device's run = %d after a peer's handshake, want %d", run, helloLessWarnAt)
	}

	// ...and a peer's failures must not count toward it.
	h.noteSocketClosed(helloLessClientFor(h, "phone"), time.Second)
	if run := helloRun(h, "mac"); run != helloLessWarnAt {
		t.Errorf("a peer device's failure counted toward the broken device: run = %d", run)
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

// The defect this fixes: warning on COUNT alone. A hello-less 1006 is usually a
// client-abandoned attempt, so a burst of them while the app is otherwise
// getting through is churn, not an outage. Modelled on the real 2026-08-16
// 13:26 burst — three aborts, then a clean hello — which the count-only version
// would have warned about.
func TestNoteSocketClosed_RecentHelloSuppressesWarn(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	h.noteHelloSeen("dev-1") // app is getting through
	for i := 0; i < helloLessWarnAt*2; i++ {
		h.noteSocketClosed(helloLessClient(h), 200*time.Millisecond)
	}
	if got := warns(); len(got) != 0 {
		t.Errorf("warned on supersession churn despite a handshake %s ago: %v", "just now", got)
	}
}

// The complement: once nothing has succeeded for the quiet period, the same run
// IS an outage and must warn. Without this the change would silence the alarm
// entirely rather than aim it.
func TestNoteSocketClosed_WarnsOnceQuietPeriodElapsed(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	h.noteHelloSeen("dev-1")
	// Backdate the last success beyond the quiet period.
	backdateHello(h, "dev-1", helloQuietPeriod+time.Minute)

	for i := 0; i < helloLessWarnAt; i++ {
		h.noteSocketClosed(helloLessClient(h), 200*time.Millisecond)
	}
	got := warns()
	if len(got) != 1 {
		t.Fatalf("warns = %d (%v), want 1 once the quiet period elapsed", len(got), got)
	}
	if !strings.Contains(got[0], "NO handshake has succeeded") {
		t.Errorf("warning does not state the discriminating condition: %q", got[0])
	}
}

// A never-connected gateway has a zero lastHelloAt; that must warn rather than
// be read as "a handshake succeeded at the zero time".
func TestNoteSocketClosed_WarnsWhenNoHelloEverSucceeded(t *testing.T) {
	warns := captureWarns(t)
	h := newTestHub()

	for i := 0; i < helloLessWarnAt; i++ {
		h.noteSocketClosed(helloLessClient(h), 200*time.Millisecond)
	}
	got := warns()
	if len(got) != 1 {
		t.Fatalf("warns = %d, want 1 when no handshake has ever succeeded", len(got))
	}
	if !strings.Contains(got[0], "since this gateway started") {
		t.Errorf("warning should say no handshake has succeeded since startup: %q", got[0])
	}
}

// The close reason is what separates a client-abandoned attempt from a socket
// something else killed, so it has to reach the DEBUG line. readPump is the
// only writer and its sole exit path sets it; this asserts the value survives
// to the diagnostic rather than being recorded and dropped.
func TestNoteSocketClosed_DebugLineCarriesCloseReason(t *testing.T) {
	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	h := newTestHub()
	c := helloLessClient(h)
	c.closeErr = "websocket: close 1006 (abnormal closure): unexpected EOF"
	h.noteSocketClosed(c, 145*time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, "1006") {
		t.Errorf("hello-less DEBUG line lacks the close reason:\n%s", out)
	}
	if !strings.Contains(out, "145ms") {
		t.Errorf("hello-less DEBUG line lacks the socket lifetime:\n%s", out)
	}
}
