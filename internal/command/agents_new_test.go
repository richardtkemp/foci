package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provision"
)

func testDeps(agents []AgentInfo, preFlightFn func(string) []string) AgentNewDeps {
	// Use a shared temp dir per test process; callers don't write here directly.
	dir := os.TempDir()
	return AgentNewDeps{
		ConfigPath:  filepath.Join(dir, "test-foci.toml"),
		DefaultsDir: "",
		HomeDir:     dir,
		ListFn:      func() []AgentInfo { return agents },
		PreFlightFn: preFlightFn,
	}
}

// Verifies the full wizard flow: name → backend → model → character mode, collecting all values correctly.
func TestAgentWizardHappyPath(t *testing.T) {
	deps := testDeps(
		[]AgentInfo{{ID: "existing"}},
		nil, // no pre-flight warnings
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
		{"Greek Tutor", false, "Backend"},
		{"claude-code", false, "Model"},
		{"opus", false, "Character files"},
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
		t.Errorf("id = %q, want greek-tutor", captured.id)
	}
	if captured.display != "Greek Tutor" {
		t.Errorf("display = %q", captured.display)
	}
	if captured.model != "anthropic/claude-opus-4-6" {
		t.Errorf("model = %q", captured.model)
	}
	if captured.modelRaw != "opus" {
		t.Errorf("modelRaw = %q, want opus", captured.modelRaw)
	}
	if captured.backend != "claude-code" {
		t.Errorf("backend = %q, want claude-code", captured.backend)
	}
	if captured.charMode != "defaults" {
		t.Errorf("charMode = %q", captured.charMode)
	}
}

// Verifies that empty or unparseable names are rejected.
func TestAgentWizardInvalidName(t *testing.T) {
	deps := testDeps(nil, nil) // no agents, no pre-flight
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	// Empty name
	resp, done := w.Handle("")
	if done {
		t.Error("empty name should not advance wizard")
	}
	if !strings.Contains(resp, "empty") {
		t.Errorf("expected empty warning, got %q", resp)
	}
	if w.step != 0 {
		t.Errorf("step = %d, want 0", w.step)
	}

	// Name that produces no valid slug (all special chars)
	resp, done = w.Handle("!!!")
	if done {
		t.Error("invalid name should not advance wizard")
	}
	if !strings.Contains(resp, "valid ID") {
		t.Errorf("expected slug error, got %q", resp)
	}
}

// Verifies that a name matching an existing agent's ID is rejected.
func TestAgentWizardDuplicateName(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "clutch"}}, nil) // no pre-flight
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	resp, done := w.Handle("Clutch")
	if done {
		t.Error("duplicate should not advance wizard")
	}
	if !strings.Contains(resp, "already exists") {
		t.Errorf("response = %q", resp)
	}
}

// Verifies that the ID is correctly slugified from the display name.
func TestAgentWizardSlugFromName(t *testing.T) {
	deps := testDeps(nil, nil) // no pre-flight
	w := newAgentWizard(deps)
	var captured *agentWizard
	w.createFn = func(wiz *agentWizard) (string, error) {
		captured = wiz
		return "ok", nil
	}

	w.Handle("My Cool Agent")
	w.Handle("claude-code")
	w.Handle("sonnet")
	w.Handle("defaults")

	if captured.id != "my-cool-agent" {
		t.Errorf("id = %q, want my-cool-agent", captured.id)
	}
	if captured.display != "My Cool Agent" {
		t.Errorf("display = %q, want My Cool Agent", captured.display)
	}
}

// Verifies that pre-flight warnings appear after the name step, on the backend prompt.
func TestAgentWizardPreFlightWarning(t *testing.T) {
	deps := testDeps(nil, func(agentID string) []string {
		return []string{"Secret `platform." + agentID + "` not found"}
	})
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	// Name step — pre-flight returns a warning, surfaced on the backend prompt.
	resp, done := w.Handle("New Agent") // name → id="new-agent"
	if done {
		t.Error("should not be done after name step")
	}
	if !strings.Contains(resp, "not found") {
		t.Errorf("expected pre-flight warning, got %q", resp)
	}
	if !strings.Contains(resp, "Backend") {
		t.Errorf("should still prompt for next step (backend), got %q", resp)
	}
}

