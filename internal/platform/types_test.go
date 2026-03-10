package platform

import (
	"context"
	"testing"

	"foci/internal/warnings"
)

// TestSenderInterface verifies that mockSender implements the Sender interface.
// This is a compile-time check that catches interface drift.
func TestSenderInterface(t *testing.T) {
	var _ Sender = (*mockSender)(nil)
}

// TestConnectionInterface verifies that mockConnection implements the Connection interface.
// This is a compile-time check that catches interface drift.
func TestConnectionInterface(t *testing.T) {
	var _ Connection = (*mockConnection)(nil)
}

// TestMessageHandlerInterface verifies that mockHandler implements the MessageHandler interface.
// This is a compile-time check that catches interface drift.
func TestMessageHandlerInterface(t *testing.T) {
	var _ MessageHandler = (*mockHandler)(nil)
}

// TestConnectionManagerInterface verifies that noopConnMgr implements ConnectionManager.
// This is a compile-time check that catches interface drift.
func TestConnectionManagerInterface(t *testing.T) {
	var _ ConnectionManager = (*noopConnMgr)(nil)
	var _ ConnectionManager = (*aggregatingConnMgr)(nil)
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

type mockConnection struct {
	*mockSender
}

func (m *mockConnection) SessionKeyForChat(chatID int64) string { return "" }
func (m *mockConnection) DefaultSessionKey() string             { return "" }
func (m *mockConnection) SetSessionKey(key string)              {}
func (m *mockConnection) SetSessionKeyDirect(key string)        {}
func (m *mockConnection) SetChatID(chatID int64)                {}
func (m *mockConnection) ChatID() int64                         { return 0 }
func (m *mockConnection) Username() string                      { return "" }
func (m *mockConnection) SendToSession(sk, text string) error   { return nil }
func (m *mockConnection) SendNotification(text string)          {}

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
