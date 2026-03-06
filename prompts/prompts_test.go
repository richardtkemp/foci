package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Documents that read() panics on missing embedded files. This path is
// unreachable in production (all callers use hardcoded literals validated by
// TestEmbeddedFilesLoadNonEmpty) but exists as a developer safety net.
func TestReadPanicsOnMissingFile(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for missing embedded file")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "missing embedded file") {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()
	read("nonexistent-file.md")
}

func TestEmbeddedFilesLoadNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		fn   func() string
	}{
		{"BranchOrientationHeadless", BranchOrientationHeadless},
		{"BranchOrientationMultiball", BranchOrientationMultiball},
		{"CompactionSummary", CompactionSummary},
		{"CompactionHandoff", CompactionHandoff},
		{"Keepalive", Keepalive},
		{"Background", Background},
		{"MemoryFormation", MemoryFormation},
		{"MemoryConsolidation", MemoryConsolidation},
		{"FirstRun", FirstRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn()
			if got == "" {
				t.Errorf("%s() returned empty string", tt.name)
			}
		})
	}
}

func TestBranchOrientationHeadlessHasVars(t *testing.T) {
	text := BranchOrientationHeadless()
	for _, v := range []string{"{branch_type}", "{branch_key}", "{parent_key}"} {
		if !strings.Contains(text, v) {
			t.Errorf("headless orientation missing template var %s", v)
		}
	}
}

func TestBranchOrientationMultiballHasVars(t *testing.T) {
	text := BranchOrientationMultiball()
	for _, v := range []string{"{branch_type}", "{branch_key}", "{parent_key}"} {
		if !strings.Contains(text, v) {
			t.Errorf("multiball orientation missing template var %s", v)
		}
	}
}

func TestReplaceVars(t *testing.T) {
	text := "Hello {name}, you are {role}."
	got := ReplaceVars(text, map[string]string{
		"name": "Alice",
		"role": "admin",
	})
	want := "Hello Alice, you are admin."
	if got != want {
		t.Errorf("ReplaceVars = %q, want %q", got, want)
	}
}

func TestReplaceVarsPartial(t *testing.T) {
	text := "Key: {branch_key}, Type: {branch_type}"
	got := ReplaceVars(text, map[string]string{
		"branch_key": "abc123",
	})
	want := "Key: abc123, Type: {branch_type}"
	if got != want {
		t.Errorf("ReplaceVars partial = %q, want %q", got, want)
	}
}

func TestReplaceVarsEmpty(t *testing.T) {
	text := "No vars here."
	got := ReplaceVars(text, nil)
	if got != text {
		t.Errorf("ReplaceVars with nil = %q, want %q", got, text)
	}
}

func TestResolvePromptEmptyPath(t *testing.T) {
	got := ResolvePrompt("", "test", "embedded-default")
	if got != "embedded-default" {
		t.Errorf("empty path: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptDefaultKeyword(t *testing.T) {
	got := ResolvePrompt("default", "test", "embedded-default")
	if got != "embedded-default" {
		t.Errorf("default keyword: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptNoneKeyword(t *testing.T) {
	got := ResolvePrompt("none", "test", "embedded-default")
	if got != "" {
		t.Errorf("none keyword: got %q, want empty", got)
	}
}

func TestResolvePromptFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.md")
	os.WriteFile(path, []byte("  custom content  "), 0644)

	got := ResolvePrompt(path, "test", "embedded-default")
	if got != "custom content" {
		t.Errorf("file exists: got %q, want %q", got, "custom content")
	}
}

func TestResolvePromptFileMissing(t *testing.T) {
	got := ResolvePrompt("/nonexistent/path/prompt.md", "test", "embedded-default")
	if got != "embedded-default" {
		t.Errorf("file missing: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptSearchDirsFirst(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("from-dir"), 0644)

	got := ResolvePrompt("", "prompt.md", "embedded-default", dir)
	if got != "from-dir" {
		t.Errorf("search dirs: got %q, want %q", got, "from-dir")
	}
}

func TestResolvePromptSearchDirsPriority(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "prompt.md"), []byte("from-dir1"), 0644)
	os.WriteFile(filepath.Join(dir2, "prompt.md"), []byte("from-dir2"), 0644)

	got := ResolvePrompt("", "prompt.md", "embedded-default", dir1, dir2)
	if got != "from-dir1" {
		t.Errorf("search dir priority: got %q, want %q", got, "from-dir1")
	}
}

func TestResolvePromptSearchDirsFallthrough(t *testing.T) {
	dir := t.TempDir() // empty dir, no prompt.md
	got := ResolvePrompt("", "prompt.md", "embedded-default", dir)
	if got != "embedded-default" {
		t.Errorf("search dir fallthrough: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptExplicitPathOverridesSearchDirs(t *testing.T) {
	searchDir := t.TempDir()
	os.WriteFile(filepath.Join(searchDir, "prompt.md"), []byte("from-dir"), 0644)

	fileDir := t.TempDir()
	path := filepath.Join(fileDir, "explicit.md")
	os.WriteFile(path, []byte("explicit-content"), 0644)

	got := ResolvePrompt(path, "prompt.md", "embedded-default", searchDir)
	if got != "explicit-content" {
		t.Errorf("explicit path: got %q, want %q", got, "explicit-content")
	}
}
