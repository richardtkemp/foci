package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testDeps(agents []AgentInfo, secrets []string) AgentNewDeps {
	return AgentNewDeps{
		ConfigPath:  filepath.Join(os.TempDir(), "test-foci.toml"),
		DefaultsDir: "",
		HomeDir:     os.TempDir(),
		ListFn:      func() []AgentInfo { return agents },
		SecretNames: func() []string { return secrets },
	}
}

func TestAgentWizardHappyPath(t *testing.T) {
	deps := testDeps(
		[]AgentInfo{{ID: "existing"}},
		[]string{"telegram.greek"},
	)

	var captured *agentWizard
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) {
		captured = wiz
		return "Created!", nil
	}

	steps := []struct {
		input    string
		wantDone bool
		contains string
	}{
		{"greek-tutor", false, "Display name"},
		{"Greek Tutor", false, "Emoji"},
		{"🏛️", false, "Model"},
		{"opus", false, "Bot token secret"},
		{"telegram.greek", false, "Character files"},
		{"defaults", true, "Created!"},
	}

	for i, s := range steps {
		resp, done := w.Handle(s.input)
		if done != s.wantDone {
			t.Fatalf("step %d: done=%v, want %v (resp=%q)", i, done, s.wantDone, resp)
		}
		if !strings.Contains(resp, s.contains) {
			t.Errorf("step %d: response %q missing %q", i, resp, s.contains)
		}
	}

	if captured == nil {
		t.Fatal("createFn not called")
	}
	if captured.id != "greek-tutor" {
		t.Errorf("id = %q", captured.id)
	}
	if captured.display != "Greek Tutor" {
		t.Errorf("display = %q", captured.display)
	}
	if captured.emoji != "🏛️" {
		t.Errorf("emoji = %q", captured.emoji)
	}
	if captured.model != "claude-opus-4-6" {
		t.Errorf("model = %q", captured.model)
	}
	if captured.botName != "greek" {
		t.Errorf("botName = %q", captured.botName)
	}
	if captured.tokenSecret != "telegram.greek" {
		t.Errorf("tokenSecret = %q", captured.tokenSecret)
	}
	if captured.charMode != "defaults" {
		t.Errorf("charMode = %q", captured.charMode)
	}
}

func TestAgentWizardInvalidID(t *testing.T) {
	// Invalid slug patterns — each gets its own wizard since valid IDs advance state
	for _, bad := range []string{"", "123", "-starts-dash"} {
		deps := testDeps(nil, nil)
		w := newAgentWizard(deps)
		w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

		resp, done := w.Handle(bad)
		if done {
			t.Errorf("invalid ID %q should not advance wizard", bad)
		}
		if !strings.Contains(resp, "Invalid ID") {
			t.Errorf("invalid ID %q: response = %q", bad, resp)
		}
		if w.step != 0 {
			t.Errorf("invalid ID %q: step = %d, want 0", bad, w.step)
		}
	}
}

func TestAgentWizardDuplicateID(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "clutch"}}, nil)
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	resp, done := w.Handle("clutch")
	if done {
		t.Error("duplicate should not advance wizard")
	}
	if !strings.Contains(resp, "already exists") {
		t.Errorf("response = %q", resp)
	}
	if w.step != 0 {
		t.Errorf("step = %d, want 0", w.step)
	}
}

func TestAgentWizardExistingSecret(t *testing.T) {
	// When the secret already exists, no warning is shown
	deps := testDeps(nil, []string{"telegram.helen", "telegram.greek"})
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	// Advance through ID, display, emoji, model
	w.Handle("myagent")
	w.Handle("My Agent")
	w.Handle("🤖")
	w.Handle("sonnet")

	// Secret exists — should proceed without warning
	resp, done := w.Handle("telegram.helen")
	if done {
		t.Error("should not be done after token step")
	}
	if strings.Contains(resp, "not found") {
		t.Errorf("should NOT warn for existing secret, got %q", resp)
	}
	if !strings.Contains(resp, "Character files") {
		t.Errorf("should advance to next step, got %q", resp)
	}
}

