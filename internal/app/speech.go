package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"foci/internal/fap"
	"foci/internal/ratelimit"
	"foci/internal/voice"
)

// Read-to-me (#2858): the app asks for a clip of a message's text, outside any
// turn. No model runs and nothing is added to the conversation. The sources the
// app may name reach it on hello (Caps.Speech); a server without this endpoint
// advertises none, so an older server is simply a ladder with no server voices.

// maxSpeechBody bounds the request read. It is a memory bound, far above any
// message: a provider's own limit is what a long text meets first, and that
// failure sends the app to its next voice.
const maxSpeechBody = 4 << 20

type speechRequest struct {
	AgentID string `json:"agentId"`
	Source  string `json:"source"`
	Text    string `json:"text"`
}

// speechSources is the Caps.Speech value: names and kinds, never a key.
func (h *Hub) speechSources() []fap.SpeechSource {
	if h.deps.SpeechSources == nil {
		return nil
	}
	sources := h.deps.SpeechSources()
	if len(sources) == 0 {
		return nil
	}
	out := make([]fap.SpeechSource, 0, len(sources))
	for _, s := range sources {
		out = append(out, fap.SpeechSource{ID: s.ID, Kind: s.Kind, MaxChars: s.MaxChars})
	}
	return out
}

// ServeSpeech handles POST /app/speech with a JSON speechRequest. The response is
// the audio, with its sniffed content type. Errors, with a short body and never a
// provider's own message: 400 bad request or no speakable text, 404 the source is
// no longer available, 413 request too large or text over the voice's max_chars
// (no provider call), 429 the voice is rate limited, 502 the voice failed. Cancelling the request cancels the synthesis.
func (h *Hub) ServeSpeech(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSpeechBody+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > maxSpeechBody {
		http.Error(w, "text too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req speechRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	text := voice.NormalizeForSpeech(req.Text)
	if strings.TrimSpace(text) == "" {
		http.Error(w, "no text to speak", http.StatusBadRequest)
		return
	}
	var tts voice.TTS
	if h.deps.SpeechTTS != nil {
		tts = h.deps.SpeechTTS(req.AgentID, req.Source)
	}
	if tts == nil {
		http.Error(w, "voice not available", http.StatusNotFound)
		return
	}
	audio, err := tts.Synthesize(r.Context(), text)
	if err == nil && len(audio) == 0 {
		err = errors.New("empty audio")
	}
	if err != nil {
		var tooLong *voice.TextTooLongError
		if errors.As(err, &tooLong) {
			appLog.Infof("speech: device=%s source=%s skipped: %v", dev.DeviceID, req.Source, err)
			http.Error(w, "text too long for this voice", http.StatusRequestEntityTooLarge)
			return
		}
		var limited *ratelimit.Error
		if errors.As(err, &limited) {
			appLog.Infof("speech: device=%s source=%s rate limited: %v", dev.DeviceID, req.Source, err)
			http.Error(w, "voice rate limited", http.StatusTooManyRequests)
			return
		}
		if r.Context().Err() == nil {
			appLog.Warnf("speech: device=%s source=%s: %v", dev.DeviceID, req.Source, err)
		}
		http.Error(w, "voice failed", http.StatusBadGateway)
		return
	}
	appLog.Debugf("speech: device=%s source=%s text=%d chars audio=%d bytes", dev.DeviceID, req.Source, len(text), len(audio))
	w.Header().Set("Content-Type", http.DetectContentType(audio))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(audio)
}
