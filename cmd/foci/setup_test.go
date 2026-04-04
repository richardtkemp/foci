package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provision"

	_ "foci/internal/discord"  // register provider for SetupProviders
	_ "foci/internal/telegram" // register provider for SetupProviders
)

// Verifies parseSetupFlags correctly parses all supported flags.
func TestParseSetupFlags(t *testing.T) {
	args := []string{
		"--config-dir", "/home/foci/config",
		"--non-interactive",
		"--telegram-bot-token", "123:ABC-test",
		"--telegram-user-id", "12345678",
		"--agent-id", "fotini",
		"--display-name", "Fotini",
		"--model", "opus",
		"--provider", "anthropic",
		"--api-key", "sk-ant-test123",
		"--char-mode", "import",
		"--char-import-dir", "/home/foci/old-agent/character",
		"--memory-import-dir", "/home/foci/old-agent/memory",
	}

	f := parseSetupFlags(args)

	if f.configDir != "/home/foci/config" {
		t.Errorf("configDir = %q, want /home/foci/config", f.configDir)
	}
	if !f.nonInteractive {
		t.Error("nonInteractive should be true")
	}
	if f.providerFlags["telegram-bot-token"] != "123:ABC-test" {
		t.Errorf("providerFlags[telegram-bot-token] = %q, want 123:ABC-test", f.providerFlags["telegram-bot-token"])
	}
	if f.providerFlags["telegram-user-id"] != "12345678" {
		t.Errorf("providerFlags[telegram-user-id] = %q, want 12345678", f.providerFlags["telegram-user-id"])
	}
	if f.agentID != "fotini" {
		t.Errorf("agentID = %q, want fotini", f.agentID)
	}
	if f.displayName != "Fotini" {
		t.Errorf("displayName = %q, want Fotini", f.displayName)
	}
	if f.model != "opus" {
		t.Errorf("model = %q, want opus", f.model)
	}
	if f.provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", f.provider)
	}
	if f.apiKey != "sk-ant-test123" {
		t.Errorf("apiKey = %q, want sk-ant-test123", f.apiKey)
	}
	if f.charMode != "import" {
		t.Errorf("charMode = %q, want import", f.charMode)
	}
	if f.charImportDir != "/home/foci/old-agent/character" {
		t.Errorf("charImportDir = %q, want /home/foci/old-agent/character", f.charImportDir)
	}
	if f.memoryImportDir != "/home/foci/old-agent/memory" {
		t.Errorf("memoryImportDir = %q, want /home/foci/old-agent/memory", f.memoryImportDir)
	}
}

// Verifies parseSetupFlags correctly parses namespaced Discord flags.
func TestParseSetupFlagsDiscord(t *testing.T) {
	args := []string{
		"--non-interactive",
		"--discord-bot-token", "NTk5MTIz.Xh4abc.abcdefghijklmnopqrstuvwxyz012",
		"--discord-user-id", "123456789012345678",
	}

	f := parseSetupFlags(args)

	if f.providerFlags["discord-bot-token"] != "NTk5MTIz.Xh4abc.abcdefghijklmnopqrstuvwxyz012" {
		t.Errorf("providerFlags[discord-bot-token] = %q, want token value", f.providerFlags["discord-bot-token"])
	}
	if f.providerFlags["discord-user-id"] != "123456789012345678" {
		t.Errorf("providerFlags[discord-user-id] = %q, want 123456789012345678", f.providerFlags["discord-user-id"])
	}
}

// Verifies parseSetupFlags applies sensible defaults when no flags are given.
func TestParseSetupFlagsDefaults(t *testing.T) {
	f := parseSetupFlags(nil)

	home, _ := os.UserHomeDir()
	wantConfigDir := filepath.Join(home, "config")
	if f.configDir != wantConfigDir {
		t.Errorf("default configDir = %q, want %q", f.configDir, wantConfigDir)
	}
	if f.homeDir != home {
		t.Errorf("default homeDir = %q, want %q", f.homeDir, home)
	}
	if f.agentID != "main" {
		t.Errorf("default agentID = %q, want main", f.agentID)
	}
	if f.nonInteractive {
		t.Error("default nonInteractive should be false")
	}
	if f.charMode != "defaults" {
		t.Errorf("default charMode = %q, want defaults", f.charMode)
	}
}

// Verifies that --char-import-dir without --char-mode infers charMode="import".
// This is the Docker scenario: entrypoint detects import files and passes
// --char-import-dir but not --char-mode.
func TestParseSetupFlagsInfersImportMode(t *testing.T) {
	f := parseSetupFlags([]string{
		"--char-import-dir", "/opt/foci-import/character",
	})

	if f.charMode != "import" {
		t.Errorf("charMode = %q, want import (should be inferred from --char-import-dir)", f.charMode)
	}
	if f.charImportDir != "/opt/foci-import/character" {
		t.Errorf("charImportDir = %q, want /opt/foci-import/character", f.charImportDir)
	}
}

// Verifies provision.IsValidAgentID works correctly through the setup code path.
func TestValidationFunctions(t *testing.T) {
	if !provision.IsValidAgentID("my-agent") {
		t.Error("expected valid agent ID")
	}
	if provision.IsValidAgentID("-bad") {
		t.Error("expected invalid agent ID")
	}
}

// Verifies findRepoShared returns a valid path or empty string, never panics.
func TestFindRepoShared(t *testing.T) {
	result := findRepoShared()
	if result != "" {
		info, err := os.Stat(result)
		if err != nil {
			t.Errorf("findRepoShared returned %q but stat failed: %v", result, err)
		} else if !info.IsDir() {
			t.Errorf("findRepoShared returned %q which is not a directory", result)
		}
	}
}

