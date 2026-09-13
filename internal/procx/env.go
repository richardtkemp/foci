package procx

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// # Two populations, one derivation
//
// foci-gw spawns children for two purposes that want OPPOSITE environments:
//
//  1. THE DAEMON'S OWN MACHINERY — sh, bash, git, tmux, sudo, systemctl,
//     pandoc, the shell that captures the operator env. These are foci's
//     tools, not the user's. They must resolve on a TRUSTED path that no
//     agent can write, or an agent that drops a `bash` into an agent-writable
//     PATH directory gets it executed BY THE DAEMON — with no approval and no
//     guard, because the guard governs only the Bash tool.
//
//  2. AGENT AND TOOL SHELLS — Claude Code, opencode, codex, the Bash tool,
//     the Tmux tool, the explore tools. These must get the OPERATOR'S FULL
//     environment. That is the point of them: agents should have the tools
//     the operator installed.
//
// Before #1914 there was one environment serving both: shellenv.Apply()
// os.Setenv'd the operator's dotfile environment onto the daemon's own
// process, so (2) won and (1) silently inherited agent-writable directories.
//
// Population makes the choice EXPLICIT AT EVERY SPAWN SITE. It is a required
// argument to Spawn/SpawnSetsid rather than a default, because a default is
// how the old behaviour stayed invisible: `procx.Spawn(ctx, "bash", ...)`
// said nothing about which environment it wanted, so nobody had to decide.
//
// # What a Population controls
//
// Two things, and they are genuinely separate:
//
//   - BINARY RESOLUTION for a bare name. Note that os/exec resolves a bare
//     name against the PARENT's $PATH (os.Getenv), NOT against cmd.Env — so
//     setting cmd.Env alone would not change which file executes. Spawn
//     therefore re-resolves the name against the population's own PATH.
//   - THE CHILD'S ENVIRONMENT (cmd.Env), which is what the child and its own
//     descendants then see.
//
// # Where the values come from
//
//   - Trusted  = the daemon's own process environment, i.e. what systemd gave
//     us (deploy/foci.service.tmpl Environment=PATH, rendered from the
//     Makefile's SERVICE_PATH) plus anything foci itself installs into its own
//     process on purpose (preload.Apply's LD_PRELOAD).
//   - Operator = Trusted, overlaid with the variables captured from the
//     operator's shell rc file by internal/shellenv.
//
// Both are derived from os.Environ() at spawn time rather than snapshotted at
// startup, so a variable foci sets on itself later (LD_PRELOAD is set AFTER
// the shellenv capture in main()) reaches both populations.

// Population names which of the two environments a spawn wants. The zero
// value is deliberately INVALID: a caller that forgets to decide gets a loud
// failure, not a silent default. Use Trusted or Operator.
type Population struct{ kind populationKind }

type populationKind int

const (
	popInvalid populationKind = iota
	popTrusted
	popOperator
)

var (
	// Trusted is foci's own machinery: the daemon's process environment,
	// untouched by the operator's dotfiles. Bare names resolve on the unit's
	// PATH, which no agent may write.
	Trusted = Population{kind: popTrusted}

	// Operator is the agent-facing population: the operator's full shell
	// environment. Bare names resolve on the operator's PATH, and the child
	// receives it, so agents get the tools the operator installed.
	Operator = Population{kind: popOperator}
)

// String names the population for logs and error messages.
func (p Population) String() string {
	switch p.kind {
	case popTrusted:
		return "trusted"
	case popOperator:
		return "operator"
	default:
		return "invalid"
	}
}

// Valid reports whether p is one of the two real populations (i.e. not the
// zero value).
func (p Population) Valid() bool { return p.kind == popTrusted || p.kind == popOperator }

// operatorOverlay holds the variables captured from the operator's shell rc
// file, as a VALUE — never applied to this process. nil until
// SetOperatorEnv runs (or when nothing resolved to load), in which case
// Operator and Trusted are identical.
var (
	operatorMu      sync.RWMutex
	operatorOverlay []string // KEY=VALUE, sorted for determinism
)

