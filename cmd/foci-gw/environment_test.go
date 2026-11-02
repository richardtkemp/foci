package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/session"
)

func TestBuildEnvironmentDelegated_SkipPermissionsOmitsApproval(t *testing.T) {
	t.Parallel()
	base := config.AgentConfig{ID: "x", Workspace: "/tmp/x", Backend: "claude-code"}
	cfg := &config.Config{Logging: config.LoggingConfig{EventFile: "/tmp/foci.log"}}

	rc := config.Resolve(cfg, base)
	normal := buildEnvironmentDelegated(base, "/tmp/foci.toml", cfg, rc, rc.Permissions, 0, nil, nil, nil, "")
	if !strings.Contains(normal, "## Command Approval") {
		t.Fatal("expected Command Approval section for a normal claude-code agent")
	}

	skip := base
	skipPerm := true
	skip.BackendConfig = config.BackendConfig{SkipPermissions: &skipPerm}
	skipRC := config.Resolve(cfg, skip)
	skipped := buildEnvironmentDelegated(skip, "/tmp/foci.toml", cfg, skipRC, skipRC.Permissions, 0, nil, nil, nil, "")
	if strings.Contains(skipped, "## Command Approval") {
		t.Error("skip_permissions should omit the Command Approval section (everything is permitted)")
	}
}

// TestBuildEnvironmentDelegated_ApprovalFollowsSpec: every backend that
// enforces foci's auto-approve allowlist gets the Command Approval section
// (#2154), and only the Claude Code family lists the --allowedTools layer.
func TestBuildEnvironmentDelegated_ApprovalFollowsSpec(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Logging: config.LoggingConfig{EventFile: "/tmp/foci.log"}}
	cfg.CCBackend.DefaultAllowedTools = []string{"Edit(/tmp/**)"}
	for _, tc := range []struct {
		backend         string
		wantPreApproved bool
	}{
		{"claude-code", true},
		{"opencode", false},
		{"codex", false},
	} {
		acfg := config.AgentConfig{ID: "x", Workspace: "/tmp/x", Backend: tc.backend}
		rc := config.Resolve(cfg, acfg)
		out := buildEnvironmentDelegated(acfg, "/tmp/foci.toml", cfg, rc, rc.Permissions, 0, nil, nil, nil, "")
		if !strings.Contains(out, "## Command Approval") {
			t.Errorf("%s: no Command Approval section", tc.backend)
		}
		if got := strings.Contains(out, "pre-approved by your backend"); got != tc.wantPreApproved {
			t.Errorf("%s: --allowedTools layer listed = %v, want %v", tc.backend, got, tc.wantPreApproved)
		}
	}
	acfg := config.AgentConfig{ID: "x", Workspace: "/tmp/x", Backend: "some-future-backend"}
	rc := config.Resolve(cfg, acfg)
	if out := buildEnvironmentDelegated(acfg, "/tmp/foci.toml", cfg, rc, rc.Permissions, 0, nil, nil, nil, ""); strings.Contains(out, "## Command Approval") {
		t.Error("an unregistered backend claims an approval regime it may not enforce")
	}
}

// TestWriteEnvironmentCore_ViaVoiceExplainsTTSReply guards #1438: the via=voice
// entry must tell the agent the message was spoken (STT) and its reply will be
// read aloud (TTS), so it replies concisely in spoken sentences. This lives in
// the foci-generated environment block (not a character file) so it reaches
// every agent regardless of character-file content.
func TestWriteEnvironmentCore_ViaVoiceExplainsTTSReply(t *testing.T) {
	t.Parallel()
	base := config.AgentConfig{ID: "x", Workspace: "/tmp/x", Backend: "claude-code"}
	cfg := &config.Config{Logging: config.LoggingConfig{EventFile: "/tmp/foci.log"}}
	rc := config.Resolve(cfg, base)

	var b strings.Builder
	writeEnvironmentCore(&b, base, "/tmp/foci.toml", cfg, rc, nil, 0)
	out := b.String()

	for _, want := range []string{
		"`voice` (speech-to-text",
		"read aloud by TTS",
		"reply concisely",
		"spoken sentences",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("via=voice explanation missing %q\n---\n%s", want, out)
		}
	}
}

