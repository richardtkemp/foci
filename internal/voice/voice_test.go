package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAISTT_Transcribe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", auth)
		}

		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("get form file: %v", err)
		}
		defer file.Close()

		if header.Filename != "voice.ogg" {
			t.Errorf("expected filename voice.ogg, got %s", header.Filename)
		}

		data, _ := io.ReadAll(file)
		if string(data) != "fake-audio-data" {
			t.Errorf("unexpected audio data: %s", string(data))
		}

		if model := r.FormValue("model"); model != "whisper-large-v3" {
			t.Errorf("expected model whisper-large-v3, got %s", model)
		}

		if format := r.FormValue("response_format"); format != "text" {
			t.Errorf("expected response_format text, got %s", format)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello, this is the transcription."))
	}))
	defer server.Close()

	stt := &OpenAISTT{
		Endpoint: server.URL,
		APIKey:   "test-key",
		Model:    "whisper-large-v3",
	}

	result, err := stt.Transcribe(context.Background(), []byte("fake-audio-data"), "voice.ogg")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if result != "Hello, this is the transcription." {
		t.Errorf("got %q, want %q", result, "Hello, this is the transcription.")
	}
}

func TestOpenAISTT_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid api key"))
	}))
	defer server.Close()

	stt := &OpenAISTT{
		Endpoint: server.URL,
		APIKey:   "bad-key",
		Model:    "whisper-1",
	}

	_, err := stt.Transcribe(context.Background(), []byte("audio"), "voice.ogg")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if got := err.Error(); got != "whisper API error 401: invalid api key" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestOpenAITTS_Synthesize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		if auth := r.Header.Get("Authorization"); auth != "Bearer tts-key" {
			t.Errorf("expected Bearer tts-key, got %s", auth)
		}

		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %s", ct)
		}

		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			t.Fatal("empty request body")
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake-mp3-audio-data"))
	}))
	defer server.Close()

	tts := &OpenAITTS{
		Endpoint: server.URL,
		APIKey:   "tts-key",
		Model:    "openai/tts-1-mini",
		Voice:    "alloy",
	}

	data, err := tts.Synthesize(context.Background(), "Hello world")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(data) != "fake-mp3-audio-data" {
		t.Errorf("got %q, want %q", string(data), "fake-mp3-audio-data")
	}
}

func TestOpenAITTS_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid voice"))
	}))
	defer server.Close()

	tts := &OpenAITTS{
		Endpoint: server.URL,
		APIKey:   "key",
		Model:    "model",
	}

	_, err := tts.Synthesize(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestOpenAITTS_DefaultVoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("audio"))
	}))
	defer server.Close()

	tts := &OpenAITTS{
		Endpoint: server.URL,
		APIKey:   "key",
		Model:    "model",
		// Voice empty — should default to "alloy"
	}

	_, err := tts.Synthesize(context.Background(), "test")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
}

func TestOpenAITTS_SpeedIncluded(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("audio"))
	}))
	defer server.Close()

	tts := &OpenAITTS{
		Endpoint: server.URL,
		APIKey:   "key",
		Model:    "model",
		Voice:    "alloy",
		Speed:    1.5,
	}

	_, err := tts.Synthesize(context.Background(), "test")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	if !strings.Contains(gotBody, `"speed":1.50`) {
		t.Errorf("payload should contain speed field: %s", gotBody)
	}
}

func TestOpenAITTS_SpeedOmittedWhenZero(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("audio"))
	}))
	defer server.Close()

	tts := &OpenAITTS{
		Endpoint: server.URL,
		APIKey:   "key",
		Model:    "model",
		Voice:    "alloy",
		// Speed is 0 — should be omitted
	}

	_, err := tts.Synthesize(context.Background(), "test")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	if strings.Contains(gotBody, "speed") {
		t.Errorf("payload should not contain speed when zero: %s", gotBody)
	}
}

func TestRateToEdgeTTS(t *testing.T) {
	tests := []struct {
		rate float64
		want string
	}{
		{1.3, "+30%"},
		{1.0, "+0%"},
		{0.8, "-20%"},
		{1.5, "+50%"},
		{0.5, "-50%"},
		{2.0, "+100%"},
	}
	for _, tt := range tests {
		got := rateToEdgeTTS(tt.rate)
		if got != tt.want {
			t.Errorf("rateToEdgeTTS(%.1f) = %q, want %q", tt.rate, got, tt.want)
		}
	}
}

