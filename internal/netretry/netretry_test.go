package netretry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/log"
)

// TestNextDelay_ExponentialAndCapped verifies the production backoff
// schedule: doubles each attempt, caps at MaxDelay, attempt 1 has zero
// delay. This is what protects foci from both a too-eager DNS retry and an
// hour-long gap between attempts in the long tail.
func TestNextDelay_ExponentialAndCapped(t *testing.T) {
	bo := StartupBackoff
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 0},                  // first try, no delay
		{2, 2 * time.Second},    // initial
		{3, 4 * time.Second},    // x2
		{4, 8 * time.Second},    // x2
		{5, 16 * time.Second},   // x2
		{6, 32 * time.Second},   // x2
		{7, 64 * time.Second},   // x2
		{8, 128 * time.Second},  // x2
		{9, 256 * time.Second},  // x2
		{10, 5 * time.Minute},   // capped (would be 512s = 8m32s)
		{20, 5 * time.Minute},   // stays capped
		{1000, 5 * time.Minute}, // stays capped, no overflow
	}
	for _, tc := range cases {
		if got := bo.NextDelay(tc.attempt); got != tc.want {
			t.Errorf("NextDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
	if bo.MaxAttempts != 0 {
		t.Errorf("StartupBackoff.MaxAttempts = %d, want 0 (unbounded)", bo.MaxAttempts)
	}
}

// TestNextDelay_ZeroMaxDelay disables capping — the delay grows without
// bound. Not used in production but the function should behave sensibly.
func TestNextDelay_ZeroMaxDelay(t *testing.T) {
	bo := Backoff{InitialDelay: 1 * time.Second, Multiplier: 2.0}
	// attempt=2 starts at InitialDelay (1s); doubles 3 times to reach attempt 5: 8s.
	if got := bo.NextDelay(5); got != 8*time.Second {
		t.Errorf("NextDelay(5) = %v, want 8s", got)
	}
}

var fast = Backoff{InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2}

// failing returns an fn that fails the first n calls with err, then succeeds.
func failing(n int, err error, calls *int) func() error {
	return func() error {
		*calls++
		if *calls <= n {
			return err
		}
		return nil
	}
}

func TestDo_NilPermanentRetriesEverything(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Policy{Name: "x", Backoff: fast}, failing(3, errors.New("401"), &calls))
	if err != nil || calls != 4 {
		t.Errorf("err=%v calls=%d, want nil/4 (nil Permanent must treat everything as transient)", err, calls)
	}
}

// TestDo_CancelStopsUnboundedRetry: an unbounded retry must still end when
// the gateway shuts down, and report what it was last failing on.
func TestDo_CancelStopsUnboundedRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	fn := func() error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("server misbehaving")
	}
	// Attempt 2 waits 1ms; attempt 3 would wait an hour, so only cancellation ends it.
	slow := Backoff{InitialDelay: time.Millisecond, Multiplier: float64(time.Hour / time.Millisecond)}
	err := Do(ctx, Policy{Name: "open gateway", Backoff: slow}, fn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "server misbehaving") || !strings.HasPrefix(err.Error(), "open gateway: ") {
		t.Errorf("err %q should name the op and the last failure", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestDo_RedactsLoggedAndReturnedText(t *testing.T) {
	calls := 0
	bo := fast
	bo.MaxAttempts = 2
	err := Do(context.Background(), Policy{
		Name:    "x",
		Backoff: bo,
		Redact:  func(s string) string { return strings.ReplaceAll(s, "SECRET", "[REDACTED]") },
	}, failing(99, errors.New("GET /botSECRET/getMe: timeout"), &calls))
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "gave up after 2 attempts") {
		t.Errorf("err = %v", err)
	}
}

func TestPermanentMarkers(t *testing.T) {
	isPerm := PermanentMarkers("close 4004")
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("Unauthorized"), true},
		{errors.New("websocket: close 4004: Authentication failed."), true},
		{errors.New("lookup discord.com: server misbehaving"), false},
	} {
		if got := isPerm(tc.err); got != tc.want {
			t.Errorf("isPerm(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	// extra must not leak into AuthMarkers (append aliasing).
	_ = PermanentMarkers("a", "b", "c", "d", "e", "f", "g")
	if PermanentMarkers()(errors.New("close 4004")) {
		t.Error("extra markers leaked into a different classifier")
	}
}

// tapLines records every event the component logs at or above INFO, by level.
// The component name is unique per test, so parallel tests' lines never mix.
func tapLines(t *testing.T, component string) func(lvl log.Level) []string {
	t.Helper()
	var mu sync.Mutex
	byLevel := map[log.Level][]string{}
	remove := log.AddTap(log.INFO, func(level log.Level, comp, msg string) {
		if comp != component {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		byLevel[level] = append(byLevel[level], msg)
	})
	t.Cleanup(remove)
	return func(lvl log.Level) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), byLevel[lvl]...)
	}
}

