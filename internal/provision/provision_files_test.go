package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateSoulFile(t *testing.T) {
	// Verifies placeholder substitution in SOUL.md files.
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("- **Name:** <!-- your name -->\n"), 0644)

	if err := templateSoulFile(soulPath, "Greek Tutor", 0640); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(soulPath)
	if !strings.Contains(string(data), "**Name:** Greek Tutor") {
		t.Errorf("name not substituted: %s", data)
	}
}

func TestTemplateSoulFileMissing(t *testing.T) {
	// Verifies templateSoulFile silently skips missing files.
	if err := templateSoulFile(filepath.Join(t.TempDir(), "nope.md"), "Name", 0640); err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}
}

func TestTemplateSoulFileEmpty(t *testing.T) {
	// Verifies empty display names don't modify the file.
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("- **Name:** <!-- your name -->\n"), 0644)

	// Empty display name should not modify
	if err := templateSoulFile(soulPath, "", 0640); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(soulPath)
	if !strings.Contains(string(data), "<!-- your name -->") {
		t.Errorf("empty name should leave placeholder: %s", data)
	}
}

func TestCopyDir(t *testing.T) {
	// Verifies copying all files (not dirs) from source to destination.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.md"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(src, "b.md"), []byte("bbb"), 0644)
	os.MkdirAll(filepath.Join(src, "subdir"), 0755) // should be skipped

	dst := filepath.Join(t.TempDir(), "target")
	if err := copyDir(src, dst, 0640); err != nil {
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

func TestCopyDirReadError(t *testing.T) {
	// Tests copyDir when source doesn't exist.
	err := copyDir("/nonexistent/source", filepath.Join(t.TempDir(), "dst"), 0640)
	if err == nil {
		t.Error("expected error when source doesn't exist")
	}
}

func TestCopyDirMkdirError(t *testing.T) {
	// Tests copyDir when destination can't be created.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("content"), 0644)

	// Try to create destination under a file (will fail)
	dst := filepath.Join(src, "file.txt", "subdir")
	err := copyDir(src, dst, 0640)
	if err == nil {
		t.Error("expected error when creating destination fails")
	}
}

func TestCopyDirCopyFileError(t *testing.T) {
	// Tests copyDir when a file within the source dir can't be copied.
	src := t.TempDir()
	// Create an unreadable file
	unreadable := filepath.Join(src, "secret.md")
	os.WriteFile(unreadable, []byte("secret"), 0644)
	os.Chmod(unreadable, 0000)
	t.Cleanup(func() { os.Chmod(unreadable, 0644) })

	dst := filepath.Join(t.TempDir(), "target")
	err := copyDir(src, dst, 0640)
	if err == nil {
		t.Error("expected error when source file is unreadable")
	}
}

func TestCopyFileReadError(t *testing.T) {
	// Tests copyFile when source can't be read.
	err := copyFile("/nonexistent/source.txt", filepath.Join(t.TempDir(), "dst.txt"), 0640)
	if err == nil {
		t.Error("expected error when source doesn't exist")
	}
}

func TestCopyFileCreateError(t *testing.T) {
	// Tests copyFile when the destination can't be created.
	src := filepath.Join(t.TempDir(), "src.txt")
	os.WriteFile(src, []byte("data"), 0644)

	// Destination is inside a file (can't create)
	dst := filepath.Join(src, "nested", "dst.txt")
	err := copyFile(src, dst, 0640)
	if err == nil {
		t.Error("expected error when destination can't be created")
	}
}

func TestTemplateSoulFileReadError(t *testing.T) {
	// Tests templateSoulFile for non-NotExist read failures.
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("content"), 0644)
	os.Chmod(soulPath, 0000)
	t.Cleanup(func() { os.Chmod(soulPath, 0644) })

	err := templateSoulFile(soulPath, "Name", 0640)
	if err == nil {
		t.Error("expected error for unreadable SOUL.md")
	}
}

func TestCopyDefaultFiles(t *testing.T) {
	// Tests copyDefaultFiles copies character files to workspace.
	tmpDir := t.TempDir()
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "character", "SOUL.md"), []byte("soul"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "character", "CRAFT.md"), []byte("craft"), 0644)

	workspace := filepath.Join(tmpDir, "workspace")
	os.MkdirAll(filepath.Join(workspace, "character"), 0755)

	if err := copyDefaultFiles(defaultsDir, workspace, 0640); err != nil {
		t.Fatalf("copyDefaultFiles: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(workspace, "character", "SOUL.md"))
	if string(data) != "soul" {
		t.Errorf("SOUL.md = %q, want soul", data)
	}
	data, _ = os.ReadFile(filepath.Join(workspace, "character", "CRAFT.md"))
	if string(data) != "craft" {
		t.Errorf("CRAFT.md = %q, want craft", data)
	}
}

func TestCopyDefaultFilesNoDefaults(t *testing.T) {
	// Tests copyDefaultFiles with missing defaults dir.
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(filepath.Join(workspace, "character"), 0755)
	os.MkdirAll(filepath.Join(workspace, "prompts"), 0755)

	// Copy from nonexistent defaults dir should fail
	err := copyDefaultFiles("/nonexistent/defaults", workspace, 0640)
	if err == nil {
		t.Error("expected error when defaults dir doesn't exist")
	}
}
