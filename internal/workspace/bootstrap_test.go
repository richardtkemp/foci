package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemBlocks(t *testing.T) {
	// Verifies system blocks are loaded in the given file order and all have type "text".
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I am Foci."), 0644)
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("Be kind."), 0644)
	os.WriteFile(filepath.Join(dir, "C.md"), []byte("You have tools."), 0644)

	b := NewBootstrap(dir, []string{"A.md", "B.md", "C.md"})
	blocks := b.SystemBlocks()

	if len(blocks) != 3 {
		t.Fatalf("len = %d, want 3", len(blocks))
	}

	if blocks[0].Text != "I am Foci." {
		t.Errorf("blocks[0].Text = %q", blocks[0].Text)
	}
	if blocks[1].Text != "Be kind." {
		t.Errorf("blocks[1].Text = %q", blocks[1].Text)
	}
	if blocks[2].Text != "You have tools." {
		t.Errorf("blocks[2].Text = %q", blocks[2].Text)
	}

	for i, b := range blocks {
		if b.Type != "text" {
			t.Errorf("blocks[%d].Type = %q", i, b.Type)
		}
	}
}

func TestSystemBlocksSkipsMissing(t *testing.T) {
	// Verifies that missing files in the file order are silently skipped.
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I exist."), 0644)

	b := NewBootstrap(dir, []string{"A.md", "B.md", "C.md"})
	blocks := b.SystemBlocks()

	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].Text != "I exist." {
		t.Errorf("text = %q", blocks[0].Text)
	}
}

func TestSystemBlocksEmpty(t *testing.T) {
	dir := t.TempDir()

	b := NewBootstrap(dir, nil)
	blocks := b.SystemBlocks()

	if len(blocks) != 0 {
		t.Errorf("len = %d, want 0", len(blocks))
	}
}

func TestSystemBlocksSkipsEmptyFiles(t *testing.T) {
	// Verifies that empty files are skipped and not included in system blocks.
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("has content"), 0644)

	b := NewBootstrap(dir, []string{"A.md", "B.md"})
	blocks := b.SystemBlocks()

	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].Text != "has content" {
		t.Errorf("text = %q", blocks[0].Text)
	}
}

func TestSystemBlocksCustomOrder(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("first"), 0644)
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("second"), 0644)
	os.WriteFile(filepath.Join(dir, "C.md"), []byte("third"), 0644)

	b := NewBootstrap(dir, []string{"C.md", "A.md", "B.md"})
	blocks := b.SystemBlocks()

	if len(blocks) != 3 {
		t.Fatalf("len = %d, want 3", len(blocks))
	}
	if blocks[0].Text != "third" {
		t.Errorf("blocks[0] = %q, want %q", blocks[0].Text, "third")
	}
	if blocks[1].Text != "first" {
		t.Errorf("blocks[1] = %q, want %q", blocks[1].Text, "first")
	}
	if blocks[2].Text != "second" {
		t.Errorf("blocks[2] = %q, want %q", blocks[2].Text, "second")
	}
}

func TestDefaultFileOrder(t *testing.T) {
	// Verifies DefaultFileOrder matches provision.DefaultSystemFiles layout.
	expected := []string{
		"character/SOUL.md",
		"character/COHERENCE.md",
		"character/CRAFT.md",
		"character/USER.md",
		"character/MEMORY.md",
	}

	if len(DefaultFileOrder) != len(expected) {
		t.Fatalf("DefaultFileOrder len = %d, want %d", len(DefaultFileOrder), len(expected))
	}
	for i, name := range expected {
		if DefaultFileOrder[i] != name {
			t.Errorf("DefaultFileOrder[%d] = %q, want %q", i, DefaultFileOrder[i], name)
		}
	}
}

func TestSetSecretNames(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I am Foci."), 0644)

	b := NewBootstrap(dir, []string{"A.md"})

	// Without secrets
	blocks := b.SystemBlocks()
	for _, blk := range blocks {
		if strings.Contains(blk.Text, "secret") {
			t.Errorf("should not have secrets before SetSecretNames: %q", blk.Text)
		}
	}

	// Set secret names
	b.SetSecretNames([]string{"anthropic.setup_token", "github.pat"}, false)

	blocks = b.SystemBlocks()
	// Should have 2 blocks: IDENTITY + secrets
	if len(blocks) != 2 {
		t.Fatalf("len = %d, want 2", len(blocks))
	}

	// Secrets block should contain the names
	secretsBlock := blocks[len(blocks)-1]
	if !strings.Contains(secretsBlock.Text, "anthropic.setup_token") {
		t.Errorf("secrets block missing anthropic.setup_token: %q", secretsBlock.Text)
	}
	if !strings.Contains(secretsBlock.Text, "github.pat") {
		t.Errorf("secrets block missing github.pat: %q", secretsBlock.Text)
	}
}

func TestSetSecretNamesCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I am Foci."), 0644)

	b := NewBootstrap(dir, []string{"A.md"})

	// First call — no secrets
	blocks1 := b.SystemBlocks()
	count1 := len(blocks1)

	// Set secret names — should invalidate cache
	b.SetSecretNames([]string{"my.secret"}, false)

	blocks2 := b.SystemBlocks()
	count2 := len(blocks2)

	if count2 != count1+1 {
		t.Errorf("expected 1 more block after SetSecretNames: before=%d, after=%d", count1, count2)
	}
}

func TestSecretsBlockIsLast(t *testing.T) {
	// Verify secrets block is the last block (translate layer will mark it for caching).
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I am Foci."), 0644)
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("Be kind."), 0644)

	b := NewBootstrap(dir, []string{"A.md", "B.md"})
	b.SetSecretNames([]string{"secret.key"}, false)

	blocks := b.SystemBlocks()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	// Secrets block text should contain the secret name and be last
	last := blocks[len(blocks)-1]
	if !strings.Contains(last.Text, "secret.key") {
		t.Errorf("last block text = %q, want secret name", last.Text)
	}
}

func TestReload(t *testing.T) {
	// Verifies Reload re-reads files from disk and updates cached blocks.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.md"), []byte("original content"), 0644)

	b := NewBootstrap(dir, []string{"A.md"})

	blocks := b.SystemBlocks()
	if blocks[0].Text != "original content" {
		t.Errorf("initial text = %q", blocks[0].Text)
	}

	// Modify file on disk
	os.WriteFile(filepath.Join(dir, "A.md"), []byte("updated content"), 0644)

	// Before reload — should still have old content
	blocks = b.SystemBlocks()
	if blocks[0].Text != "original content" {
		t.Errorf("before reload text = %q, want original", blocks[0].Text)
	}

	// Reload
	b.Reload()

	blocks = b.SystemBlocks()
	if blocks[0].Text != "updated content" {
		t.Errorf("after reload text = %q, want updated", blocks[0].Text)
	}
}

func TestReloadInvalidatesSecretCache(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.md"), []byte("content"), 0644)

	b := NewBootstrap(dir, []string{"A.md"})
	b.SetSecretNames([]string{"my.key"}, false)

	// Build cache with secrets
	blocks1 := b.SystemBlocks()

	// Reload — should rebuild
	b.Reload()

	blocks2 := b.SystemBlocks()

	// Both should still have the same structure
	if len(blocks1) != len(blocks2) {
		t.Errorf("block counts differ: %d vs %d", len(blocks1), len(blocks2))
	}
}

func TestCheckSizes(t *testing.T) {
	dir := t.TempDir()

	// Create a small file and a large file
	os.WriteFile(filepath.Join(dir, "SMALL.md"), []byte("small"), 0644)
	os.WriteFile(filepath.Join(dir, "BIG.md"), make([]byte, 25000), 0644)

	b := NewBootstrap(dir, []string{"SMALL.md", "BIG.md"})

	// No warnings with high thresholds
	warnings := b.CheckSizes(100000, 200000)
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings with high thresholds, got %d: %v", len(warnings), warnings)
	}

	// Per-file warning
	warnings = b.CheckSizes(20000, 200000)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 per-file warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "BIG.md") {
		t.Errorf("warning should mention BIG.md: %s", warnings[0])
	}

	// Total warning
	warnings = b.CheckSizes(100000, 10000)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 total warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "total") {
		t.Errorf("warning should mention 'total': %s", warnings[0])
	}

	// Both warnings
	warnings = b.CheckSizes(20000, 10000)
	if len(warnings) != 2 {
		t.Errorf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}

	// Zero thresholds disable checks
	warnings = b.CheckSizes(0, 0)
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings with zero thresholds, got %d", len(warnings))
	}
}

