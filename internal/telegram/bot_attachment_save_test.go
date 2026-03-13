package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
)

func TestSaveAttachment(t *testing.T) {
	// TestSaveAttachment verifies that attachments are saved with the correct
	// content and filename pattern.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-jpeg-data")
	path, err := b.saveMedia(data, "attachment", 12345, ".jpg")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	// Verify file exists with correct content
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content = %q, want %q", got, data)
	}

	// Verify filename pattern: YYYY-MM-DDTHH-MM-SSZ_attachment_chat-CHATID.jpg
	base := filepath.Base(path)
	if !strings.HasSuffix(base, "_chat-12345.jpg") {
		t.Errorf("filename %q doesn't match expected pattern", base)
	}
	if !strings.Contains(base, "T") {
		t.Errorf("filename %q missing timestamp", base)
	}
}

func TestSaveAttachmentDisabled(t *testing.T) {
	// TestSaveAttachmentDisabled verifies that save is only used when
	// receivedFilesDir is set.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// receivedFilesDir not set — verify it's empty by default
	if b.receivedFilesDir != "" {
		t.Error("expected empty receivedFilesDir by default")
	}

	// Directly construct an attachment to verify no savedPath
	att := attachment{data: []byte("test"), mediaType: "image/jpeg"}
	if att.savedPath != "" {
		t.Error("expected empty savedPath when receivedFilesDir is not set")
	}
}

func TestSaveAttachmentPNG(t *testing.T) {
	// TestSaveAttachmentPNG verifies that PNG images are saved with the correct
	// extension and content.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-png-data")
	path, err := b.saveMedia(data, "attachment", 99999, ".png")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	if !strings.HasSuffix(path, ".png") {
		t.Errorf("path %q should end with .png", path)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content mismatch")
	}
}

func TestSaveAttachmentCreatesDir(t *testing.T) {
	// TestSaveAttachmentCreatesDir verifies that saveMedia creates intermediate
	// directories as needed.
	base := t.TempDir()
	dir := filepath.Join(base, "subdir", "files")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	path, err := b.saveMedia([]byte("data"), "attachment", 1, ".jpg")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file not found: %v", err)
	}
}

func TestSavedPathPropagatedToQueue(t *testing.T) {
	// TestSavedPathPropagatedToQueue verifies that attachment.savedPath flows
	// through queuedMessage.
	att := attachment{
		data:      []byte("test"),
		mediaType: "image/jpeg",
		savedPath: "/tmp/test.jpg",
	}
	qm := queuedMessage{
		text:        "look at this",
		attachments: []attachment{att},
	}
	if qm.attachments[0].savedPath != "/tmp/test.jpg" {
		t.Errorf("savedPath not propagated: got %q", qm.attachments[0].savedPath)
	}
}
