package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/config"
)

// fakeGoogleTTS is a stand-in for texttospeech.googleapis.com v1 text:synthesize.
// It records the last request and answers with audio, base64 encoded as Google
// does, or with the status and body it is given.
type fakeGoogleTTS struct {
	srv     *httptest.Server
	calls   atomic.Int32
	lastReq googleTTSRequest
	lastKey string
	lastURL string
	status  int
	body    string // raw body for a non-200 answer
	audio   []byte
}

func newFakeGoogleTTS(t *testing.T) *fakeGoogleTTS {
	t.Helper()
	f := &fakeGoogleTTS{status: http.StatusOK, audio: []byte("ID3fake-mp3-bytes")}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		f.lastKey = r.Header.Get("X-Goog-Api-Key")
		f.lastURL = r.URL.String()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &f.lastReq); err != nil {
			t.Errorf("request body is not the Google shape: %v: %s", err, raw)
		}
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"audioContent": base64.StdEncoding.EncodeToString(f.audio),
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestGoogleTTS_Synthesize(t *testing.T) {
	// Proves the request is Google's v1 shape (input.text, voice.languageCode
	// from the voice name, audioConfig.audioEncoding), the key goes in the
	// X-Goog-Api-Key header and never the URL, and the base64 audioContent is
	// decoded to the raw clip.
	f := newFakeGoogleTTS(t)
	g := &GoogleTTS{Endpoint: f.srv.URL + "/v1/text:synthesize", APIKey: "goog-key", Voice: "en-US-Neural2-J"}

	audio, err := g.Synthesize(context.Background(), "Hello there")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(audio) != string(f.audio) {
		t.Errorf("audio = %q, want %q", audio, f.audio)
	}
	if f.lastKey != "goog-key" {
		t.Errorf("X-Goog-Api-Key = %q, want goog-key", f.lastKey)
	}
	if strings.Contains(f.lastURL, "goog-key") || strings.Contains(f.lastURL, "key=") {
		t.Errorf("key leaked into URL %q", f.lastURL)
	}
	r := f.lastReq
	if r.Input.Text != "Hello there" {
		t.Errorf("input.text = %q", r.Input.Text)
	}
	if r.Voice.Name != "en-US-Neural2-J" || r.Voice.LanguageCode != "en-US" {
		t.Errorf("voice = %+v, want name en-US-Neural2-J, languageCode en-US", r.Voice)
	}
	if r.AudioConfig.AudioEncoding != "MP3" {
		t.Errorf("audioEncoding = %q, want MP3 (the default)", r.AudioConfig.AudioEncoding)
	}
	if r.AudioConfig.SpeakingRate != 0 {
		t.Errorf("speakingRate = %v, want omitted at no rate", r.AudioConfig.SpeakingRate)
	}
}

func TestGoogleTTS_SpeakingRateOmittedFromJSON(t *testing.T) {
	// Proves a zero rate is left out of the wire JSON, not sent as 0.0.
	b, err := json.Marshal(googleTTSAudioConfig{AudioEncoding: "MP3"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "speakingRate") {
		t.Errorf("speakingRate present at zero: %s", b)
	}
}

func TestGoogleTTS_Defaults(t *testing.T) {
	// Proves an entry with no voice gets the default Neural2 voice and its
	// language code.
	f := newFakeGoogleTTS(t)
	g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k"}
	if _, err := g.Synthesize(context.Background(), "hi"); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if f.lastReq.Voice.Name != googleTTSDefaultVoice || f.lastReq.Voice.LanguageCode != "en-GB" {
		t.Errorf("voice = %+v, want %s / en-GB", f.lastReq.Voice, googleTTSDefaultVoice)
	}
}

func TestGoogleTTS_ResponseFormats(t *testing.T) {
	// Proves each [[tts]] response_format reaches Google as its audioEncoding.
	f := newFakeGoogleTTS(t)
	for format, want := range map[string]string{"mp3": "MP3", "wav": "LINEAR16", "WAV": "LINEAR16", "ogg": "OGG_OPUS", "opus": "OGG_OPUS"} {
		g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k", ResponseFormat: format}
		if _, err := g.Synthesize(context.Background(), "hi"); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if got := f.lastReq.AudioConfig.AudioEncoding; got != want {
			t.Errorf("response_format %q → audioEncoding %q, want %q", format, got, want)
		}
	}
}

func TestGoogleTTS_UnknownResponseFormat(t *testing.T) {
	// Proves a format with no Google encoding fails before any call.
	f := newFakeGoogleTTS(t)
	g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k", ResponseFormat: "aac"}
	if _, err := g.Synthesize(context.Background(), "hi"); err == nil {
		t.Fatal("want an error for response_format aac")
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("provider called %d times", n)
	}
}

func TestGoogleTTS_RateSentAndClamped(t *testing.T) {
	// Proves the rate reaches Google as speakingRate, and a rate outside
	// Google's 0.25–2.0 (which Google refuses) is clamped instead of losing
	// the clip.
	f := newFakeGoogleTTS(t)
	for _, tc := range []struct{ in, want float64 }{{1.3, 1.3}, {2.6, 2.0}, {0.1, 0.25}} {
		g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k", Speed: tc.in}
		if _, err := g.Synthesize(context.Background(), "hi"); err != nil {
			t.Fatalf("rate %v: %v", tc.in, err)
		}
		if got := f.lastReq.AudioConfig.SpeakingRate; got != tc.want {
			t.Errorf("rate %v → speakingRate %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGoogleTTS_APIError(t *testing.T) {
	// Proves a Google error body is reported by its status and message, the
	// shape a bad key gets ("API key not valid").
	f := newFakeGoogleTTS(t)
	f.status = http.StatusBadRequest
	f.body = `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`
	g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "bad"}
	_, err := g.Synthesize(context.Background(), "hi")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"400", "INVALID_ARGUMENT", "API key not valid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestGoogleTTS_NonJSONError(t *testing.T) {
	// Proves a non-JSON error body is still reported, raw.
	f := newFakeGoogleTTS(t)
	f.status = http.StatusBadGateway
	f.body = "upstream down"
	g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k"}
	_, err := g.Synthesize(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("err = %v", err)
	}
}

func TestGoogleTTS_BadAudioContent(t *testing.T) {
	// Proves a 200 with no audio, or audio that is not base64, is an error and
	// not an empty clip.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/empty") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"audioContent":"%%% not base64"}`))
	}))
	defer srv.Close()
	for _, path := range []string{"/empty", "/garbled"} {
		g := &GoogleTTS{Endpoint: srv.URL + path, APIKey: "k"}
		if audio, err := g.Synthesize(context.Background(), "hi"); err == nil {
			t.Errorf("%s: want an error, got %d bytes", path, len(audio))
		}
	}
}

