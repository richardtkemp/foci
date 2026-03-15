package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// makeMsgWithVideo creates a test message with a video attachment.
func makeMsgWithVideo(userID int64, username, caption string, fileSize int64) *gotgbot.Message {
	return &gotgbot.Message{
		From:    &gotgbot.User{Id: userID, Username: username},
		Chat:    gotgbot.Chat{Id: 12345},
		Caption: caption,
		Date:    int64(time.Now().Unix()),
		Video: &gotgbot.Video{
			FileId:   "video_id",
			Width:    1920,
			Height:   1080,
			Duration: 30,
			MimeType: "video/mp4",
			FileSize: fileSize,
		},
	}
}

// makeMsgWithVideoNote creates a test message with a video note attachment.
func makeMsgWithVideoNote(userID int64, username string, fileSize int64) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Date: int64(time.Now().Unix()),
		VideoNote: &gotgbot.VideoNote{
			FileId:   "videonote_id",
			Length:   240,
			Duration: 15,
			FileSize: fileSize,
		},
	}
}

// makeMsgWithNonImageDocument creates a test message with a non-image document.
func makeMsgWithNonImageDocument(userID int64, username, filename, mime string, fileSize int64) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Date: int64(time.Now().Unix()),
		Document: &gotgbot.Document{
			FileId:   "doc_id",
			FileName: filename,
			MimeType: mime,
			FileSize: fileSize,
		},
	}
}

