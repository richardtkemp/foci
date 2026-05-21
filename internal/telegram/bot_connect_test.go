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

// fastBackoff makes the retry loop near-instant for unit tests.
var fastBackoff = connectBackoff{
	MaxAttempts: 4,
	Delays:      []time.Duration{0, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond},
}

// stubBotFactory returns a factory that fails the first `failures` calls with
// `err`, then succeeds. successCount holds the attempt index that succeeded.
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
	// Two transient failures, then success.
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

func TestConnectBot_GivesUpAfterMaxAttempts(t *testing.T) {
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

func TestConnectBot_RedactsTokenInError(t *testing.T) {
	var attempts int32
	token := "123456:SECRET-DO-NOT-LEAK"
	// gotgbot-style error including the URL+token.
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
