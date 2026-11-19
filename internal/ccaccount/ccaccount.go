// Package ccaccount answers two questions about a Claude Code process foci
// starts (#3104, #3186):
//
//   - its CC config folder: where that CC keeps transcripts (projects/),
//     settings.json and, on Linux, its login;
//   - its login location: where that CC reads its login.
//
// Both come from the FINAL environment foci hands the CC process: the
// operator population (the daemon env overlaid with the operator's captured
// shell env, procx.Operator), then the launch env of the session (the agent's
// backend_config.env and the env of the session's account, FolderEnv), later
// values winning, as in ccstream's buildEnv. A session on the host's account
// adds no folder, so it gets the host's folder (fociroot.CCConfigDir, or a
// CLAUDE_CONFIG_DIR exported by the operator's shell rc file, because that is
// the folder CC is given). A session on an account with its own folder
// (backend_config.accounts) gets that folder.
//
// This is the one definition. An account's location is ForFolder; ccstream
// resolves the location of the process it runs with ForLaunchEnv, and the two
// agree because the account's FolderEnv is part of that launch env.
package ccaccount

import (
	"crypto/sha256"
	"encoding/hex"
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

// Key identifies the account in maps and file names: equal Accounts have
// equal Keys, and different Accounts different ones.
func (a Account) Key() string {
	if a.Keychain {
		return "keychain:" + a.Path
	}
	return "file:" + a.Path
}

// ShortKey is a short stable hash of Key, for names that must stay short (a
// tmux session, a state file).
func (a Account) ShortKey() string {
	sum := sha256.Sum256([]byte(a.Key()))
	return hex.EncodeToString(sum[:4])
}

// String names the account by its location, for a person (Dick 2026-11-14,
// Q15: the email when it is known, this path as the fallback): the config
// folder on Linux, the folder that names the Keychain item on macOS, or
// "the default Keychain sign-in".
func (a Account) String() string {
	switch {
	case a.Keychain && a.Path == "":
		return "the default Keychain sign-in"
	case a.Keychain:
		return a.Path
	}
	return filepath.Dir(a.Path)
}

// Host is the location of the host's Claude Code account: the folder and
// login of a CC process started with no folder of its own.
func Host() (Location, error) { return ForLaunchEnv(nil) }

// ForFolder is the location of the account whose CC config folder is dir
// (one backend_config.accounts entry). "" and the host's own folder (by any
// spelling, a symlink included) are the host's account, so listing the
// host's folder, alone or beside other folders, never makes a second login
// of it (#3186 F11). That matters on macOS: CC given CLAUDE_CONFIG_DIR
// names its Keychain item after that folder, so the host's folder passed
// as a folder of its own would read an empty item, not the host's login.
func ForFolder(dir string) (Location, error) { return forFolder(dir, runtime.GOOS) }

func forFolder(dir, goos string) (Location, error) {
	host, err := FromEnv(Env(nil), goos)
	if err != nil || dir == "" {
		return host, err
	}
	loc, err := FromEnv(Env(FolderEnv(dir)), goos)
	if err != nil {
		return Location{}, err
	}
	if sameFolder(loc.Dir, host.Dir) {
		return host, nil
	}
	return loc, nil
}

// sameFolder reports whether a and b (absolute, cleaned) name one folder:
// equal, or equal once symlinks are resolved.
func sameFolder(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// FolderEnv is what a CC process must be given, on top of the operator's
// environment, to run on the account whose folder is dir: the account's part
// of a session's launch env.
func FolderEnv(dir string) map[string]string {
	return map[string]string{fociroot.CCConfigDirEnv: dir}
}

// ForLaunchEnv resolves the CC folder and login location of a CC process
// whose launch env (what foci adds to the operator's environment) is
// launchEnv, nil for none, on this host.
func ForLaunchEnv(launchEnv map[string]string) (Location, error) {
	return FromEnv(Env(launchEnv), runtime.GOOS)
}

// Env is the part of the CC process's environment that decides its folder
// and account: the operator population, then agentEnv (sorted, so the
// result is deterministic). ccstream's buildEnv layers the same two sources
// in the same order, with only foci's own CC defaults (none of which name a
// folder) between them.
//
// It applies OwnLogin, as buildEnv does.
func Env(agentEnv map[string]string) []string {
	env := procx.Env(procx.Operator)
	for _, k := range slices.Sorted(maps.Keys(agentEnv)) {
		env = append(env, k+"="+agentEnv[k])
	}
	return OwnLogin(env, agentEnv)
}

// OwnLogin gives an account with its own config folder its own login (Dick
// 2026-11-14, #3104 Q12, "Its own login"). On a separate-root install the
// gateway exports CLAUDE_SECURESTORAGE_CONFIG_DIR="" (fociroot.EnsureCCConfigEnv)
// so that foci's CC keeps the user's default Keychain login, and every CC
// child inherits it. An agent whose backend_config.env sets CLAUDE_CONFIG_DIR
// and not CLAUDE_SECURESTORAGE_CONFIG_DIR must not get it: CC then names the
// Keychain item after the agent's folder. So env (a final CC environment
// built from the operator population and agentEnv) loses every inherited
// CLAUDE_SECURESTORAGE_CONFIG_DIR entry in that case. An agent env that sets
// CLAUDE_SECURESTORAGE_CONFIG_DIR (to a path or to "") keeps its value, and an
// agent with no CLAUDE_CONFIG_DIR of its own is left as today. A map can
// override a variable but cannot unset it, which is why this is code.
func OwnLogin(env []string, agentEnv map[string]string) []string {
	if !ownFolderOnly(agentEnv) {
		return env
	}
	prefix := fociroot.CCSecureStorageEnv + "="
	return slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, prefix) })
}

