package discord

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSendMediaFile_CaptionFits proves a short caption rides along as the
// message Content on the file send, with no follow-up message.
func TestSendMediaFile_CaptionFits(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.SendDocument(path, "**bold** caption"); err != nil {
		t.Fatal(err)
	}
	if fs.sendCount() != 1 {
		t.Fatalf("sends = %d, want 1 (caption inline)", fs.sendCount())
	}
	got := fs.lastSend(t)
	if got.content != "**bold** caption" {
		t.Errorf("content = %q, want raw markdown (Discord renders natively)", got.content)
	}
	if len(got.fileNames) != 1 {
		t.Errorf("files = %v, want the attachment", got.fileNames)
	}
}

// TestSendMediaFile_Overflow proves an over-length caption (> 2000 chars) is
// detached: the file is sent with empty content, then the caption follows as a
// separate chunked message.
func TestSendMediaFile_Overflow(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2200 chars > 2000-char cap: the caption detaches and chunks into 2200 =
	// 2000 + 200, so the file send is followed by two text chunks (3 sends).
	long := strings.Repeat("y", 2200)
	if err := b.SendDocument(path, long); err != nil {
		t.Fatal(err)
	}

	if fs.sendCount() != 3 {
		t.Fatalf("sends = %d, want 3 (file + 2 overflow chunks)", fs.sendCount())
	}
	// First send carries the file with no caption.
	first := fs.sends[0]
	if first.content != "" || len(first.fileNames) != 1 {
		t.Errorf("first send = %+v, want empty content + 1 file", first)
	}
	// Follow-ups carry the text, no file, and reassemble to the caption.
	var reassembled string
	for _, s := range fs.sends[1:] {
		if len(s.fileNames) != 0 {
			t.Errorf("follow-up send carried a file: %+v", s)
		}
		reassembled += s.content
	}
	if reassembled != long {
		t.Errorf("overflow chunks reassembled to %d chars, want %d", len(reassembled), len(long))
	}
}
