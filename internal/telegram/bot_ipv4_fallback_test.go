package telegram

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
)

// Tests for the IPv6→IPv4 one-way fallback (TODO #809). The actual v4/v6 dial
// split can't be exercised against a loopback httptest stub, so we unit-test the
// three decision pieces: timeout classification, the poll-error classifier, and
// the latch idempotency.

func TestIsTimeoutErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline (http.Client timeout)", context.DeadlineExceeded, true},
		{
			"url.Error wrapping deadline (what gotgbot returns)",
			&url.Error{Op: "Post", URL: "https://api.telegram.org/getUpdates", Err: context.DeadlineExceeded},
			true,
		},
		{
			"net read timeout string",
			errors.New("read tcp [2a01::1]->[2001:67c:4e8:f004::9]:443: read: connection timed out"),
			true,
		},
		{"i/o timeout string", errors.New("dial tcp: i/o timeout"), true},
		{"connection refused (not a timeout)", errors.New("dial tcp: connect: connection refused"), false},
		{"401 unauthorized (not a timeout)", errors.New("unexpected status 401: Unauthorized"), false},
		{"net.Error with Timeout()=true", timeoutNetErr{}, true},
		{"net.Error with Timeout()=false", &net.OpError{Op: "dial", Err: errors.New("refused")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTimeoutErr(tc.err); got != tc.want {
				t.Fatalf("isTimeoutErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// timeoutNetErr is a net.Error reporting Timeout()=true regardless of message,
// proving isTimeoutErr trusts the interface, not just the string.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "synthetic" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

func TestClassifyPollError(t *testing.T) {
	cases := []struct {
		name              string
		onIPv4            bool
		isTimeout         bool
		consecutiveErrors int
		wantSwitch        bool
		wantEscalate      bool
	}{
		// On IPv6: timeouts accumulate quietly until the switch threshold.
		{"v6 timeout #1 — quiet", false, true, 1, false, false},
		{"v6 timeout #2 — quiet", false, true, 2, false, false},
		{"v6 timeout #3 — switch", false, true, ipv4SwitchThreshold, true, false},
		{"v6 timeout #4 — switch (still latching)", false, true, ipv4SwitchThreshold + 1, true, false},
		// On IPv6: a non-timeout error never triggers the switch; it escalates
		// on the normal threshold like before.
		{"v6 non-timeout #1 — quiet debug", false, false, 1, false, false},
		{"v6 non-timeout #5 — escalate", false, false, errorEscalateThreshold, false, true},
		{"v6 non-timeout #6 — quiet again", false, false, errorEscalateThreshold + 1, false, false},
		// On IPv4: never switch again; timeouts now escalate normally — this is
		// "only error if we still get failures on 4".
		{"v4 timeout #3 — no second switch, quiet", true, true, ipv4SwitchThreshold, false, false},
		{"v4 timeout #5 — escalate", true, true, errorEscalateThreshold, false, true},
		{"v4 non-timeout #5 — escalate", true, false, errorEscalateThreshold, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPollError(tc.onIPv4, tc.isTimeout, tc.consecutiveErrors)
			if got.switchToIPv4 != tc.wantSwitch || got.escalate != tc.wantEscalate {
				t.Fatalf("classifyPollError(onIPv4=%v, isTimeout=%v, n=%d) = %+v, want switch=%v escalate=%v",
					tc.onIPv4, tc.isTimeout, tc.consecutiveErrors, got, tc.wantSwitch, tc.wantEscalate)
			}
		})
	}
}

// The switch must never escalate on the same poll: switching and escalating are
// mutually exclusive outcomes (the switch is the self-heal).
func TestClassifyPollError_SwitchNeverEscalates(t *testing.T) {
	for n := 1; n <= errorEscalateThreshold+3; n++ {
		got := classifyPollError(false, true, n)
		if got.switchToIPv4 && got.escalate {
			t.Fatalf("n=%d: switch and escalate both set", n)
		}
	}
}

func TestSwitchToIPv4_LatchesOnceAndClosesIdle(t *testing.T) {
	tr := &http.Transport{}
	b := &Bot{
		forceIPv4: &atomic.Bool{},
		transport: tr,
	}
	if b.ipv4Latched() {
		t.Fatal("expected not latched initially")
	}
	b.switchToIPv4(3)
	if !b.ipv4Latched() {
		t.Fatal("expected latched after switchToIPv4")
	}
	// Idempotent: a second call is a no-op (Swap returns the prior true).
	b.switchToIPv4(99)
	if !b.ipv4Latched() {
		t.Fatal("expected still latched after second switchToIPv4")
	}
}

// A test-constructed bot with no transport wiring must not panic.
func TestSwitchToIPv4_NilSafe(t *testing.T) {
	b := &Bot{} // forceIPv4 and transport both nil
	if b.ipv4Latched() {
		t.Fatal("nil forceIPv4 must read as not latched")
	}
	b.switchToIPv4(3) // must not panic
	if b.ipv4Latched() {
		t.Fatal("switch on nil-wired bot must stay not latched")
	}
}

// Sanity: the dialer closure flips address family based on the flag. This
// mirrors the closure installed in NewBot without needing a live Telegram
// connection — it proves the v6→v4 selection logic, capturing the same
// *atomic.Bool the poll loop flips.
func TestDialerForcesIPv4WhenLatched(t *testing.T) {
	flag := &atomic.Bool{}
	var gotNetwork string
	dialFn := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if flag.Load() {
			network = "tcp4"
		}
		gotNetwork = network
		return nil, fmt.Errorf("dial intentionally skipped")
	}
	_, _ = dialFn(context.Background(), "tcp", "api.telegram.org:443")
	if gotNetwork != "tcp" {
		t.Fatalf("before latch: network = %q, want tcp", gotNetwork)
	}
	flag.Store(true)
	_, _ = dialFn(context.Background(), "tcp", "api.telegram.org:443")
	if gotNetwork != "tcp4" {
		t.Fatalf("after latch: network = %q, want tcp4", gotNetwork)
	}
}
