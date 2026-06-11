package mcp

import (
	"strings"
	"testing"
)

// envHas reports whether env contains the given KEY=VALUE entry.
func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// envHasKey reports whether env contains any entry for the given key name.
func envHasKey(env []string, name string) bool {
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// Proves the MCP env allowlist drops sensitive gateway vars (notably the
// FOCI_*_SOCK control-socket paths) while keeping allowlisted basics like PATH
// and passing through the explicit mcp.toml `env` extras — so a third-party MCP
// server can't reach the gateway's internal sockets through inherited env.
func TestAllowlistedEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/foci")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("FOCI_GW_SOCK", "/run/foci/gw.sock")
	t.Setenv("FOCI_SOCK", "/run/foci/exec.sock")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")

	env := allowlistedEnv([]string{"MY_SERVER_TOKEN=abc"})

	// Allowlisted vars survive.
	if !envHasKey(env, "PATH") {
		t.Error("PATH should be inherited by MCP servers")
	}
	if !envHasKey(env, "HOME") {
		t.Error("HOME should be inherited by MCP servers")
	}
	if !envHasKey(env, "LC_ALL") {
		t.Error("LC_* vars should be inherited (locale)")
	}

	// Sensitive vars are dropped.
	for _, name := range []string{"FOCI_GW_SOCK", "FOCI_SOCK", "ANTHROPIC_API_KEY"} {
		if envHasKey(env, name) {
			t.Errorf("%s must not be passed to MCP servers", name)
		}
	}

	// Explicit mcp.toml env passes through verbatim.
	if !envHas(env, "MY_SERVER_TOKEN=abc") {
		t.Error("explicit mcp.toml env should be passed through")
	}
}