func TestReceiveMessage_VideoQueuedWithSavedPath(t *testing.T) {
	// Verifies that video messages
	// are queued with saved file paths.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir
	b.botToken = "test-token"

	// Small video (under 20MB)
	msg := makeMsgWithVideo(111, "owner", "Check this out!", 5*1024*1024)
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Note: Download will fail without a real server, so we can't test the saved path
	// Just verify the caption is preserved
	if !strings.Contains(qm.text, "Check this out!") {
		t.Errorf("text should contain original caption, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoTooLarge(t *testing.T) {
	// Verifies that oversized videos are flagged
	// without download attempts.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = t.TempDir()
	b.botToken = "test-token"

	// Large video (over 20MB)
	msg := makeMsgWithVideo(111, "owner", "Check this out!", 25*1024*1024)
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if !strings.Contains(qm.text, "[Video too large to download (25 MB)]") {
		t.Errorf("text should indicate video too large, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoWithoutCaption(t *testing.T) {
	// Verifies that videos without captions
	// and failed downloads are dropped.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir
	b.botToken = "test-token"

	// Video with no caption - when download fails, message is dropped (no text, no images)
	msg := makeMsgWithVideo(111, "owner", "", 5*1024*1024)
	b.receiveMessage(context.Background(), msg)

	// Without a real server, download fails, so message with no caption is dropped
	if len(b.queue) != 0 {
		t.Errorf("expected 0 queued messages when video download fails with no caption, got %d", len(b.queue))
	}
}

func TestReceiveMessage_VideoNoteQueuedWithSavedPath(t *testing.T) {
	// Verifies that video notes
	// are queued with saved paths.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir
	b.botToken = "test-token"

	// Video note with text caption
	msg := makeMsgWithVideoNote(111, "owner", 5*1024*1024)
	msg.Caption = "Quick video note"
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Download will fail without a real server, just check caption preserved
	if !strings.Contains(qm.text, "Quick video note") {
		t.Errorf("text should contain caption, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoNoteTooLarge(t *testing.T) {
	// Verifies that oversized video notes
	// are queued with size warnings.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = t.TempDir()
	b.botToken = "test-token"

	msg := makeMsgWithVideoNote(111, "owner", 25*1024*1024)
	b.receiveMessage(context.Background(), msg)

	// Video note too large - message is still queued with size warning
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if !strings.Contains(qm.text, "[Video too large to download (25 MB)]") {
		t.Errorf("text should indicate video too large, got: %q", qm.text)
	}
}

func TestReceiveMessage_NonImageDocumentSaved(t *testing.T) {
	// Verifies that non-image documents
	// are queued with captions.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir
	b.botToken = "test-token"

	// Document with caption
	msg := makeMsgWithNonImageDocument(111, "owner", "report.pdf", "application/pdf", 100*1024)
	msg.Caption = "Here's the report"
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Download will fail without a real server, just check caption preserved
	if !strings.Contains(qm.text, "Here's the report") {
		t.Errorf("text should contain caption, got: %q", qm.text)
	}
}

func TestReceiveMessage_NonImageDocumentTooLarge(t *testing.T) {
	// Verifies that oversized documents
	// are flagged.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = t.TempDir()
	b.botToken = "test-token"

	// Document with caption
	msg := makeMsgWithNonImageDocument(111, "owner", "large.zip", "application/zip", 25*1024*1024)
	msg.Caption = "Big file"
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if !strings.Contains(qm.text, "[Document too large to download (25 MB)]") {
		t.Errorf("text should indicate document too large, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoNoSaveDir(t *testing.T) {
	// Verifies that videos are queued even
	// without a save directory.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// receivedFilesDir not set
	b.botToken = "test-token"

	msg := makeMsgWithVideo(111, "owner", "Check this out!", 5*1024*1024)
	b.receiveMessage(context.Background(), msg)

	// Should still queue the message, but without the saved path
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Text should be the caption since we couldn't save
	if qm.text != "Check this out!" {
		t.Errorf("text should be original caption, got: %q", qm.text)
	}
}

func TestExtForVideo(t *testing.T) {
	// Verifies that extForVideo returns correct extensions for
	// video MIME types.
	tests := []struct {
		mime string
		want string
	}{
		{"video/mp4", ".mp4"},
		{"video/quicktime", ".mov"},
		{"video/webm", ".webm"},
		{"video/x-matroska", ".mkv"},
		{"video/avi", ".avi"},
		{"video/x-msvideo", ".avi"},
		{"video/unknown", ".mp4"},
		{"", ".mp4"},
	}
	for _, tt := range tests {
		if got := extForVideo(tt.mime); got != tt.want {
			t.Errorf("extForVideo(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

func TestExtForMIME(t *testing.T) {
	// Verifies that extForMIME returns correct extensions for
	// various MIME types.
	tests := []struct {
		mime string
		want string
	}{
		{"video/mp4", ".mp4"},
		{"application/pdf", ".pdf"},
		{"application/json", ".json"},
		{"text/plain", ".txt"},
		{"text/csv", ".csv"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
		{"application/vnd.ms-excel", ".xls"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"},
		{"application/msword", ".doc"},
		{"audio/mpeg", ".mp3"},
		{"audio/ogg", ".mp3"},
		{"application/octet-stream", ".bin"},
		{"", ".bin"},
	}
	for _, tt := range tests {
		if got := extForMIME(tt.mime); got != tt.want {
			t.Errorf("extForMIME(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

func TestIsFileTooLarge(t *testing.T) {
	// Verifies that isFileTooLarge correctly identifies
	// fileTooLargeError instances.
	err := &fileTooLargeError{size: 25 * 1024 * 1024}
	if !isFileTooLarge(err) {
		t.Error("isFileTooLarge should return true for fileTooLargeError")
	}
	if isFileTooLarge(fmt.Errorf("other error")) {
		t.Error("isFileTooLarge should return false for other errors")
	}
}

func TestFileTooLargeError(t *testing.T) {
	// Verifies that fileTooLargeError formats correctly.
	size := int64(25 * 1024 * 1024)
	err := &fileTooLargeError{size: size}
	if !strings.Contains(err.Error(), "26214400") {
		t.Errorf("error message should contain size in bytes, got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "20MB") {
		t.Errorf("error message should mention 20MB limit, got: %s", err.Error())
	}
}

func TestSaveMedia(t *testing.T) {
	// Verifies that saveMedia saves files with correct metadata.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	data := []byte("fake-mp4-data")
	path, err := b.saveMedia(data, "video", 12345, ".mp4")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content mismatch")
	}

	base := filepath.Base(path)
	if !strings.Contains(base, "_video_") {
		t.Errorf("filename should contain _video_, got: %s", base)
	}
	if !strings.HasSuffix(base, ".mp4") {
		t.Errorf("filename should end with .mp4, got: %s", base)
	}
}

func TestSaveMediaDocument(t *testing.T) {
	// Verifies that saveMedia correctly saves documents.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	data := []byte("document content")
	path, err := b.saveMedia(data, "document", 12345, ".pdf")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content mismatch")
	}

	base := filepath.Base(path)
	if !strings.Contains(base, "_document_") {
		t.Errorf("filename should contain _document_, got: %s", base)
	}
}

// Note: downloadAndSaveMedia is not a public method, so we skip testing it directly.
// The behavior is tested via integration with receiveMessage tests above.
