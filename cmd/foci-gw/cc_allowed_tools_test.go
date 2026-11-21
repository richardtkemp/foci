package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/delegator"
)

// TestCCLaunchPreapprovesFociConfig proves a Claude Code agent launches with
// `foci config set/unset/add` in --allowedTools (#3232): a permission rule is
// checked before CC's auto-mode classifier, so the classifier cannot refuse
// the call and hide the in-chat approval that gates it. The operator's own
// rules stay, first.
func TestCCLaunchPreapprovesFociConfig(t *testing.T) {
	cfg := &config.Config{CCBackend: config.CCBackendConfig{DefaultAllowedTools: []string{"Read(/tmp/**)"}}}
	cc := delegator.Spec{ConfigFamily: delegator.ConfigFamilyClaudeCode}
	bc := foldGlobalBackendConfig(cfg, cc, config.BackendConfig{AllowedTools: []string{"Bash(git:*)"}})

	if len(bc.AllowedTools) < 2 || bc.AllowedTools[0] != "Read(/tmp/**)" || bc.AllowedTools[1] != "Bash(git:*)" {
		t.Fatalf("operator rules not kept first: %v", bc.AllowedTools)
	}
	launched, _ := bc.ToMap()["allowed_tools"].(string)
	for _, want := range []string{"Bash(foci config set:*)", "Bash(foci config unset:*)", "Bash(foci config add:*)"} {
		if !slices.Contains(strings.Split(launched, ","), want) {
			t.Errorf("allowed_tools %q lacks %s", launched, want)
		}
	}
	// The read-only subcommands need no rule, and must not get one by accident.
	if strings.Contains(launched, "config get") || strings.Contains(launched, "config show") {
		t.Errorf("allowed_tools pre-approves more than the gated writes: %q", launched)
	}
}

// TestCCPromptNamesFociConfigRules proves the environment block's
// "pre-approved by your backend" line lists the builtin rules, so the agent
// is told what the backend applies.
func TestCCPromptNamesFociConfigRules(t *testing.T) {
	got := ccAllowedTools(config.CCBackendConfig{}, nil)
	if !strings.Contains(got, "Bash(foci config set:*)") {
		t.Errorf("ccAllowedTools = %q, want the foci config rules", got)
	}
}

// TestFociCLIPath proves the CLI path is the `foci` file beside the gateway
// binary, and "" when it is missing, a directory, or the executable is unknown.
func TestFociCLIPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "foci-gw")
	at := func() (string, error) { return exe, nil }

	if got := fociCLIPath(at); got != "" {
		t.Errorf("no CLI beside the gateway: got %q, want empty", got)
	}
	if err := os.Mkdir(filepath.Join(dir, "foci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := fociCLIPath(at); got != "" {
		t.Errorf("a directory named foci: got %q, want empty", got)
	}
	if err := os.Remove(filepath.Join(dir, "foci")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foci"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := fociCLIPath(at), filepath.Join(dir, "foci"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := fociCLIPath(func() (string, error) { return "", errors.New("no exe") }); got != "" {
		t.Errorf("unknown executable: got %q, want empty", got)
	}

	comma := filepath.Join(t.TempDir(), "a,b")
	if err := os.Mkdir(comma, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(comma, "foci"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := fociCLIPath(func() (string, error) { return filepath.Join(comma, "foci-gw"), nil }); got != "" {
		t.Errorf("path with a comma: got %q, want empty", got)
	}
}
