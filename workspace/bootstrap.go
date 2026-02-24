package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"clod/anthropic"
	"clod/log"
)

// DefaultFileOrder is the default order for loading workspace files.
var DefaultFileOrder = []string{
	"IDENTITY.md",
	"SOUL.md",
	"COHERENCE.md",
	"AGENTS.md",
	"TOOLS.md",
	"USER.md",
	"MEMORY.md",
	"HEARTBEAT.md",
}

// Bootstrap loads workspace markdown files as system prompt blocks.
// Blocks are cached in memory and only re-read on Reload().
type Bootstrap struct {
	dir           string
	fileOrder     []string
	secretNames   []string // available secret names for {{secret:NAME}} templates
	hasBitwarden  bool     // bitwarden integration is enabled
	cached        []anthropic.SystemBlock
	cachedWithSec []anthropic.SystemBlock // cached blocks with secrets injected
	mu            sync.RWMutex
}

// NewBootstrap creates a Bootstrap that reads files from dir in the given order.
// Performs the initial load from disk.
func NewBootstrap(dir string, fileOrder []string) *Bootstrap {
	if len(fileOrder) == 0 {
		fileOrder = DefaultFileOrder
	}
	b := &Bootstrap{dir: dir, fileOrder: fileOrder}
	b.cached = b.loadFromDisk()
	return b
}

// SetSecretNames sets the available secret names to be injected into system blocks.
// Names should be sorted alphabetically. When hasBitwarden is true, a note about
// bitwarden availability is appended to the secrets block.
func (b *Bootstrap) SetSecretNames(names []string, hasBitwarden bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.secretNames = names
	b.hasBitwarden = hasBitwarden
	b.cachedWithSec = nil // invalidate cache so it's regenerated with new secrets
}

// SystemBlocks returns the cached system prompt blocks for the API request,
// including injected secret names if available.
func (b *Bootstrap) SystemBlocks() []anthropic.SystemBlock {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Return cached version if we already built it
	if b.cachedWithSec != nil {
		return b.cachedWithSec
	}

	// Build blocks with secrets injected
	blocks := make([]anthropic.SystemBlock, len(b.cached))
	copy(blocks, b.cached)

	// Inject secrets block if we have secret names or bitwarden
	if len(b.secretNames) > 0 || b.hasBitwarden {
		secretsBlock := buildSecretsBlock(b.secretNames, b.hasBitwarden)
		blocks = append(blocks, secretsBlock)
	}

	// Mark last block for caching
	if len(blocks) > 0 {
		blocks[len(blocks)-1].CacheControl = anthropic.Ephemeral()
	}

	return blocks
}

// buildSecretsBlock creates a system block listing available secrets
func buildSecretsBlock(names []string, hasBitwarden bool) anthropic.SystemBlock {
	var text string
	if len(names) > 0 {
		text = "Available secrets for {{secret:NAME}} templates in http_request headers/body (preferred) or exec commands: " + strings.Join(names, ", ")
	}
	if hasBitwarden {
		if text != "" {
			text += "\n\n"
		}
		text += "Bitwarden vault is available. Use bitwarden_search to find items, then bitwarden_unlock to unlock a specific item (requires administrator approval). Once unlocked, reference with {{secret:bw.ITEM_ID}} in http_request. Host validation uses the vault item's URI fields."
	}
	return anthropic.SystemBlock{
		Type: "text",
		Text: text,
	}
}

// Reload re-reads workspace files from disk. Call after compaction or session reset.
func (b *Bootstrap) Reload() {
	blocks := b.loadFromDisk()
	b.mu.Lock()
	b.cached = blocks
	b.cachedWithSec = nil // invalidate cached blocks with secrets
	b.mu.Unlock()
}

// CheckSizes reports per-file and total system prompt size warnings.
// Returns a list of warning strings (empty if all within thresholds).
// A zero threshold disables that check.
func (b *Bootstrap) CheckSizes(maxFileChars, maxTotalChars int) []string {
	b.mu.RLock()
	blocks := b.cached
	b.mu.RUnlock()

	var warnings []string
	total := 0
	for i, block := range blocks {
		size := len(block.Text)
		total += size
		if maxFileChars > 0 && size > maxFileChars {
			name := "unknown"
			if i < len(b.fileOrder) {
				name = b.fileOrder[i]
			}
			warnings = append(warnings, fmt.Sprintf("system prompt file %s is %d chars (threshold: %d)", name, size, maxFileChars))
		}
	}
	if maxTotalChars > 0 && total > maxTotalChars {
		warnings = append(warnings, fmt.Sprintf("total system prompt is %d chars (threshold: %d)", total, maxTotalChars))
	}
	return warnings
}

// loadFromDisk reads workspace files and builds system blocks.
func (b *Bootstrap) loadFromDisk() []anthropic.SystemBlock {
	var blocks []anthropic.SystemBlock

	for _, name := range b.fileOrder {
		data, err := os.ReadFile(filepath.Join(b.dir, name))
		if err != nil {
			if !os.IsNotExist(err) {
				log.Warnf("workspace", "read %s: %v", name, err)
			}
			continue // skip missing files
		}
		content := string(data)
		if content == "" {
			continue
		}
		blocks = append(blocks, anthropic.SystemBlock{
			Type: "text",
			Text: content,
		})
	}

	// Mark last block for caching
	if len(blocks) > 0 {
		blocks[len(blocks)-1].CacheControl = anthropic.Ephemeral()
	}

	return blocks
}