// SetOperatorEnv records the operator's captured shell environment as the
// overlay that distinguishes Operator from Trusted. Called once at startup
// from cmd/foci-gw with the map internal/shellenv captured. Passing nil or an
// empty map collapses the two populations, which is the correct behaviour
// when no rc file was loaded.
//
// It lives here, rather than in shellenv, because procx is a leaf package
// (stdlib + internal/log only) that every spawn site can already import;
// shellenv imports procx to run its capture subshell, so the dependency
// cannot go the other way.
func SetOperatorEnv(env map[string]string) {
	kv := make([]string, 0, len(env))
	for k, v := range env {
		kv = append(kv, k+"="+v)
	}
	slices.Sort(kv)
	operatorMu.Lock()
	operatorOverlay = kv
	operatorMu.Unlock()
}

// OperatorOverlay returns a copy of the captured operator overlay (KEY=VALUE),
// for diagnostics. Empty when nothing was captured.
func OperatorOverlay() []string {
	operatorMu.RLock()
	defer operatorMu.RUnlock()
	return append([]string(nil), operatorOverlay...)
}

// Env returns the population's environment as a fresh slice of KEY=VALUE
// pairs, suitable for cmd.Env. This is the ONE derivation: every caller that
// needs to build a modified environment for a child should start from here
// rather than from os.Environ(), so the population's identity travels with
// the value.
func Env(p Population) []string {
	base := os.Environ()
	if p.kind != popOperator {
		return base
	}
	operatorMu.RLock()
	overlay := operatorOverlay
	operatorMu.RUnlock()
	if len(overlay) == 0 {
		return base
	}
	// Later entries win in execve, so appending the overlay is enough to
	// override the inherited value — the same layering shellenv.Apply used to
	// get by calling os.Setenv.
	return append(base, overlay...)
}

// PathDirs returns the population's $PATH split into directories. This is what
// a bare command name resolves against for that population, and it is what
// internal/execguard must judge when it predicts what an agent shell will run.
func PathDirs(p Population) []string {
	return filepath.SplitList(envValue(Env(p), "PATH"))
}

// envValue returns the last value of key in a KEY=VALUE list (last wins, as
// execve does), or "" if absent.
func envValue(env []string, key string) string {
	prefix := key + "="
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
		}
	}
	return val
}

// LookPath resolves name against the population's PATH.
//
// It exists because exec.LookPath consults os.Getenv("PATH") — the DAEMON's
// PATH — which is exactly the conflation #1914 removes. A site that asks "is
// rg installed?" on behalf of an agent must ask about the agent's PATH; a site
// that resolves `systemctl` for foci itself must ask about the trusted one.
//
// Semantics match exec.LookPath: a name containing a separator is returned as
// given (after an executability check), and a failure is an *exec.Error
// wrapping exec.ErrNotFound so callers' errors.Is checks keep working.
func LookPath(p Population, name string) (string, error) {
	return lookPathIn(PathDirs(p), name)
}

func lookPathIn(dirs []string, name string) (string, error) {
	if strings.Contains(name, string(os.PathSeparator)) {
		if err := findExecutable(name); err != nil {
			return "", &exec.Error{Name: name, Err: err}
		}
		return name, nil
	}
	for _, dir := range dirs {
		if dir == "" {
			dir = "." // Unix shell semantics: an empty PATH entry means cwd.
		}
		path := filepath.Join(dir, name)
		if err := findExecutable(path); err == nil {
			if !filepath.IsAbs(path) {
				return path, &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// xOK is access(2)'s X_OK. Spelled out rather than pulled from
// golang.org/x/sys/unix so this package stays stdlib-only (see the package
// doc's "living outside internal/tools" note).
const xOK = 0x1

// findExecutable mirrors os/exec's unix findExecutable: a regular file this
// process may execute. access(2) (not a mode-bit reconstruction) so ACLs,
// supplementary groups and read-only mounts are accounted for — the same
// reason internal/execguard uses it.
func findExecutable(file string) error {
	d, err := os.Stat(file)
	if err != nil {
		return err
	}
	if d.IsDir() {
		return syscall.EISDIR
	}
	if err := syscall.Access(file, xOK); err != nil {
		return &fs.PathError{Op: "access", Path: file, Err: err}
	}
	return nil
}
