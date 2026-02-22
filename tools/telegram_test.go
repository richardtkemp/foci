package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockTelegramSender records calls to SendText, SendDocument, SendVoice.
type mockTelegramSender struct {
	textCalls     []string
	documentCalls []string
	voiceCalls    []string
	textErr       error
	documentErr   error
	voiceErr      error
}

func (m *mockTelegramSender) SendText(text string) error {
	m.textCalls = append(m.textCalls, text)
	return m.textErr
}

func (m *mockTelegramSender) SendDocument(filePath string) error {
	m.documentCalls = append(m.documentCalls, filePath)
	return m.documentErr
}

func (m *mockTelegramSender) SendVoice(filePath string) error {
	m.voiceCalls = append(m.voiceCalls, filePath)
	return m.voiceErr
}

func TestSendTelegramTextOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello user",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: text" {
		t.Errorf("result = %q, want %q", result, "Sent: text")
	}
	if len(mock.textCalls) != 1 || mock.textCalls[0] != "hello user" {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 0 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramDocumentOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: document" {
		t.Errorf("result = %q", result)
	}
	if len(mock.documentCalls) != 1 || mock.documentCalls[0] != "/tmp/report.pdf" {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"as_voice":  true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: voice note" {
		t.Errorf("result = %q", result)
	}
	if len(mock.voiceCalls) != 1 || mock.voiceCalls[0] != "/tmp/note.ogg" {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

func TestSendTelegramTextAndDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"text":      "here's the file",
		"file_path": "/tmp/data.csv",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: text + document" {
		t.Errorf("result = %q", result)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramNoInput(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for no input")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramNilSender(t *testing.T) {
	tool := NewSendTelegramTool(func() TelegramSender { return nil })

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

func TestSendTelegramTextError(t *testing.T) {
	mock := &mockTelegramSender{textErr: fmt.Errorf("network down")}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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

func TestSendTelegramDocumentError(t *testing.T) {
	mock := &mockTelegramSender{documentErr: fmt.Errorf("file too large")}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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

func TestSendTelegramVoiceError(t *testing.T) {
	mock := &mockTelegramSender{voiceErr: fmt.Errorf("codec error")}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/voice.ogg",
		"as_voice":  true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "codec error") {
		t.Errorf("error = %q", err.Error())
	}
}

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
