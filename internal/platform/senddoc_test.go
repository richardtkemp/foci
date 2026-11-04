package platform

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/log"
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
	// No live connection for the session must still remove the temp file, not
	// just skip the send, and must not read as a delivery.
	path := writeTempFile(t)
	if err := SendDocAndRemove(nil, 0, path, ""); !errors.Is(err, ErrNoSender) {
		t.Fatalf("err = %v, want ErrNoSender", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should be removed even with a nil sender")
	}
}

func TestSendDocAndRemove_LogsUndelivered(t *testing.T) {
	// Most callers discard the returned error, so the helper itself must leave
	// a line when a document is lost: the file is gone once it returns.
	for name, send := range map[string]func(path string) error{
		"failed send": func(path string) error {
			return SendDocAndRemove(&docSendRecorder{err: errors.New("upload refused")}, 42, path, "")
		},
		"nil sender": func(path string) error { return SendDocAndRemove(nil, 42, path, "") },
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			path := writeTempFile(t)
			if err := send(path); err == nil {
				t.Fatal("want an error")
			}
			out := buf.String()
			if !strings.Contains(out, "WARN") || !strings.Contains(out, "chat 42") {
				t.Errorf("want a WARN naming chat 42, got: %q", out)
			}
		})
	}
}

func TestSendDocAndRemove_DeliveredAndNoopDoNotLog(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if err := SendDocAndRemove(&docSendRecorder{}, 42, writeTempFile(t), ""); err != nil {
		t.Fatal(err)
	}
	if err := SendDocAndRemove(nil, 42, "", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("no document was lost, got: %q", buf.String())
	}
}
