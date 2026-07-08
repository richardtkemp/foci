package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSessionSystemFile_CreatesFile(t *testing.T) {
	sessionID := "sess-sys-write-123"
	prompt := "You are scout. Some character prompt.\nMultiple lines."
	t.Cleanup(func() { RemoveSessionSystemFile(sessionID) })

	WriteSessionSystemFile(sessionID, prompt)

	data, err := os.ReadFile(sessionSystemPath(sessionID))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != prompt {
		t.Errorf("file content = %q, want %q", string(data), prompt)
	}
}

func TestWriteSessionSystemFile_SkipsEmpty(t *testing.T) {
	t.Cleanup(func() {
		RemoveSessionSystemFile("sess-sys-empty-prompt")
		RemoveSessionSystemFile("")
	})

	// Empty prompt → no file.
	WriteSessionSystemFile("sess-sys-empty-prompt", "")
	if _, err := os.Stat(sessionSystemPath("sess-sys-empty-prompt")); !os.IsNotExist(err) {
		t.Errorf("expected no file for empty prompt, got err=%v", err)
	}
	// Empty sessionID → no write, no panic.
	WriteSessionSystemFile("", "some prompt")
}

func TestRemoveSessionSystemFile_Deletes(t *testing.T) {
	sessionID := "sess-sys-remove-123"
	WriteSessionSystemFile(sessionID, "prompt")
	if _, err := os.Stat(sessionSystemPath(sessionID)); err != nil {
		t.Fatalf("setup: file not written: %v", err)
	}
	RemoveSessionSystemFile(sessionID)
	if _, err := os.Stat(sessionSystemPath(sessionID)); !os.IsNotExist(err) {
		t.Errorf("file still present after remove: err=%v", err)
	}
}

func TestEnsureBlankSystemPlugin_WritesPlugin(t *testing.T) {
	workDir := t.TempDir()
	EnsureBlankSystemPlugin(workDir)

	path := filepath.Join(workDir, ".opencode", "plugin", blankSystemPluginFn)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "experimental.chat.system.transform") {
		t.Errorf("plugin missing the system.transform hook:\n%s", src)
	}
	if !strings.Contains(src, "experimental.session.compacting") {
		t.Errorf("plugin missing the session.compacting hook:\n%s", src)
	}
	if !strings.Contains(src, "input.sessionID") {
		t.Errorf("plugin should read per-session files by input.sessionID:\n%s", src)
	}
}

func TestEnsureBlankSystemPlugin_EmptyWorkDir(t *testing.T) {
	// Must not panic or write anywhere for an empty workDir.
	EnsureBlankSystemPlugin("")
}

func TestWriteSessionCompactFile_CreatesFile(t *testing.T) {
	sessionID := "sess-compact-write-123"
	prompt := "Summarize the conversation into the foci handoff format."
	t.Cleanup(func() { RemoveSessionCompactFile(sessionID) })

	WriteSessionCompactFile(sessionID, prompt)

	data, err := os.ReadFile(sessionCompactPath(sessionID))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != prompt {
		t.Errorf("file content = %q, want %q", string(data), prompt)
	}
}

func TestRemoveSessionCompactFile_Deletes(t *testing.T) {
	sessionID := "sess-compact-remove-123"
	WriteSessionCompactFile(sessionID, "prompt")
	RemoveSessionCompactFile(sessionID)
	if _, err := os.Stat(sessionCompactPath(sessionID)); !os.IsNotExist(err) {
		t.Errorf("file still present after remove: err=%v", err)
	}
}

func TestBlankSystemPluginSource_ContainsDirs(t *testing.T) {
	sysDir := "/tmp/foci/session-system-test"
	compactDir := "/tmp/foci/session-compact-test"
	src := blankSystemPluginSource(sysDir, compactDir)
	if !strings.Contains(src, sysDir) {
		t.Errorf("source missing templated sysDir %q:\n%s", sysDir, src)
	}
	if !strings.Contains(src, compactDir) {
		t.Errorf("source missing templated compactDir %q:\n%s", compactDir, src)
	}
}
