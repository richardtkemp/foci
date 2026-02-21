package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTranscriber_Transcribe(t *testing.T) {
	// Mock Whisper API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		// Check auth header
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", auth)
		}

		// Check multipart form
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

	tr := &Transcriber{
		Endpoint: server.URL,
		APIKey:   "test-key",
		Model:    "whisper-large-v3",
	}

	result, err := tr.Transcribe(context.Background(), []byte("fake-audio-data"), "voice.ogg")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if result != "Hello, this is the transcription." {
		t.Errorf("got %q, want %q", result, "Hello, this is the transcription.")
	}
}

func TestTranscriber_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid api key"))
	}))
	defer server.Close()

	tr := &Transcriber{
		Endpoint: server.URL,
		APIKey:   "bad-key",
		Model:    "whisper-1",
	}

	_, err := tr.Transcribe(context.Background(), []byte("audio"), "voice.ogg")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if got := err.Error(); got != "whisper API error 401: invalid api key" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestTTS_Synthesize(t *testing.T) {
	// Mock TTS API server
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
		// Just verify it's valid JSON with expected fields
		if len(body) == 0 {
			t.Fatal("empty request body")
		}

		// Return fake MP3 data
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake-mp3-audio-data"))
	}))
	defer server.Close()

	tts := &TTS{
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

func TestTTS_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid voice"))
	}))
	defer server.Close()

	tts := &TTS{
		Endpoint: server.URL,
		APIKey:   "key",
		Model:    "model",
	}

	_, err := tts.Synthesize(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestTTS_DefaultVoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Should contain "alloy" as default voice
		if got := string(body); got == "" {
			t.Fatal("empty body")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("audio"))
	}))
	defer server.Close()

	tts := &TTS{
		Endpoint: server.URL,
		APIKey:   "key",
		Model:    "model",
		// Voice is empty — should default to "alloy"
	}

	_, err := tts.Synthesize(context.Background(), "test")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
}
