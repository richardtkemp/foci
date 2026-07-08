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
		t.Errorf("plugin missing the transform hook:\n%s", src)
	}
	if !strings.Contains(src, "input.sessionID") {
		t.Errorf("plugin should read per-session file by input.sessionID:\n%s", src)
	}
}

func TestEnsureBlankSystemPlugin_EmptyWorkDir(t *testing.T) {
	// Must not panic or write anywhere for an empty workDir.
	EnsureBlankSystemPlugin("")
}

func TestBlankSystemPluginSource_ContainsSysDir(t *testing.T) {
	dir := "/tmp/foci/session-system-test"
	src := blankSystemPluginSource(dir)
	if !strings.Contains(src, dir) {
		t.Errorf("source missing templated sysDir %q:\n%s", dir, src)
	}
}
