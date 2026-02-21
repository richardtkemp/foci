package workspace

import (
	"os"
	"path/filepath"

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
type Bootstrap struct {
	dir       string
	fileOrder []string
}

// NewBootstrap creates a Bootstrap that reads files from dir in the given order.
func NewBootstrap(dir string, fileOrder []string) *Bootstrap {
	if len(fileOrder) == 0 {
		fileOrder = DefaultFileOrder
	}
	return &Bootstrap{dir: dir, fileOrder: fileOrder}
}

// SystemBlocks returns the system prompt blocks for the API request.
// Missing files are silently skipped. The last block gets cache_control: ephemeral.
func (b *Bootstrap) SystemBlocks() []anthropic.SystemBlock {
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
