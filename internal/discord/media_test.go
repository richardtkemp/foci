package discord

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestDownloadURL verifies a successful download returns the body and a 4xx
// status fails immediately without retries.
func TestDownloadURL(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	data, err := downloadURL(srv.URL + "/file")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Errorf("got %q", data)
	}

	hits = 0
	if _, err := downloadURL(srv.URL + "/missing"); err == nil {
		t.Error("expected error for 404")
	}
	if hits != 1 {
		t.Errorf("4xx should not retry, got %d attempts", hits)
	}
}

// TestDownloadAttachment verifies attachment download via ProxyURL, MIME
// normalization, and saving to the configured received-files dir with the
// configured file mode.
func TestDownloadAttachment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("image-bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	b, _, _ := newTestBot(t, "a")
	b.display.ReceivedFilesDir = dir
	b.fileMode = 0o600

	att, ok := b.downloadAttachment(&discordgo.MessageAttachment{
		ProxyURL:    srv.URL + "/img",
		ContentType: "image/png",
		Filename:    "shot.png",
	})
	if !ok {
		t.Fatal("expected download success")
	}
	if string(att.data) != "image-bytes" || att.mediaType != "image/png" {
		t.Errorf("unexpected attachment %+v", att)
	}
	if att.savedPath == "" {
		t.Fatal("expected file saved")
	}
	info, err := os.Stat(att.savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected mode 0600, got %v", info.Mode().Perm())
	}
	saved, _ := os.ReadFile(att.savedPath)
	if string(saved) != "image-bytes" {
		t.Error("saved content mismatch")
	}
}

// TestDownloadAttachmentNoSaveDir verifies attachments are kept in memory only
// when no received-files dir is configured.
func TestDownloadAttachmentNoSaveDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("%PDF-1.4 fake"))
	}))
	defer srv.Close()

	b, _, _ := newTestBot(t, "a")
	att, ok := b.downloadAttachment(&discordgo.MessageAttachment{URL: srv.URL + "/doc.pdf"})
	if !ok {
		t.Fatal("expected success")
	}
	if att.savedPath != "" {
		t.Error("expected no saved path without configured dir")
	}
	if att.mediaType == "" {
		t.Error("expected MIME sniffed from content")
	}
}

// TestSaveMediaDefaultMode verifies saveMedia falls back to 0640 when no file
// mode is configured.
func TestSaveMediaDefaultMode(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.display.ReceivedFilesDir = t.TempDir()

	path, err := b.saveMedia([]byte("x"), "attachment", ".bin")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("expected default mode 0640, got %v", info.Mode().Perm())
	}
}

// TestExtForMediaType verifies the MIME-to-extension mapping including the
// fallback.
func TestExtForMediaType(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"application/pdf", ".pdf"},
		{"application/octet-stream", ".bin"},
	}
	for _, tt := range tests {
		if got := extForMediaType(tt.mime); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.mime, got, tt.want)
		}
	}
}
