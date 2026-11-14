package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"sync"
)

// ccConfigDirEnv is Claude Code's config-folder variable (fociroot.CCConfigDirEnv;
// config cannot import fociroot's users, so the name is repeated here).
const ccConfigDirEnv = "CLAUDE_CONFIG_DIR"

// warnedRelativeCCConfigDir remembers each (agent, value) already warned
// about, so a config reload does not repeat the warning: a config problem is
// ONE warning (Dick's rule).
var warnedRelativeCCConfigDir sync.Map

// DropRelativeCCConfigDirs removes a relative CLAUDE_CONFIG_DIR from every
// agent's backend_config.env and returns one warning for each (#3104 phase 2).
// foci would resolve a relative folder against the gateway's working
// directory, and Claude Code against its own (the agent workspace), so the
// two would read different folders, and so different transcripts and logins.
// The agent then runs as if the key were unset (the host's folder and
// account). The map is copied before the delete, as it may be shared with
// a defaults block.
func DropRelativeCCConfigDirs(cfg *Config) []string {
	var out []string
	for i := range cfg.Agents {
		env := cfg.Agents[i].BackendConfig.Env
		v, ok := env[ccConfigDirEnv]
		if !ok || v == "" || filepath.IsAbs(v) {
			continue
		}
		env = maps.Clone(env)
		delete(env, ccConfigDirEnv)
		cfg.Agents[i].BackendConfig.Env = env
		id := cfg.Agents[i].ID
		if _, seen := warnedRelativeCCConfigDir.LoadOrStore(id+"\x00"+v, true); seen {
			continue
		}
		out = append(out, fmt.Sprintf(
			"agent %q: backend_config.env CLAUDE_CONFIG_DIR = %q is a relative path and is ignored: give an absolute path. The agent uses the host's Claude Code folder and account until then",
			id, v))
	}
	return out
}
