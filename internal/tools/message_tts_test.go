package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/internal/platform"
)

// mockTTS provides a mock TTS synthesizer for testing.
type mockTTS struct {
	data []byte
	err  error
}

// Synthesize returns mock audio data or error.
func (m *mockTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.data, nil
}

func TestSendMessageToUserVoiceTTS(t *testing.T) {
	// Verifies that providing text with send_as=voice synthesizes audio via TTS and sends it as voice data, without sending the text separately.
	t.Parallel()
	mock := &mockSender{}
	tts := &mockTTS{data: []byte("fake-audio")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello world",
		"send_as": "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q, want %q", result.Text, "Sent: voice note")
	}
	if len(mock.voiceDataCalls) != 1 {
		t.Fatalf("expected 1 voiceDataCall, got %d", len(mock.voiceDataCalls))
	}
	if string(mock.voiceDataCalls[0]) != "fake-audio" {
		t.Errorf("voiceDataCalls[0] = %q", string(mock.voiceDataCalls[0]))
	}
	// Should NOT send text separately
	if len(mock.textCalls) != 0 {
		t.Errorf("textCalls = %v, should be empty for TTS synthesis", mock.textCalls)
	}
}

func TestSendMessageToUserVoiceTTSChatRouting(t *testing.T) {
	// Verifies that TTS-synthesized voice data is dispatched to the specific chat via SendVoiceDataToChat when a chat ID is in the session key.
	t.Parallel()
	mock := &mockSender{}
	tts := &mockTTS{data: []byte("fake-audio")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, tts)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello world",
		"send_as": "voice",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.chatVoiceDataCalls) != 1 {
		t.Fatalf("expected 1 chatVoiceDataCall, got %d", len(mock.chatVoiceDataCalls))
	}
	if mock.chatVoiceDataCalls[0].chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", mock.chatVoiceDataCalls[0].chatID)
	}
	if len(mock.voiceDataCalls) != 0 {
		t.Errorf("default SendVoiceData should not be called")
	}
}

func TestSendMessageToUserVoiceTTSNoProvider(t *testing.T) {
	// Verifies that requesting TTS synthesis when no TTS provider is configured returns a "tts not configured" error.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello world",
		"send_as": "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil TTS")
	}
	if !strings.Contains(err.Error(), "tts not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserVoiceTTSSynthesizeError(t *testing.T) {
	// Verifies that errors from the TTS synthesis step (e.g. API rate limit) are propagated back to the caller.
	t.Parallel()
	mock := &mockSender{}
	tts := &mockTTS{err: fmt.Errorf("API rate limit")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello",
		"send_as": "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error from synthesize")
	}
	if !strings.Contains(err.Error(), "API rate limit") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserVoiceFilePathStillWorks(t *testing.T) {
	// Verifies that when a file_path is provided with send_as=voice, the file-based path takes precedence over TTS synthesis.
	t.Parallel()
	mock := &mockSender{}
	tts := &mockTTS{data: []byte("should-not-be-used")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"file":    "/tmp/note.ogg",
		"send_as": "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	// Should use file-based voice, not TTS synthesis
	if len(mock.voiceCalls) != 1 {
		t.Errorf("voiceCalls = %v, want 1 file-based call", mock.voiceCalls)
	}
	if len(mock.voiceDataCalls) != 0 {
		t.Errorf("voiceDataCalls should be empty for file-based voice")
	}
}