func TestCheckSizesMissingFileBeforeBig(t *testing.T) {
	// Regression: when fileOrder lists a missing file before a present
	// over-threshold file, the warning must name the real file, not the
	// missing slot it would have occupied. Previously CheckSizes indexed
	// fileOrder by the loaded-block position, causing the labels to drift
	// when any earlier file in fileOrder was absent from disk.
	dir := t.TempDir()

	// MISSING.md not written. BIG.md is at fileOrder index 1, but after
	// MISSING.md is skipped it becomes loaded-block index 0.
	os.WriteFile(filepath.Join(dir, "BIG.md"), make([]byte, 25000), 0644)

	b := NewBootstrap(dir, []string{"MISSING.md", "BIG.md"})
	warnings := b.CheckSizes(20000, 200000)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "BIG.md") {
		t.Errorf("warning should mention BIG.md, got: %s", warnings[0])
	}
	if strings.Contains(warnings[0], "MISSING.md") {
		t.Errorf("warning misattributes to MISSING.md: %s", warnings[0])
	}
}

func TestSectionSizesMissingFileBeforePresent(t *testing.T) {
	// Regression: SectionSizes must label loaded blocks with the file they
	// came from, not fileOrder[block_index]. With MISSING.md skipped,
	// indices drift and previously every block was mis-labelled.
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("alpha"), 0644) // 5 chars
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("beta"), 0644)  // 4 chars
	// MISSING.md in the middle — should be skipped

	b := NewBootstrap(dir, []string{"A.md", "MISSING.md", "B.md"})
	sizes := b.SectionSizes()

	if len(sizes) != 2 {
		t.Fatalf("len = %d, want 2", len(sizes))
	}
	if sizes[0].Name != "A.md" || sizes[0].Chars != 5 {
		t.Errorf("sizes[0] = %+v, want {A.md, 5}", sizes[0])
	}
	if sizes[1].Name != "B.md" || sizes[1].Chars != 4 {
		t.Errorf("sizes[1] = %+v, want {B.md, 4}", sizes[1])
	}
}

func TestSectionSizes(t *testing.T) {
	// Verifies SectionSizes reports name and char count for loaded files,
	// skipping missing files.
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I am Foci."), 0644) // 10 chars
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("Be kind."), 0644)   // 8 chars
	// C.md missing — should be skipped

	b := NewBootstrap(dir, []string{"A.md", "B.md", "C.md"})
	sizes := b.SectionSizes()

	if len(sizes) != 2 {
		t.Fatalf("len = %d, want 2", len(sizes))
	}
	if sizes[0].Name != "A.md" || sizes[0].Chars != 10 {
		t.Errorf("sizes[0] = %+v, want {A.md, 10}", sizes[0])
	}
	if sizes[1].Name != "B.md" || sizes[1].Chars != 8 {
		t.Errorf("sizes[1] = %+v, want {B.md, 8}", sizes[1])
	}
}

func TestSectionSizesEmpty(t *testing.T) {
	dir := t.TempDir()
	b := NewBootstrap(dir, nil)
	sizes := b.SectionSizes()
	if len(sizes) != 0 {
		t.Errorf("expected empty sizes, got %d", len(sizes))
	}
}

// TestBuildSecretsBlock_NoSecretsNoBitwarden tests buildSecretsBlock with no secrets
func TestBuildSecretsBlock_NoSecretsNoBitwarden(t *testing.T) {
	block := buildSecretsBlock([]string{}, false)

	if block.Type != "text" {
		t.Errorf("Type = %q, want text", block.Type)
	}
	if block.Text != "" {
		t.Errorf("Text should be empty for no secrets, got %q", block.Text)
	}
}

// TestBuildSecretsBlock_WithSecrets tests buildSecretsBlock with secret names
func TestBuildSecretsBlock_WithSecrets(t *testing.T) {
	secretNames := []string{"anthropic.api_key", "openai.token", "custom.secret"}
	block := buildSecretsBlock(secretNames, false)

	if block.Type != "text" {
		t.Errorf("Type = %q, want text", block.Type)
	}
	if !strings.Contains(block.Text, "Available secrets") {
		t.Errorf("Text should mention available secrets, got %q", block.Text)
	}
	for _, name := range secretNames {
		if !strings.Contains(block.Text, name) {
			t.Errorf("Text should contain %q", name)
		}
	}
}

