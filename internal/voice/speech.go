package voice

// SpeechSource names one server voice the app can ask for by name (#2858): the
// "read this message to me" path, as opposed to voice-mode autoplay. It carries
// no endpoint, model or key, so a list of these is safe to send to any client.
type SpeechSource struct {
	ID   string // the [[tts]] entry id, or the built-in "edge-tts"
	Kind string // SpeechKindAPI or SpeechKindEdge
}

// The kinds of speech source. The app's default ladder puts API voices before
// the device's own voice and edge-tts after it (Dick, 2026-11-05).
const (
	SpeechKindAPI  = "api"
	SpeechKindEdge = "edge"
)
