package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTemplateSoulFile verifies placeholder substitution in SOUL.md files.
func TestTemplateSoulFile(t *testing.T) {
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("- **Name:** <!-- your name -->\n"), 0644)

	if err := templateSoulFile(soulPath, "Greek Tutor"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(soulPath)
	if !strings.Contains(string(data), "**Name:** Greek Tutor") {
		t.Errorf("name not substituted: %s", data)
	}
}

// TestTemplateSoulFileMissing verifies templateSoulFile silently skips missing files.
func TestTemplateSoulFileMissing(t *testing.T) {
	if err := templateSoulFile(filepath.Join(t.TempDir(), "nope.md"), "Name"); err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}
}

// TestTemplateSoulFileEmpty verifies empty display names don't modify the file.
func TestTemplateSoulFileEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("- **Name:** <!-- your name -->\n"), 0644)

	// Empty display name should not modify
	if err := templateSoulFile(soulPath, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(soulPath)
	if !strings.Contains(string(data), "<!-- your name -->") {
		t.Errorf("empty name should leave placeholder: %s", data)
	}
}

// TestCopyDir verifies copying all files (not dirs) from source to destination.
func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.md"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(src, "b.md"), []byte("bbb"), 0644)
	os.MkdirAll(filepath.Join(src, "subdir"), 0755) // should be skipped

	dst := filepath.Join(t.TempDir(), "target")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "a.md"))
	if string(data) != "aaa" {
		t.Errorf("a.md = %q", data)
	}
	data, _ = os.ReadFile(filepath.Join(dst, "b.md"))
	if string(data) != "bbb" {
		t.Errorf("b.md = %q", data)
	}
}

// TestCopyDirReadError tests copyDir when source doesn't exist.
func TestCopyDirReadError(t *testing.T) {
	err := copyDir("/nonexistent/source", filepath.Join(t.TempDir(), "dst"))
	if err == nil {
		t.Error("expected error when source doesn't exist")
	}
}

// TestCopyDirMkdirError tests copyDir when destination can't be created.
func TestCopyDirMkdirError(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("content"), 0644)

	// Try to create destination under a file (will fail)
	dst := filepath.Join(src, "file.txt", "subdir")
	err := copyDir(src, dst)
	if err == nil {
		t.Error("expected error when creating destination fails")
	}
}

// TestCopyDirCopyFileError tests copyDir when a file within the source dir can't be copied.
func TestCopyDirCopyFileError(t *testing.T) {
	src := t.TempDir()
	// Create an unreadable file
	unreadable := filepath.Join(src, "secret.md")
	os.WriteFile(unreadable, []byte("secret"), 0644)
	os.Chmod(unreadable, 0000)
	t.Cleanup(func() { os.Chmod(unreadable, 0644) })

	dst := filepath.Join(t.TempDir(), "target")
	err := copyDir(src, dst)
	if err == nil {
		t.Error("expected error when source file is unreadable")
	}
}

// TestCopyFileReadError tests copyFile when source can't be read.
func TestCopyFileReadError(t *testing.T) {
	err := copyFile("/nonexistent/source.txt", filepath.Join(t.TempDir(), "dst.txt"))
	if err == nil {
		t.Error("expected error when source doesn't exist")
	}
}

// TestCopyFileCreateError tests copyFile when the destination can't be created.
func TestCopyFileCreateError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	os.WriteFile(src, []byte("data"), 0644)

	// Destination is inside a file (can't create)
	dst := filepath.Join(src, "nested", "dst.txt")
	err := copyFile(src, dst)
	if err == nil {
		t.Error("expected error when destination can't be created")
	}
}

// TestTemplateSoulFileReadError tests templateSoulFile for non-NotExist read failures.
func TestTemplateSoulFileReadError(t *testing.T) {
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("content"), 0644)
	os.Chmod(soulPath, 0000)
	t.Cleanup(func() { os.Chmod(soulPath, 0644) })

	err := templateSoulFile(soulPath, "Name")
	if err == nil {
		t.Error("expected error for unreadable SOUL.md")
	}
}

// TestCopyDefaultFilesWithKeepalive tests copyDefaultFiles with keepalive file.
func TestCopyDefaultFilesWithKeepalive(t *testing.T) {
	tmpDir := t.TempDir()
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	os.MkdirAll(filepath.Join(defaultsDir, "prompts"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "character", "SOUL.md"), []byte("soul"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "prompts", "KEEPALIVE.md"), []byte("keepalive"), 0644)

	workspace := filepath.Join(tmpDir, "workspace")
	os.MkdirAll(filepath.Join(workspace, "character"), 0755)
	os.MkdirAll(filepath.Join(workspace, "prompts"), 0755)

	if err := copyDefaultFiles(defaultsDir, workspace); err != nil {
		t.Fatalf("copyDefaultFiles: %v", err)
	}

	// Verify KEEPALIVE.md was copied
	data, _ := os.ReadFile(filepath.Join(workspace, "prompts", "KEEPALIVE.md"))
	if string(data) != "keepalive" {
		t.Errorf("KEEPALIVE.md = %q, want keepalive", data)
	}
}

// TestCopyDefaultFilesNoKeepalive verifies copyDefaultFiles returns nil when no KEEPALIVE.md exists.
func TestCopyDefaultFilesNoKeepalive(t *testing.T) {
	tmpDir := t.TempDir()
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "character", "SOUL.md"), []byte("soul"), 0644)
	// No prompts/KEEPALIVE.md

	workspace := filepath.Join(tmpDir, "workspace")
	os.MkdirAll(filepath.Join(workspace, "character"), 0755)
	os.MkdirAll(filepath.Join(workspace, "prompts"), 0755)

	if err := copyDefaultFiles(defaultsDir, workspace); err != nil {
		t.Fatalf("copyDefaultFiles: %v", err)
	}

	// KEEPALIVE.md should not exist in workspace
	if _, err := os.Stat(filepath.Join(workspace, "prompts", "KEEPALIVE.md")); !os.IsNotExist(err) {
		t.Errorf("expected no KEEPALIVE.md, but it exists or stat returned unexpected error: %v", err)
	}
}

// TestCopyDefaultFilesNoDefaults tests copyDefaultFiles with missing defaults dir.
func TestCopyDefaultFilesNoDefaults(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(filepath.Join(workspace, "character"), 0755)
	os.MkdirAll(filepath.Join(workspace, "prompts"), 0755)

	// Copy from nonexistent defaults dir should fail
	err := copyDefaultFiles("/nonexistent/defaults", workspace)
	if err == nil {
		t.Error("expected error when defaults dir doesn't exist")
	}
}
