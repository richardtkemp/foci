package netretry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
