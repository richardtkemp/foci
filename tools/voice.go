package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/log"
	"foci/voice"
)

// VoiceReplyFunc is called to deliver a voice note from the TTS tool.
type VoiceReplyFunc func(audioData []byte)

// NewTTSTool creates a tool that converts text to a voice note via TTS.
// The voice reply function is extracted from context at execution time
// (set by the agent loop via WithVoiceReplyFunc).
func NewTTSTool(tts voice.TTS) *Tool {
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

			fn := VoiceReplyFuncFromContext(ctx)
			if fn == nil {
				return "Voice note generated but no delivery channel available.", nil
			}

			fn(audioData)
			log.Infof("tts", "sent voice note (%d bytes)", len(audioData))
			return fmt.Sprintf("Voice note sent (%d bytes).", len(audioData)), nil
		},
	}
}
