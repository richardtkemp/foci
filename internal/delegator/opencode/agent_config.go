package opencode

import (
	"os"
	"path/filepath"

	"foci/internal/log"
)

const fociAgentName = "foci"

// fociAgentConfig is the minimal agent config that suppresses opencode's
// default system prompt. opencode's request builder (session/llm/request.ts)
// checks agent.prompt: if truthy, it skips SystemPrompt.provider(model) and
// uses the agent's prompt instead. We set a minimal prompt (" ") so the
// default is skipped, and supply foci's actual system prompt dynamically via
// the "system" field in each POST /prompt_async body.
//
// permission:* = allow because foci handles its own permission surface.
// mode: subagent matches opencode's convention for non-interactive agents.
const fociAgentConfigSource = `{
  "name": "foci",
  "prompt": " ",
  "mode": "subagent",
  "permission": { "*": "allow" }
}
`

// EnsureFociAgentConfig writes the foci agent config file into the workspace's
// .opencode/agents/ directory if it doesn't exist or has stale content.
// Called from Start before acquireServer — opencode loads agent configs at
// subprocess startup, so the file must exist before the server launches.
// Idempotent: skips the write if content already matches (avoids touching
// mtime, which could trigger file-watchers).
func EnsureFociAgentConfig(workDir string) {
	if workDir == "" {
		return
	}
	path := filepath.Join(workDir, ".opencode", "agents", fociAgentName+".json")

	if existing, err := os.ReadFile(path); err == nil && string(existing) == fociAgentConfigSource {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Warnf("opencode", "agent-config: mkdir %s: %v", filepath.Dir(path), err)
		return
	}
	if err := os.WriteFile(path, []byte(fociAgentConfigSource), 0o644); err != nil {
		log.Warnf("opencode", "agent-config: write %s: %v", path, err)
		return
	}
	log.Debugf("opencode", "agent-config: wrote %s", path)
}
