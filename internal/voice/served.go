package voice

import "context"

// servedKey is the context key of a served-voice recorder.
type servedKey struct{}

// WithServedRecorder returns ctx carrying fn, which a voice chain calls with
// the config id of the voice that produced the audio (#2992: the app's
// "Message info" names the TTS provider of a voice-mode clip). Only a chain
// knows ids: a bare voice (a chain of one) reports nothing, so the caller
// shows the provider as not recorded.
func WithServedRecorder(ctx context.Context, fn func(id string)) context.Context {
	return context.WithValue(ctx, servedKey{}, fn)
}

// reportServed tells ctx's recorder, if any, which voice spoke.
func reportServed(ctx context.Context, id string) {
	if fn, ok := ctx.Value(servedKey{}).(func(string)); ok && fn != nil {
		fn(id)
	}
}

// Named is one voice with its config id: a chain of one, which reports the id
// it spoke with like a FallbackTTS does (#2992). Errors pass through
// untouched, so a caller classifies a single voice's failure as before.
type Named struct {
	ID  string
	TTS TTS
}

// Synthesize speaks through the one voice and, on success, reports its id.
func (n *Named) Synthesize(ctx context.Context, text string) ([]byte, error) {
	data, err := n.TTS.Synthesize(ctx, text)
	if err == nil {
		reportServed(ctx, n.ID)
	}
	return data, err
}
