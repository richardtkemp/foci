package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// docSendRecorder is a minimal Sender stub that records which of
// SendDocument/SendDocumentToChat was called and can be told to fail.
type docSendRecorder struct {
	mockSender   // embed for the rest of the Sender methods
	err          error
	toChatID     int64
	calledToChat bool
	calledDoc    bool
}

func (d *docSendRecorder) SendDocument(filePath, caption string) error {
	d.calledDoc = true
	return d.err
}

func (d *docSendRecorder) SendDocumentToChat(chatID int64, filePath, caption string) error {
	d.calledToChat = true
	d.toChatID = chatID
	return d.err
}

func writeTempFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSendDocAndRemove_SuccessRemovesFile(t *testing.T) {
	path := writeTempFile(t)
	rec := &docSendRecorder{}
	if err := SendDocAndRemove(rec, 42, path, "cap"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.calledToChat || rec.toChatID != 42 {
		t.Errorf("expected SendDocumentToChat(42, ...), calledToChat=%v chatID=%d", rec.calledToChat, rec.toChatID)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should be removed after a successful send")
	}
}

func TestSendDocAndRemove_FailureStillRemovesFile(t *testing.T) {
	// A failed send must not leak the temp file any more than a successful
	// one keeps it around — this is the exact bug #1511 fixed in
	// internal/app/dispatch.go (the removal was missing entirely there).
	path := writeTempFile(t)
	wantErr := errors.New("send failed")
	rec := &docSendRecorder{err: wantErr}
	err := SendDocAndRemove(rec, 42, path, "cap")
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("file should be removed even after a failed send")
	}
}

func TestSendDocAndRemove_ZeroChatIDUsesDefaultSend(t *testing.T) {
	path := writeTempFile(t)
	rec := &docSendRecorder{}
	if err := SendDocAndRemove(rec, 0, path, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.calledDoc || rec.calledToChat {
		t.Errorf("expected SendDocument (default chat), calledDoc=%v calledToChat=%v", rec.calledDoc, rec.calledToChat)
	}
}

func TestSendDocAndRemove_EmptyPathIsNoop(t *testing.T) {
	rec := &docSendRecorder{}
	if err := SendDocAndRemove(rec, 42, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.calledDoc || rec.calledToChat {
		t.Error("expected no send for an empty path")
	}
}

func TestSendDocAndRemove_NilSenderStillRemovesFile(t *testing.T) {
	// Mirrors cmd/foci-gw/http_handlers.go's POST /command site: no live
	// connection for the session must still remove the temp file, not just
	// skip the send.
	path := writeTempFile(t)
	if err := SendDocAndRemove(nil, 0, path, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should be removed even with a nil sender")
	}
}
