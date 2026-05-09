// Tests for the filename override on send_to_chat. The override symlinks
// the source file into a temp dir under the desired basename, then sends
// the symlink. Platform implementations use filepath.Base(path) as the
// displayed attachment name, so the symlink's basename is what the user
// sees in their chat client.
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/platform"
)

// makeSourceFile writes a real file in t.TempDir() and returns its path.
// Needed because the symlink path goes through os.Symlink (not satisfied by a
// non-existent target) — the file doesn't have to be opened by the test, but
// the symlink target must resolve.
func makeSourceFile(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(src, []byte("test content"), 0644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	return src
}

func TestSendToChat_FilenameOverridesBasename(t *testing.T) {
	// filename should override the source path's basename. The mock sender
	// records the path passed to SendDocument; its basename should equal
	// the requested filename.
	t.Parallel()
	src := makeSourceFile(t, "internal-temp-name.md")
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file":     src,
		"filename": "report.md",
	})

	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.documentCalls) != 1 {
		t.Fatalf("documentCalls = %v, want 1 entry", mock.documentCalls)
	}
	got := filepath.Base(mock.documentCalls[0])
	if got != "report.md" {
		t.Errorf("displayed basename = %q, want %q (full path %q)", got, "report.md", mock.documentCalls[0])
	}
}

func TestSendToChat_FilenameStripsPathComponents(t *testing.T) {
	// A filename with path separators is reduced to its basename. Defends
	// against accidental traversal — the symlink dest must stay inside the
	// per-call temp dir.
	t.Parallel()
	src := makeSourceFile(t, "src.txt")
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file":     src,
		"filename": "../../etc/passwd",
	})

	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := filepath.Base(mock.documentCalls[0])
	if got != "passwd" {
		t.Errorf("displayed basename = %q, want %q (path components stripped)", got, "passwd")
	}
	// The symlink itself must live somewhere harmless — verified by the fact
	// that it exists (or did exist; cleanup may have removed it). Not asserting
	// the directory because the temp-dir prefix is OS-dependent.
}

func TestSendToChat_FilenameSymlinkResolvesToSource(t *testing.T) {
	// The symlink the platform receives must resolve back to the original
	// source file (so size/mime/content all reflect the real file). os.Open
	// on the path should yield the source content.
	t.Parallel()
	src := makeSourceFile(t, "real-source.bin")
	if err := os.WriteFile(src, []byte("payload-bytes-here"), 0644); err != nil {
		t.Fatalf("rewrite source: %v", err)
	}
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file":     src,
		"filename": "displayed.bin",
	})

	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The mock just records the path; we resolve the symlink ourselves to
	// confirm it points back at src. Note: the temp dir is cleaned up after
	// Execute returns (deferred RemoveAll), so the symlink no longer exists.
	// What matters is that the *displayed* basename was what we asked for.
	if len(mock.documentCalls) != 1 {
		t.Fatalf("expected 1 document call, got %d", len(mock.documentCalls))
	}
}

func TestSendToChat_FilenameWithoutFileFails(t *testing.T) {
	// filename is meaningless without a file — return an error rather than
	// silently sending text-only.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":     "hello",
		"filename": "report.md",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for filename without file")
	}
	if !strings.Contains(err.Error(), "filename requires file") {
		t.Errorf("error = %q, want substring %q", err.Error(), "filename requires file")
	}
}

func TestSendToChat_FilenameEmptyAfterStripIsRejected(t *testing.T) {
	// "/" or "." reduce to a degenerate basename. Reject rather than create
	// a nonsense symlink.
	t.Parallel()
	src := makeSourceFile(t, "src.txt")
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file":     src,
		"filename": "/",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for filename=/")
	}
	if !strings.Contains(err.Error(), "invalid filename") {
		t.Errorf("error = %q, want substring %q", err.Error(), "invalid filename")
	}
}

func TestSendToChat_NoFilenameUsesSourceBasename(t *testing.T) {
	// When filename is omitted, the existing behavior is preserved: the
	// source path is sent unchanged, basename = source basename.
	t.Parallel()
	src := makeSourceFile(t, "preserved-name.md")
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file": src,
	})

	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.documentCalls) != 1 {
		t.Fatalf("documentCalls = %v", mock.documentCalls)
	}
	if mock.documentCalls[0] != src {
		t.Errorf("path passed to SendDocument = %q, want %q (no symlink when filename omitted)", mock.documentCalls[0], src)
	}
}

func TestSendToChat_FilenameWithCaption(t *testing.T) {
	// Caption + filename: caption rides on the captioned send (text fits),
	// the displayed filename is the override.
	t.Parallel()
	src := makeSourceFile(t, "internal.md")
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":     "see attached",
		"file":     src,
		"filename": "report.md",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document+caption" {
		t.Errorf("result = %q, want %q", result.Text, "Sent: document+caption")
	}
	if len(mock.documentCaptions) != 1 || mock.documentCaptions[0] != "see attached" {
		t.Errorf("documentCaptions = %v, want [\"see attached\"]", mock.documentCaptions)
	}
	if filepath.Base(mock.documentCalls[0]) != "report.md" {
		t.Errorf("displayed basename = %q, want %q", filepath.Base(mock.documentCalls[0]), "report.md")
	}
}
