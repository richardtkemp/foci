package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWhisperSTT_Transcribe(t *testing.T) {
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

	stt := &WhisperSTT{
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

func TestWhisperSTT_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid api key"))
	}))
	defer server.Close()

	stt := &WhisperSTT{
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

// Verify interface compliance at compile time.
var _ STT = (*WhisperSTT)(nil)
var _ TTS = (*EdgeTTS)(nil)
var _ TTS = (*OpenAITTS)(nil)
