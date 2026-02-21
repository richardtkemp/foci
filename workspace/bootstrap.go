package workspace

import (
	"os"
	"path/filepath"
	"sync"

	"clod/anthropic"
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
	dir       string
	fileOrder []string
	cached    []anthropic.SystemBlock
	mu        sync.RWMutex
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

// SystemBlocks returns the cached system prompt blocks for the API request.
func (b *Bootstrap) SystemBlocks() []anthropic.SystemBlock {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cached
}

// Reload re-reads workspace files from disk. Call after compaction or session reset.
func (b *Bootstrap) Reload() {
	blocks := b.loadFromDisk()
	b.mu.Lock()
	b.cached = blocks
	b.mu.Unlock()
}

// loadFromDisk reads workspace files and builds system blocks.
func (b *Bootstrap) loadFromDisk() []anthropic.SystemBlock {
	var blocks []anthropic.SystemBlock

	for _, name := range b.fileOrder {
		data, err := os.ReadFile(filepath.Join(b.dir, name))
		if err != nil {
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
