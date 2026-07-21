package ratelimit

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryHint(t *testing.T) {
	cases := map[string]time.Duration{
		"Please try again in 7m12s.":            7*time.Minute + 12*time.Second,
		"try again in 20s":                      20 * time.Second,
		"Rate limit reached. Try again in 1.5s": 1500 * time.Millisecond,
		"no hint here":                          0,
		"try again in soon":                     0,
	}
	for body, want := range cases {
		if got := parseRetryHint(body); got != want {
			t.Errorf("parseRetryHint(%q) = %v, want %v", body, got, want)
		}
	}
}

// A non-429 response passes through untouched, body intact.
func TestTransport_PassesThroughNon429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("body = %q, want hello", body)
	}
}

// ModeDegrade returns a typed *Error carrying the body's hint; the body itself is
// not part of the error message.
func TestTransport_DegradeReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached. Please try again in 7m12s.","url":"https://x/billing"}}`)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Kind: KindRequest, Mode: ModeDegrade}}
	_, err := client.Get(srv.URL)
	var rl *Error
	if !errors.As(err, &rl) {
		t.Fatalf("expected *ratelimit.Error, got %v", err)
	}
	if rl.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", rl.StatusCode)
	}
	if rl.RetryAfter < 7*time.Minute {
		t.Errorf("RetryAfter = %v, want >= 7m", rl.RetryAfter)
	}
	if strings.Contains(rl.Error(), "billing") || strings.Contains(rl.Error(), "{") {
		t.Errorf("Error() must not leak the body: %q", rl.Error())
	}
}

// Retry-After header (integer seconds) is honoured.
func TestTransport_HonoursRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Mode: ModeDegrade}}
	_, err := client.Get(srv.URL)
	var rl *Error
	if !errors.As(err, &rl) {
		t.Fatalf("expected *ratelimit.Error, got %v", err)
	}
	if rl.RetryAfter < 40*time.Second || rl.RetryAfter > 45*time.Second {
		t.Errorf("RetryAfter = %v, want ~45s", rl.RetryAfter)
	}
}

// A short rate limit within the inline budget is retried and succeeds.
func TestTransport_BoundedRetrySucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "try again in 50ms")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{
		Kind: KindRequest, Mode: ModePassthrough, MaxInlineWait: time.Second, MaxRetries: 2,
	}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Errorf("after retry: status=%d body=%q, want 200/ok", resp.StatusCode, body)
	}
	if hits.Load() != 2 {
		t.Errorf("server hits = %d, want 2", hits.Load())
	}
}

// In ModePassthrough, a rate limit beyond the inline budget returns the 429
// response unchanged, with its body fully readable.
func TestTransport_PassthroughReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "try again in 10s: slow down")
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{
		Kind: KindRequest, Mode: ModePassthrough, MaxInlineWait: time.Second, MaxRetries: 3,
	}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "slow down") {
		t.Errorf("passthrough body = %q, want the full 429 body", body)
	}
}
