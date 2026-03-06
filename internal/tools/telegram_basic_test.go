package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSendTelegramTextOnly verifies that sending text alone works correctly.
func TestSendTelegramTextOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

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

// TestSendTelegramDocumentOnly verifies that sending a document alone works correctly.
func TestSendTelegramDocumentOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
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

// TestSendTelegramVoice verifies that sending voice notes works correctly.
func TestSendTelegramVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

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
	if len(mock.voiceCalls) != 1 || mock.voiceCalls[0] != "/tmp/note.ogg" {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

// TestSendTelegramTextAndDocument verifies that sending text and document together works.
func TestSendTelegramTextAndDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":      "here's the file",
		"file_path": "/tmp/data.csv",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text + document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

// TestSendTelegramNoInput verifies that an error is returned when no text or file is provided.
func TestSendTelegramNoInput(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for no input")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramNilSender verifies that an error is returned when telegram is not configured.
func TestSendTelegramNilSender(t *testing.T) {
	tool := NewSendTelegramTool(func(string) TelegramSender { return nil }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil sender")
	}
	if !strings.Contains(err.Error(), "telegram not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramTextError verifies that send errors are propagated for text.
func TestSendTelegramTextError(t *testing.T) {
	mock := &mockTelegramSender{textErr: fmt.Errorf("network down")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

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

// TestSendTelegramDocumentError verifies that send errors are propagated for documents.
func TestSendTelegramDocumentError(t *testing.T) {
	mock := &mockTelegramSender{documentErr: fmt.Errorf("file too large")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/huge.bin",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "file too large") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramVoiceError verifies that send errors are propagated for voice notes.
func TestSendTelegramVoiceError(t *testing.T) {
	mock := &mockTelegramSender{voiceErr: fmt.Errorf("codec error")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/voice.ogg",
		"send_as":   "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "codec error") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestJoinWords verifies the joinWords helper function for various combinations.
func TestJoinWords(t *testing.T) {
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
