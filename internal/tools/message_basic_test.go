package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/internal/platform"
)

func TestSendMessageToUserTextOnly(t *testing.T) {
	// Verifies that providing only text sends exactly one text message and no document or voice calls.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello user",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text" {
		t.Errorf("result = %q, want %q", result.Text, "Sent: text")
	}
	if len(mock.textCalls) != 1 || mock.textCalls[0] != "hello user" {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 0 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendMessageToUserDocumentOnly(t *testing.T) {
	// Verifies that providing only a file path sends exactly one document and no text calls.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file": "/tmp/report.pdf",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.documentCalls) != 1 || mock.documentCalls[0] != "/tmp/report.pdf" {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendMessageToUserVoice(t *testing.T) {
	// Verifies that a file with send_as=voice is sent as a voice note, not a document.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

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
	if len(mock.voiceCalls) != 1 || mock.voiceCalls[0] != "/tmp/note.ogg" {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

func TestSendMessageToUserTextAndDocument(t *testing.T) {
	// Verifies that text + file collapses into a single captioned-document
	// message rather than two separate messages. The text travels as the
	// document caption (Telegram caption / Discord Content) — single
	// message, no separate text send.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "here's the file",
		"file": "/tmp/data.csv",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document+caption" {
		t.Errorf("result = %q", result.Text)
	}
	// No standalone text send when caption rides on the document.
	if len(mock.textCalls) != 0 {
		t.Errorf("textCalls should be empty (caption rides on document), got %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
	if len(mock.documentCaptions) != 1 || mock.documentCaptions[0] != "here's the file" {
		t.Errorf("documentCaptions = %v, want [\"here's the file\"]", mock.documentCaptions)
	}
}

func TestSendMessageToUserNoInput(t *testing.T) {
	// Verifies that omitting both text and file_path returns a validation error requiring at least one input.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for no input")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserNilSender(t *testing.T) {
	// Verifies that a nil platform.Sender (messaging not configured) returns a "messaging not configured" error rather than panicking.
	t.Parallel()
	tool := NewSendToChatTool(func(string) platform.Sender { return nil }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil sender")
	}
	if !strings.Contains(err.Error(), "messaging not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserTextError(t *testing.T) {
	// Verifies that errors from the text sender are propagated back to the caller.
	t.Parallel()
	mock := &mockSender{textErr: fmt.Errorf("network down")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "network down") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserDocumentError(t *testing.T) {
	// Verifies that errors from the document sender are propagated back to the caller.
	t.Parallel()
	mock := &mockSender{documentErr: fmt.Errorf("file too large")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file": "/tmp/huge.bin",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "file too large") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserVoiceError(t *testing.T) {
	// Verifies that errors from the voice sender are propagated back to the caller.
	t.Parallel()
	mock := &mockSender{voiceErr: fmt.Errorf("codec error")}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file":    "/tmp/voice.ogg",
		"send_as": "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "codec error") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendMessageToUserCaptionTooLongFallsBack(t *testing.T) {
	// When the caption exceeds platform.MaxCaptionLen (1024), the tool
	// should fall back to a separate text message + uncaptioned file.
	// Telegram caps document captions at 1024; sending longer would error
	// at the platform.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	longText := strings.Repeat("x", platform.MaxCaptionLen+1)
	params, _ := json.Marshal(map[string]interface{}{
		"text": longText,
		"file": "/tmp/data.csv",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text + document" {
		t.Errorf("result = %q (expected fallback to two-message form)", result.Text)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls should have 1 entry (text didn't fit as caption), got %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
	if len(mock.documentCaptions) != 1 || mock.documentCaptions[0] != "" {
		t.Errorf("documentCaptions = %v, want [\"\"]", mock.documentCaptions)
	}
}

func TestSendMessageToUserVoiceWithTextDoesNotCaption(t *testing.T) {
	// Voice notes don't caption — text + voice should still be two
	// separate messages even when text fits within MaxCaptionLen.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "listen to this",
		"file":    "/tmp/note.ogg",
		"send_as": "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text + voice note" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls = %v (expected separate text)", mock.textCalls)
	}
	if len(mock.voiceCalls) != 1 {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

func TestJoinWords(t *testing.T) {
	// Verifies that joinWords produces correct human-readable summaries for nil, single, and multiple word slices.
	t.Parallel()
	tests := []struct {
		words []string
		want  string
	}{
		{nil, "nothing"},
		{[]string{"text"}, "text"},
		{[]string{"text", "document"}, "text + document"},
	}
	for _, tt := range tests {
		got := joinWords(tt.words)
		if got != tt.want {
			t.Errorf("joinWords(%v) = %q, want %q", tt.words, got, tt.want)
		}
	}
}
