package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSessionEnvFile_CreatesMapping(t *testing.T) {
	sessionID := "sess-test-write-123"
	env := map[string]string{
		"FOCI_SOCK": "/tmp/foci/exec-test.sock",
		"BASH_ENV":  "/tmp/foci/exec-test-funcs.sh",
		"OTHER":     "ignored",
	}
	t.Cleanup(func() { RemoveSessionEnvFile(sessionID) })

	WriteSessionEnvFile(sessionID, env)

	data, err := os.ReadFile(sessionEnvPath(sessionID))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var entry sessionEnvEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if entry.FociSock != env["FOCI_SOCK"] {
		t.Errorf("FOCI_SOCK = %q, want %q", entry.FociSock, env["FOCI_SOCK"])
	}
	if entry.BashEnv != env["BASH_ENV"] {
		t.Errorf("BASH_ENV = %q, want %q", entry.BashEnv, env["BASH_ENV"])
	}
}

func TestWriteSessionEnvFile_SkipsEmpty(t *testing.T) {
	sessionID := "sess-no-bridge-test"
	t.Cleanup(func() { RemoveSessionEnvFile(sessionID) })

	WriteSessionEnvFile(sessionID, map[string]string{"HOME": "/tmp"})

	if _, err := os.Stat(sessionEnvPath(sessionID)); !os.IsNotExist(err) {
		t.Error("expected no file when FOCI_SOCK/BASH_ENV are absent")
	}
}

func TestWriteSessionEnvFile_SkipsEmptySessionID(t *testing.T) {
	WriteSessionEnvFile("", map[string]string{"FOCI_SOCK": "/tmp/x.sock"})
}

func TestRemoveSessionEnvFile_DeletesMapping(t *testing.T) {
	sessionID := "sess-remove-test"
	env := map[string]string{"FOCI_SOCK": "/tmp/x.sock", "BASH_ENV": "/tmp/x.sh"}
	WriteSessionEnvFile(sessionID, env)

	if _, err := os.Stat(sessionEnvPath(sessionID)); err != nil {
		t.Fatalf("file should exist after write: %v", err)
	}

	RemoveSessionEnvFile(sessionID)

	if _, err := os.Stat(sessionEnvPath(sessionID)); !os.IsNotExist(err) {
		t.Error("expected file removed after RemoveSessionEnvFile")
	}
}

func TestRemoveSessionEnvFile_NoOpOnMissing(t *testing.T) {
	RemoveSessionEnvFile("never-existed-test")
}

func TestEnsureSessionEnvPlugin_WritesPlugin(t *testing.T) {
	workDir := t.TempDir()

	EnsureSessionEnvPlugin(workDir)

	path := filepath.Join(workDir, ".opencode", "plugin", sessionEnvPluginFn)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plugin file not written: %v", err)
	}
	src := string(data)

	if !strings.Contains(src, `"shell.env"`) {
		t.Error("plugin source must hook shell.env")
	}
	if !strings.Contains(src, "Bun.file") {
		t.Error("plugin source must use Bun.file for reads")
	}
	if !strings.Contains(src, "export default function") {
		t.Error("plugin source must have default export")
	}
}

func TestEnsureSessionEnvPlugin_Idempotent(t *testing.T) {
	workDir := t.TempDir()

	EnsureSessionEnvPlugin(workDir)
	path := filepath.Join(workDir, ".opencode", "plugin", sessionEnvPluginFn)
	firstStat, _ := os.Stat(path)

	// Second call should skip the write (content unchanged).
	EnsureSessionEnvPlugin(workDir)
	secondStat, _ := os.Stat(path)

	if !firstStat.ModTime().Equal(secondStat.ModTime()) {
		t.Error("expected mtime unchanged on idempotent re-write")
	}
}

func TestEnsureSessionEnvPlugin_EmptyWorkDir(t *testing.T) {
	// Should not panic or write anything.
	EnsureSessionEnvPlugin("")
}

func TestSessionEnvPluginSource_ContainsEnvDir(t *testing.T) {
	dir := "/some/known/path"
	src := sessionEnvPluginSource(dir)
	if !strings.Contains(src, dir) {
		t.Errorf("plugin source must contain env dir %q", dir)
	}
}
