package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockTTS is a mock voice.TTS implementation.
type mockTTS struct {
	data []byte
	err  error
}

func (m *mockTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.data, nil
}

func TestTTSToolSuccess(t *testing.T) {
	audioData := []byte("fake-mp3-audio-data")
	tts := &mockTTS{data: audioData}

	var delivered []byte
	voiceReplyFn := func() VoiceReplyFunc {
		return func(data []byte) {
			delivered = data
		}
	}

	tool := NewTTSTool(tts, voiceReplyFn)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "Hello world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "Voice note sent") {
		t.Errorf("result = %q", result)
	}
	if !strings.Contains(result, fmt.Sprintf("%d bytes", len(audioData))) {
		t.Errorf("result = %q, want byte count", result)
	}

	// Verify the exact bytes were delivered
	if string(delivered) != string(audioData) {
		t.Errorf("delivered = %q, want %q", string(delivered), string(audioData))
	}
}

func TestTTSToolEmptyText(t *testing.T) {
	tts := &mockTTS{data: []byte("audio")}
	tool := NewTTSTool(tts, func() VoiceReplyFunc { return nil })

	params, _ := json.Marshal(map[string]interface{}{
		"text": "",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty text")
	}
	if !strings.Contains(err.Error(), "text is required") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestTTSToolSynthesizeError(t *testing.T) {
	tts := &mockTTS{err: fmt.Errorf("API rate limit")}
	tool := NewTTSTool(tts, func() VoiceReplyFunc { return nil })

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error from synthesize")
	}
	if !strings.Contains(err.Error(), "API rate limit") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestTTSToolNoDeliveryChannel(t *testing.T) {
	tts := &mockTTS{data: []byte("audio")}
	tool := NewTTSTool(tts, func() VoiceReplyFunc { return nil })

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "no delivery channel") {
		t.Errorf("result = %q, want 'no delivery channel'", result)
	}
}

func TestTTSToolName(t *testing.T) {
	tts := &mockTTS{}
	tool := NewTTSTool(tts, func() VoiceReplyFunc { return nil })
	if tool.Name != "tts" {
		t.Errorf("name = %q, want %q", tool.Name, "tts")
	}
}