func TestAgentWizardEmptyInputs(t *testing.T) {
	deps := testDeps(nil, nil)
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	// Advance to step 1 (display name)
	w.Handle("myagent")

	// Empty display name
	resp, done := w.Handle("")
	if done {
		t.Error("empty display name should not advance")
	}
	if !strings.Contains(resp, "cannot be empty") {
		t.Errorf("response = %q", resp)
	}

	// Advance to step 2 (emoji)
	w.Handle("My Agent")

	// Empty emoji
	resp, done = w.Handle("")
	if done {
		t.Error("empty emoji should not advance")
	}
	if !strings.Contains(resp, "cannot be empty") {
		t.Errorf("response = %q", resp)
	}
}

func TestAgentWizardModelResolution(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-4-6"},
		{"sonnet", "claude-sonnet-4-6"},
		{"haiku", "claude-haiku-4-5"},
		{"", "claude-sonnet-4-6"},
		{"claude-custom-model", "claude-custom-model"},
		{"OPUS", "claude-opus-4-6"},
	}

	for _, tt := range tests {
		got := defaultResolveModel(tt.input)
		if got != tt.want {
			t.Errorf("defaultResolveModel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestAgentWizardModelResolutionWithCustomAliases(t *testing.T) {
	customAliases := map[string]string{
		"opus":   "claude-opus-5-0",
		"sonnet": "claude-sonnet-5-0",
		"haiku":  "claude-haiku-5-0",
	}
	resolveWithAliases := func(input string) string {
		key := strings.ToLower(strings.TrimSpace(input))
		if resolved, ok := customAliases[key]; ok {
			return resolved
		}
		if input == "" {
			return customAliases["sonnet"]
		}
		return input
	}

	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-5-0"},
		{"sonnet", "claude-sonnet-5-0"},
		{"haiku", "claude-haiku-5-0"},
		{"", "claude-sonnet-5-0"},
		{"claude-custom-model", "claude-custom-model"},
	}

	for _, tt := range tests {
		got := resolveWithAliases(tt.input)
		if got != tt.want {
			t.Errorf("resolveWithAliases(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestAgentWizardTokenWarning(t *testing.T) {
	deps := testDeps(nil, []string{"telegram.existing"})
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	// Advance through ID, display, emoji, model
	w.Handle("myagent")
	w.Handle("My Agent")
	w.Handle("🤖")
	w.Handle("sonnet")

	// Non-existent secret: should warn but continue
	resp, done := w.Handle("telegram.newbot")
	if done {
		t.Error("should not be done after token step")
	}
	if !strings.Contains(resp, "not found") {
		t.Errorf("expected warning about missing secret, got %q", resp)
	}
	if !strings.Contains(resp, "Character files") {
		t.Errorf("should still prompt for next step, got %q", resp)
	}

	// Now test with existing secret — no warning
	w2 := newAgentWizard(deps)
	w2.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }
	w2.Handle("otheragent")
	w2.Handle("Other Agent")
	w2.Handle("🎯")
	w2.Handle("sonnet")

	resp2, _ := w2.Handle("telegram.existing")
	if strings.Contains(resp2, "not found") {
		t.Errorf("should NOT warn for existing secret, got %q", resp2)
	}
}

func TestAgentWizardTokenFormat(t *testing.T) {
	deps := testDeps(nil, nil)
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	w.Handle("myagent")
	w.Handle("My Agent")
	w.Handle("🤖")
	w.Handle("sonnet")

	// Invalid format (no dot)
	resp, done := w.Handle("nodot")
	if done {
		t.Error("should not advance with invalid token format")
	}
	if !strings.Contains(resp, "section.key") {
		t.Errorf("expected format hint, got %q", resp)
	}

	// Empty
	_, done = w.Handle("")
	if done {
		t.Error("should not advance with empty token")
	}
}

func TestAgentWizardCharModeCopy(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "clutch"}}, []string{"telegram.test"})
	w := newAgentWizard(deps)
	var captured *agentWizard
	w.createFn = func(wiz *agentWizard) (string, error) {
		captured = wiz
		return "ok", nil
	}

	// Walk through to char mode step
	w.Handle("newagent")
	w.Handle("New Agent")
	w.Handle("🆕")
	w.Handle("sonnet")
	w.Handle("telegram.test")

	// Copy existing agent
	resp, done := w.Handle("copy clutch")
	if !done {
		t.Error("should be done after final step")
	}
	if captured.charMode != "copy" || captured.copyFrom != "clutch" {
		t.Errorf("charMode=%q, copyFrom=%q", captured.charMode, captured.copyFrom)
	}
	_ = resp
}

