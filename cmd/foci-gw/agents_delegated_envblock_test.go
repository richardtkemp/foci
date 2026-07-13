package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// wiringFixture builds the minimal setupParams + a temp workspace needed to
// drive the real configureDelegated end-to-end. The workspace holds a single
// character file so the bootstrap produces a non-empty workspace block (lets us
// assert env-precedes-workspace ordering). Environment is enabled so the env
// block is built.
// charFileMarkers are the per-file sentinels seeded into the fixture workspace,
// one per default character file. Each must survive into both prompt paths so a
// regression that drops any character file (not just CRAFT) is caught.
var charFileMarkers = map[string]string{
	"character/SOUL.md":      "MARKER-SOUL",
	"character/COHERENCE.md": "MARKER-COHERENCE",
	"character/CRAFT.md":     "WORKSPACE-MARKER",
	"character/USER.md":      "MARKER-USER",
	"character/MEMORY.md":    "MARKER-MEMORY",
}

func wiringFixture(t *testing.T) (setupParams, *sharedAgentSetup) {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "character"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, marker := range charFileMarkers {
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(marker+" content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolved := &config.ResolvedAgentConfig{}
	p := setupParams{
		acfg:         config.AgentConfig{ID: "wiretester", Workspace: ws},
		cfg:          &config.Config{},
		resolved:     resolved,
		resolvedLive: config.NewLiveValue(resolved),
		store:        &secrets.Store{},
		connMgr:      stubConnMgr{},
		plat:         &platform.Messaging{},
	}
	p.resolved.Environment.Enabled = true
	return p, &sharedAgentSetup{wakeScheduleFn: stubWakeFn}
}

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

	buildEnv := func(string) string { return env }
	platformFor := func(string) string { return "" }
	fn := newDelegatedSystemPromptFunc(buildEnv, platformFor, reload)
	got := fn("test/c1")

	for _, want := range []string{"## Backend", "## Foci Shell Tools", "foci_todo", "CRAFT content", "Available Skills"} {
		if !strings.Contains(got, want) {
			t.Errorf("rebuilt prompt missing %q:\n%s", want, got)
		}
	}
}

// TestConfigureDelegated_WiresEnvBlockIntoBothPromptPaths is the real-wiring
// guard the helper tests above cannot provide: it drives the actual
// configureDelegated and asserts the StartOptions it builds carry the env block
// on BOTH prompt sources — the static SystemPrompt AND the per-session
// SystemPromptFunc() rebuild. The original bug was a wiring bug: the rebuild
// closure existed but reconstructed the prompt without the env block, and the
// rebuild wins at every session start. Helper-level tests stay green through
// such a regression because they never exercise configureDelegated's wiring;
// this test fails if either path is wired to drop the env block.
func TestConfigureDelegated_WiresEnvBlockIntoBothPromptPaths(t *testing.T) {
	p, shared := wiringFixture(t)
	ag := &agent.Agent{}

	if _, ok := configureDelegated(ag, p, shared, "claude-code", map[string]any{}); !ok {
		t.Fatal("configureDelegated returned ok=false")
	}
	if ag.DelegatedManager == nil {
		t.Fatal("configureDelegated did not set ag.DelegatedManager")
	}

	static := ag.DelegatedManager.StartOpts.SystemPrompt
	fn := ag.DelegatedManager.StartOpts.SystemPromptFunc
	if fn == nil {
		t.Fatal("StartOpts.SystemPromptFunc is nil — rebuild path unwired")
	}
	rebuilt := fn("test/c1")

	// Both paths must carry the env block (## Backend + shell-tools list) and
	// EVERY character file's identity content, not just CRAFT. foci_send_to_chat
	// is an unconditional core exec tool, so the shell-tools section is always
	// populated.
	wants := []string{"## Backend", "## Foci Shell Tools", "foci_send_to_chat", "jobs scheduled"}
	for _, marker := range charFileMarkers {
		wants = append(wants, marker)
	}
	for _, label := range []struct {
		name string
		got  string
	}{{"static SystemPrompt", static}, {"rebuilt SystemPromptFunc()", rebuilt}} {
		for _, want := range wants {
			if !strings.Contains(label.got, want) {
				t.Errorf("%s missing %q:\n%s", label.name, want, label.got)
			}
		}
		// Env block must precede the workspace identity in both.
		if be, wm := strings.Index(label.got, "## Backend"), strings.Index(label.got, "WORKSPACE-MARKER"); be >= 0 && wm >= 0 && be > wm {
			t.Errorf("%s: env block must precede workspace (backend=%d workspace=%d)", label.name, be, wm)
		}
	}
}

// TestConfigureDelegated_BothPromptPathsAgree encodes the exact property that
// broke: the static SystemPrompt and the per-session rebuild diverged (static
// had the env block, the rebuild dropped it). With the workspace unchanged on
// disk, the two paths MUST produce identical output. A future change that makes
// the rebuild reconstruct the prompt differently from setup-time fails here.
func TestConfigureDelegated_BothPromptPathsAgree(t *testing.T) {
	p, shared := wiringFixture(t)
	ag := &agent.Agent{}

	if _, ok := configureDelegated(ag, p, shared, "claude-code", map[string]any{}); !ok {
		t.Fatal("configureDelegated returned ok=false")
	}
	so := ag.DelegatedManager.StartOpts
	if so.SystemPromptFunc == nil {
		t.Fatal("StartOpts.SystemPromptFunc is nil")
	}
	if got := so.SystemPromptFunc("test/c1"); got != so.SystemPrompt {
		t.Errorf("static SystemPrompt and rebuilt SystemPromptFunc() diverge with unchanged disk:\n--- static ---\n%s\n--- rebuilt ---\n%s", so.SystemPrompt, got)
	}
}
