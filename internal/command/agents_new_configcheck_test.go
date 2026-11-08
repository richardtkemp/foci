package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/provision"
)

// #2976: /agents new must not write an [[agents]] block that makes foci.toml
// fail the next start's load. These tests run the real load + Validate
// (config.CheckFileContent) as the gateway wires it.

const ccOnlyConfig = "[[agents]]\nid = \"alpha\"\nbackend = \"claude-code\"\n"

// configCheckWizard returns a wizard over a temp foci.toml holding base, with
// the real config check wired, the crontab write counted, and the name step
// already answered.
func configCheckWizard(t *testing.T, base string) (w *agentWizard, configPath, home string, crontabWrites *int) {
	t.Helper()
	home = t.TempDir()
	configPath = filepath.Join(home, "foci.toml")
	if err := os.WriteFile(configPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := filepath.Join(home, "shared")
	if err := os.MkdirAll(defaults, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaults, "crontab.template"), []byte("40 2 * * 1 foci branch -a AGENT_NAME\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writes := 0
	orig := provision.AppendCrontab
	t.Cleanup(func() { provision.AppendCrontab = orig })
	provision.AppendCrontab = func([]string) error { writes++; return nil }

	w = newAgentWizard(AgentNewDeps{
		ConfigPath:  configPath,
		DefaultsDir: defaults,
		HomeDir:     home,
		ListFn:      func() []AgentInfo { return []AgentInfo{{ID: "alpha"}} },
		CheckConfig: func(content string) error {
			return config.CheckFileContent(content, configPath, nil)
		},
	})
	if _, done := w.Handle("qwen"); done {
		t.Fatal("name step ended the wizard")
	}
	return w, configPath, home, &writes
}

func TestAgentWizard_APIBackendWithoutPowerfulGroupIsRefusedAtBackendStep(t *testing.T) {
	w, configPath, home, writes := configCheckWizard(t, ccOnlyConfig)

	resp, done := w.Handle("api")
	if done {
		t.Fatalf("wizard ended: %q", resp)
	}
	if !strings.Contains(resp, "[groups] powerful is required") {
		t.Errorf("backend step reply does not name the load error: %q", resp)
	}
	if w.step != stepBackend {
		t.Errorf("step = %d, want the backend step re-asked (%d)", w.step, stepBackend)
	}

	// A delegated backend is still accepted from the same re-ask.
	if resp, _ := w.Handle("claude-code"); !strings.Contains(resp, "Model") {
		t.Errorf("claude-code after the refusal: got %q, want the model question", resp)
	}

	got, _ := os.ReadFile(configPath)
	if string(got) != ccOnlyConfig {
		t.Errorf("foci.toml changed:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "qwen")); !os.IsNotExist(err) {
		t.Errorf("workspace created (stat err %v)", err)
	}
	if *writes != 0 {
		t.Errorf("crontab written %d times", *writes)
	}
}

// The final write re-checks: foci.toml can change between the backend step
// and the last answer. A refused create writes nothing at all.
func TestCreateAgent_RefusesBlockThatBreaksLoadAndWritesNothing(t *testing.T) {
	w, configPath, home, writes := configCheckWizard(t, ccOnlyConfig)
	w.backend, w.charMode = apiBackend, "blank"

	_, err := createAgent(w)
	if err == nil || !strings.Contains(err.Error(), "[groups] powerful is required") {
		t.Fatalf("createAgent err = %v, want the load error", err)
	}
	got, _ := os.ReadFile(configPath)
	if string(got) != ccOnlyConfig {
		t.Errorf("foci.toml changed:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "qwen")); !os.IsNotExist(err) {
		t.Errorf("workspace created (stat err %v)", err)
	}
	if *writes != 0 {
		t.Errorf("crontab written %d times", *writes)
	}
}

// Positive control: a block that keeps the config loadable is written.
func TestAgentWizard_DelegatedBackendPassesTheCheck(t *testing.T) {
	w, configPath, _, writes := configCheckWizard(t, ccOnlyConfig)
	w.Handle("claude-code")
	w.Handle("sonnet")
	resp, done := w.Handle("blank")
	if !done || strings.Contains(resp, "Creation failed") {
		t.Fatalf("create did not succeed: done=%v %q", done, resp)
	}
	got, _ := os.ReadFile(configPath)
	if !strings.Contains(string(got), `id = "qwen"`) {
		t.Errorf("block not appended:\n%s", got)
	}
	if err := config.CheckFileContent(string(got), configPath, nil); err != nil {
		t.Errorf("written foci.toml does not load: %v", err)
	}
	if *writes != 1 {
		t.Errorf("crontab written %d times, want 1", *writes)
	}
}

// A foci.toml that already fails to load is not blamed on the new agent (the
// config editor's rule): the wizard still runs.
func TestAgentWizard_AlreadyBrokenConfigDoesNotBlockTheWizard(t *testing.T) {
	broken := ccOnlyConfig + "\n[[agents]]\nid = \"beta\"\nbackend = \"api\"\n"
	if err := config.CheckFileContent(broken, "x", nil); err == nil {
		t.Fatal("fixture: the base config was meant to fail the check already")
	}
	w, _, _, _ := configCheckWizard(t, broken)
	if resp, _ := w.Handle("api"); !strings.Contains(resp, "Character files") {
		t.Errorf("api on an already-broken config: got %q, want the character-files step", resp)
	}
}
