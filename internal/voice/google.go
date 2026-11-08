package voice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"foci/internal/config"
	"foci/internal/log"
)

// Google Cloud Text-to-Speech, v1 text:synthesize (#2921). The request shape is
// not OpenAI's, so it is its own [[tts]] format, "google".
//
// Facts from Google's REST reference (8 Nov 2026):
//   - POST https://texttospeech.googleapis.com/v1/text:synthesize
//   - body {input:{text}, voice:{languageCode, name}, audioConfig:{audioEncoding, speakingRate}}
//   - voice.languageCode is required; audioConfig.audioEncoding is required
//   - speakingRate is 0.25–2.0; any other value is an error; 0 means 1.0
//   - response {audioContent: base64}; LINEAR16 includes the WAV header
//   - at most 5,000 bytes of input per request (quotas page)
const (
	googleTTSDefaultEndpoint = "https://texttospeech.googleapis.com/v1/text:synthesize"
	googleTTSDefaultVoice    = "en-GB-Neural2-B"
	// googleTTSMaxInputBytes is Google's per-request input limit. A longer
	// text is refused here, before the call, as a skip.
	googleTTSMaxInputBytes = 5000
	googleTTSMinRate       = 0.25
	googleTTSMaxRate       = 2.0
	// googleTTSAPIKeyHeader carries the key. A header, not the ?key= query
	// parameter, so the key never appears in a URL that an error can print.
	googleTTSAPIKeyHeader = "X-Goog-Api-Key"
)

// GoogleTTS uses Google Cloud Text-to-Speech (Neural2, WaveNet, Standard,
// Chirp 3 HD voices: the voice name picks the type).
type GoogleTTS struct {
	Endpoint       string  // default googleTTSDefaultEndpoint
	APIKey         string  // Google Cloud API key, sent as X-Goog-Api-Key
	Voice          string  // e.g. "en-GB-Neural2-B"; default googleTTSDefaultVoice
	Speed          float64 // speakingRate; 0 means omit (1.0). Clamped to 0.25–2.0
	ResponseFormat string  // [[tts]] response_format; see config.GoogleTTSEncodings
	HTTP           HTTPOpts
}

type googleTTSRequest struct {
	Input       googleTTSInput       `json:"input"`
	Voice       googleTTSVoice       `json:"voice"`
	AudioConfig googleTTSAudioConfig `json:"audioConfig"`
}

type googleTTSInput struct {
	Text string `json:"text"`
}

type googleTTSVoice struct {
	LanguageCode string `json:"languageCode"`
	Name         string `json:"name"`
}

type googleTTSAudioConfig struct {
	AudioEncoding string  `json:"audioEncoding"`
	SpeakingRate  float64 `json:"speakingRate,omitempty"`
}

type googleTTSResponse struct {
	AudioContent string `json:"audioContent"`
}

type googleTTSError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// googleLanguageCode takes the language code from a voice name: the part before
// the second dash ("en-GB-Neural2-B" → "en-GB", "cmn-CN-Wavenet-A" → "cmn-CN").
// Google requires the code, and every Google voice name starts with it.
func googleLanguageCode(voice string) string {
	parts := strings.SplitN(voice, "-", 3)
	if len(parts) < 3 {
		return voice
	}
	return parts[0] + "-" + parts[1]
}

// googleSpeakingRate turns a speed multiplier into Google's speakingRate. Google
// refuses a value outside 0.25–2.0, which would lose the clip, so foci clamps.
func googleSpeakingRate(speed float64) float64 {
	if speed == 0 {
		return 0
	}
	if speed < googleTTSMinRate {
		voiceLog.Warnf("tts google: rate %.2f is below Google's minimum; using %.2f", speed, googleTTSMinRate)
		return googleTTSMinRate
	}
	if speed > googleTTSMaxRate {
		voiceLog.Warnf("tts google: rate %.2f is above Google's maximum; using %.2f", speed, googleTTSMaxRate)
		return googleTTSMaxRate
	}
	return speed
}

func (g *GoogleTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	voiceLog.Debugf("tts google text=%d chars voice=%s key=%s", len(text), g.Voice, log.FormatKeySuffix(g.APIKey))
	if n := len(text); n > googleTTSMaxInputBytes {
		return nil, &TextTooLongError{Chars: n, Max: googleTTSMaxInputBytes, Unit: "bytes"}
	}
	encoding, ok := config.GoogleTTSEncodings[strings.ToLower(g.ResponseFormat)]
	if !ok {
		return nil, fmt.Errorf("google tts: unknown response_format %q", g.ResponseFormat)
	}
	endpoint := g.Endpoint
	if endpoint == "" {
		endpoint = googleTTSDefaultEndpoint
	}
	voice := g.Voice
	if voice == "" {
		voice = googleTTSDefaultVoice
	}

	payload, err := json.Marshal(&googleTTSRequest{
		Input: googleTTSInput{Text: text},
		Voice: googleTTSVoice{LanguageCode: googleLanguageCode(voice), Name: voice},
		AudioConfig: googleTTSAudioConfig{
			AudioEncoding: encoding,
			SpeakingRate:  googleSpeakingRate(g.Speed),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal google tts request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create google tts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if g.APIKey != "" {
		req.Header.Set(googleTTSAPIKeyHeader, g.APIKey)
	}

	resp, err := g.HTTP.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("google tts request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, g.HTTP.maxResponse()))
	if err != nil {
		return nil, fmt.Errorf("read google tts response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var e googleTTSError
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("google tts API error %d %s: %s", resp.StatusCode, e.Error.Status, e.Error.Message)
		}
		return nil, fmt.Errorf("google tts API error %d: %s", resp.StatusCode, string(body))
	}

	var out googleTTSResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode google tts response: %w", err)
	}
	if out.AudioContent == "" {
		return nil, fmt.Errorf("google tts: response has no audioContent")
	}
	audio, err := base64.StdEncoding.DecodeString(out.AudioContent)
	if err != nil {
		return nil, fmt.Errorf("decode google tts audioContent: %w", err)
	}
	return audio, nil
}