func TestWriteAPIConfig(t *testing.T) {
	t.Parallel()
	acfg := config.AgentConfig{BlockedPaths: []config.BlockedPath{{Path: "/etc"}}}
	cfg := &config.Config{}
	cfg.Tools.ExecDefaultTimeout = 30
	rc := &config.ResolvedAgentConfig{}
	rc.Loop.MaxToolLoops = 25
	rc.Summary.MaxResultChars = 15000
	rc.Summary.AutoSummarise = true
	rc.Tools.MaxFileReadBytes = 52428800
	rc.Tools.MaxConcurrentSpawns = 3
	rc.Tools.ExecAutoBackground = 10

	var b strings.Builder
	writeAPIConfig(&b, acfg, cfg, rc)
	out := b.String()
	for _, want := range []string{
		"## Tool & Loop Limits",
		"up to 25 tool iterations",
		"over 15000 chars",
		"over 50 MB",
		"up to 3 concurrent spawn",
		"30s timeout",
		"after 10s auto-background",
		"(write/edit refused): `/etc`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("API config block missing %q\n---\n%s", want, out)
		}
	}

	var empty strings.Builder
	writeAPIConfig(&empty, config.AgentConfig{}, &config.Config{}, &config.ResolvedAgentConfig{})
	if empty.Len() != 0 {
		t.Errorf("empty config should emit no section, got:\n%s", empty.String())
	}
}

func TestWriteMemorySearch(t *testing.T) {
	t.Parallel()
	mk := func(backend string) (config.AgentConfig, *config.ResolvedAgentConfig) {
		acfg := config.AgentConfig{}
		acfg.Memory.Sources = []config.MemorySource{{Name: "canonical", Dir: "/home/foci/clutch/memory", Weight: 1.0}}
		rc := &config.ResolvedAgentConfig{}
		rc.MemorySearch.SearchBackend = backend
		rc.MemorySearch.SearchLimit = 20
		rc.MemorySearch.ConversationWeight = 0.1
		return acfg, rc
	}

	var bl strings.Builder
	acfg, rc := mk("bleve")
	writeMemorySearch(&bl, acfg, rc)
	for _, want := range []string{"## Memory & Search", "**bleve**", "stemmed", "NOT conversation history", "canonical", "up to 20 results"} {
		if !strings.Contains(strings.ToLower(bl.String()), strings.ToLower(want)) {
			t.Errorf("bleve block missing %q\n---\n%s", want, bl.String())
		}
	}

	var f5 strings.Builder
	acfg, rc = mk("fts5")
	writeMemorySearch(&f5, acfg, rc)
	if !strings.Contains(f5.String(), "and conversation history") {
		t.Errorf("fts5 block should note conversation history is indexed\n---\n%s", f5.String())
	}

	// No backend → no section.
	var none strings.Builder
	writeMemorySearch(&none, config.AgentConfig{}, &config.ResolvedAgentConfig{})
	if none.Len() != 0 {
		t.Errorf("no backend should emit nothing, got:\n%s", none.String())
	}
}

func TestWritePlatform(t *testing.T) {
	t.Parallel()
	var tg strings.Builder
	writePlatform(&tg, "telegram", nil, false)
	out := tg.String()
	if !strings.Contains(out, "## Platform") || !strings.Contains(out, "Telegram") {
		t.Errorf("telegram platform block missing header/content:\n%s", out)
	}

	// Unknown platform with no embedded default → no section.
	var unknown strings.Builder
	writePlatform(&unknown, "signal", nil, false)
	if unknown.Len() != 0 {
		t.Errorf("unknown platform should emit nothing, got:\n%s", unknown.String())
	}

	// Empty platform (unknown session) → no section.
	var empty strings.Builder
	writePlatform(&empty, "", nil, false)
	if empty.Len() != 0 {
		t.Errorf("empty platform should emit nothing, got:\n%s", empty.String())
	}
}

