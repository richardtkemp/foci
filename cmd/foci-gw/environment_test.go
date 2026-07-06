package main

import (
	"strings"
	"testing"

	"foci/internal/config"
)

func TestWriteCommandApproval(t *testing.T) {
	var b strings.Builder
	rc := &config.ResolvedAgentConfig{}
	rc.Permissions.AutoApproveCommonReadonly = true
	rc.Permissions.AutoApproveCommonSafeWrite = false
	rc.Permissions.AutoApproveRules = []string{"Bash:gh search", "Bash:git -C /home/rich/git/foci"}
	writeCommandApproval(&b, rc)
	out := b.String()

	for _, want := range []string{
		"## Command Approval",
		"every `foci_*` shell function is always auto-approved",
		"**read-only** (on):",
		"sqlite3 -readonly",           // a rendered read-only rule (Bash: stripped)
		"**safe-write** (off",         // disabled state surfaced
		"**configured for this agent**: gh search, git -C /home/rich/git/foci", // Bash: stripped
	} {
		if !strings.Contains(out, want) {
			t.Errorf("command-approval block missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Bash:") {
		t.Errorf("Bash: prefix should be stripped for prose readability\n---\n%s", out)
	}
}

func TestWriteBackend(t *testing.T) {
	for _, tt := range []struct {
		backend string
		want    string
		absent  string
	}{
		{"claude-code", "Claude Code", "opencode"},
		{"opencode", "opencode", "Claude Code"}, // regression: opencode agents were mislabelled as Claude Code
		{"api", "native API loop", "Claude Code"},
		{"", "native API loop", ""}, // empty backend → api
	} {
		var b strings.Builder
		writeBackend(&b, tt.backend, nil)
		out := b.String()
		if !strings.Contains(out, "## Backend") {
			t.Errorf("backend=%q: missing Backend section", tt.backend)
		}
		if !strings.Contains(out, tt.want) {
			t.Errorf("backend=%q: want %q in\n%s", tt.backend, tt.want, out)
		}
		if tt.absent != "" && strings.Contains(out, tt.absent) {
			t.Errorf("backend=%q: should not contain %q in\n%s", tt.backend, tt.absent, out)
		}
	}
}

func TestWriteBackend_UnknownYieldsNoSection(t *testing.T) {
	var b strings.Builder
	writeBackend(&b, "no-such-backend", nil)
	if b.Len() != 0 {
		t.Errorf("unknown backend with no file should emit nothing, got:\n%s", b.String())
	}
}

func TestWriteCommandApproval_ReadonlyDisabled(t *testing.T) {
	var b strings.Builder
	rc := &config.ResolvedAgentConfig{}
	rc.Permissions.AutoApproveCommonReadonly = false
	writeCommandApproval(&b, rc)
	out := b.String()
	if strings.Contains(out, "**read-only** (on)") {
		t.Error("read-only line should be absent when the allowlist is disabled")
	}
}
