package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/ratelimit"
	"foci/internal/voice"
)

func speechHub(t *testing.T, tts voice.TTS) (*Hub, string) {
	t.Helper()
	h := newTestHub()
	h.deps.SpeechSources = func() []voice.SpeechSource {
		return []voice.SpeechSource{{ID: "groq", Kind: voice.SpeechKindAPI}, {ID: "edge-tts", Kind: voice.SpeechKindEdge}}
	}
	h.deps.SpeechTTS = func(agentID, source string) voice.TTS {
		if source == "groq" {
			return tts
		}
		return nil
	}
	return h, h.devices.pair("phone-1", "Phone").Token
}

func postSpeech(h *Hub, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/app/speech", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeSpeech(w, r)
	return w
}

func TestServeSpeech_ReturnsAudioOfCleanedText(t *testing.T) {
	tts := &mockVoiceTTS{data: []byte("RIFF\x00\x00\x00\x00WAVEfmt ")}
	h, tok := speechHub(t, tts)
	w := postSpeech(h, tok, `{"agentId":"a","source":"groq","text":"**bold** see #1443"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/wave" {
		t.Errorf("content-type = %q, want the sniffed audio/wave", ct)
	}
	if w.Body.String() != string(tts.data) {
		t.Errorf("body = %q", w.Body.String())
	}
	if len(tts.got) != 1 || tts.got[0] != "bold see 1 4 4 3" {
		t.Errorf("synthesized %q, want the text cleaned as voice turns clean it", tts.got)
	}
}

func TestServeSpeech_Refusals(t *testing.T) {
	cases := []struct {
		name, token, body string
		want              int
	}{
		{"bad token", "nope", `{"source":"groq","text":"hi"}`, http.StatusForbidden},
		{"not json", "", `nope`, http.StatusBadRequest},
		{"only markup", "", `{"source":"groq","text":"** _ #"}`, http.StatusBadRequest},
		{"source gone", "", `{"source":"gone","text":"hi"}`, http.StatusNotFound},
		{"too large", "", fmt.Sprintf(`{"source":"groq","text":"%s"}`, strings.Repeat("a", maxSpeechBody)), http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tts := &mockVoiceTTS{data: []byte("x")}
			h, tok := speechHub(t, tts)
			if c.token != "" {
				tok = c.token
			}
			if w := postSpeech(h, tok, c.body); w.Code != c.want {
				t.Fatalf("code = %d, want %d (%q)", w.Code, c.want, w.Body.String())
			}
			if len(tts.got) != 0 {
				t.Errorf("a refused request reached the voice: %q", tts.got)
			}
		})
	}
}

// The app's fallback and its message to the user rest on these codes. The body
// must not carry the provider's message: it can name an endpoint or an org.
func TestServeSpeech_FailureCodesHideProviderMessage(t *testing.T) {
	const secretish = "org_123 at https://provider.example"
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"rate limited", fmt.Errorf("%s: %w", secretish, &ratelimit.Error{}), http.StatusTooManyRequests},
		{"failed", errors.New(secretish), http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, tok := speechHub(t, &mockVoiceTTS{err: c.err})
			w := postSpeech(h, tok, `{"source":"groq","text":"hi"}`)
			if w.Code != c.want {
				t.Fatalf("code = %d, want %d", w.Code, c.want)
			}
			if strings.Contains(w.Body.String(), secretish) {
				t.Errorf("provider message reached the client: %q", w.Body.String())
			}
		})
	}
	t.Run("empty audio is a failure", func(t *testing.T) {
		h, tok := speechHub(t, &mockVoiceTTS{})
		if w := postSpeech(h, tok, `{"source":"groq","text":"hi"}`); w.Code != http.StatusBadGateway {
			t.Fatalf("code = %d, want 502", w.Code)
		}
	})
}

// Req 7/11: hello names the sources and nothing else; no deps means no field.
func TestCaps_SpeechNamesOnly(t *testing.T) {
	h, _ := speechHub(t, &mockVoiceTTS{})
	raw, err := json.Marshal(h.caps())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"speech":[{"id":"groq","kind":"api"},{"id":"edge-tts","kind":"edge"}]`) {
		t.Errorf("caps = %s", raw)
	}
	bare, err := json.Marshal(newTestHub().caps())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "speech") {
		t.Errorf("server with no voices advertises speech: %s", bare)
	}
}

// #2919: a text over the voice's maximum is refused with no provider call, so the
// app moves to its next voice. The count is on the cleaned text.
func TestServeSpeech_OverMaxCharsIs413WithNoProviderCall(t *testing.T) {
	inner := &mockVoiceTTS{data: []byte("x")}
	h, tok := speechHub(t, voice.WithMaxChars(inner, 4))
	if w := postSpeech(h, tok, `{"source":"groq","text":"abcde"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d (%q)", w.Code, w.Body.String())
	}
	if len(inner.got) != 0 {
		t.Fatalf("provider called with %q", inner.got)
	}
	// 8 characters of source, 4 after normalisation.
	if w := postSpeech(h, tok, `{"source":"groq","text":"**abcd**"}`); w.Code != http.StatusOK {
		t.Fatalf("cleaned text at the limit: code = %d (%q)", w.Code, w.Body.String())
	}
}

func TestCaps_SpeechCarriesMaxChars(t *testing.T) {
	h := newTestHub()
	h.deps.SpeechSources = func() []voice.SpeechSource {
		return []voice.SpeechSource{{ID: "groq", Kind: voice.SpeechKindAPI, MaxChars: 200}}
	}
	raw, err := json.Marshal(h.caps())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"speech":[{"id":"groq","kind":"api","maxChars":200}]`) {
		t.Errorf("caps = %s", raw)
	}
}
