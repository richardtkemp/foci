// Package shellenv captures a shell rc/env file's exports at startup, so the
// tool shells spawned by the delegated backends get the operator's common
// environment (PATH additions, GOPATH, …) the same way an interactive login
// would — without the service unit having to duplicate those values.
//
// Rationale: the Bash-tool shells the CC/opencode backends spawn are
// non-interactive+non-login, so bash sources only $BASH_ENV — never .bashrc or
// .profile. This package bridges that gap.
//
// It captures the rc file ONCE, as a VALUE, and does NOT apply it to
// foci-gw's own environment (#1914). Applying it was the defect: there is one
// environment serving two consumers with opposite requirements — foci's own
// machinery, which needs a trusted PATH no agent can influence, and the agent
// shells, which need the operator's full PATH — and mutating the process
// global let the agent-facing side win, so the daemon inherited
// agent-writable directories ahead of /usr/bin.
//
// The captured value is handed to procx.SetOperatorEnv and read back by name
// at each spawn site that declares procx.Operator. A per-agent
// backend_config.env is still appended at spawn time, so it overrides these
// values on collision.
package shellenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/log"
	"foci/internal/procx"
)

var (
	shellenvLog = log.NewComponentLogger("shellenv")
)

// ladder is the default search order when no explicit file is configured. The
// first file that exists is loaded; at most one is ever loaded. Ordered
// bash-first because the delegated backends' tool shells are bash; .zshenv is
// zsh's always-sourced file (the correct non-interactive zsh equivalent);
// .profile is the POSIX-sh fallback.
var ladder = []string{".bashrc", ".zshenv", ".profile"}

// capture-time environment noise the sourcing shell adds that must not leak
// into the process environment.
var skipVars = map[string]bool{"_": true, "SHLVL": true, "PWD": true, "OLDPWD": true}

// Resolve decides which file to load from the config value and home directory.
//
//   - cfg == nil  → ladder: first existing of ~/.bashrc, ~/.zshenv, ~/.profile
//   - *cfg == ""  → load nothing (operator blanked it out)
//   - *cfg != ""  → that exact path (leading ~ expanded), always
//
// Returns ("", false) when nothing should be loaded.
func Resolve(cfg *string, home string) (string, bool) {
	if cfg != nil {
		if *cfg == "" {
			return "", false
		}
		return expandHome(*cfg, home), true
	}
	for _, name := range ladder {
		p := filepath.Join(home, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Capture sources path in a subshell and returns the resulting environment as
// KEY→VALUE, minus shell-internal noise. .zshenv is sourced with zsh when
// available (its exports may use zsh syntax); everything else with bash.
func Capture(path string) (map[string]string, error) {
	shell := "bash"
	if strings.HasSuffix(path, "zshenv") {
		// Trusted, for the same reason the capture spawn below is: the shell
		// that derives the operator env must not be chosen by it.
		if _, err := procx.LookPath(procx.Trusted, "zsh"); err == nil {
			shell = "zsh"
		}
	}
	// NUL-delimit so values containing newlines survive parsing.
	// Trusted, and load-bearing: this subshell DERIVES the operator population.
	// Resolving it on the operator PATH would let an agent-planted `bash`
	// dictate the very environment the guard then predicts — a cycle with no
	// floor. The capture shell must come from the unit's PATH.
	cmd := procx.Spawn(context.Background(), procx.Trusted, shell, "-c", ". "+shellQuote(path)+" >/dev/null 2>&1; env -0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("source %s via %s: %w", path, shell, err)
	}
	m := make(map[string]string)
	for _, kv := range bytes.Split(out, []byte{0}) {
		if len(kv) == 0 {
			continue
		}
		eq := bytes.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := string(kv[:eq])
		if skipVars[k] {
			continue
		}
		m[k] = string(kv[eq+1:])
	}
	return m, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Load resolves the configured file and captures it, returning the operator's
// environment AS A VALUE. It does not touch this process's environment.
//
// This is the one derivation (#1914). The caller hands the result to
// procx.SetOperatorEnv, which is what every operator-facing spawn site then
// reads by name — so the environment agents get and the environment
// internal/execguard predicts cannot drift apart, because they are the same
// value.
//
// Returns (nil, "", false) when nothing should be loaded (no rc file
// configured or found) or when the capture failed; both are ordinary,
// supported states in which Operator and Trusted are simply identical. A
// capture failure is logged at WARN, a no-op at DEBUG.
func Load(cfg *string) (map[string]string, string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		shellenvLog.Warnf("cannot resolve home dir: %v", err)
		return nil, "", false
	}
	path, load := Resolve(cfg, home)
	if !load {
		shellenvLog.Debugf("no shell env file loaded (cfg=%v)", derefOr(cfg, "<ladder>"))
		return nil, "", false
	}
	env, err := Capture(path)
	if err != nil {
		shellenvLog.Warnf("capture %s failed: %v", path, err)
		return nil, path, false
	}
	return env, path, true
}

func derefOr(p *string, dflt string) string {
	if p == nil {
		return dflt
	}
	return *p
}
