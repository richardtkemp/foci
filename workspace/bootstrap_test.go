package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSystemBlocks(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("I am Clod."), 0644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be kind."), 0644)
	os.WriteFile(filepath.Join(dir, "TOOLS.md"), []byte("You have tools."), 0644)

	b := NewBootstrap(dir, nil) // default file order
	blocks := b.SystemBlocks()

	if len(blocks) != 3 {
		t.Fatalf("len = %d, want 3", len(blocks))
	}

	// Check order matches file order (IDENTITY before SOUL before TOOLS)
	if blocks[0].Text != "I am Clod." {
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

	// Only last block should have cache control
	if blocks[0].CacheControl != nil {
		t.Error("blocks[0] should not have cache control")
	}
	if blocks[1].CacheControl != nil {
		t.Error("blocks[1] should not have cache control")
	}
	if blocks[2].CacheControl == nil || blocks[2].CacheControl.Type != "ephemeral" {
		t.Errorf("blocks[2] cache control = %+v, want ephemeral", blocks[2].CacheControl)
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
	// Single block should get cache control
	if blocks[0].CacheControl == nil {
		t.Error("single block should have cache control")
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
		"TOOLS.md", "USER.md", "MEMORY.md", "HEARTBEAT.md",
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