// Verifies no warning when pre-flight returns nothing.
func TestAgentWizardNoPreFlightWarning(t *testing.T) {
	deps := testDeps(nil, func(agentID string) []string { return nil })
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	resp, done := w.Handle("myagent")
	if done {
		t.Error("should not be done after name step")
	}
	if strings.Contains(resp, "⚠️") {
		t.Errorf("should NOT show warnings, got %q", resp)
	}
}

// Verifies copying character files from an existing agent.
func TestAgentWizardCharModeCopy(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "clutch"}}, nil)
	w := newAgentWizard(deps)
	var captured *agentWizard
	w.createFn = func(wiz *agentWizard) (string, error) {
		captured = wiz
		return "ok", nil
	}

	w.Handle("newagent")
	w.Handle("claude-code")
	w.Handle("sonnet")

	resp, done := w.Handle("copy clutch")
	if !done {
		t.Error("should be done after final step")
	}
	if captured.charMode != "copy" || captured.copyFrom != "clutch" {
		t.Errorf("charMode=%q, copyFrom=%q", captured.charMode, captured.copyFrom)
	}
	_ = resp
}

// Verifies that copying from a nonexistent agent is rejected.
func TestAgentWizardCharModeCopyNonexistent(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "clutch"}}, nil)
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }

	w.Handle("newagent")
	w.Handle("claude-code")
	w.Handle("sonnet")

	resp, done := w.Handle("copy nonexistent")
	if done {
		t.Error("should not be done when source agent doesn't exist")
	}
	if !strings.Contains(resp, "not found") {
		t.Errorf("expected not found, got %q", resp)
	}
}

// Verifies the openclaw character mode.
func TestAgentWizardCharModeOpenclaw(t *testing.T) {
	deps := testDeps(nil, nil)
	w := newAgentWizard(deps)
	var mode string
	w.createFn = func(wiz *agentWizard) (string, error) {
		mode = wiz.charMode
		return "ok", nil
	}

	w.Handle("OC Agent")
	w.Handle("claude-code")
	w.Handle("sonnet")
	w.Handle("openclaw")
	if mode != "openclaw" {
		t.Errorf("charMode = %q, want openclaw", mode)
	}
}

// Verifies blank, defaults (empty input), and invalid character modes.
func TestAgentWizardCharModeBlankAndDefaults(t *testing.T) {
	deps := testDeps(nil, nil)

	// Test "blank"
	w := newAgentWizard(deps)
	var mode string
	w.createFn = func(wiz *agentWizard) (string, error) {
		mode = wiz.charMode
		return "ok", nil
	}
	w.Handle("agent1")
	w.Handle("") // backend → default
	w.Handle("") // model → default
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
	w2.Handle("") // backend → default
	w2.Handle("") // model → default
	w2.Handle("")
	if mode != "defaults" {
		t.Errorf("charMode = %q, want defaults", mode)
	}

	// Test invalid char mode
	w3 := newAgentWizard(deps)
	w3.createFn = func(wiz *agentWizard) (string, error) { return "ok", nil }
	w3.Handle("agent3")
	w3.Handle("") // backend → default
	w3.Handle("") // model → default
	resp, done := w3.Handle("invalid")
	if done {
		t.Error("invalid char mode should not advance")
	}
	if !strings.Contains(resp, "Must be") {
		t.Errorf("expected usage hint, got %q", resp)
	}
}

// Verifies empty backend input selects the default (claude-code) — the fix for
// agents previously created with no backend at all.
func TestAgentWizardBackendDefault(t *testing.T) {
	deps := testDeps(nil, nil)
	w := newAgentWizard(deps)
	var captured *agentWizard
	w.createFn = func(wiz *agentWizard) (string, error) { captured = wiz; return "ok", nil }

	w.Handle("backend-default")
	resp, done := w.Handle("") // backend step, empty → default
	if done {
		t.Fatalf("should not be done after backend step, resp=%q", resp)
	}
	w.Handle("opus")
	w.Handle("defaults")

	if captured.backend != "claude-code" {
		t.Errorf("backend = %q, want claude-code", captured.backend)
	}
	if captured.modelRaw != "opus" {
		t.Errorf("modelRaw = %q, want opus", captured.modelRaw)
	}
}