func TestGoogleTTS_OverInputLimitSkipsWithoutCall(t *testing.T) {
	// Proves a text over Google's 5,000-byte input limit is refused before the
	// call, as a *TextTooLongError, so a fallback chain treats it as a skip.
	// Bytes, not characters: 2,000 two-byte runes (4,000 bytes) pass, 2,501
	// (5,002 bytes) do not.
	f := newFakeGoogleTTS(t)
	g := &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k"}

	if _, err := g.Synthesize(context.Background(), strings.Repeat("é", 2000)); err != nil {
		t.Fatalf("4000 bytes: %v", err)
	}
	_, err := g.Synthesize(context.Background(), strings.Repeat("é", 2501))
	var tooLong *TextTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v, want *TextTooLongError", err)
	}
	if tooLong.Chars != 5002 || tooLong.Max != googleTTSMaxInputBytes || tooLong.Unit != "bytes" {
		t.Errorf("tooLong = %+v", tooLong)
	}
	if !strings.Contains(err.Error(), "5002 bytes") {
		t.Errorf("message %q does not name bytes", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("provider called %d times, want 1 (the 4000-byte text only)", n)
	}
}

func TestGoogleTTS_OverLimitFallsBack(t *testing.T) {
	// Proves the end-to-end skip: in a chain, a too-long text passes over the
	// Google voice to the next one.
	f := newFakeGoogleTTS(t)
	next := &recordingTTS{out: []byte("edge")}
	chain := &FallbackTTS{Chain: []ChainEntry{
		{ID: "google", TTS: &GoogleTTS{Endpoint: f.srv.URL, APIKey: "k"}},
		{ID: "edge-tts", TTS: next},
	}}
	audio, err := chain.Synthesize(context.Background(), strings.Repeat("a", 5001))
	if err != nil || string(audio) != "edge" {
		t.Fatalf("audio=%q err=%v, want the next voice's clip", audio, err)
	}
	if f.calls.Load() != 0 {
		t.Error("google was called for an over-limit text")
	}
}

type recordingTTS struct{ out []byte }

func (r *recordingTTS) Synthesize(context.Context, string) ([]byte, error) { return r.out, nil }

func TestGoogleLanguageCode(t *testing.T) {
	for voice, want := range map[string]string{
		"en-GB-Neural2-B":          "en-GB",
		"cmn-CN-Wavenet-A":         "cmn-CN",
		"en-US-Chirp3-HD-Achernar": "en-US",
		"el-GR-Standard-B":         "el-GR",
		"weird":                    "weird",
	} {
		if got := googleLanguageCode(voice); got != want {
			t.Errorf("googleLanguageCode(%q) = %q, want %q", voice, got, want)
		}
	}
}

func TestNewTTS_Google(t *testing.T) {
	// Proves the google format builds a GoogleTTS from the entry and key, keeps
	// max_chars, and takes a rate through WithRate (the max_chars wrapper
	// included) all the way to the request.
	f := newFakeGoogleTTS(t)
	tts, err := NewTTS(config.TTSConfig{
		ID: "neural2", Format: "google", Endpoint: f.srv.URL, Voice: "en-GB-Neural2-D",
		ResponseFormat: "wav", MaxChars: 10,
	}, "the-key", HTTPOpts{})
	if err != nil {
		t.Fatalf("NewTTS: %v", err)
	}
	tts = WithRate(tts, 1.4)

	if _, err := tts.Synthesize(context.Background(), "short"); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if f.lastKey != "the-key" || f.lastReq.Voice.Name != "en-GB-Neural2-D" ||
		f.lastReq.AudioConfig.AudioEncoding != "LINEAR16" || f.lastReq.AudioConfig.SpeakingRate != 1.4 {
		t.Errorf("request key=%q %+v", f.lastKey, f.lastReq)
	}

	var tooLong *TextTooLongError
	if _, err := tts.Synthesize(context.Background(), "longer than ten"); !errors.As(err, &tooLong) {
		t.Errorf("max_chars not applied: err = %v", err)
	}
}