// ownFolderOnly reports whether agentEnv names its own CC config folder and
// leaves the secure-storage folder to CC (OwnLogin).
func ownFolderOnly(agentEnv map[string]string) bool {
	if agentEnv[fociroot.CCConfigDirEnv] == "" {
		return false
	}
	_, set := agentEnv[fociroot.CCSecureStorageEnv]
	return !set
}

// LoginEnv is what a Claude Code process started OUTSIDE foci's environment
// must be told so that it reads and writes this agent's login: the relogin
// pane's claude runs under a tmux server that may predate the gateway (#2580).
// It starts from the host's variables (fociroot.CCEnv: HOME on a separate
// root, the gateway's CLAUDE_CONFIG_DIR and CLAUDE_SECURESTORAGE_CONFIG_DIR),
// so for an agent with no override it is exactly what main gave the pane.
// The agent's backend_config.env then replaces CLAUDE_CONFIG_DIR and
// CLAUDE_SECURESTORAGE_CONFIG_DIR where it sets them, and for an agent that
// gets its own login (OwnLogin) the bare name "CLAUDE_SECURESTORAGE_CONFIG_DIR"
// (no "=") asks the caller to UNSET it, as the tmux server may carry the
// gateway's empty value.
func LoginEnv(agentEnv map[string]string) []string {
	out := fociroot.CCEnv()
	set := func(key, kv string) {
		out = slices.DeleteFunc(out, func(e string) bool { return e == key || strings.HasPrefix(e, key+"=") })
		out = append(out, kv)
	}
	if v := agentEnv[fociroot.CCConfigDirEnv]; v != "" {
		set(fociroot.CCConfigDirEnv, fociroot.CCConfigDirEnv+"="+v)
	}
	if v, ok := agentEnv[fociroot.CCSecureStorageEnv]; ok {
		set(fociroot.CCSecureStorageEnv, fociroot.CCSecureStorageEnv+"="+v)
	} else if ownFolderOnly(agentEnv) {
		set(fociroot.CCSecureStorageEnv, fociroot.CCSecureStorageEnv)
	}
	return out
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