// Verifies the "api" choice produces an in-process (non-delegated) agent, and
// that the model step is SKIPPED for api — the backend answer is followed
// directly by the character-files prompt (API model resolves globally).
func TestAgentWizardBackendAPI(t *testing.T) {
	deps := testDeps(nil, nil)
	w := newAgentWizard(deps)
	var captured *agentWizard
	w.createFn = func(wiz *agentWizard) (string, error) { captured = wiz; return "ok", nil }

	w.Handle("api-agent")

	// Selecting api must skip straight to character files, not ask for a model.
	resp, done := w.Handle("api")
	if done {
		t.Fatalf("should not be done after backend step, resp=%q", resp)
	}
	if strings.Contains(resp, "Model") {
		t.Errorf("api backend should skip the model question, got %q", resp)
	}
	if !strings.Contains(resp, "Character files") {
		t.Errorf("api backend should advance to character files, got %q", resp)
	}

	w.Handle("defaults")

	if captured.backend != "api" {
		t.Errorf("backend = %q, want api", captured.backend)
	}
	if captured.modelRaw != "" {
		t.Errorf("modelRaw = %q, want empty (model skipped for api)", captured.modelRaw)
	}
}

// Verifies a backend from the injected available-list can be selected, and that
// an unrecognised backend is rejected with a re-prompt.
func TestAgentWizardBackendSelectionAndInvalid(t *testing.T) {
	deps := testDeps(nil, nil)
	deps.AvailableBackends = []string{"claude-code", "claude-code-tmux"}
	w := newAgentWizard(deps)
	var captured *agentWizard
	w.createFn = func(wiz *agentWizard) (string, error) { captured = wiz; return "ok", nil }

	w.Handle("tmux-agent")

	// Unrecognised backend → re-prompt, no advance.
	resp, done := w.Handle("nonsense")
	if done {
		t.Error("invalid backend should not advance")
	}
	if !strings.Contains(resp, "Must be one of") {
		t.Errorf("expected re-prompt, got %q", resp)
	}

	// Valid non-default backend from the injected list, then model.
	w.Handle("claude-code-tmux")
	w.Handle("opus")
	w.Handle("defaults")
	if captured.backend != "claude-code-tmux" {
		t.Errorf("backend = %q, want claude-code-tmux", captured.backend)
	}
}