func TestWithRate_EdgeTTS(t *testing.T) {
	orig := &EdgeTTS{Voice: "en-US-AndrewNeural", Rate: 1.0}
	overridden := WithRate(orig, 1.5)

	edge, ok := overridden.(*EdgeTTS)
	if !ok {
		t.Fatal("expected *EdgeTTS")
	}
	if edge.Rate != 1.5 {
		t.Errorf("rate = %v, want 1.5", edge.Rate)
	}
	// Original unchanged
	if orig.Rate != 1.0 {
		t.Errorf("original rate changed to %v", orig.Rate)
	}
}

func TestWithRate_OpenAITTS(t *testing.T) {
	orig := &OpenAITTS{Model: "tts-1", Speed: 1.0}
	overridden := WithRate(orig, 2.0)

	oai, ok := overridden.(*OpenAITTS)
	if !ok {
		t.Fatal("expected *OpenAITTS")
	}
	if oai.Speed != 2.0 {
		t.Errorf("speed = %v, want 2.0", oai.Speed)
	}
	if orig.Speed != 1.0 {
		t.Errorf("original speed changed to %v", orig.Speed)
	}
}

func TestWithRate_ZeroReturnsOriginal(t *testing.T) {
	orig := &EdgeTTS{Rate: 1.3}
	result := WithRate(orig, 0)
	if result != orig {
		t.Error("zero rate should return original provider")
	}
}

// --- Factory function tests ---

// TestNewTTS_OpenAI verifies that NewTTS("openai", ...) returns an *OpenAITTS
// with all fields wired correctly.
func TestNewTTS_OpenAI(t *testing.T) {
	tts, err := NewTTS("openai", "https://api.example.com/tts", "key123", "tts-1", "alloy", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oai, ok := tts.(*OpenAITTS)
	if !ok {
		t.Fatalf("expected *OpenAITTS, got %T", tts)
	}
	if oai.Endpoint != "https://api.example.com/tts" {
		t.Errorf("endpoint = %q", oai.Endpoint)
	}
	if oai.APIKey != "key123" {
		t.Errorf("apiKey = %q", oai.APIKey)
	}
	if oai.Model != "tts-1" {
		t.Errorf("model = %q", oai.Model)
	}
	if oai.Voice != "alloy" {
		t.Errorf("voice = %q", oai.Voice)
	}
	if oai.Speed != 0 {
		t.Errorf("speed should be 0 (rate applied later), got %v", oai.Speed)
	}
}

// TestNewTTS_EdgeTTS verifies that NewTTS("edge-tts", ...) returns an *EdgeTTS
// with voice and command fields set.
func TestNewTTS_EdgeTTS(t *testing.T) {
	tts, err := NewTTS("edge-tts", "", "", "", "en-US-AndrewNeural", "/usr/bin/edge-tts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	edge, ok := tts.(*EdgeTTS)
	if !ok {
		t.Fatalf("expected *EdgeTTS, got %T", tts)
	}
	if edge.Voice != "en-US-AndrewNeural" {
		t.Errorf("voice = %q", edge.Voice)
	}
	if edge.Command != "/usr/bin/edge-tts" {
		t.Errorf("command = %q", edge.Command)
	}
}

// TestNewTTS_UnknownFormat verifies that NewTTS rejects unknown format strings.
func TestNewTTS_UnknownFormat(t *testing.T) {
	_, err := NewTTS("whisper", "", "", "", "", "")
	if err == nil {
		t.Fatal("expected error for unknown format")
	}
	if !strings.Contains(err.Error(), "unknown TTS format") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestNewSTT_OpenAI verifies that NewSTT("openai", ...) returns an *OpenAISTT
// with all fields wired correctly.
func TestNewSTT_OpenAI(t *testing.T) {
	stt, err := NewSTT("openai", "https://api.groq.com/stt", "groq-key", "whisper-large-v3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oai, ok := stt.(*OpenAISTT)
	if !ok {
		t.Fatalf("expected *OpenAISTT, got %T", stt)
	}
	if oai.Endpoint != "https://api.groq.com/stt" {
		t.Errorf("endpoint = %q", oai.Endpoint)
	}
	if oai.APIKey != "groq-key" {
		t.Errorf("apiKey = %q", oai.APIKey)
	}
	if oai.Model != "whisper-large-v3" {
		t.Errorf("model = %q", oai.Model)
	}
}

// TestNewSTT_UnknownFormat verifies that NewSTT rejects unknown format strings.
func TestNewSTT_UnknownFormat(t *testing.T) {
	_, err := NewSTT("edge-tts", "", "", "")
	if err == nil {
		t.Fatal("expected error for unknown format")
	}
	if !strings.Contains(err.Error(), "unknown STT format") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Verify interface compliance at compile time.
var _ STT = (*OpenAISTT)(nil)
var _ TTS = (*EdgeTTS)(nil)
var _ TTS = (*OpenAITTS)(nil)
