package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"sync"
)

// The variables with which Claude Code picks its config folder and its login
// (fociroot.CCConfigDirEnv, fociroot.CCSecureStorageEnv; config cannot import
// fociroot's users, so the names are repeated here).
var accountEnvVars = []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"}

// warnedAccounts remembers each account problem already warned about, so a
// config reload does not repeat the warning: a config problem is ONE warning
// (Dick's rule).
var warnedAccounts sync.Map

// NormalizeAccounts makes backend_config.accounts the only way an agent names
// its Claude Code accounts (#3186, Dick 2026-11-19: "New way is the only
// way"), and returns one warning per problem:
//
//   - CLAUDE_CONFIG_DIR or CLAUDE_SECURESTORAGE_CONFIG_DIR in
//     backend_config.env is removed: it no longer gives the agent an account.
//     The warning says what to write instead.
//   - A relative accounts entry is removed. foci would resolve it against the
//     gateway's working directory and Claude Code against its own (the agent
//     workspace), so the two would read different folders.
//   - A list of several accounts keeps its first entry: one agent has one
//     account until #3186 phase 2.
//
// An agent left with no entry runs on the host's Claude Code folder and
// account. The env map is copied before a delete, as it may be shared with a
// defaults block.
func NormalizeAccounts(cfg *Config) []string {
	var out []string
	warn := func(id, problem, format string, args ...any) {
		if _, seen := warnedAccounts.LoadOrStore(id+"\x00"+problem, true); !seen {
			out = append(out, fmt.Sprintf("agent %q: ", id)+fmt.Sprintf(format, args...))
		}
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		for _, name := range accountEnvVars {
			v, ok := a.BackendConfig.Env[name]
			if !ok {
				continue
			}
			a.BackendConfig.Env = maps.Clone(a.BackendConfig.Env)
			delete(a.BackendConfig.Env, name)
			instead := "list the agent's Claude Code config folder in backend_config.accounts"
			if name == accountEnvVars[0] && filepath.IsAbs(v) {
				instead = fmt.Sprintf("write backend_config.accounts = [%q]", v)
			}
			warn(a.ID, "env\x00"+name+"\x00"+v,
				"backend_config.env %s = %q is ignored: an agent's Claude Code account is named by backend_config.accounts, so %s. The agent uses the host's Claude Code folder and account until then",
				name, v, instead)
		}
		var kept []string
		for _, dir := range a.BackendConfig.Accounts {
			if dir != "" && !filepath.IsAbs(dir) {
				warn(a.ID, "relative\x00"+dir,
					"backend_config.accounts entry %q is a relative path and is ignored: give an absolute path", dir)
				continue
			}
			if dir != "" {
				kept = append(kept, dir)
			}
		}
		if len(kept) > 1 {
			warn(a.ID, "several\x00"+fmt.Sprint(kept),
				"backend_config.accounts lists %d Claude Code accounts, and one agent can use only one for now: it uses the first, %q, and ignores the rest",
				len(kept), kept[0])
			kept = kept[:1]
		}
		if len(kept) != len(a.BackendConfig.Accounts) {
			a.BackendConfig.Accounts = kept
		}
	}
	return out
}