func TestAgentWizardCharModeCopyNonexistent(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "clutch"}}, []string{"telegram.test"})
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	w.Handle("newagent")
	w.Handle("New Agent")
	w.Handle("🆕")
	w.Handle("sonnet")
	w.Handle("telegram.test")

	// Copy nonexistent agent
	resp, done := w.Handle("copy nonexistent")
	if done {
		t.Error("should not be done when source agent doesn't exist")
	}
	if !strings.Contains(resp, "not found") {
		t.Errorf("expected not found, got %q", resp)
	}
}

func TestAgentWizardCharModeBlankAndDefaults(t *testing.T) {
	deps := testDeps(nil, []string{"telegram.test"})

	// Test "blank"
	w := newAgentWizard(deps)
	var mode string
	w.createFn = func(wiz *agentWizard) (string, error) {
		mode = wiz.charMode
		return "ok", nil
	}
	w.Handle("agent1")
	w.Handle("Agent")
	w.Handle("🔵")
	w.Handle("")
	w.Handle("telegram.test")
	w.Handle("blank")
	if mode != "blank" {
		t.Errorf("charMode = %q, want blank", mode)
	}

	// Test empty input defaults to "defaults"
	w2 := newAgentWizard(deps)
	w2.createFn = func(wiz *agentWizard) (string, error) {
		mode = wiz.charMode
		return "ok", nil
	}
	w2.Handle("agent2")
	w2.Handle("Agent")
	w2.Handle("🔴")
	w2.Handle("")
	w2.Handle("telegram.test")
	w2.Handle("")
	if mode != "defaults" {
		t.Errorf("charMode = %q, want defaults", mode)
	}

	// Test invalid char mode
	w3 := newAgentWizard(deps)
	w3.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }
	w3.Handle("agent3")
	w3.Handle("Agent")
	w3.Handle("🟢")
	w3.Handle("")
	w3.Handle("telegram.test")
	resp, done := w3.Handle("invalid")
	if done {
		t.Error("invalid char mode should not advance")
	}
	if !strings.Contains(resp, "Must be") {
		t.Errorf("expected usage hint, got %q", resp)
	}
}

func TestCreateWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up defaults directory with character files, keepalive, and crontab template
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	os.MkdirAll(filepath.Join(defaultsDir, "prompts"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "character", "SOUL.md"), []byte("- **Name:** <!-- your name -->\n- **Emoji:** <!-- your symbol -->\n"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "character", "CRAFT.md"), []byte("craft content"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "prompts", "KEEPALIVE.md"), []byte("keepalive"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "crontab.template"), []byte("*/30 * * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/shared/prompts/memory-formation.md 2>&1 >> HOMEDIR/logs/cron.log\n"), 0644)

	// Create a config file to append to
	configPath := filepath.Join(tmpDir, "foci.toml")
	os.WriteFile(configPath, []byte("# existing config\n"), 0644)

	// Override crontab command to prevent real crontab modification
	origCrontab := runCrontabCmd
	defer func() { runCrontabCmd = origCrontab }()
	runCrontabCmd = func(cmd string) error { return nil }

	deps := AgentNewDeps{
		ConfigPath:  configPath,
		DefaultsDir: defaultsDir,
		HomeDir:     tmpDir,
		ListFn:      func() []AgentInfo { return nil },
		SecretNames: func() []string { return nil },
	}

	w := &agentWizard{
		deps:        deps,
		id:          "test-agent",
		display:     "Test Agent",
		emoji:       "🧪",
		model:       "claude-sonnet-4-6",
		botName:     "test",
		tokenSecret: "telegram.test",
		charMode:    "defaults",
	}

	result, err := createAgent(w)
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}

	// Check workspace dirs exist
	workspace := filepath.Join(tmpDir, "test-agent")
	for _, dir := range []string{"character", "memory", "prompts"} {
		if _, err := os.Stat(filepath.Join(workspace, dir)); os.IsNotExist(err) {
			t.Errorf("directory %s not created", dir)
		}
	}

	// Check character files were copied and SOUL.md was templated
	data, err := os.ReadFile(filepath.Join(workspace, "character", "SOUL.md"))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	soulContent := string(data)
	if !strings.Contains(soulContent, "**Name:** Test Agent") {
		t.Errorf("SOUL.md name not substituted: %q", soulContent)
	}
	if !strings.Contains(soulContent, "**Emoji:** 🧪") {
		t.Errorf("SOUL.md emoji not substituted: %q", soulContent)
	}

	// Check keepalive prompt was copied
	data, err = os.ReadFile(filepath.Join(workspace, "prompts", "KEEPALIVE.md"))
	if err != nil {
		t.Fatalf("read KEEPALIVE.md: %v", err)
	}
	if string(data) != "keepalive" {
		t.Errorf("KEEPALIVE.md = %q", string(data))
	}

	// Check config was appended
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config := string(configData)
	if !strings.Contains(config, "[[agents]]") {
		t.Error("missing [[agents]] in config")
	}
	if !strings.Contains(config, `id = "test-agent"`) {
		t.Error("missing agent ID in config")
	}
	// telegram_bot should be emitted since botName ("test") != id ("test-agent")
	if !strings.Contains(config, `telegram_bot = "test"`) {
		t.Error("missing telegram_bot in config")
	}

	// Check result message
	if !strings.Contains(result, "Workspace") {
		t.Errorf("missing workspace in result: %s", result)
	}
	if !strings.Contains(result, "test-agent") {
		t.Errorf("missing agent name in result: %s", result)
	}
	if !strings.Contains(result, "/restart") {
		t.Errorf("missing restart hint in result: %s", result)
	}
}

func TestCreateWorkspaceBlank(t *testing.T) {
	tmpDir := t.TempDir()

	configPath := filepath.Join(tmpDir, "foci.toml")
	os.WriteFile(configPath, []byte("# config\n"), 0644)

	origCrontab := runCrontabCmd
	defer func() { runCrontabCmd = origCrontab }()
	runCrontabCmd = func(cmd string) error { return nil }

	deps := AgentNewDeps{
		ConfigPath:  configPath,
		DefaultsDir: filepath.Join(tmpDir, "nonexistent-defaults"),
		HomeDir:     tmpDir,
		ListFn:      func() []AgentInfo { return nil },
		SecretNames: func() []string { return nil },
	}

	w := &agentWizard{
		deps:        deps,
		id:          "blank-agent",
		display:     "Blank",
		emoji:       "⬜",
		model:       "claude-haiku-4-5",
		botName:     "blank",
		tokenSecret: "telegram.blank",
		charMode:    "blank",
	}

	_, err := createAgent(w)
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}

	workspace := filepath.Join(tmpDir, "blank-agent")
	for _, name := range []string{"SOUL.md", "COHERENCE.md", "CRAFT.md", "USER.md", "MEMORY.md"} {
		path := filepath.Join(workspace, "character", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("missing %s: %v", name, err)
			continue
		}
		if len(data) != 0 {
			t.Errorf("%s should be empty, got %q", name, string(data))
		}
	}
}

func TestTemplateSoulFile(t *testing.T) {
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")

	template := `# SOUL.md — Who I Am

- **Name:** <!-- your name -->
- **Creature:** <!-- what you are, in a sentence -->
- **Emoji:** <!-- your symbol -->

## Vibe
`
	os.WriteFile(soulPath, []byte(template), 0644)

	if err := templateSoulFile(soulPath, "Greek Tutor", "🏛️"); err != nil {
		t.Fatalf("templateSoulFile: %v", err)
	}

	data, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "**Name:** Greek Tutor") {
		t.Errorf("expected name substitution, got:\n%s", content)
	}
	if !strings.Contains(content, "**Emoji:** 🏛️") {
		t.Errorf("expected emoji substitution, got:\n%s", content)
	}
	// Creature placeholder should remain untouched
	if !strings.Contains(content, "<!-- what you are") {
		t.Errorf("creature placeholder should remain, got:\n%s", content)
	}
}