// Verifies the full agent creation including workspace, config, and character files.
func TestCreateWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up defaults directory
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "character", "SOUL.md"), []byte("- **Name:** <!-- your name -->\n"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "character", "CRAFT.md"), []byte("craft content"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "crontab.template"), []byte("*/30 * * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/shared/prompts/reflection.md 2>&1 >> HOMEDIR/logs/cron.log\n"), 0644)

	configPath := filepath.Join(tmpDir, "foci.toml")
	os.WriteFile(configPath, []byte("# existing config\n"), 0644)

	origCrontab := provision.RunCrontabCmd
	defer func() { provision.RunCrontabCmd = origCrontab }()
	provision.RunCrontabCmd = func(cmd string) error { return nil }

	deps := AgentNewDeps{
		ConfigPath:  configPath,
		DefaultsDir: defaultsDir,
		HomeDir:     tmpDir,
		ListFn:      func() []AgentInfo { return nil },
	}

	w := &agentWizard{
		deps:     deps,
		id:       "test-agent",
		display:  "Test Agent",
		model:    "anthropic/claude-sonnet-4-6",
		modelRaw: "sonnet",
		backend:  "claude-code",
		charMode: "defaults",
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
	if !strings.Contains(config, `backend = "claude-code"`) {
		t.Errorf("missing backend in config:\n%s", config)
	}
	if !strings.Contains(config, `backend_config.model = "sonnet"`) {
		t.Errorf("missing backend_config.model in config:\n%s", config)
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

// Verifies blank character mode creates empty template files.
func TestCreateWorkspaceBlank(t *testing.T) {
	tmpDir := t.TempDir()

	configPath := filepath.Join(tmpDir, "foci.toml")
	os.WriteFile(configPath, []byte("# config\n"), 0644)

	origCrontab := provision.RunCrontabCmd
	defer func() { provision.RunCrontabCmd = origCrontab }()
	provision.RunCrontabCmd = func(cmd string) error { return nil }

	deps := AgentNewDeps{
		ConfigPath:  configPath,
		DefaultsDir: filepath.Join(tmpDir, "nonexistent-defaults"),
		HomeDir:     tmpDir,
		ListFn:      func() []AgentInfo { return nil },
	}

	w := &agentWizard{
		deps:     deps,
		id:       "blank-agent",
		display:  "Blank",
		model:    "anthropic/claude-haiku-4-5-20251001",
		charMode: "blank",
	}

	_, err := createAgent(w)
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}

	workspace := filepath.Join(tmpDir, "blank-agent")
	for _, name := range provision.DefaultCharacterFileNames {
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

// Verifies the wizard registry routes messages correctly and cleans up when done.
func TestRegistryHandleMessage(t *testing.T) {
	reg := NewRegistry()

	// No wizard: should pass through
	resp, _, ok := reg.HandleMessage("hello")
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
	resp, _, ok = reg.HandleMessage("input 1")
	if !ok {
		t.Error("expected wizard to handle message")
	}
	if resp != "step 1 done" {
		t.Errorf("resp = %q", resp)
	}

	// Second message completes wizard
	resp, _, ok = reg.HandleMessage("input 2")
	if !ok {
		t.Error("expected wizard to handle message")
	}
	if resp != "all done" {
		t.Errorf("resp = %q", resp)
	}

	// After completion, wizard should be cleared
	_, _, ok = reg.HandleMessage("input 3")
	if ok {
		t.Error("wizard should be cleared after done")
	}
}

// Verifies /cancel clears the active wizard.
func TestRegistryHandleMessageCancel(t *testing.T) {
	reg := NewRegistry()
	w := &testWizard{responses: []string{"should not see"}, doneAt: 99}
	reg.SetWizard(w)

	resp, _, ok := reg.HandleMessage("/cancel")
	if !ok {
		t.Error("expected wizard intercept for /cancel")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("resp = %q", resp)
	}

	_, _, ok = reg.HandleMessage("hello")
	if ok {
		t.Error("wizard should be cleared after cancel")
	}
}

// Verifies /stop also clears the active wizard.
func TestRegistryHandleMessageStop(t *testing.T) {
	reg := NewRegistry()
	w := &testWizard{responses: []string{"should not see"}, doneAt: 99}
	reg.SetWizard(w)

	resp, _, ok := reg.HandleMessage("/stop")
	if !ok {
		t.Error("expected wizard intercept for /stop")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("resp = %q", resp)
	}
}

// Verifies the /agents new subcommand starts the wizard with the correct prompt.
func TestAgentsNewSubcommand(t *testing.T) {
	reg := NewRegistry()
	td := t.TempDir()
	deps := &AgentNewDeps{
		ConfigPath:  filepath.Join(td, "test.toml"),
		DefaultsDir: filepath.Join(td, "defaults"),
		HomeDir:     td,
		ListFn:      func() []AgentInfo { return nil },
		Registry:    reg,
	}
	cc := CommandContext{
		AgentListFn:  func() []AgentInfo { return nil },
		AgentNewDeps: deps,
	}
	cmd := AgentsCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "new"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Wizard") {
		t.Errorf("expected wizard prompt, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "Agent name") {
		t.Errorf("expected Agent name prompt, got %q", result.Text)
	}

	_, _, ok := reg.HandleMessage("test-input")
	if !ok {
		t.Error("wizard should be active after /agents new")
	}
}

// Verifies wizard is unavailable when deps are nil.
func TestAgentsNewDisabled(t *testing.T) {
	cc := CommandContext{
		AgentListFn:  func() []AgentInfo { return nil },
		AgentNewDeps: nil,
	}
	cmd := AgentsCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "new"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "not available") {
		t.Errorf("expected not available, got %q", result.Text)
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
