package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
)

// writeTempMedia creates a small file to feed the media send helpers.
func writeTempMedia(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLastChatID(t *testing.T) {
	// Proves lastChatID errors before any message has been seen and returns
	// the recorded chat afterwards.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	if _, err := b.lastChatID(); err == nil {
		t.Error("expected error with no chat ID")
	}
	b.SetChatID(12345)
	id, err := b.lastChatID()
	if err != nil || id != 12345 {
		t.Errorf("lastChatID = %d/%v, want 12345/nil", id, err)
	}
}

func TestSendMediaToLastChat(t *testing.T) {
	// Proves the captioned send helpers (document, photo, video, audio,
	// animation, voice) resolve the last chat and stream the opened file to
	// the API, and fail cleanly with no chat or a missing file.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	path := writeTempMedia(t, "m.bin")

	// No chat yet: every helper must refuse.
	if err := b.SendAnimation(path, "cap"); err == nil {
		t.Error("SendAnimation without chat should error")
	}

	b.SetChatID(12345)
	sends := []struct {
		name string
		fn   func() error
	}{
		{"document", func() error { return b.SendDocument(path, "cap") }},
		{"photo", func() error { return b.SendPhoto(path, "cap") }},
		{"video", func() error { return b.SendVideo(path, "cap") }},
		{"audio", func() error { return b.SendAudio(path, "cap") }},
		{"animation", func() error { return b.SendAnimation(path, "cap") }},
		{"voice", func() error { return b.SendVoice(path) }},
	}
	for _, s := range sends {
		if err := s.fn(); err != nil {
			t.Errorf("Send %s: %v", s.name, err)
		}
	}
	if mock.docCount() != 1 {
		t.Errorf("documents = %d, want 1", mock.docCount())
	}

	// Missing file: open fails before any API call.
	if err := b.SendAnimation(filepath.Join(t.TempDir(), "missing.gif"), ""); err == nil {
		t.Error("SendAnimation with missing file should error")
	}
}

func TestSendVoiceData(t *testing.T) {
	// Proves voice bytes are sent to the last known chat, with an error when
	// no chat is known.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	if err := b.SendVoiceData([]byte("audio")); err == nil {
		t.Error("expected error with no chat")
	}
	b.SetChatID(12345)
	if err := b.SendVoiceData([]byte("audio")); err != nil {
		t.Errorf("SendVoiceData: %v", err)
	}
}

func TestOpenMediaFile(t *testing.T) {
	// Proves openMediaFile returns a reader-backed input file for an existing
	// path and a typed error mentioning the media type when the file is absent.
	path := writeTempMedia(t, "x.png")
	in, f, err := openMediaFile(path, "photo")
	if err != nil || in == nil || f == nil {
		t.Fatalf("openMediaFile = %v/%v/%v", in, f, err)
	}
	_ = f.Close()

	_, _, err = openMediaFile(filepath.Join(t.TempDir(), "gone.png"), "photo")
	if err == nil || !strings.Contains(err.Error(), "open photo file") {
		t.Errorf("err = %v, want open photo file error", err)
	}
}

func TestSendInjectedToChat_Header(t *testing.T) {
	// Proves the injected-message header is prepended when configured and
	// omitted for empty text (nothing is sent at all).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.display.InjectedMessageHeader = "[system]"

	if err := b.SendInjectedToChat(12345, "wake up"); err != nil {
		t.Fatalf("SendInjectedToChat: %v", err)
	}
	if !strings.Contains(mock.lastSendInjected, "[system]") || !strings.Contains(mock.lastSendInjected, "wake up") {
		t.Errorf("sent = %q, want header + text", mock.lastSendInjected)
	}

	if err := b.SendInjectedToChat(12345, "   "); err != nil {
		t.Fatalf("whitespace injected: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1 (whitespace dropped)", mock.sentCount())
	}
}
