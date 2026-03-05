package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"foci/internal/log"
)

// STT transcribes audio to text.
type STT interface {
	Transcribe(ctx context.Context, audioData []byte, filename string) (string, error)
}

// TTS synthesizes text to audio bytes (MP3 or OGG).
type TTS interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

// WithRate returns a copy of the TTS provider with the given speech rate.
// If the provider doesn't support rate overrides, it returns itself unchanged.
func WithRate(t TTS, rate float64) TTS {
	if rate == 0 {
		return t
	}
	switch p := t.(type) {
	case *EdgeTTS:
		cp := *p
		cp.Rate = rate
		return &cp
	case *OpenAITTS:
		cp := *p
		cp.Speed = rate
		return &cp
	default:
		return t
	}
}

// NewTTS creates a TTS provider from config values.
// Rate is NOT baked in — apply at resolution time via WithRate.
func NewTTS(format, endpoint, apiKey, model, voiceName, command, responseFormat string) (TTS, error) {
	switch format {
	case "edge-tts":
		return &EdgeTTS{Command: command, Voice: voiceName}, nil
	case "openai":
		return &OpenAITTS{
			Endpoint:       endpoint,
			APIKey:         apiKey,
			Model:          model,
			Voice:          voiceName,
			ResponseFormat: responseFormat,
		}, nil
	default:
		return nil, fmt.Errorf("unknown TTS format %q (must be \"openai\" or \"edge-tts\")", format)
	}
}

// NewSTT creates an STT provider from config values.
func NewSTT(format, endpoint, apiKey, model string) (STT, error) {
	switch format {
	case "openai":
		return &OpenAISTT{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    model,
		}, nil
	default:
		return nil, fmt.Errorf("unknown STT format %q (must be \"openai\")", format)
	}
}

// --- STT implementations ---

// OpenAISTT sends audio to an OpenAI-compatible audio transcription API.
// Works with Groq, OpenAI, or any compatible endpoint.
type OpenAISTT struct {
	Endpoint string // e.g. "https://api.groq.com/openai/v1/audio/transcriptions"
	APIKey   string // Bearer token
	Model    string // e.g. "whisper-large-v3"
}

func (w *OpenAISTT) Transcribe(ctx context.Context, audioData []byte, filename string) (string, error) {
	log.Debugf("voice", "stt audio=%d bytes model=%s", len(audioData), w.Model)
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
		if err := mw.WriteField("model", w.Model); err != nil {
			return "", fmt.Errorf("write model field: %w", err)
		}
	}
	if err := mw.WriteField("response_format", "text"); err != nil {
		return "", fmt.Errorf("write response_format field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

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
	defer func() { _ = resp.Body.Close() }()

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
	Command string  // binary name, default "edge-tts"
	Voice   string  // e.g. "en-US-AndrewNeural"
	Rate    float64 // speed multiplier: 1.3 = +30%, 0.8 = -20% (0 means omit --rate)
}

func (e *EdgeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	log.Debugf("voice", "tts edge-tts text=%d chars voice=%s", len(text), e.Voice)
	cmd := e.Command
	if cmd == "" {
		cmd = "edge-tts"
	}

	tmpFile, err := os.CreateTemp("", "foci-tts-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close temp file: %w", err)
	}
	mp3Path := tmpFile.Name()
	defer func() { _ = os.Remove(mp3Path) }()

	args := []string{"--text", text, "--write-media", mp3Path}
	if e.Voice != "" {
		args = append([]string{"--voice", e.Voice}, args...)
	}
	if e.Rate != 0 {
		args = append(args, "--rate", rateToEdgeTTS(e.Rate))
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

// rateToEdgeTTS converts a speed multiplier (e.g. 1.3) to edge-tts --rate format (e.g. "+30%").
func rateToEdgeTTS(rate float64) string {
	pct := math.Round((rate - 1.0) * 100)
	if pct >= 0 {
		return fmt.Sprintf("+%d%%", int(pct))
	}
	return fmt.Sprintf("%d%%", int(pct))
}

// OpenAITTS uses an OpenAI-compatible TTS API (OpenRouter, Groq, OpenAI).
type OpenAITTS struct {
	Endpoint       string  // e.g. "https://openrouter.ai/api/v1/audio/speech"
	APIKey         string  // Bearer token
	Model          string  // e.g. "openai/tts-1-mini"
	Voice          string  // e.g. "alloy"
	Speed          float64 // 0.25–4.0 (default 1.0, 0 means omit)
	ResponseFormat string  // e.g. "mp3", "wav" (default: "wav")
}

func (o *OpenAITTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	log.Debugf("voice", "tts openai text=%d chars model=%s", len(text), o.Model)
	voice := o.Voice
	if voice == "" {
		voice = "alloy"
	}
	responseFormat := o.ResponseFormat
	if responseFormat == "" {
		responseFormat = "wav"
	}

	var payload string
	if o.Speed > 0 {
		payload = fmt.Sprintf(`{"model":%q,"input":%q,"voice":%q,"speed":%.2f,"response_format":%q}`, o.Model, text, voice, o.Speed, responseFormat)
	} else {
		payload = fmt.Sprintf(`{"model":%q,"input":%q,"voice":%q,"response_format":%q}`, o.Model, text, voice, responseFormat)
	}

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
	defer func() { _ = resp.Body.Close() }()

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tts response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts API error %d: %s", resp.StatusCode, string(audioData))
	}

	return audioData, nil
}
