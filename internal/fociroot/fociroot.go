// Package fociroot answers "where is foci installed?" separately from "where
// is the user's home?" (#2580).
//
// On Linux foci runs as its own `foci` user, so the install root (config/,
// data/, logs/, shared/, bin/, agent workspaces, relative config paths) IS
// $HOME and nothing needs configuring. On macOS foci runs as the login user,
// and the install root (~/foci) is not that user's home. Setting HOME to the
// install root there made every tool foci starts (codex, opencode, gh, git,
// ssh, Homebrew tools) miss the user's real logins and config, so instead the
// LaunchAgent sets FOCI_HOME to the install root and leaves HOME alone.
//
// The rule for callers: a path that is part of foci's install comes from
// Dir(); a path that means "the user's home" (a `~` an agent typed, the
// operator's shell rc file, where Claude Code's updater lives) stays
// os.UserHomeDir(). Claude Code's own config folder is neither: foci keeps
// it under the install root (CCConfigDir), separate from the user's
// interactive ~/.claude.
//
// Stdlib-only leaf package.
package fociroot

import (
	"os"
	"path/filepath"
)

// EnvVar names the environment variable that sets foci's install root.
const EnvVar = "FOCI_HOME"

// ccConfigEnv is Claude Code's own variable for its config folder (settings,
// .claude.json, projects/ transcripts, plugins). CC falls back to ~/.claude.
const ccConfigEnv = "CLAUDE_CONFIG_DIR"

// Dir returns foci's install root: $FOCI_HOME when set (made absolute), else
// the user's home directory — exactly the old behaviour, so a host that sets
// nothing (every Linux install) is unchanged.
func Dir() (string, error) {
	if v := os.Getenv(EnvVar); v != "" {
		return filepath.Abs(v)
	}
	return os.UserHomeDir()
}

// Separate reports whether the install root differs from the user's home,
// i.e. FOCI_HOME is set to somewhere other than $HOME. False on any error:
// the caller then keeps the single-home behaviour.
func Separate() bool {
	root, err := Dir()
	if err != nil {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return filepath.Clean(root) != filepath.Clean(home)
}

// CCConfigDir returns the config folder of the Claude Code that foci runs:
// $CLAUDE_CONFIG_DIR when set (that is the folder CC itself uses), else
// <install root>/.claude. On a single-home install that is ~/.claude, as
// before. Everything foci reads of CC's on-disk state (projects/ transcripts,
// settings.json, .credentials.json) is under it.
func CCConfigDir() (string, error) {
	if v := os.Getenv(ccConfigEnv); v != "" {
		return filepath.Abs(v)
	}
	root, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".claude"), nil
}

// EnsureCCConfigEnv points the Claude Code foci starts at its own config
// folder when the install root is separate from HOME: it sets
// CLAUDE_CONFIG_DIR=<install root>/.claude in this process's environment,
// which every spawn inherits (procx derives child environments from
// os.Environ at spawn time). A CLAUDE_CONFIG_DIR already set is left alone,
// and so is a single-home install: there CC's default (~/.claude, with its
// state file at ~/.claude.json) is already foci's own, and setting the
// variable would move that state file into the folder. Returns the value set,
// or "" when nothing was set.
func EnsureCCConfigEnv() (string, error) {
	if os.Getenv(ccConfigEnv) != "" || !Separate() {
		return "", nil
	}
	dir, err := CCConfigDir()
	if err != nil {
		return "", err
	}
	return dir, os.Setenv(ccConfigEnv, dir)
}

// CCEnv returns the KEY=VALUE pairs a Claude Code process needs to see when
// it is started through something that does NOT inherit this process's
// environment (a tmux server that may predate it): CLAUDE_CONFIG_DIR when set,
// and HOME when the install root is separate (an older tmux server may still
// carry HOME=<install root>). Nil on a single-home install with no
// CLAUDE_CONFIG_DIR, so the caller adds nothing there.
func CCEnv() []string {
	var env []string
	if Separate() {
		if home, err := os.UserHomeDir(); err == nil {
			env = append(env, "HOME="+home)
		}
	}
	if v := os.Getenv(ccConfigEnv); v != "" {
		env = append(env, ccConfigEnv+"="+v)
	}
	return env
}
