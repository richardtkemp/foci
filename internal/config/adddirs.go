package config

import (
	"fmt"
	"path/filepath"
	"sync"
)

// warnedRelativeAddDir remembers each (agent, value) already warned about, so
// a config reload (liveAddDirs re-reads the file at every Claude Code launch)
// does not repeat the warning: a config problem is ONE warning (Dick's rule).
var warnedRelativeAddDir sync.Map

// DropRelativeAddDirs removes every relative path from each agent's
// backend_config.add_dirs and returns one warning for each (#3123). Claude
// Code would resolve a relative --add-dir against its own working directory
// (the session's workspace), which differs between sessions and is not what
// the operator can see in foci.toml. The slice is rebuilt, not edited in
// place, as it may be shared with a defaults block.
func DropRelativeAddDirs(cfg *Config) []string {
	var out []string
	for i := range cfg.Agents {
		dirs := cfg.Agents[i].BackendConfig.AddDirs
		if len(dirs) == 0 {
			continue
		}
		kept := make([]string, 0, len(dirs))
		id := cfg.Agents[i].ID
		for _, d := range dirs {
			if filepath.IsAbs(d) {
				kept = append(kept, d)
				continue
			}
			if _, seen := warnedRelativeAddDir.LoadOrStore(id+"\x00"+d, true); seen {
				continue
			}
			out = append(out, fmt.Sprintf(
				"agent %q: backend_config.add_dirs entry %q is not an absolute path and is ignored: give an absolute path",
				id, d))
		}
		if len(kept) != len(dirs) {
			cfg.Agents[i].BackendConfig.AddDirs = kept
		}
	}
	return out
}

// AddDirs returns an agent's backend_config.add_dirs (#3123), after Load has
// dropped the relative entries. ok is false when no agent has that id.
func (cfg *Config) AddDirs(agentID string) (dirs []string, ok bool) {
	for _, a := range cfg.Agents {
		if a.ID == agentID {
			return a.BackendConfig.AddDirs, true
		}
	}
	return nil, false
}
