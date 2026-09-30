package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/ratelimit"
	"foci/internal/turnevent"
)

// #1809: a voice-mode TTS failure must reach the client as a per-bubble
// voiceUnavailable reason on the text frame the missing clip belonged to, not
// only as a server log line.

func voiceSink(t *testing.T, tts *mockVoiceTTS) (*appSink, *wsClient) {
	t.Helper()
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	s.tts = tts
	s.attachVoice = func([]byte) error { return nil }
	return s, c
}

func framesOf(ds []decoded, typ string) []decoded {
	var out []decoded
	for _, d := range ds {
		if d.t == typ {
			out = append(out, d)
		}
	}
	return out
}

// A rate-limited synthesis on a non-streamed reply marks the message frame
// with the rate-limit reason, including how long the provider stays out.
func TestAppSink_VoiceUnavailable_RateLimitedMarksMessage(t *testing.T) {
	rl := &ratelimit.Error{StatusCode: 429, RetryAfter: 4*time.Hour + 45*time.Minute + 36*time.Second, Detail: "raw body, secret-ish"}
	s, c := voiceSink(t, &mockVoiceTTS{err: fmt.Errorf("groq: %w", rl)})

	s.Emit(agent.WithTrigger(context.Background(), "voice"), turnevent.TurnComplete{FinalText: "Hello there"})

	msgs := framesOf(drain(t, c), fap.TypeMessage)
	if len(msgs) != 1 {
		t.Fatalf("want one message frame, got %d", len(msgs))
	}
	if got, want := msgs[0].d["voiceUnavailable"], "rate limited for another 4h45m36s"; got != want {
		t.Errorf("voiceUnavailable = %v, want %q", got, want)
	}
}

// Any other synthesis failure is reported with a generic reason — the raw
// provider error text is never forwarded to the client.
func TestAppSink_VoiceUnavailable_ProviderErrorMarksStreamedEnd(t *testing.T) {
	s, c := voiceSink(t, &mockVoiceTTS{err: errors.New("dial tcp: https://billing.example/secret")})
	ctx := agent.WithTrigger(context.Background(), "voice")

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.TextDelta{Delta: "Hello there"})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "Hello there"})

	ends := framesOf(drain(t, c), fap.TypeTextEnd)
	if len(ends) != 1 {
		t.Fatalf("want one text.end frame, got %d", len(ends))
	}
	if got, want := ends[0].d["voiceUnavailable"], "TTS provider error"; got != want {
		t.Errorf("voiceUnavailable = %v, want %q", got, want)
	}
}

// Per-block synthesis (#1444): only the bubble whose clip failed is marked.
func TestAppSink_VoiceUnavailable_OnlyFailedBlockMarked(t *testing.T) {
	tts := &flakyTTS{failFirst: true}
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	s.tts = tts
	s.attachVoice = func([]byte) error { return nil }
	ctx := agent.WithTrigger(context.Background(), "voice")

	s.Emit(ctx, turnevent.TextBlock{Text: "first", Phase: turnevent.PhaseIntermediate})
	s.Emit(ctx, turnevent.TextBlock{Text: "second", Phase: turnevent.PhaseIntermediate})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "first\n\nsecond"})

	msgs := framesOf(drain(t, c), fap.TypeMessage)
	if len(msgs) != 2 {
		t.Fatalf("want two message frames, got %d", len(msgs))
	}
	if msgs[0].d["voiceUnavailable"] != "TTS provider error" {
		t.Errorf("first bubble voiceUnavailable = %v, want the failure reason", msgs[0].d["voiceUnavailable"])
	}
	if v, ok := msgs[1].d["voiceUnavailable"]; ok {
		t.Errorf("second bubble synthesized fine but carries voiceUnavailable=%v", v)
	}
}

// Successful synthesis and typed (non-voice) turns carry no marker.
func TestAppSink_VoiceUnavailable_AbsentWhenNotApplicable(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx context.Context
		tts *mockVoiceTTS
	}{
		"synthesized":         {agent.WithTrigger(context.Background(), "voice"), &mockVoiceTTS{data: []byte("clip")}},
		"typed turn, failing": {context.Background(), &mockVoiceTTS{err: errors.New("boom")}},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := voiceSink(t, tc.tts)
			s.Emit(tc.ctx, turnevent.TurnComplete{FinalText: "Hello there"})
			msgs := framesOf(drain(t, c), fap.TypeMessage)
			if len(msgs) != 1 {
				t.Fatalf("want one message frame, got %d", len(msgs))
			}
			if v, ok := msgs[0].d["voiceUnavailable"]; ok {
				t.Errorf("voiceUnavailable = %v, want absent", v)
			}
		})
	}
}

// flakyTTS fails its first call only.
type flakyTTS struct {
	failFirst bool
	calls     int
}

func (f *flakyTTS) Synthesize(context.Context, string) ([]byte, error) {
	f.calls++
	if f.failFirst && f.calls == 1 {
		return nil, errors.New("boom")
	}
	return []byte("clip"), nil
}