// TestBuildSecretsBlock_WithBitwarden tests buildSecretsBlock with Bitwarden
func TestBuildSecretsBlock_WithBitwarden(t *testing.T) {
	block := buildSecretsBlock([]string{}, true)

	if !strings.Contains(block.Text, "Bitwarden") {
		t.Errorf("Text should mention Bitwarden, got %q", block.Text)
	}
	if !strings.Contains(block.Text, "bitwarden_search") {
		t.Errorf("Text should mention bitwarden_search, got %q", block.Text)
	}
}

// TestBuildSecretsBlock_WithSecretsAndBitwarden tests buildSecretsBlock with both
func TestBuildSecretsBlock_WithSecretsAndBitwarden(t *testing.T) {
	secretNames := []string{"api.key"}
	block := buildSecretsBlock(secretNames, true)

	if !strings.Contains(block.Text, "api.key") {
		t.Errorf("Text should contain api.key")
	}
	if !strings.Contains(block.Text, "Bitwarden") {
		t.Errorf("Text should contain Bitwarden")
	}
	if !strings.Contains(block.Text, "\n\n") {
		t.Errorf("Text should have separator between sections")
	}
}

// TestSystemBlocks_AllFiles tests SystemBlocks when all files are loaded
func TestSystemBlocks_AllFiles(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("I am test."), 0644)
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("My soul."), 0644)
	os.WriteFile(filepath.Join(dir, "C.md"), []byte("My craft."), 0644)

	b := NewBootstrap(dir, []string{"A.md", "B.md", "C.md"})

	// Set secret names to trigger secrets block
	b.SetSecretNames([]string{"api.key"}, false)

	blocks := b.SystemBlocks()

	// Should have: IDENTITY, SOUL, CRAFT, secrets
	if len(blocks) < 3 {
		t.Errorf("Expected at least 3 blocks, got %d", len(blocks))
	}

	// Find the secrets block
	foundSecrets := false
	for _, block := range blocks {
		if strings.Contains(block.Text, "Available secrets") {
			foundSecrets = true
			break
		}
	}
	if !foundSecrets {
		t.Error("SystemBlocks should include secrets block")
	}
}

// TestLoadFromDisk tests loadFromDisk with existing files
func TestLoadFromDisk(t *testing.T) {
	// Verifies loadFromDisk returns blocks for files that exist.
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "A.md"), []byte("Identity content"), 0644)
	os.WriteFile(filepath.Join(dir, "B.md"), []byte("Soul content"), 0644)

	b := NewBootstrap(dir, []string{"A.md", "B.md"})
	blocks, names := b.loadFromDisk()

	if len(blocks) != 2 {
		t.Fatalf("loadFromDisk returned %d blocks, want 2", len(blocks))
	}
	if blocks[0].Text != "Identity content" {
		t.Errorf("blocks[0].Text = %q", blocks[0].Text)
	}
	if blocks[1].Text != "Soul content" {
		t.Errorf("blocks[1].Text = %q", blocks[1].Text)
	}
	if len(names) != 2 || names[0] != "A.md" || names[1] != "B.md" {
		t.Errorf("loadFromDisk names = %v, want [A.md B.md]", names)
	}
}

func TestBootstrapWithCharacterSubdir(t *testing.T) {
	// Verifies bootstrap loads character files from the character/ subdirectory
	// using the default file order — the standard layout for provisioned agents.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "character"), 0755)
	os.WriteFile(filepath.Join(dir, "character", "SOUL.md"), []byte("soul content"), 0644)
	os.WriteFile(filepath.Join(dir, "character", "CRAFT.md"), []byte("craft content"), 0644)

	b := NewBootstrap(dir, nil) // uses DefaultFileOrder (character/*.md)
	blocks := b.SystemBlocks()

	if len(blocks) != 2 {
		t.Fatalf("len = %d, want 2", len(blocks))
	}
	if blocks[0].Text != "soul content" {
		t.Errorf("blocks[0] = %q, want soul content", blocks[0].Text)
	}
	if blocks[1].Text != "craft content" {
		t.Errorf("blocks[1] = %q, want craft content", blocks[1].Text)
	}
}

// TestLoadFromDisk_Empty tests loadFromDisk with empty directory
func TestLoadFromDisk_Empty(t *testing.T) {
	dir := t.TempDir()
	b := NewBootstrap(dir, nil)

	blocks, names := b.loadFromDisk()

	// Should return empty slice when no files exist
	if len(blocks) != 0 {
		t.Errorf("Expected no blocks for empty dir, got %d", len(blocks))
	}
	if len(names) != 0 {
		t.Errorf("Expected no names for empty dir, got %d", len(names))
	}
}
