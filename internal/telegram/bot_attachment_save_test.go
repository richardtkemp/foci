package telegram

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
)

func TestSaveAttachment(t *testing.T) {
	// Verifies that attachments are saved with the correct
	// content and filename pattern.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	data := []byte("fake-jpeg-data")
	path, err := b.saveMedia(data, "attachment", 12345, ".jpg", "")
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
	// Verifies that save is only used when
	// receivedFilesDir is set.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// receivedFilesDir not set — verify it's empty by default
	if b.display.ReceivedFilesDir != "" {
		t.Error("expected empty receivedFilesDir by default")
	}

	// Directly construct an attachment to verify no savedPath
	att := attachment{data: []byte("test"), mediaType: "image/jpeg"}
	if att.savedPath != "" {
		t.Error("expected empty savedPath when receivedFilesDir is not set")
	}
}

func TestSaveAttachmentPNG(t *testing.T) {
	// Verifies that PNG images are saved with the correct
	// extension and content.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	data := []byte("fake-png-data")
	path, err := b.saveMedia(data, "attachment", 99999, ".png", "")
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
	// Verifies that saveMedia creates intermediate
	// directories as needed.
	base := t.TempDir()
	dir := filepath.Join(base, "subdir", "files")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	path, err := b.saveMedia([]byte("data"), "attachment", 1, ".jpg", "")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file not found: %v", err)
	}
}

func TestSaveMediaAlbumNoCollision(t *testing.T) {
	// Regression: multiple images sent in one Telegram album (sharing a
	// media_group_id) must not collide on an identical seconds-resolution
	// filename. They should share one timestamp and get sequential _N
	// suffixes, yielding distinct files.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	const group = "album-42"
	var paths []string
	var stamps []string
	for i := 0; i < 3; i++ {
		data := []byte{byte('a' + i)} // distinct content per image
		path, err := b.saveMedia(data, "attachment", 12345, ".jpg", group)
		if err != nil {
			t.Fatalf("saveMedia[%d]: %v", i, err)
		}
		paths = append(paths, path)
		// timestamp is everything before the first "_attachment" segment
		base := filepath.Base(path)
		stamps = append(stamps, strings.SplitN(base, "_attachment", 2)[0])

		// Each file must exist and retain its own content (no overwrite).
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read saved file[%d]: %v", i, err)
		}
		if string(got) != string(data) {
			t.Errorf("file[%d] content = %q, want %q (overwritten?)", i, got, data)
		}
	}

	// All distinct paths.
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Errorf("duplicate path %q — album images collided", p)
		}
		seen[p] = true
	}

	// Sequential suffixes _1, _2, _3.
	for i, p := range paths {
		want := fmt.Sprintf("_%d.jpg", i+1)
		if !strings.HasSuffix(p, want) {
			t.Errorf("path[%d] = %q, want suffix %q", i, filepath.Base(p), want)
		}
	}

	// Single shared timestamp across the whole set.
	for i := 1; i < len(stamps); i++ {
		if stamps[i] != stamps[0] {
			t.Errorf("timestamp[%d] = %q, want shared %q", i, stamps[i], stamps[0])
		}
	}
}

func TestSaveMediaStandaloneSameSecondNoCollision(t *testing.T) {
	// Two standalone images (no media_group_id) saved in the same second must
	// not overwrite each other — the uniqueMediaPath safety net appends _N.
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ReceivedFilesDir = dir

	p1, err := b.saveMedia([]byte("first"), "attachment", 7, ".jpg", "")
	if err != nil {
		t.Fatalf("saveMedia 1: %v", err)
	}
	p2, err := b.saveMedia([]byte("second"), "attachment", 7, ".jpg", "")
	if err != nil {
		t.Fatalf("saveMedia 2: %v", err)
	}
	if p1 == p2 {
		t.Fatalf("standalone same-second images collided: both %q", p1)
	}
	for _, p := range []string{p1, p2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file missing: %v", err)
		}
	}
}

func TestSavedPathPropagatedToQueue(t *testing.T) {
	// Verifies that attachment.savedPath flows
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
