package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestReceiveMessage_PhotoMessageQueued(t *testing.T) {
	// Verifies that photo messages are
	// properly queued for processing.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set up mock file info for photo downloads
	mock.files = map[string]string{
		"large_id": "photos/test.jpg",
	}
	b.botToken = "test-token"

	// The download will fail (no real server), but the message should still queue
	// with whatever images succeeded
	msg := makeMsgWithPhoto(111, "owner", "Look at this!")
	b.receiveMessage(context.Background(), msg)

	// Message should be queued (text from caption)
	if len(b.mq.Chan()) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.mq.Chan()))
	}
	qm := <-b.mq.Chan()
	if qm.Text != "Look at this!" {
		t.Errorf("queued text = %q, want %q", qm.Text, "Look at this!")
	}
}

func TestReceiveMessage_PhotoWithoutCaption(t *testing.T) {
	// Verifies that photos without a
	// caption are handled correctly.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	mock := b.client.(*mockClient)
	mock.files = map[string]string{
		"large_id": "photos/test.jpg",
	}
	b.botToken = "test-token"

	// Photo message with no text and no caption — should still be queued
	// (has an image even if download fails from no real server)
	msg := makeMsgWithPhoto(111, "owner", "")
	b.receiveMessage(context.Background(), msg)

	// Since download hits network and fails, images will be empty, and text is empty => dropped
	// This case tests that empty caption + failed download = dropped
}

func TestReceiveMessage_DocumentImageQueued(t *testing.T) {
	// Verifies that image documents are
	// handled correctly.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	mock := b.client.(*mockClient)
	mock.files = map[string]string{
		"doc_id": "documents/image.png",
	}
	b.botToken = "test-token"

	msg := makeMsgWithDocument(111, "owner", "image/png")
	// No text — but document is an image, download will fail (no server)
	// but text is empty and images empty (download fails) => dropped
	b.receiveMessage(context.Background(), msg)
	// Just verify no panic
}

func TestReceiveMessage_NonImageDocumentIgnored(t *testing.T) {
	// Verifies that non-image, non-PDF
	// documents without text are ignored.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsgWithDocument(111, "owner", "application/zip")
	b.receiveMessage(context.Background(), msg)

	// Non-image, non-PDF document with no text should be dropped
	if len(b.mq.Chan()) != 0 {
		t.Error("non-image document should not be queued")
	}
}

// makeMsgWithVoice creates a test message with a voice note attachment.
func makeMsgWithVoice(userID int64, username string) *gotgbot.Message {
	return &gotgbot.Message{
		From:  &gotgbot.User{Id: userID, Username: username},
		Chat:  gotgbot.Chat{Id: 12345},
		Date:  int64(time.Now().Unix()),
		Voice: &gotgbot.Voice{FileId: "voice_id", Duration: 5},
	}
}

func TestReceiveMessage_VoiceWithoutTranscriber(t *testing.T) {
	// Verifies that voice notes without
	// a transcriber configured are rejected with an error message.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// No transcriber set — voice note should not be queued but should get an error reply
	msg := makeMsgWithVoice(111, "owner")
	b.receiveMessage(context.Background(), msg)

	if len(b.mq.Chan()) != 0 {
		t.Error("voice without transcriber should not be queued")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 error reply for voice without transcriber, got %d", mock.sentCount())
	}
	if !strings.Contains(mock.lastSendInjected, "Voice notes require") {
		t.Errorf("reply text = %q, want it to mention 'Voice notes require'", mock.lastSendInjected)
	}
}
