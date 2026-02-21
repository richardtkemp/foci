package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// Transcriber sends audio to an OpenAI-compatible Whisper API endpoint.
type Transcriber struct {
	Endpoint string // e.g. "https://api.groq.com/openai/v1/audio/transcriptions"
	APIKey   string // Bearer token
	Model    string // e.g. "whisper-large-v3"
}

// Transcribe sends audio data to the Whisper API and returns the transcript text.
func (t *Transcriber) Transcribe(ctx context.Context, audioData []byte, filename string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioData); err != nil {
		return "", fmt.Errorf("write audio data: %w", err)
	}

	if t.Model != "" {
		w.WriteField("model", t.Model)
	}
	w.WriteField("response_format", "text")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", t.Endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if t.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.APIKey)
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

// TTS converts text to speech using an OpenAI-compatible TTS API.
type TTS struct {
	Endpoint string // e.g. "https://openrouter.ai/api/v1/audio/speech"
	APIKey   string // Bearer token
	Model    string // e.g. "openai/tts-1-mini"
	Voice    string // e.g. "alloy"
}

// Synthesize converts text to MP3 audio bytes.
func (t *TTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	body := map[string]string{
		"model": t.Model,
		"input": text,
		"voice": t.Voice,
	}
	if body["voice"] == "" {
		body["voice"] = "alloy"
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal tts request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", t.Endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create tts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.APIKey)
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