// Verifies importMDFiles copies selected .md files from src to dest.
// Creates a temp dir with .md files, simulates user pressing Enter to confirm,
// and checks that all pre-selected files are copied.
func TestImportMDFiles(t *testing.T) {
	srcDir := t.TempDir()
	destDir := filepath.Join(t.TempDir(), "dest")

	// Create test .md files
	for _, name := range []string{"2025-01-01.md", "2025-01-02.md", "notes.txt"} {
		content := "# " + name
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate pressing Enter immediately (accept pre-selected)
	reader := bufio.NewReader(strings.NewReader("\n"))

	opts := mdImportOptions{
		label:     "test",
		preSelect: func(_ string) bool { return true },
		emptySkip: false,
	}

	if err := importMDFiles(reader, srcDir, destDir, opts); err != nil {
		t.Fatalf("importMDFiles: %v", err)
	}

	// Verify .md files were copied (not .txt)
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read destDir: %v", err)
	}

	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}

	if len(got) != 2 {
		t.Errorf("got %d files, want 2: %v", len(got), got)
	}
	if !got["2025-01-01.md"] {
		t.Error("missing 2025-01-01.md")
	}
	if !got["2025-01-02.md"] {
		t.Error("missing 2025-01-02.md")
	}
}

// Verifies importMDFiles with emptySkip=true returns nil on an empty directory
// instead of erroring out.
func TestImportMDFilesEmpty(t *testing.T) {
	srcDir := t.TempDir()
	destDir := filepath.Join(t.TempDir(), "dest")

	reader := bufio.NewReader(strings.NewReader("\n"))

	opts := mdImportOptions{
		label:     "memory",
		preSelect: func(_ string) bool { return true },
		emptySkip: true,
	}

	if err := importMDFiles(reader, srcDir, destDir, opts); err != nil {
		t.Errorf("expected nil error for empty dir with emptySkip=true, got: %v", err)
	}
}

// Verifies importMDFiles with emptySkip=false returns an error on an empty directory.
func TestImportMDFilesEmptyError(t *testing.T) {
	srcDir := t.TempDir()
	destDir := filepath.Join(t.TempDir(), "dest")

	reader := bufio.NewReader(strings.NewReader("\n"))

	opts := mdImportOptions{
		label:     "character",
		preSelect: func(_ string) bool { return true },
		emptySkip: false,
	}

	err := importMDFiles(reader, srcDir, destDir, opts)
	if err == nil {
		t.Error("expected error for empty dir with emptySkip=false")
	}
}

// Verifies copyMDFiles copies all .md files non-interactively.
func TestCopyMDFiles(t *testing.T) {
	srcDir := t.TempDir()
	destDir := filepath.Join(t.TempDir(), "dest")

	for _, name := range []string{"2025-01-01.md", "2025-01-02.md", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := copyMDFiles(srcDir, destDir); err != nil {
		t.Fatalf("copyMDFiles: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read destDir: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("got %d files, want 2", len(entries))
	}
}

// Verifies suggestMemoryDir finds memory/ relative to import dir.
func TestSuggestMemoryDir(t *testing.T) {
	// Case 1: importDir/memory/ exists (user pointed at workspace root)
	root := t.TempDir()
	memDir := filepath.Join(root, "memory")
	if err := os.Mkdir(memDir, 0755); err != nil {
		t.Fatal(err)
	}
	if got := suggestMemoryDir(root); got != memDir {
		t.Errorf("suggestMemoryDir(%q) = %q, want %q", root, got, memDir)
	}

	// Case 2: importDir/../memory/ exists (user pointed at character/ subdir)
	root2 := t.TempDir()
	charDir := filepath.Join(root2, "character")
	memDir2 := filepath.Join(root2, "memory")
	if err := os.Mkdir(charDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(memDir2, 0755); err != nil {
		t.Fatal(err)
	}
	if got := suggestMemoryDir(charDir); got != memDir2 {
		t.Errorf("suggestMemoryDir(%q) = %q, want %q", charDir, got, memDir2)
	}

	// Case 3: no memory dir found
	noMem := t.TempDir()
	if got := suggestMemoryDir(noMem); got != "" {
		t.Errorf("suggestMemoryDir(%q) = %q, want empty", noMem, got)
	}

	// Case 4: empty import dir
	if got := suggestMemoryDir(""); got != "" {
		t.Errorf("suggestMemoryDir(\"\") = %q, want empty", got)
	}
}

// Verifies backupIfExists renames an existing file to *.old.<timestamp>
// and returns "" for non-existent files.
func TestBackupIfExists(t *testing.T) {
	dir := t.TempDir()

	// Non-existent file: no backup, no error.
	backup, err := backupIfExists(filepath.Join(dir, "missing.toml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backup != "" {
		t.Errorf("expected empty backup path, got %q", backup)
	}

	// Existing file: should be renamed.
	path := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	backup, err = backupIfExists(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backup == "" {
		t.Fatal("expected non-empty backup path")
	}
	if !strings.Contains(backup, ".old.") {
		t.Errorf("backup path %q missing .old. segment", backup)
	}

	// Original path should no longer exist.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original file should not exist after backup")
	}

	// Backup file should contain original content.
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(data) != "original" {
		t.Errorf("backup content = %q, want original", data)
	}
}
