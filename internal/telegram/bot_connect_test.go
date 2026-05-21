package telegram

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// fastBackoff makes the retry loop near-instant for unit tests. We still
// exercise the exponential-growth path via nextDelay tests; for the loop
// tests we keep MaxAttempts bounded so failing cases terminate.
var fastBackoff = connectBackoff{
	MaxAttempts:  4,
	InitialDelay: 1 * time.Millisecond,
	MaxDelay:     1 * time.Millisecond,
	Multiplier:   2.0,
}

// stubBotFactory returns a factory that fails the first `failures` calls with
// `err`, then succeeds.
func stubBotFactory(failures int, err error, success *gotgbot.Bot, attempts *int32) func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
	return func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		n := atomic.AddInt32(attempts, 1)
		if int(n) <= failures {
			return nil, err
		}
		return success, nil
	}
}

func withStubFactory(t *testing.T, f func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error)) {
	t.Helper()
	orig := botFactory
	botFactory = f
	t.Cleanup(func() { botFactory = orig })
}

func TestConnectBot_SuccessFirstTry(t *testing.T) {
	var attempts int32
	want := &gotgbot.Bot{Token: "tok"}
	withStubFactory(t, stubBotFactory(0, errors.New("unused"), want, &attempts))

	got, err := connectBot("tok", nil, nil, fastBackoff)
	if err != nil {
		t.Fatalf("connectBot: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got bot %v, want %v", got, want)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

func TestConnectBot_RetriesTransientThenSucceeds(t *testing.T) {
	var attempts int32
	want := &gotgbot.Bot{Token: "tok"}
	transientErr := &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}
	withStubFactory(t, stubBotFactory(2, transientErr, want, &attempts))

	got, err := connectBot("tok", nil, nil, fastBackoff)
	if err != nil {
		t.Fatalf("connectBot: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got bot %v, want %v", got, want)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestConnectBot_FailsFastOnPermanent(t *testing.T) {
	var attempts int32
	permErr := errors.New("Unauthorized: 401")
	withStubFactory(t, stubBotFactory(99, permErr, nil, &attempts))

	_, err := connectBot("tok", nil, nil, fastBackoff)
	if err == nil {
		t.Fatal("connectBot: expected error, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on permanent err)", attempts)
	}
}

// TestConnectBot_BoundedGivesUp exercises the bounded path. The production
// default uses MaxAttempts == 0 (unbounded); we use a bounded variant here
// so the test terminates.
func TestConnectBot_BoundedGivesUp(t *testing.T) {
	var attempts int32
	transientErr := &net.OpError{Op: "dial", Err: errors.New("server misbehaving")}
	withStubFactory(t, stubBotFactory(99, transientErr, nil, &attempts))

	_, err := connectBot("tok", nil, nil, fastBackoff)
	if err == nil {
		t.Fatal("connectBot: expected error, got nil")
	}
	if int(attempts) != fastBackoff.MaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, fastBackoff.MaxAttempts)
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("error %q should mention attempts exhausted", err.Error())
	}
}

// TestConnectBot_UnboundedRetriesUntilSuccess verifies the production
// "MaxAttempts == 0" behaviour: retries indefinitely on transient errors
// until something succeeds. We let it eat 8 failures then succeed; the loop
// must keep going past any prior cap.
func TestConnectBot_UnboundedRetriesUntilSuccess(t *testing.T) {
	var attempts int32
	want := &gotgbot.Bot{Token: "tok"}
	transientErr := &net.OpError{Op: "dial", Err: errors.New("server misbehaving")}
	withStubFactory(t, stubBotFactory(8, transientErr, want, &attempts))

	unbounded := connectBackoff{
		MaxAttempts:  0, // unbounded
		InitialDelay: 1 * time.Millisecond,
		MaxDelay:     1 * time.Millisecond,
		Multiplier:   2.0,
	}
	got, err := connectBot("tok", nil, nil, unbounded)
	if err != nil {
		t.Fatalf("connectBot: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got bot %v, want %v", got, want)
	}
	if attempts != 9 {
		t.Errorf("attempts = %d, want 9", attempts)
	}
}

func TestConnectBot_RedactsTokenInError(t *testing.T) {
	var attempts int32
	token := "123456:SECRET-DO-NOT-LEAK"
	leaky := errors.New(`Post "https://api.telegram.org/bot` + token + `/getMe": context deadline exceeded`)
	withStubFactory(t, stubBotFactory(99, leaky, nil, &attempts))

	_, err := connectBot(token, nil, nil, fastBackoff)
	if err == nil {
		t.Fatal("connectBot: expected error, got nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error contains raw token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("error should contain [REDACTED] marker, got %q", err.Error())
	}
}

// TestConnectBot_PermanentErrorRedactsToken ensures the fail-fast path also
// strips the token from the returned error.
func TestConnectBot_PermanentErrorRedactsToken(t *testing.T) {
	var attempts int32
	token := "999999:OTHER-SECRET"
	leaky := errors.New(`Post "https://api.telegram.org/bot` + token + `/getMe": Unauthorized`)
	withStubFactory(t, stubBotFactory(99, leaky, nil, &attempts))

	_, err := connectBot(token, nil, nil, fastBackoff)
	if err == nil {
		t.Fatal("connectBot: expected error, got nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("permanent-error path leaks token: %q", err.Error())
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (permanent fails fast)", attempts)
	}
}

func TestIsPermanentTelegramErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unauthorized 401", errors.New("Unauthorized"), true},
		{"lowercase unauthorized", errors.New("unauthorized"), true},
		{"raw 401", errors.New("HTTP 401 Bad Request"), true},
		{"forbidden", errors.New("Forbidden: bot was deleted"), true},
		{"invalid token", errors.New("invalid token format"), true},
		{"dns failure (net.OpError)", &net.OpError{Op: "dial", Err: errors.New("no such host")}, false},
		{"context deadline", errors.New("context deadline exceeded"), false},
		{"server misbehaving", errors.New("server misbehaving"), false},
		{"connection refused (net.OpError)", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		{"url.Error transient", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("i/o timeout")}, false},
		{"url.Error wrapping auth", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("Unauthorized")}, true},
		{"random transport error", errors.New("EOF"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentTelegramErr(tc.err); got != tc.want {
				t.Errorf("isPermanentTelegramErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRedactToken(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		token string
		want  string
	}{
		{"replaces token", "url with bot12345:SECRET/getMe", "12345:SECRET", "url with bot[REDACTED]/getMe"},
		{"empty token returns input unchanged", "no token here", "", "no token here"},
		{"no match returns input", "no token here", "12345:SECRET", "no token here"},
		{"multiple occurrences", "12345:SECRET twice 12345:SECRET", "12345:SECRET", "[REDACTED] twice [REDACTED]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactToken(tc.s, tc.token); got != tc.want {
				t.Errorf("redactToken(%q, %q) = %q, want %q", tc.s, tc.token, got, tc.want)
			}
		})
	}
}

// TestNextDelay_ExponentialAndCapped verifies the production backoff
// schedule: doubles each attempt, caps at MaxDelay, attempt 1 has zero
// delay. This is what protects foci from both a too-eager DNS retry and an
// hour-long gap between attempts in the long tail.
func TestNextDelay_ExponentialAndCapped(t *testing.T) {
	bo := connectBackoff{
		InitialDelay: 2 * time.Second,
		MaxDelay:     5 * time.Minute,
		Multiplier:   2.0,
	}
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
		got := bo.nextDelay(tc.attempt)
		if got != tc.want {
			t.Errorf("nextDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// TestNextDelay_ZeroMaxDelay disables capping — the delay grows without
// bound. This isn't used in production but the function should behave
// sensibly.
func TestNextDelay_ZeroMaxDelay(t *testing.T) {
	bo := connectBackoff{
		InitialDelay: 1 * time.Second,
		MaxDelay:     0, // no cap
		Multiplier:   2.0,
	}
	// attempt=2 starts at InitialDelay (1s); doubles 3 times to reach attempt 5: 8s.
	if got := bo.nextDelay(5); got != 8*time.Second {
		t.Errorf("nextDelay(5) = %v, want 8s", got)
	}
}
