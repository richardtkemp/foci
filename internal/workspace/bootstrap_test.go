package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemBlocks(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am Foci."), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be kind."), 0644)
	os.WriteFile(filepath.Join(dir, "TOOLS.md"), []byte("You have tools."), 0644)

	b := NewBootstrap(dir, nil) // default file order
	blocks := b.SystemBlocks()

	if len(blocks) != 3 {
		t.Fatalf("len = %d, want 3", len(blocks))
	}

	// Check order matches file order (IDENTITY before SOUL before TOOLS)
	if blocks[0].Text != "I am Foci." {
		t.Errorf("blocks[0].Text = %q", blocks[0].Text)
	}
	if blocks[1].Text != "Be kind." {
		t.Errorf("blocks[1].Text = %q", blocks[1].Text)
	}
	if blocks[2].Text != "You have tools." {
		t.Errorf("blocks[2].Text = %q", blocks[2].Text)
	}

	// All should be type "text"
	for i, b := range blocks {
		if b.Type != "text" {
			t.Errorf("blocks[%d].Type = %q", i, b.Type)
		}
	}
}

func TestSystemBlocksSkipsMissing(t *testing.T) {
	dir := t.TempDir()

	// Only create IDENTITY.md — all others should be skipped silently
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I exist."), 0644)

	b := NewBootstrap(dir, nil)
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
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("has content"), 0644)

	b := NewBootstrap(dir, nil)
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
	expected := []string{
		"IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md",
		"TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md",
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
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am Foci."), 0644)

	b := NewBootstrap(dir, nil)

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
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am Foci."), 0644)

	b := NewBootstrap(dir, nil)

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
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am Foci."), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be kind."), 0644)

	b := NewBootstrap(dir, nil)
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
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("original content"), 0644)

	b := NewBootstrap(dir, nil)

	blocks := b.SystemBlocks()
	if blocks[0].Text != "original content" {
		t.Errorf("initial text = %q", blocks[0].Text)
	}

	// Modify file on disk
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("updated content"), 0644)

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
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("content"), 0644)

	b := NewBootstrap(dir, nil)
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

func TestSectionSizes(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am Foci."), 0644) // 10 chars
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be kind."), 0644)       // 8 chars
	// COHERENCE.md missing — should be skipped

	b := NewBootstrap(dir, nil) // default file order
	sizes := b.SectionSizes()

	if len(sizes) != 2 {
		t.Fatalf("len = %d, want 2", len(sizes))
	}
	if sizes[0].Name != "IDENTITY.md" || sizes[0].Chars != 10 {
		t.Errorf("sizes[0] = %+v, want {IDENTITY.md, 10}", sizes[0])
	}
	if sizes[1].Name != "SOUL.md" || sizes[1].Chars != 8 {
		t.Errorf("sizes[1] = %+v, want {SOUL.md, 8}", sizes[1])
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

	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am test."), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("My soul."), 0644)
	os.WriteFile(filepath.Join(dir, "CRAFT.md"), []byte("My craft."), 0644)

	// Create Bootstrap with custom file order
	b := NewBootstrap(dir, []string{"IDENTITY.md", "SOUL.md", "CRAFT.md"})

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
	dir := t.TempDir()

	// Create some workspace files
	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("Identity content"), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Soul content"), 0644)

	b := NewBootstrap(dir, nil)
	blocks := b.loadFromDisk()

	// Should return blocks for files that exist
	if len(blocks) == 0 {
		t.Error("loadFromDisk should return blocks for existing files")
	}

	// Each block should have its content
	hasIdentity := false
	hasSoul := false
	for _, block := range blocks {
		if strings.Contains(block.Text, "Identity") {
			hasIdentity = true
		}
		if strings.Contains(block.Text, "Soul") {
			hasSoul = true
		}
	}

	if !hasIdentity || !hasSoul {
		t.Errorf("Expected blocks for IDENTITY and SOUL files, got %d blocks", len(blocks))
	}
}

// TestLoadFromDisk_Empty tests loadFromDisk with empty directory
func TestLoadFromDisk_Empty(t *testing.T) {
	dir := t.TempDir()
	b := NewBootstrap(dir, nil)

	blocks := b.loadFromDisk()

	// Should return empty slice when no files exist
	if len(blocks) != 0 {
		t.Errorf("Expected no blocks for empty dir, got %d", len(blocks))
	}
}
