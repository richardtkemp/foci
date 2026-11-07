package voice

import (
	"errors"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/ratelimit"
)

func TestWithMaxChars(t *testing.T) {
	cases := []struct {
		name      string
		max       int
		text      string
		wantCalls int
	}{
		{"under", 5, "abcd", 1},
		{"exactly at the limit is allowed", 5, "abcde", 1},
		{"over skips with no provider call", 5, "abcdef", 0},
		{"counts characters, not bytes", 5, "ééééé", 1},
		{"no limit", 0, strings.Repeat("a", 10000), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inner := &countingTTS{out: "audio"}
			_, err := WithMaxChars(inner, c.max).Synthesize(t.Context(), c.text)
			if inner.calls != c.wantCalls {
				t.Errorf("provider calls = %d, want %d", inner.calls, c.wantCalls)
			}
			var tooLong *TextTooLongError
			if got := errors.As(err, &tooLong); got != (c.wantCalls == 0) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestWithMaxChars_ZeroIsTheBareProvider(t *testing.T) {
	inner := &countingTTS{}
	if WithMaxChars(inner, 0) != TTS(inner) {
		t.Error("no limit must not wrap")
	}
}

// NewTTS bakes the limit in, and a later WithRate must neither lose it nor lose
// the rate on the provider inside.
func TestNewTTS_MaxCharsSurvivesWithRate(t *testing.T) {
	tts, err := NewTTS(config.TTSConfig{Format: "edge-tts", MaxChars: 3}, "", HTTPOpts{})
	if err != nil {
		t.Fatal(err)
	}
	rated := WithRate(tts, 1.5)
	m, ok := rated.(*maxCharsTTS)
	if !ok {
		t.Fatalf("WithRate returned %T: the limit is gone", rated)
	}
	if edge := m.inner.(*EdgeTTS); edge.Rate != 1.5 {
		t.Errorf("rate = %v: WithRate did not reach the provider", edge.Rate)
	}
	if _, err := rated.Synthesize(t.Context(), "abcd"); err == nil || !strings.Contains(err.Error(), "maximum of 3") {
		t.Errorf("err = %v, want the limit", err)
	}
}

func TestFallbackTTS_SkipsVoiceOverItsMax(t *testing.T) {
	limited, next := &countingTTS{out: "groq"}, &countingTTS{out: "edge"}
	f := chain(t, WithMaxChars(limited, 3), next)
	data, err := f.Synthesize(t.Context(), "abcd")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "edge" || limited.calls != 0 || next.calls != 1 {
		t.Errorf("data=%q limited.calls=%d next.calls=%d", data, limited.calls, next.calls)
	}
}

func TestFallbackTTS_AllSkippedIsTextTooLong(t *testing.T) {
	f := chain(t, WithMaxChars(&countingTTS{}, 3), WithMaxChars(&countingTTS{}, 2))
	_, err := f.Synthesize(t.Context(), "abcd")
	var tooLong *TextTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v, want TextTooLongError", err)
	}
}

// A real failure later in the chain must not be reported as "text too long", and
// a rate limit must stay reachable.
func TestFallbackTTS_SkipThenFailureIsTheFailure(t *testing.T) {
	broken := errors.New("boom")
	_, err := chain(t, WithMaxChars(&countingTTS{}, 3), &countingTTS{err: broken}).Synthesize(t.Context(), "abcd")
	var tooLong *TextTooLongError
	if errors.As(err, &tooLong) {
		t.Errorf("err = %v reads as a skip, but a voice failed", err)
	}
	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want the failure reachable", err)
	}
	if !strings.Contains(err.Error(), "maximum of 3") {
		t.Errorf("err = %q, want the skip named too", err)
	}

	limited := &ratelimit.Error{StatusCode: 429}
	_, err = chain(t, WithMaxChars(&countingTTS{}, 3), &countingTTS{err: limited}).Synthesize(t.Context(), "abcd")
	var rl *ratelimit.Error
	if !errors.As(err, &rl) {
		t.Errorf("err = %v, want the rate limit reachable", err)
	}
}
