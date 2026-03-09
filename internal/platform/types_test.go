package platform

import (
	"context"
	"testing"
	"time"

	"foci/internal/warnings"
)

// TestSenderInterface verifies that mockSender implements the Sender interface.
// This is a compile-time check that catches interface drift.
func TestSenderInterface(t *testing.T) {
	var _ Sender = (*mockSender)(nil)
}

// TestPlatformInterface verifies that mockPlatform implements the Platform interface.
// This is a compile-time check that catches interface drift.
func TestPlatformInterface(t *testing.T) {
	var _ Platform = (*mockPlatform)(nil)
}

// TestMessageHandlerInterface verifies that mockHandler implements the MessageHandler interface.
// This is a compile-time check that catches interface drift.
func TestMessageHandlerInterface(t *testing.T) {
	var _ MessageHandler = (*mockHandler)(nil)
}

// TestMessageStruct verifies that Message struct fields are correctly assigned and accessible.
func TestMessageStruct(t *testing.T) {
	m := Message{
		ID:        "msg-123",
		Text:      "Hello",
		SenderID:  "user-1",
		ChatID:    "chat-1",
		Timestamp: time.Now(),
	}
	if m.ID != "msg-123" {
		t.Errorf("Expected ID 'msg-123', got %s", m.ID)
	}
	if m.Text != "Hello" {
		t.Errorf("Expected Text 'Hello', got %s", m.Text)
	}
}

func TestAttachmentStruct(t *testing.T) {
	a := Attachment{
		Type:      "image",
		Data:      []byte{0x01, 0x02},
		MimeType:  "image/png",
		SavedPath: "/tmp/image.png",
	}
	if a.Type != "image" {
		t.Errorf("Expected Type 'image', got %s", a.Type)
	}
}

func TestTurnCallbacksStruct(t *testing.T) {
	called := false
	cb := TurnCallbacks{
		ReplyFunc: func(text string) {
			called = true
		},
	}
	cb.ReplyFunc("test")
	if !called {
		t.Error("ReplyFunc was not called")
	}
}

type mockSender struct{}

func (m *mockSender) SessionKey() string                                       { return "" }
func (m *mockSender) SendText(text string) error                               { return nil }
func (m *mockSender) SendDocument(filePath string) error                       { return nil }
func (m *mockSender) SendVoice(filePath string) error                          { return nil }
func (m *mockSender) SendVideo(filePath string) error                          { return nil }
func (m *mockSender) SendPhoto(filePath string) error                          { return nil }
func (m *mockSender) SendAudio(filePath string) error                          { return nil }
func (m *mockSender) SendAnimation(filePath string) error                      { return nil }
func (m *mockSender) SendVoiceData(audioData []byte) error                     { return nil }
func (m *mockSender) SendTextToChat(chatID int64, text string) error           { return nil }
func (m *mockSender) SendDocumentToChat(chatID int64, filePath string) error   { return nil }
func (m *mockSender) SendVoiceToChat(chatID int64, filePath string) error      { return nil }
func (m *mockSender) SendVideoToChat(chatID int64, filePath string) error      { return nil }
func (m *mockSender) SendPhotoToChat(chatID int64, filePath string) error      { return nil }
func (m *mockSender) SendAudioToChat(chatID int64, filePath string) error      { return nil }
func (m *mockSender) SendAnimationToChat(chatID int64, filePath string) error  { return nil }
func (m *mockSender) SendVoiceDataToChat(chatID int64, audioData []byte) error { return nil }

type mockPlatform struct {
	*mockSender
}

func (m *mockPlatform) Receive(ctx context.Context) (<-chan Message, error) {
	ch := make(chan Message)
	close(ch)
	return ch, nil
}
func (m *mockPlatform) SessionKeyForChat(chatID string) string { return "chat:" + chatID }
func (m *mockPlatform) Start(ctx context.Context) error        { return nil }
func (m *mockPlatform) Stop() error                            { return nil }

type mockHandler struct{}

func (m *mockHandler) HandleMessage(ctx context.Context, sessionKey, text string) (string, error) {
	return "", nil
}
func (m *mockHandler) HandleMessageWithAttachments(ctx context.Context, sessionKey, text string, attachments []Attachment) (string, error) {
	return "", nil
}
func (m *mockHandler) IsProcessing() bool                  { return false }
func (m *mockHandler) TransformMessage(text string) string { return text }
func (m *mockHandler) Warnings() *warnings.Queue           { return nil }
