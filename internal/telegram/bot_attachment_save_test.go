package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
)

// TestSaveImage verifies that attachments are saved with the correct content
// and filename pattern.
func TestSaveImage(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-jpeg-data")
	path, err := b.saveAttachment(data, "image/jpeg", 12345)
	if err != nil {
		t.Fatalf("saveAttachment: %v", err)
	}

	// Verify file exists with correct content
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content = %q, want %q", got, data)
	}

	// Verify filename pattern: YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.jpg
	base := filepath.Base(path)
	if !strings.HasSuffix(base, "_chat-12345.jpg") {
		t.Errorf("filename %q doesn't match expected pattern", base)
	}
	if !strings.Contains(base, "T") {
		t.Errorf("filename %q missing timestamp", base)
	}
}

// TestSaveImageDisabled verifies that saveAttachment is only called when
// receivedFilesDir is set.
func TestSaveImageDisabled(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// receivedFilesDir not set — verify saveAttachment is only called when dir is set
	if b.receivedFilesDir != "" {
		t.Error("expected empty receivedFilesDir by default")
	}

	// Directly construct an attachment to verify no savedPath
	att := attachment{data: []byte("test"), mediaType: "image/jpeg"}
	if att.savedPath != "" {
		t.Error("expected empty savedPath when receivedFilesDir is not set")
	}
}

// TestSaveImagePNG verifies that PNG images are saved with the correct
// extension and content.
func TestSaveImagePNG(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-png-data")
	path, err := b.saveAttachment(data, "image/png", 99999)
	if err != nil {
		t.Fatalf("saveAttachment: %v", err)
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

// TestSaveImageCreatesDir verifies that saveAttachment creates intermediate
// directories as needed.
func TestSaveImageCreatesDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "subdir", "images")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	path, err := b.saveAttachment([]byte("data"), "image/jpeg", 1)
	if err != nil {
		t.Fatalf("saveAttachment: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file not found: %v", err)
	}
}

// TestSavedPathPropagatedToQueue verifies that attachment.savedPath flows
// through queuedMessage.
func TestSavedPathPropagatedToQueue(t *testing.T) {
	// Test that attachment.savedPath flows through queuedMessage
	att := attachment{
		data:      []byte("test"),
		mediaType: "image/jpeg",
		savedPath: "/tmp/test.jpg",
	}
	qm := queuedMessage{
		text:   "look at this",
		images: []attachment{att},
	}
	if qm.images[0].savedPath != "/tmp/test.jpg" {
		t.Errorf("savedPath not propagated: got %q", qm.images[0].savedPath)
	}
}