// TestDo_TransientThenSuccessLogsNoWarn is the boot-time DNS gap (#3189):
// three attempts fail, the fourth connects. Every failed attempt is INFO and
// nothing is WARN, because retrying worked.
func TestDo_TransientThenSuccessLogsNoWarn(t *testing.T) {
	const comp = "netretry-test-transient"
	lines := tapLines(t, comp)
	calls := 0
	err := Do(context.Background(), Policy{
		Name:    "x",
		Backoff: fast, // unbounded, like StartupBackoff
		Log:     log.NewComponentLogger(comp),
	}, failing(3, errors.New("lookup api.telegram.org: i/o timeout"), &calls))
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if w := lines(log.WARN); len(w) != 0 {
		t.Errorf("WARN lines = %q, want none", w)
	}
	info := strings.Join(lines(log.INFO), "\n")
	if n := strings.Count(info, "(transient, will retry)"); n != 3 {
		t.Errorf("INFO retry lines = %d, want 3; INFO:\n%s", n, info)
	}
	if !strings.Contains(info, "connected on attempt 4") {
		t.Errorf("no recovery line; INFO:\n%s", info)
	}
}

// TestDo_BoundedAllFailWarnsOnce: when every attempt of a bounded schedule
// fails, exactly one WARN reports it, and the earlier attempts stay INFO.
func TestDo_BoundedAllFailWarnsOnce(t *testing.T) {
	const comp = "netretry-test-bounded"
	lines := tapLines(t, comp)
	calls := 0
	bo := fast
	bo.MaxAttempts = 3
	err := Do(context.Background(), Policy{
		Name:    "x",
		Backoff: bo,
		Log:     log.NewComponentLogger(comp),
	}, failing(99, errors.New("connection refused"), &calls))
	if err == nil {
		t.Fatal("Do = nil, want the give-up error")
	}
	w := lines(log.WARN)
	if len(w) != 1 || !strings.Contains(w[0], "all 3 attempts failed") {
		t.Errorf("WARN lines = %q, want exactly one 'all 3 attempts failed'", w)
	}
	if n := len(lines(log.INFO)); n != 2 {
		t.Errorf("INFO lines = %d, want 2 (attempts 1 and 2)", n)
	}
}

// TestDo_UnboundedWarnsOnceAfterWarnAfter: an unbounded schedule has no last
// attempt, so the WARN fires once the outage has lasted WarnAfter, and never
// again however long it continues.
func TestDo_UnboundedWarnsOnceAfterWarnAfter(t *testing.T) {
	const comp = "netretry-test-unbounded"
	lines := tapLines(t, comp)
	clk := clock.NewFake()
	calls := 0
	err := Do(context.Background(), Policy{
		Name:      "x",
		Backoff:   fast,
		Log:       log.NewComponentLogger(comp),
		WarnAfter: 2 * time.Minute,
		Clock:     clk,
	}, func() error {
		calls++
		clk.Advance(time.Minute) // each attempt "takes" a minute of outage
		if calls <= 6 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	w := lines(log.WARN)
	if len(w) != 1 || !strings.Contains(w[0], "still failing after 2m0s (2 attempts") {
		t.Errorf("WARN lines = %q, want exactly one 'still failing after 2m0s (2 attempts'", w)
	}
}