func TestWriteCommandApproval(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	rc := &config.ResolvedAgentConfig{}
	rc.Permissions.AutoApproveCommonReadonly = true
	rc.Permissions.AutoApproveCommonSafeWrite = false
	rc.Permissions.AutoApproveRules = []string{"Bash:gh search", "Bash:git -C /home/rich/git/foci"}
	writeCommandApproval(&b, rc.Permissions, "Read(/tmp/**), Write(/tmp/**)")
	out := b.String()

	for _, want := range []string{
		"## Command Approval",
		"**pre-approved by your backend** (auto-run, no prompt — not a restriction): Read(/tmp/**), Write(/tmp/**)", // the CC --allowedTools layer
		"every `foci_*` shell function is always auto-approved",
		"**read-only** (on):",
		"sqlite3 -readonly",   // a rendered read-only rule (Bash: stripped)
		"**safe-write** (off", // disabled state surfaced
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
	t.Parallel()
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

// TestWriteBackend_CronCreateIsCCOnly pins the CC-specific "CronCreate is
// blocked" note to the claude-code Backend section. CronCreate is a Claude Code
// tool; the note is meaningless (and misleading) for backends that have no such
// tool, so it must not leak into their sections.
func TestWriteBackend_CronCreateIsCCOnly(t *testing.T) {
	t.Parallel()
	var cc strings.Builder
	writeBackend(&cc, "claude-code", nil)
	if !strings.Contains(cc.String(), "CronCreate") {
		t.Errorf("claude-code Backend section should carry the CronCreate note, got:\n%s", cc.String())
	}
	for _, alt := range []string{"foci_remind", "crontab"} {
		if !strings.Contains(cc.String(), alt) {
			t.Errorf("claude-code Backend section should point at the alternative %q, got:\n%s", alt, cc.String())
		}
	}
	for _, backend := range []string{"opencode", "api", ""} {
		var b strings.Builder
		writeBackend(&b, backend, nil)
		if strings.Contains(b.String(), "CronCreate") {
			t.Errorf("backend=%q: CronCreate is a Claude Code tool and must not appear, got:\n%s", backend, b.String())
		}
	}
}

// TestWriteBackend_CCFinalMessageRule pins the "verbatim content goes in the
// final message" rule to the claude-code Backend section. On Opus 5.5 / Fable 5.1
// text written before a tool call comes back as a progress-update thinking block,
// which foci does not render, so a result written there never reaches the user
// (#1303).
func TestWriteBackend_CCFinalMessageRule(t *testing.T) {
	t.Parallel()
	var cc strings.Builder
	writeBackend(&cc, "claude-code", nil)
	if !strings.Contains(cc.String(), "final message") {
		t.Errorf("claude-code Backend section should carry the final-message rule, got:\n%s", cc.String())
	}
}

func TestWriteBackend_UnknownYieldsNoSection(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writeBackend(&b, "no-such-backend", nil)
	if b.Len() != 0 {
		t.Errorf("unknown backend with no file should emit nothing, got:\n%s", b.String())
	}
}

func TestWriteCommandApproval_ReadonlyDisabled(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	rc := &config.ResolvedAgentConfig{}
	rc.Permissions.AutoApproveCommonReadonly = false
	writeCommandApproval(&b, rc.Permissions, "")
	out := b.String()
	if strings.Contains(out, "**read-only** (on)") {
		t.Error("read-only line should be absent when the allowlist is disabled")
	}
	if strings.Contains(out, "CC pre-approved") {
		t.Error("CC pre-approved line should be absent when no --allowedTools are configured")
	}
}

// TestPlatformForSession proves the ## Platform section is keyed on the
// durable chat claim (identity), not connection liveness (routing): a claimed
// chat resolves to its owning platform, a branch key resolves identically to
// its parent (byte-identical prompts → cache sharing, docs/CACHING.md), and
// chat-less or unclaimed keys yield "" (no section) instead of a fallback
// platform's guidance.
func TestPlatformForSession(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	const agentID = "main"
	const chatID = int64(12345)
	// Simulate chatmeta's first-contact registration of platform ownership.
	if err := idx.SetChatMetadata(agentID, "telegram", chatID, "registered", "true"); err != nil {
		t.Fatalf("SetChatMetadata: %v", err)
	}

	parentKey := session.NewChatSessionKey(agentID, chatID)
	if got := platformForSession(idx, agentID, parentKey); got != "telegram" {
		t.Errorf("claimed chat: got %q, want telegram", got)
	}
	// A branch key carries the parent's chat ID, so it must resolve to the
	// same platform — the prompt-prefix cache-sharing property.
	branchKey := parentKey + "/b1700000000"
	if got := platformForSession(idx, agentID, branchKey); got != "telegram" {
		t.Errorf("branch key: got %q, want telegram (same as parent)", got)
	}
	// Named session: no chat ID in the key → no platform, no section.
	if got := platformForSession(idx, agentID, agentID+"/research"); got != "" {
		t.Errorf("named session: got %q, want empty", got)
	}
	// Chat with no registered claim (first message not yet processed).
	if got := platformForSession(idx, agentID, session.NewChatSessionKey(agentID, 999)); got != "" {
		t.Errorf("unclaimed chat: got %q, want empty", got)
	}
	// Nil index (test agents / no persistence).
	if got := platformForSession(nil, agentID, parentKey); got != "" {
		t.Errorf("nil index: got %q, want empty", got)
	}
}

// #2739: the environment section says LaTeX math renders when, and only when,
// every prerequisite holds — today, that the session is on the foci app.
func TestWritePlatform_MathLine(t *testing.T) {
	t.Parallel()
	const marker = "**Math renders.**"
	for _, tt := range []struct {
		platform string
		want     bool
	}{
		{"app", true},
		{"telegram", false},
		{"discord", false},
		{"signal", false}, // unknown platform
		{"", false},       // unknown session
	} {
		var b strings.Builder
		writePlatform(&b, tt.platform, nil, false)
		if got := strings.Contains(b.String(), marker); got != tt.want {
			t.Errorf("platform %q: math line present=%v, want %v\n%s", tt.platform, got, tt.want, b.String())
		}
	}
}

// The mermaid line appears only on an app session whose server can render.
func TestWritePlatform_MermaidLine(t *testing.T) {
	t.Parallel()
	const marker = "**Diagrams render.**"
	for _, tt := range []struct {
		platform  string
		canRender bool
		want      bool
	}{
		{"app", true, true},
		{"app", false, false},
		{"telegram", true, false},
		{"discord", true, false},
		{"", true, false},
	} {
		var b strings.Builder
		writePlatform(&b, tt.platform, nil, tt.canRender)
		if got := strings.Contains(b.String(), marker); got != tt.want {
			t.Errorf("platform %q canRender=%v: mermaid line present=%v, want %v\n%s", tt.platform, tt.canRender, got, tt.want, b.String())
		}
	}
}

// mermaidRenders follows the app's configured mermaid_cmd: off or a missing
// command means no line; an executable means the line, on app sessions only.
func TestMermaidRenders_FollowsConfiguredCmd(t *testing.T) {
	t.Parallel()
	exe := filepath.Join(t.TempDir(), "fake-mmdc")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgWith := func(cmd string) *config.Config {
		return &config.Config{DataDir: t.TempDir(), Platforms: []config.PlatformConfig{{ID: "app", App: &config.AppSpecific{MermaidCmd: cmd}}}}
	}
	for _, tt := range []struct {
		platform, cmd string
		want          bool
	}{
		{"app", exe, true},
		{"app", "off", false},
		{"app", "/no/such/mmdc", false},
		{"telegram", exe, false},
	} {
		if got := mermaidRenders(tt.platform, cfgWith(tt.cmd)); got != tt.want {
			t.Errorf("platform %q cmd %q: mermaidRenders=%v, want %v", tt.platform, tt.cmd, got, tt.want)
		}
	}
}

// One agent, an app session and a Telegram session: only the app session's
// prompt has the math line, for API and delegated agents alike (#2739).
func TestBuildEnvironment_MathLineFollowsSessionPlatform(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Logging: config.LoggingConfig{EventFile: "/tmp/foci.log"}}
	for _, backend := range []string{"api", "claude-code"} {
		acfg := config.AgentConfig{ID: "x", Workspace: "/tmp/x", Backend: backend}
		rc := config.Resolve(cfg, acfg)
		build := func(platform string) string {
			if backend == "api" {
				return buildEnvironmentAPI(acfg, "/tmp/foci.toml", cfg, rc, 0, []string{"app", "telegram"}, nil, platform)
			}
			return buildEnvironmentDelegated(acfg, "/tmp/foci.toml", cfg, rc, rc.Permissions, 0, []string{"app", "telegram"}, nil, nil, platform)
		}
		if out := build("app"); !strings.Contains(out, "**Math renders.**") {
			t.Errorf("%s: app session lacks the math line", backend)
		}
		for _, p := range []string{"telegram", "discord", ""} {
			if out := build(p); strings.Contains(out, "Math renders") {
				t.Errorf("%s: %q session has the math line", backend, p)
			}
		}
		// Stable: two builds of the same session give identical text.
		if a, b := build("app"), build("app"); a != b {
			t.Errorf("%s: two builds of the app session's environment differ", backend)
		}
	}
}

// A workspace copy of platform-app.md replaces the embedded notes but not the
// math line: it still follows the prerequisites, for the app and for others (#2739).
func TestWritePlatform_MathLineSurvivesWorkspaceOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "platform-app.md"), []byte("Custom app notes, no math here."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "platform-telegram.md"), []byte("Custom telegram notes. **Math renders.** is not ours to add."), 0o600); err != nil {
		t.Fatal(err)
	}
	var app strings.Builder
	writePlatform(&app, "app", []string{dir}, false)
	out := app.String()
	if !strings.Contains(out, "Custom app notes") {
		t.Fatalf("override not used:\n%s", out)
	}
	if !strings.Contains(out, "**Math renders.** The app typesets") {
		t.Errorf("app override dropped the math line:\n%s", out)
	}
	var again strings.Builder
	writePlatform(&again, "app", []string{dir}, false)
	if again.String() != out {
		t.Errorf("two builds with the override differ")
	}

	// An override that empties platform-app.md still leaves the math line.
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "platform-app.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var e strings.Builder
	writePlatform(&e, "app", []string{empty}, false)
	if !strings.Contains(e.String(), "## Platform") || !strings.Contains(e.String(), "**Math renders.** The app typesets") {
		t.Errorf("empty app override lost the math line:\n%s", e.String())
	}

	var tg strings.Builder
	writePlatform(&tg, "telegram", []string{dir}, false)
	if strings.Contains(tg.String(), "The app typesets") {
		t.Errorf("telegram session got the math line:\n%s", tg.String())
	}
}
