package telegram

import (
	"fmt"
	"strings"
	"testing"

	"foci/internal/command"
)

// TestSendMedia_CaptionRendersMarkdown proves a short caption is converted to
// Telegram HTML and sent with ParseMode "HTML", inline on the document (no
// follow-up message).
func TestSendMedia_CaptionRendersMarkdown(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)
	path := writeTempMedia(t, "m.bin")

	if err := b.SendDocument(path, "**bold** and `code`"); err != nil {
		t.Fatalf("SendDocument: %v", err)
	}

	if mock.lastDocOpts == nil {
		t.Fatal("no SendDocument opts captured")
	}
	if mock.lastDocOpts.ParseMode != "HTML" {
		t.Errorf("ParseMode = %q, want HTML", mock.lastDocOpts.ParseMode)
	}
	if !strings.Contains(mock.lastDocOpts.Caption, "<b>bold</b>") {
		t.Errorf("caption = %q, want rendered <b>bold</b>", mock.lastDocOpts.Caption)
	}
	if mock.docCount() != 1 {
		t.Errorf("docs = %d, want 1", mock.docCount())
	}
	if mock.sentCount() != 0 {
		t.Errorf("follow-up sends = %d, want 0 (caption fits)", mock.sentCount())
	}
}

// TestSendMedia_OverflowCaption proves an over-length caption is detached: the
// file is sent with no caption and the text follows as a separate message.
func TestSendMedia_OverflowCaption(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)
	path := writeTempMedia(t, "m.bin")

	long := strings.Repeat("x", 1100) // > 1024-char Telegram caption cap
	if err := b.SendDocument(path, long); err != nil {
		t.Fatalf("SendDocument: %v", err)
	}

	if mock.lastDocOpts == nil || mock.lastDocOpts.Caption != "" || mock.lastDocOpts.ParseMode != "" {
		t.Errorf("doc opts = %+v, want empty caption + no parse mode", mock.lastDocOpts)
	}
	if mock.docCount() != 1 {
		t.Errorf("docs = %d, want 1", mock.docCount())
	}
	if mock.sentCount() != 1 {
		t.Errorf("follow-up sends = %d, want 1 (overflow message)", mock.sentCount())
	}
	if !strings.Contains(mock.lastSendInjected, "xxxx") {
		t.Errorf("overflow message = %q, want the caption text", mock.lastSendInjected)
	}
}

// TestSendMedia_HTMLFallback proves that when the HTML caption send is rejected
// (malformed entities), the file is retried once with the raw caption and no
// parse mode so it still goes out.
func TestSendMedia_HTMLFallback(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)
	path := writeTempMedia(t, "m.bin")

	mock.docErr = fmt.Errorf("can't parse entities")
	mock.docErrOnce = true

	if err := b.SendDocument(path, "**bold**"); err != nil {
		t.Fatalf("SendDocument should recover via fallback: %v", err)
	}

	if mock.docCount() != 2 {
		t.Errorf("docs = %d, want 2 (HTML attempt + raw fallback)", mock.docCount())
	}
	if mock.lastDocOpts.ParseMode != "" {
		t.Errorf("fallback ParseMode = %q, want empty", mock.lastDocOpts.ParseMode)
	}
	if mock.lastDocOpts.Caption != "**bold**" {
		t.Errorf("fallback caption = %q, want raw **bold**", mock.lastDocOpts.Caption)
	}
}
