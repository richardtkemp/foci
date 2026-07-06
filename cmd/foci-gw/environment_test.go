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
