// Package shellenv loads a shell rc/env file into the process environment at
// startup, so tool shells spawned by the delegated backends inherit the
// operator's common environment (PATH additions, GOPATH, …) the same way an
// interactive login would — without the service unit having to duplicate those
// values.
//
// Rationale: the Bash-tool shells the CC/opencode backends spawn are
// non-interactive+non-login, so bash sources only $BASH_ENV — never .bashrc or
// .profile. This package bridges that gap by capturing the rc file's exports
// once and applying them to foci-gw's own environment, which every spawned
// process then inherits via os.Environ(). A per-agent backend_config.env is
// still appended at spawn time, so it overrides these values on collision.
package shellenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
		if _, err := exec.LookPath("zsh"); err == nil {
			shell = "zsh"
		}
	}
	// NUL-delimit so values containing newlines survive parsing.
	cmd := procx.Spawn(context.Background(), shell, "-c", ". "+shellQuote(path)+" >/dev/null 2>&1; env -0")
	cmd.Env = os.Environ()
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

// Apply resolves the configured file, captures it, and sets each variable on
// the current process (overriding the inherited value on collision — the rc
// file is tier 2, above the inherited base). No-op when nothing resolves.
func Apply(cfg *string) {
	home, err := os.UserHomeDir()
	if err != nil {
		shellenvLog.Warnf("cannot resolve home dir: %v", err)
		return
	}
	path, load := Resolve(cfg, home)
	if !load {
		shellenvLog.Debugf("no shell env file loaded (cfg=%v)", derefOr(cfg, "<ladder>"))
		return
	}
	env, err := Capture(path)
	if err != nil {
		shellenvLog.Warnf("capture %s failed: %v", path, err)
		return
	}
	for k, v := range env {
		_ = os.Setenv(k, v)
	}
	shellenvLog.Infof("loaded %d vars from %s", len(env), path)
}

func derefOr(p *string, dflt string) string {
	if p == nil {
		return dflt
	}
	return *p
}
