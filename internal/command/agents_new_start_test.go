package command

// #963: /agents new starts the new agent itself, after the config block and
// the crontab are written, with no restart.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provision"
)

// liveStartWizard builds a wizard at the creation step over a temp home,
// with crontab writes recorded instead of run.
func liveStartWizard(t *testing.T, start func(string) ([]string, error)) (w *agentWizard, configPath string, crontabWrites *int) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath = filepath.Join(tmpDir, "foci.toml")
	if err := os.WriteFile(configPath, []byte("# existing config\n"), 0644); err != nil {
		t.Fatal(err)
	}
	defaultsDir := filepath.Join(tmpDir, "shared")
	if err := os.MkdirAll(defaultsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultsDir, "crontab.template"), []byte("*/30 * * * * foci branch -a AGENT_NAME\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writes := 0
	orig := provision.RunCrontabCmd
	t.Cleanup(func() { provision.RunCrontabCmd = orig })
	provision.RunCrontabCmd = func(string) error { writes++; return nil }

	w = &agentWizard{
		deps: AgentNewDeps{
			ConfigPath:  configPath,
			DefaultsDir: defaultsDir,
			HomeDir:     tmpDir,
			ListFn:      func() []AgentInfo { return nil },
			StartAgent:  start,
		},
		id:       "live-agent",
		display:  "Live Agent",
		backend:  "api",
		charMode: "blank",
	}
	return w, configPath, &writes
}

func TestCreateAgent_StartsAgentAfterConfigIsWritten(t *testing.T) {
	var started []string
	var configAtStart string
	var crontabAtStart int
	var crontabWrites *int
	var configPath string
	start := func(id string) ([]string, error) {
		started = append(started, id)
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("read config at start: %v", err)
		}
		configAtStart = string(data)
		crontabAtStart = *crontabWrites
		return nil, nil
	}
	var w *agentWizard
	w, configPath, crontabWrites = liveStartWizard(t, start)

	result, err := createAgent(w)
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if len(started) != 1 || started[0] != "live-agent" {
		t.Fatalf("StartAgent calls = %v, want exactly [live-agent]", started)
	}
	if !strings.Contains(configAtStart, `id = "live-agent"`) {
		t.Errorf("agent started before its config block was written; config at start:\n%s", configAtStart)
	}
	if crontabAtStart == 0 {
		t.Error("agent started before its crontab entries were written")
	}
	if strings.Contains(result, "/restart") {
		t.Errorf("a successful live start still tells the user to /restart:\n%s", result)
	}
	if !strings.Contains(result, "is running") {
		t.Errorf("reply does not say the agent is running:\n%s", result)
	}
}

func TestCreateAgent_StartHintsAreShown(t *testing.T) {
	hint := "Telegram: no bot token. Add it with `/secrets set telegram.live-agent <token>`, then /restart to connect Telegram."
	w, _, _ := liveStartWizard(t, func(string) ([]string, error) { return []string{hint}, nil })
	result, err := createAgent(w)
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if !strings.Contains(result, "is running") || !strings.Contains(result, hint) {
		t.Errorf("reply lacks the running line or the hint:\n%s", result)
	}
}

func TestCreateAgent_StartFailureKeepsBlockAndNamesRestart(t *testing.T) {
	w, configPath, _ := liveStartWizard(t, func(string) ([]string, error) {
		return nil, errors.New("duplicate telegram bot \"x\" (already used by agent \"clutch\")")
	})
	result, err := createAgent(w)
	if err != nil {
		t.Fatalf("a failed start must still finish the wizard, got error: %v", err)
	}
	data, rerr := os.ReadFile(configPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), `id = "live-agent"`) {
		t.Errorf("the config block was rolled back after a failed start:\n%s", data)
	}
	if !strings.Contains(result, `already used by agent "clutch"`) {
		t.Errorf("reply does not give the reason:\n%s", result)
	}
	if !strings.Contains(result, "/restart") {
		t.Errorf("reply does not say to /restart:\n%s", result)
	}
	if strings.Contains(result, "is running") {
		t.Errorf("reply claims the agent runs after a failed start:\n%s", result)
	}
}

// Characterisation (red-gate exempt): with no live start available (unit
// tests, first-run setup) the wizard keeps today's closing text.
func TestCreateAgent_NoLiveStartKeepsRestartText(t *testing.T) {
	w, _, _ := liveStartWizard(t, nil)
	result, err := createAgent(w)
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	want := "\nLive Agent (live-agent) is ready.\nRestart foci for the new agent to start: /restart"
	if !strings.HasSuffix(result, want) {
		t.Errorf("reply ending changed; got:\n%s\nwant suffix:\n%s", result, want)
	}
}
