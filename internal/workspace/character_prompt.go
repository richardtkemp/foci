package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CharacterSystemPrompt composes an agent's character files verbatim, each
// under a filename header, for use as the system prompt of one-shot batch
// runs (nudge extraction, memory consolidation). Missing/empty files are
// skipped; order follows fileOrder. Shared by internal/nudge and
// internal/periodic so the two batch consumers cannot drift apart on what
// "the character is loaded in the system prompt" means (#1310).
func CharacterSystemPrompt(dir string, fileOrder []string) string {
	var b strings.Builder
	for _, name := range fileOrder {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(data) == 0 {
			continue
		}
		fmt.Fprintf(&b, "===== %s =====\n\n%s\n\n", name, string(data))
	}
	return strings.TrimSpace(b.String())
}
