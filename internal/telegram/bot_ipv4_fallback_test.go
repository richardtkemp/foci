package telegram

import (
	"context"
	"errors"
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
		name                string
		onIPv4              bool
		isTimeout           bool
		consecutiveTimeouts int
		consecutiveErrors   int
		wantSwitch          bool
		wantEscalate        bool
	}{
		// On IPv6: a run of consecutive timeouts accumulates quietly until the
		// switch threshold.
		{"v6 timeout run #1 — quiet", false, true, 1, 1, false, false},
		{"v6 timeout run #2 — quiet", false, true, 2, 2, false, false},
		{"v6 timeout run #3 — switch", false, true, ipv4SwitchThreshold, ipv4SwitchThreshold, true, false},
		{"v6 timeout run #4 — switch (still latching)", false, true, ipv4SwitchThreshold + 1, ipv4SwitchThreshold + 1, true, false},
		// Regression guard for the founding bug (#823): the switch must gate on
		// *consecutive timeouts*, not total failures. A lone timeout after a run
		// of non-timeout errors (429/502) must NOT switch, even though the total
		// failure count is well past the threshold — IPv4 can't fix a 429.
		{"v6 lone timeout amid errors — no switch", false, true, 1, ipv4SwitchThreshold + 1, false, false},
		{"v6 lone timeout at escalate threshold — escalate not switch", false, true, 1, errorEscalateThreshold, false, true},
		// On IPv6: a non-timeout error never triggers the switch; it escalates
		// on the normal threshold like before.
		{"v6 non-timeout #1 — quiet debug", false, false, 0, 1, false, false},
		{"v6 non-timeout #5 — escalate", false, false, 0, errorEscalateThreshold, false, true},
		{"v6 non-timeout #6 — quiet again", false, false, 0, errorEscalateThreshold + 1, false, false},
		// On IPv4: never switch again; timeouts now escalate normally — this is
		// "only error if we still get failures on 4".
		{"v4 timeout run #3 — no second switch, quiet", true, true, ipv4SwitchThreshold, ipv4SwitchThreshold, false, false},
		{"v4 timeout run #5 — escalate", true, true, errorEscalateThreshold, errorEscalateThreshold, false, true},
		{"v4 non-timeout #5 — escalate", true, false, 0, errorEscalateThreshold, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPollError(tc.onIPv4, tc.isTimeout, tc.consecutiveTimeouts, tc.consecutiveErrors)
			if got.switchToIPv4 != tc.wantSwitch || got.escalate != tc.wantEscalate {
				t.Fatalf("classifyPollError(onIPv4=%v, isTimeout=%v, timeouts=%d, errors=%d) = %+v, want switch=%v escalate=%v",
					tc.onIPv4, tc.isTimeout, tc.consecutiveTimeouts, tc.consecutiveErrors, got, tc.wantSwitch, tc.wantEscalate)
			}
		})
	}
}

// The switch must never escalate on the same poll: switching and escalating are
// mutually exclusive outcomes (the switch is the self-heal).
func TestClassifyPollError_SwitchNeverEscalates(t *testing.T) {
	for n := 1; n <= errorEscalateThreshold+3; n++ {
		got := classifyPollError(false, true, n, n)
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

// revertToDualStack must clear the latch, be idempotent, and survive a
// switch→revert→switch cycle (the flap the maxIPv4Reverts budget guards).
func TestRevertToDualStack_ClearsLatchAndCycles(t *testing.T) {
	tr := &http.Transport{}
	b := &Bot{
		forceIPv4: &atomic.Bool{},
		transport: tr,
	}
	b.switchToIPv4(3)
	if !b.ipv4Latched() {
		t.Fatal("expected latched after switchToIPv4")
	}
	b.revertToDualStack(1)
	if b.ipv4Latched() {
		t.Fatal("expected not latched after revertToDualStack")
	}
	// Idempotent: a second revert is a no-op (Swap returns the prior false).
	b.revertToDualStack(2)
	if b.ipv4Latched() {
		t.Fatal("expected still dual-stack after second revert")
	}
	// A fresh blackhole can re-latch after a revert (not a one-way door).
	b.switchToIPv4(3)
	if !b.ipv4Latched() {
		t.Fatal("expected re-latched after a second switchToIPv4")
	}
}

// A test-constructed bot with no transport wiring must not panic on revert.
func TestRevertToDualStack_NilSafe(t *testing.T) {
	b := &Bot{} // forceIPv4 and transport both nil
	b.revertToDualStack(1) // must not panic
	if b.ipv4Latched() {
		t.Fatal("revert on nil-wired bot must stay not latched")
	}
}