func TestTemplateSoulFileMissing(t *testing.T) {
	// Non-existent file should not error
	if err := templateSoulFile(filepath.Join(t.TempDir(), "nope.md"), "Name", "🔵"); err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}
}

func TestGenerateConfigEntry(t *testing.T) {
	t.Run("botName differs from id", func(t *testing.T) {
		w := &agentWizard{
			id:          "greek-tutor",
			model:       "claude-sonnet-4-6",
			botName:     "greek",
			tokenSecret: "telegram.greek",
		}

		result := generateConfigEntry(w, "/home/foci/greek-tutor")

		checks := []string{
			"[[agents]]",
			`id = "greek-tutor"`,
			`model = "claude-sonnet-4-6"`,
			`telegram_bot = "greek"`,
			`workspace = "/home/foci/greek-tutor"`,
			`system_files = ["character/SOUL.md"`,
			"[[agents.memory.sources]]",
			`name = "greek-tutor"`,
			`dir = "/home/foci/greek-tutor/memory"`,
			"weight = 1.0",
		}

		for _, check := range checks {
			if !strings.Contains(result, check) {
				t.Errorf("missing %q in:\n%s", check, result)
			}
		}

		// Should NOT contain [telegram.bots] section
		if strings.Contains(result, "[telegram.bots") {
			t.Errorf("should not contain [telegram.bots] in:\n%s", result)
		}
		// Convention key matches — should NOT emit bot_secret
		if strings.Contains(result, "bot_secret") {
			t.Errorf("should not contain bot_secret when convention matches in:\n%s", result)
		}
	})

	t.Run("botName matches id — omit telegram_bot", func(t *testing.T) {
		w := &agentWizard{
			id:          "greek",
			model:       "claude-sonnet-4-6",
			botName:     "greek",
			tokenSecret: "telegram.greek",
		}

		result := generateConfigEntry(w, "/home/foci/greek")

		// telegram_bot should be omitted when it equals id
		if strings.Contains(result, "telegram_bot") {
			t.Errorf("should not contain telegram_bot when botName == id in:\n%s", result)
		}
	})

	t.Run("custom secret — emit bot_secret", func(t *testing.T) {
		w := &agentWizard{
			id:          "greek",
			model:       "claude-sonnet-4-6",
			botName:     "greek",
			tokenSecret: "custom.key",
		}

		result := generateConfigEntry(w, "/home/foci/greek")

		if !strings.Contains(result, `bot_secret = "custom.key"`) {
			t.Errorf("missing bot_secret in:\n%s", result)
		}
	})
}

func TestGenerateCrontabMissingTemplate(t *testing.T) {
	// No template file — should return error
	w := &agentWizard{
		deps: AgentNewDeps{
			DefaultsDir: filepath.Join(t.TempDir(), "nonexistent"),
			HomeDir:     "/home/foci",
			ListFn:      func() []AgentInfo { return nil },
		},
		id:      "greek-tutor",
		display: "Greek Tutor",
	}
	_, err := generateCrontab(w, "/home/foci/greek-tutor")
	if err == nil {
		t.Fatal("expected error when template file is missing")
	}
	if !strings.Contains(err.Error(), "crontab template") {
		t.Errorf("error should mention crontab template, got: %v", err)
	}
}

func TestGenerateCrontabFromTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	templateDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(templateDir, 0755)

	// Write a template file with comments and crontab lines
	template := `# AGENT_NAME cron
# This is a comment that should be stripped
0 4 * * * foci branch --oneshot -a AGENT_NAME "$(cat WORKSPACE/prompts/review.md)" 2>&1 >> HOMEDIR/logs/cron.log
*/30 * * * * foci send -a AGENT_NAME "[keepalive]" 2>&1 >> HOMEDIR/logs/cron.log
`
	os.WriteFile(filepath.Join(templateDir, "crontab.template"), []byte(template), 0644)

	w := &agentWizard{
		deps: AgentNewDeps{
			DefaultsDir: templateDir,
			HomeDir:     "/home/foci",
			ListFn:      func() []AgentInfo { return nil },
		},
		id:      "helen",
		display: "Helen",
	}
	lines, err := generateCrontab(w, "/home/foci/helen")
	if err != nil {
		t.Fatalf("generateCrontab: %v", err)
	}
	joined := strings.Join(lines, "\n")

	// Comment lines should be stripped
	if strings.Contains(joined, "# ") {
		t.Errorf("comment lines should be stripped:\n%s", joined)
	}

	// Placeholders should be replaced
	if strings.Contains(joined, "AGENT_NAME") {
		t.Errorf("AGENT_NAME not replaced:\n%s", joined)
	}
	if strings.Contains(joined, "WORKSPACE") {
		t.Errorf("WORKSPACE not replaced:\n%s", joined)
	}
	if strings.Contains(joined, "HOMEDIR") {
		t.Errorf("HOMEDIR not replaced:\n%s", joined)
	}
	if !strings.Contains(joined, "foci branch --oneshot -a helen") {
		t.Errorf("missing agent name substitution:\n%s", joined)
	}
	if !strings.Contains(joined, "/home/foci/helen/prompts/review.md") {
		t.Errorf("missing workspace substitution:\n%s", joined)
	}

	// Should have exactly 2 lines (the two crontab entries, no comments)
	if len(lines) != 2 {
		t.Errorf("expected 2 lines (no comments), got %d:\n%s", len(lines), joined)
	}
}

func TestGenerateCrontabStagger(t *testing.T) {
	// Set up a template file with stagger-testable entries
	tmpDir := t.TempDir()
	os.MkdirAll(tmpDir, 0755)
	template := `# Comment line
*/30 * * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/shared/prompts/memory-formation.md 2>&1 >> HOMEDIR/logs/cron.log
0 4 * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/shared/prompts/daily-memory-review.md 2>&1 >> HOMEDIR/logs/cron.log
`
	os.WriteFile(filepath.Join(tmpDir, "crontab.template"), []byte(template), 0644)

	// 3 existing agents → offset 9 minutes
	w := &agentWizard{
		deps: AgentNewDeps{
			DefaultsDir: tmpDir,
			HomeDir:     "/home/foci",
			ListFn: func() []AgentInfo {
				return []AgentInfo{{ID: "a"}, {ID: "b"}, {ID: "c"}}
			},
		},
		id:      "fourth",
		display: "Fourth",
	}
	lines, err := generateCrontab(w, "/home/foci/fourth")
	if err != nil {
		t.Fatalf("generateCrontab: %v", err)
	}

	// The "0 4 * * *" daily entry should become "9 4 * * *"
	found := false
	for _, line := range lines {
		if strings.Contains(line, "daily-memory-review") {
			if !strings.HasPrefix(line, "9 4 ") {
				t.Errorf("expected staggered minute 9, got: %s", line)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("daily-memory-review entry not found in:\n%s", strings.Join(lines, "\n"))
	}

	// Interval entries (*/30) should NOT be modified
	for _, line := range lines {
		if strings.Contains(line, "memory-formation") {
			if !strings.HasPrefix(line, "*/30 ") {
				t.Errorf("interval entry should not be staggered: %s", line)
			}
			break
		}
	}
}

func TestStaggerCrontabLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		offset int
		want   string
	}{
		{"absolute minute", "0 4 * * * cmd", 9, "9 4 * * * cmd"},
		{"wrap at 60", "55 4 * * * cmd", 9, "4 4 * * * cmd"},
		{"interval unchanged", "*/30 * * * * cmd", 9, "*/30 * * * * cmd"},
		{"comment unchanged", "# comment", 5, "# comment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := staggerCrontabLine(tt.line, tt.offset)
			if got != tt.want {
				t.Errorf("staggerCrontabLine(%q, %d) = %q, want %q", tt.line, tt.offset, got, tt.want)
			}
		})
	}
}

