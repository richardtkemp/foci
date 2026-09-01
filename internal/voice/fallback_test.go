package voice

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"foci/internal/ratelimit"
)

// countingTTS records every call and returns a fixed result.
type countingTTS struct {
	out   string
	err   error
	calls int
}

func (c *countingTTS) Synthesize(_ context.Context, _ string) ([]byte, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return []byte(c.out), nil
}

func chain(t *testing.T, providers ...TTS) *FallbackTTS {
	t.Helper()
	f := &FallbackTTS{}
	for i, p := range providers {
		f.Chain = append(f.Chain, ChainEntry{ID: string(rune('a' + i)), TTS: p})
	}
	return f
}

// TestFallbackTTS_FirstSucceedsStopsThere proves the chain is lazy: a working
// primary means the fallback provider is never invoked, so adding a fallback
// costs nothing on the happy path.
func TestFallbackTTS_FirstSucceedsStopsThere(t *testing.T) {
	primary := &countingTTS{out: "primary-audio"}
	backup := &countingTTS{out: "backup-audio"}

	got, err := chain(t, primary, backup).Synthesize(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(got) != "primary-audio" {
		t.Errorf("audio = %q, want %q", got, "primary-audio")
	}
	if backup.calls != 0 {
		t.Errorf("backup calls = %d, want 0 — the fallback ran despite the primary succeeding", backup.calls)
	}
}

// TestFallbackTTS_FailoverToNext is the whole point of the type: a rate-limited
// primary must yield audio from the next provider instead of nothing.
func TestFallbackTTS_FailoverToNext(t *testing.T) {
	primary := &countingTTS{err: &ratelimit.Error{StatusCode: http.StatusTooManyRequests, RetryAfter: 4 * time.Hour}}
	backup := &countingTTS{out: "backup-audio"}

	got, err := chain(t, primary, backup).Synthesize(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(got) != "backup-audio" {
		t.Errorf("audio = %q, want %q", got, "backup-audio")
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Errorf("calls = primary %d / backup %d, want 1/1", primary.calls, backup.calls)
	}
}

// TestFallbackTTS_AllFailKeepsRateLimitType guards the error contract the app
// sink depends on: it classifies a quota exhaustion at info (not warn) via
// errors.As, so wrapping must stay unwrappable even after a second failure.
func TestFallbackTTS_AllFailKeepsRateLimitType(t *testing.T) {
	rl := &ratelimit.Error{StatusCode: http.StatusTooManyRequests, RetryAfter: 4 * time.Hour}
	primary := &countingTTS{err: rl}
	backup := &countingTTS{err: errors.New("edge-tts: exit status 1")}

	_, err := chain(t, primary, backup).Synthesize(t.Context(), "hello")
	if err == nil {
		t.Fatal("Synthesize: want an error when every provider fails")
	}
	var got *ratelimit.Error
	if !errors.As(err, &got) {
		t.Errorf("errors.As(*ratelimit.Error) = false for %v — the sink would log this at warn", err)
	}
	// Both failures must be visible, each attributed to its provider.
	if !strings.Contains(err.Error(), "a: ") || !strings.Contains(err.Error(), "b: ") {
		t.Errorf("error = %q, want both providers named", err)
	}
	// One log line, not one per provider — errors.Join renders with newlines.
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error = %q, want a single line", err)
	}
}

// TestFallbackTTS_DeadContextStops proves a cancelled turn does not march
// through the whole chain: every remaining provider would fail identically.
func TestFallbackTTS_DeadContextStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	primary := &countingTTS{err: context.Canceled}
	backup := &countingTTS{out: "backup-audio"}

	if _, err := chain(t, primary, backup).Synthesize(ctx, "hello"); err == nil {
		t.Fatal("Synthesize: want an error on a cancelled context")
	}
	if backup.calls != 0 {
		t.Errorf("backup calls = %d, want 0 — a cancelled turn still tried the fallback", backup.calls)
	}
}

// TestFallbackTTS_EmptyChain: an empty chain must error rather than report
// success with no audio, which would deliver a silent voice note.
func TestFallbackTTS_EmptyChain(t *testing.T) {
	data, err := (&FallbackTTS{}).Synthesize(t.Context(), "hello")
	if err == nil {
		t.Fatalf("Synthesize = (%q, nil), want an error", data)
	}
}
