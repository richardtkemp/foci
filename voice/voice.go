package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// STT transcribes audio to text.
type STT interface {
	Transcribe(ctx context.Context, audioData []byte, filename string) (string, error)
}

// TTS synthesizes text to audio bytes (MP3 or OGG).
type TTS interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

// --- STT implementations ---

// WhisperSTT sends audio to an OpenAI-compatible Whisper API.
// Works with Groq, OpenAI, or any compatible endpoint.
type WhisperSTT struct {
	Endpoint string // e.g. "https://api.groq.com/openai/v1/audio/transcriptions"
	APIKey   string // Bearer token
	Model    string // e.g. "whisper-large-v3"
}

func (w *WhisperSTT) Transcribe(ctx context.Context, audioData []byte, filename string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioData); err != nil {
		return "", fmt.Errorf("write audio data: %w", err)
	}

	if w.Model != "" {
		mw.WriteField("model", w.Model)
	}
	mw.WriteField("response_format", "text")
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", w.Endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if w.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper API error %d: %s", resp.StatusCode, string(body))
	}

	return strings.TrimSpace(string(body)), nil
}

// --- TTS implementations ---

// EdgeTTS uses the edge-tts CLI (free, no API key).
type EdgeTTS struct {
	Command string // binary name, default "edge-tts"
	Voice   string // e.g. "en-US-AndrewNeural"
}

func (e *EdgeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	cmd := e.Command
	if cmd == "" {
		cmd = "edge-tts"
	}

	tmpFile, err := os.CreateTemp("", "clod-tts-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpFile.Close()
	mp3Path := tmpFile.Name()
	defer os.Remove(mp3Path)

	args := []string{"--text", text, "--write-media", mp3Path}
	if e.Voice != "" {
		args = append([]string{"--voice", e.Voice}, args...)
	}

	ttsCmd := exec.CommandContext(ctx, cmd, args...)
	if output, err := ttsCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("edge-tts: %w: %s", err, string(output))
	}

	data, err := os.ReadFile(mp3Path)
	if err != nil {
		return nil, fmt.Errorf("read tts output: %w", err)
	}

	return data, nil
}

// OpenAITTS uses an OpenAI-compatible TTS API (OpenRouter, Groq, OpenAI).
type OpenAITTS struct {
	Endpoint string // e.g. "https://openrouter.ai/api/v1/audio/speech"
	APIKey   string // Bearer token
	Model    string // e.g. "openai/tts-1-mini"
	Voice    string // e.g. "alloy"
}

func (o *OpenAITTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	voice := o.Voice
	if voice == "" {
		voice = "alloy"
	}

	payload := fmt.Sprintf(`{"model":%q,"input":%q,"voice":%q}`, o.Model, text, voice)

	req, err := http.NewRequestWithContext(ctx, "POST", o.Endpoint, strings.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create tts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts request: %w", err)
	}
	defer resp.Body.Close()

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tts response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts API error %d: %s", resp.StatusCode, string(audioData))
	}

	return audioData, nil
}