func TestRegistryHandleMessage(t *testing.T) {
	reg := NewRegistry()

	// No wizard: should pass through
	resp, ok := reg.HandleMessage("hello")
	if ok {
		t.Error("expected false with no wizard")
	}
	if resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}

	// Set a wizard
	w := &testWizard{responses: []string{"step 1 done", "all done"}, doneAt: 1}
	reg.SetWizard(w)

	// First message goes to wizard
	resp, ok = reg.HandleMessage("input 1")
	if !ok {
		t.Error("expected wizard to handle message")
	}
	if resp != "step 1 done" {
		t.Errorf("resp = %q", resp)
	}

	// Second message completes wizard
	resp, ok = reg.HandleMessage("input 2")
	if !ok {
		t.Error("expected wizard to handle message")
	}
	if resp != "all done" {
		t.Errorf("resp = %q", resp)
	}

	// After completion, wizard should be cleared
	_, ok = reg.HandleMessage("input 3")
	if ok {
		t.Error("wizard should be cleared after done")
	}
}

func TestRegistryHandleMessageCancel(t *testing.T) {
	reg := NewRegistry()
	w := &testWizard{responses: []string{"should not see"}, doneAt: 99}
	reg.SetWizard(w)

	// /cancel clears wizard
	resp, ok := reg.HandleMessage("/cancel")
	if !ok {
		t.Error("expected wizard intercept for /cancel")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("resp = %q", resp)
	}

	// Wizard should be cleared
	_, ok = reg.HandleMessage("hello")
	if ok {
		t.Error("wizard should be cleared after cancel")
	}
}

func TestRegistryHandleMessageStop(t *testing.T) {
	reg := NewRegistry()
	w := &testWizard{responses: []string{"should not see"}, doneAt: 99}
	reg.SetWizard(w)

	resp, ok := reg.HandleMessage("/stop")
	if !ok {
		t.Error("expected wizard intercept for /stop")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("resp = %q", resp)
	}
}

func TestIsValidSlug(t *testing.T) {
	valid := []string{"a", "abc", "my-agent", "a1", "a-b-c", "x123-test"}
	for _, s := range valid {
		if !IsValidSlug(s) {
			t.Errorf("IsValidSlug(%q) = false, want true", s)
		}
	}

	invalid := []string{"", "1abc", "-abc", "ABC", "has space", "has.dot", "a_b"}
	for _, s := range invalid {
		if IsValidSlug(s) {
			t.Errorf("IsValidSlug(%q) = true, want false", s)
		}
	}
}

func TestAgentsNewSubcommand(t *testing.T) {
	reg := NewRegistry()
	deps := &AgentNewDeps{
		ConfigPath:  "/tmp/test.toml",
		DefaultsDir: "/tmp/defaults",
		HomeDir:     "/tmp",
		ListFn:      func() []AgentInfo { return nil },
		SecretNames: func() []string { return nil },
	}
	cmd := NewAgentsCommand(func() []AgentInfo { return nil }, reg, deps)

	// "/agents new" should start wizard
	result, err := cmd.Execute(nil, "new")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Wizard") {
		t.Errorf("expected wizard prompt, got %q", result)
	}
	if !strings.Contains(result, "Agent ID") {
		t.Errorf("expected Agent ID prompt, got %q", result)
	}

	// Wizard should be active now
	_, ok := reg.HandleMessage("test-input")
	if !ok {
		t.Error("wizard should be active after /agents new")
	}
}

func TestAgentsNewDisabled(t *testing.T) {
	// With nil registry/deps, "new" returns error message
	cmd := NewAgentsCommand(func() []AgentInfo { return nil }, nil, nil)

	result, err := cmd.Execute(nil, "new")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "not available") {
		t.Errorf("expected not available, got %q", result)
	}
}

// testWizard is a mock WizardHandler for testing Registry routing.
type testWizard struct {
	responses []string
	doneAt    int
	step      int
}

func (tw *testWizard) Handle(text string) (string, bool) {
	resp := tw.responses[tw.step]
	done := tw.step >= tw.doneAt
	tw.step++
	return resp, done
}
