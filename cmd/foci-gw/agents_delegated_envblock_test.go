package main

import (
	"strings"
	"testing"

	"foci/internal/provider"
)

// TestDelegatedSystemPrompt_PrependsEnvBlock locks in that the per-agent
// environment block is prepended ahead of the workspace + skills blocks in the
// single concatenated string CC-backend agents launch with. Empty env => no
// leading separator.
func TestDelegatedSystemPrompt_PrependsEnvBlock(t *testing.T) {
	t.Parallel()

	ws := []provider.SystemBlock{{Type: "text", Text: "CRAFT content"}}
	extra := []provider.SystemBlock{{Type: "text", Text: "Available Skills: foo"}}
	env := "# Environment\n\n## Foci Shell Tools\n- `foci_todo` — manage todos"

	got := delegatedSystemPrompt(env, ws, extra)

	if !strings.Contains(got, "foci_todo") {
		t.Errorf("env block (with foci_todo) missing from prompt:\n%s", got)
	}
	envIdx := strings.Index(got, "# Environment")
	craftIdx := strings.Index(got, "CRAFT content")
	if !(envIdx >= 0 && envIdx < craftIdx) {
		t.Errorf("env block must precede workspace; env=%d craft=%d", envIdx, craftIdx)
	}

	// Empty env block: no leading separator, base unchanged.
	if got := delegatedSystemPrompt("", ws, extra); strings.HasPrefix(got, "\n") {
		t.Errorf("empty env should not add leading newline, got %q", got)
	}
}

// TestNewDelegatedSystemPromptFunc_RetainsEnvBlock is the CC-backend sister to
// TestEnvironmentBlockPrepended (which covers the API path). It proves the
// per-session prompt-rebuild closure — which WINS over the static
// StartOptions.SystemPrompt at every session start (#828/#706) — re-prepends
// the environment block. The #828/#706 disk-reload fix rebuilt the prompt from
// workspace+skills only, silently dropping the env block (and with it the
// "Foci Shell Tools" list), so CC agents never saw foci_todo etc. This guards
// that regression.
func TestNewDelegatedSystemPromptFunc_RetainsEnvBlock(t *testing.T) {
	t.Parallel()

	env := "# Environment\n\n## Backend\nYou run inside Claude Code.\n\n## Foci Shell Tools\n- `foci_todo` — manage todos"
	reload := func() (ws, extra []provider.SystemBlock) {
		return []provider.SystemBlock{{Type: "text", Text: "CRAFT content"}},
			[]provider.SystemBlock{{Type: "text", Text: "Available Skills: foo"}}
	}

	fn := newDelegatedSystemPromptFunc(env, reload)
	got := fn()

	for _, want := range []string{"## Backend", "## Foci Shell Tools", "foci_todo", "CRAFT content", "Available Skills"} {
		if !strings.Contains(got, want) {
			t.Errorf("rebuilt prompt missing %q:\n%s", want, got)
		}
	}
}
