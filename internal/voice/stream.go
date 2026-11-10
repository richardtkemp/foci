package voice

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Streaming speech-to-text (#3018). A batch STT takes a whole recording; a
// streaming one takes audio while the user speaks, reports the words as they
// are heard, and says when the speaker has finished their turn. The app's
// hands-free voice input relays its microphone through foci to one of these
// (internal/app, /app/stt/stream), so the provider key never leaves the server.

// StreamAudio is the one audio shape a stream accepts: raw little-endian
// signed 16-bit PCM, one channel, at StreamSampleRate. The app captures this
// shape on both platforms, and Deepgram Flux recommends 16 kHz raw audio
// (https://developers.deepgram.com/docs/flux/quickstart, "Audio Format
// Requirements").
const (
	StreamEncoding   = "linear16"
	StreamSampleRate = 16000
)

// StreamEventKind is what a StreamEvent reports.
type StreamEventKind int

const (
	// StreamPartial carries the words of the current turn so far. Each one
	// replaces the previous one: it is the whole turn, not a delta.
	StreamPartial StreamEventKind = iota
	// StreamEndOfTurn carries the final words of a turn: the speaker has
	// finished.
	StreamEndOfTurn
)

// StreamEvent is one report from a stream.
type StreamEvent struct {
	Kind StreamEventKind
	Text string
}

// STTStream is one open streaming transcription. Send and Close may be called
// from a different goroutine than the one reading Events.
type STTStream interface {
	// Send passes one chunk of StreamAudio to the provider.
	Send(pcm []byte) error
	// Finish asks the provider to end the current turn now (the user
	// pressed stop): it answers with a StreamEndOfTurn for the words so far.
	Finish() error
	// Events delivers the stream's reports. It is closed when the stream
	// ends, for any reason; Err then says why.
	Events() <-chan StreamEvent
	// Err is the reason the stream ended: nil after a clean Close, else the
	// provider or network failure. Valid once Events is closed.
	Err() error
	// Close ends the stream and releases the provider connection (billing
	// runs while it is open). Safe to call more than once.
	Close() error
}

// StreamOptions are the per-stream end-of-turn settings. A zero field leaves
// the provider's own default.
type StreamOptions struct {
	// EOTThreshold is the end-of-turn confidence needed to finish a turn.
	EOTThreshold float64
	// EOTTimeoutMs ends a turn this long after speech stops, whatever the
	// confidence.
	EOTTimeoutMs int
}

// StreamingSTT opens streaming transcriptions.
type StreamingSTT interface {
	OpenStream(ctx context.Context, opts StreamOptions) (STTStream, error)
}

// ErrStreamClosed is returned by Send after the stream has ended.
var ErrStreamClosed = errors.New("stt stream closed")

// WrapStreamingSTT applies word replacements to every event text of the
// streams s opens, as WrapSTT does for a batch transcript. With no
// replacements it returns s unchanged.
func WrapStreamingSTT(s StreamingSTT, replacements map[string]string) StreamingSTT {
	if len(replacements) == 0 || s == nil {
		return s
	}
	return &replacingStreamingSTT{inner: s, replacements: replacements}
}

type replacingStreamingSTT struct {
	inner        StreamingSTT
	replacements map[string]string
}

func (r *replacingStreamingSTT) OpenStream(ctx context.Context, opts StreamOptions) (STTStream, error) {
	st, err := r.inner.OpenStream(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := &replacingStream{STTStream: st, events: make(chan StreamEvent, cap(st.Events())), done: make(chan struct{})}
	go func() {
		defer close(out.events)
		for ev := range st.Events() {
			ev.Text = ApplyReplacements(ev.Text, r.replacements)
			select {
			case out.events <- ev:
			case <-out.done:
				// The reader has gone: drop the rest, so this goroutine
				// ends when the inner stream does.
			}
		}
	}()
	return out, nil
}

// replacingStream re-delivers the inner stream's events with replacements
// applied. Send and Err go to the inner stream.
type replacingStream struct {
	STTStream
	events    chan StreamEvent
	done      chan struct{}
	closeOnce sync.Once
}

func (r *replacingStream) Events() <-chan StreamEvent { return r.events }

func (r *replacingStream) Close() error {
	r.closeOnce.Do(func() { close(r.done) })
	return r.STTStream.Close()
}

// StreamSetup is everything one hands-free stream needs, resolved when the
// stream opens, so a live config edit reaches the next stream (#3018). A nil
// STT means the agent has no streaming provider now.
type StreamSetup struct {
	// STT opens the stream; word replacements are already applied.
	STT StreamingSTT
	// Options are the end-of-turn settings for the provider.
	Options StreamOptions
	// SilenceTimeout closes the stream when no new words are heard for this
	// long. Zero means no silence close.
	SilenceTimeout time.Duration
}
