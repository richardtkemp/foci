package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"clod/log"
	"clod/voice"
)

// VoiceReplyFunc is called to deliver a voice note from the TTS tool.
type VoiceReplyFunc func(audioData []byte)

// NewTTSTool creates a tool that converts text to a voice note via TTS.
// The voiceReplyFn callback delivers the audio to the current chat.
func NewTTSTool(tts *voice.TTS, voiceReplyFn func() VoiceReplyFunc) *Tool {
	return &Tool{
		Name:        "tts",
		Description: "Convert text to a voice note and send it to the user. Use when you want to reply with audio instead of text. The voice note is sent immediately.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "The text to convert to speech. Keep it conversational — no markdown, no code blocks, no long lists."
				}
			},
			"required": ["text"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.Text == "" {
				return "", fmt.Errorf("text is required")
			}

			log.Infof("tts", "synthesizing %d chars", len(p.Text))

			audioData, err := tts.Synthesize(ctx, p.Text)
			if err != nil {
				return "", fmt.Errorf("tts: %w", err)
			}

			fn := voiceReplyFn()
			if fn == nil {
				return "Voice note generated but no delivery channel available.", nil
			}

			fn(audioData)
			log.Infof("tts", "sent voice note (%d bytes)", len(audioData))
			return fmt.Sprintf("Voice note sent (%d bytes).", len(audioData)), nil
		},
	}
}
