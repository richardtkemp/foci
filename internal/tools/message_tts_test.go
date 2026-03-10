package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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

// TestSendMessageToUserVoiceTTS verifies that text with send_as=voice synthesizes audio.
func TestSendMessageToUserVoiceTTS(t *testing.T) {
	mock := &mockMessageSender{}
	tts := &mockTTS{data: []byte("fake-audio")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, tts)

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

// TestSendMessageToUserVoiceTTSChatRouting verifies that TTS audio is routed to chat-targeted method.
func TestSendMessageToUserVoiceTTSChatRouting(t *testing.T) {
	mock := &mockMessageSender{}
	tts := &mockTTS{data: []byte("fake-audio")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, tts)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
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

// TestSendMessageToUserVoiceTTSNoProvider verifies that error is returned when TTS is not configured.
func TestSendMessageToUserVoiceTTSNoProvider(t *testing.T) {
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

// TestSendMessageToUserVoiceTTSSynthesizeError verifies that TTS synthesis errors are propagated.
func TestSendMessageToUserVoiceTTSSynthesizeError(t *testing.T) {
	mock := &mockMessageSender{}
	tts := &mockTTS{err: fmt.Errorf("API rate limit")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, tts)

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

// TestSendMessageToUserVoiceFilePathStillWorks verifies that file_path takes precedence over TTS synthesis.
func TestSendMessageToUserVoiceFilePathStillWorks(t *testing.T) {
	// When file_path is provided with send_as=voice, it should use the file-based path
	mock := &mockMessageSender{}
	tts := &mockTTS{data: []byte("should-not-be-used")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"send_as":   "voice",
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
