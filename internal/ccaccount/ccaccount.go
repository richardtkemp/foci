// Package ccaccount answers, for one agent, two questions about the Claude
// Code process foci starts for it (#3104):
//
//   - its CC config folder: where that CC keeps transcripts (projects/),
//     settings.json and, on Linux, its login;
//   - its account: where that CC reads its login.
//
// Both come from the FINAL environment foci hands the CC process: the
// operator population (the daemon env overlaid with the operator's captured
// shell env, procx.Operator), then the agent's backend_config.env, later
// values winning, as in ccstream's buildEnv. So an agent that sets
// CLAUDE_CONFIG_DIR in its backend_config.env gets that folder, an agent with
// no override gets the host's folder exactly as before (fociroot.CCConfigDir),
// and a CLAUDE_CONFIG_DIR exported by the operator's shell rc file applies,
// because that is the folder CC is given.
//
// This is the one definition. Every reader of an agent's CC on-disk state
// (ccstream's transcript paths, fork and cleanup, retention, the session
// index path, rename-agent) resolves through ForAgent; login and limits work
// keyed on the account (phases 2 and 3 of #3104) uses the Account it returns.
package ccaccount

import (
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"foci/internal/fociroot"
	"foci/internal/procx"
)

// Location is where one agent's Claude Code keeps its state, and whose login
// it uses.
type Location struct {
	// Dir is the CC config folder: absolute and cleaned, so two spellings
	// of one folder give the same Dir.
	Dir string
	// Account is where that CC reads its login.
	Account Account
}

// ProjectsDir is <Dir>/projects, where CC keeps per-project transcripts:
// projects/<cwd slug>/<session id>.jsonl (fociroot.CCProjectSlug).
func (l Location) ProjectsDir() string { return filepath.Join(l.Dir, "projects") }

// TranscriptPath is where CC keeps the transcript of sessionID for a process
// run in workDir.
func (l Location) TranscriptPath(workDir, sessionID string) string {
	return filepath.Join(l.ProjectsDir(), fociroot.CCProjectSlug(workDir), sessionID+".jsonl")
}

// Account is a login location (Dick 2026-11-14, Q11): two folders signed in
// to the same email are two accounts; two spellings of one folder are one.
type Account struct {
	// Keychain is true on macOS, where CC keeps its login in the Keychain.
	Keychain bool
	// Path is, on Linux, the login file <config folder>/.credentials.json.
	// On macOS it is the folder whose hash names the Keychain item, or ""
	// for CC's default item ("Claude Code-credentials"). Account is
	// comparable: two agents share an account exactly when their Accounts
	// are equal.
	Path string
}

// ForAgent resolves the CC folder and account of an agent whose
// backend_config.env is agentEnv (nil for no overrides), on this host.
func ForAgent(agentEnv map[string]string) (Location, error) {
	return FromEnv(Env(agentEnv), runtime.GOOS)
}

// Env is the part of the CC process's environment that decides its folder
// and account: the operator population, then agentEnv (sorted, so the
// result is deterministic). ccstream's buildEnv layers the same two sources
// in the same order, with only foci's own CC defaults (none of which name a
// folder) between them.
func Env(agentEnv map[string]string) []string {
	env := procx.Env(procx.Operator)
	for _, k := range slices.Sorted(maps.Keys(agentEnv)) {
		env = append(env, k+"="+agentEnv[k])
	}
	return env
}

// FromEnv resolves the CC folder and account from a final CC environment
// (KEY=VALUE, later entries win, as in execve) on the given GOOS.
//
// Folder: CLAUDE_CONFIG_DIR when set and non-empty, made absolute; else the
// install root's .claude (fociroot.Dir, read from this process: on a
// single-home install that is CC's own default, ~/.claude, and a
// separate-root install exports CLAUDE_CONFIG_DIR at startup).
//
// Account: on Linux <folder>/.credentials.json. On macOS the Keychain item,
// chosen by CLAUDE_SECURESTORAGE_CONFIG_DIR when it is set and non-empty, the
// default item when it is set and empty, else by the folder when
// CLAUDE_CONFIG_DIR is set, else the default item.
func FromEnv(env []string, goos string) (Location, error) {
	dir, err := configDir(env)
	if err != nil {
		return Location{}, err
	}
	return Location{Dir: dir, Account: account(env, dir, goos)}, nil
}

func configDir(env []string) (string, error) {
	if v, _ := lookup(env, fociroot.CCConfigDirEnv); v != "" {
		return filepath.Abs(v)
	}
	root, err := fociroot.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".claude"), nil
}

func account(env []string, dir, goos string) Account {
	if goos != "darwin" {
		return Account{Path: filepath.Join(dir, ".credentials.json")}
	}
	if v, ok := lookup(env, fociroot.CCSecureStorageEnv); ok {
		if v == "" {
			return Account{Keychain: true}
		}
		if abs, err := filepath.Abs(v); err == nil {
			v = abs
		}
		return Account{Keychain: true, Path: filepath.Clean(v)}
	}
	if v, _ := lookup(env, fociroot.CCConfigDirEnv); v != "" {
		return Account{Keychain: true, Path: dir}
	}
	return Account{Keychain: true}
}

// lookup returns the last value of key in a KEY=VALUE list and whether the
// key is present at all (set-but-empty is present).
func lookup(env []string, key string) (string, bool) {
	prefix := key + "="
	val, ok := "", false
	for _, kv := range env {
		if v, found := strings.CutPrefix(kv, prefix); found {
			val, ok = v, true
		}
	}
	return val, ok
}
