package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const configEditFixture = `# top comment
data_dir = "/tmp/data"

[notify]
# a comment that must survive edits
startup_notify = false
compaction_notify = true

[groups]
powerful = "haiku"

[[agents]]
id = "clutch"
name = "Clutch"

[agents.loop]
max_tool_loops = 50

[[agents]]
id = "spud"
`

func writeConfigEditFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(configEditFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnsetInFileRemovesKeyAndPreservesComments(t *testing.T) {
	path := writeConfigEditFixture(t)

	old, err := UnsetInFile(path, SetTarget{Section: "notify", Key: "startup_notify"}, 0o600)
	if err != nil {
		t.Fatalf("UnsetInFile: %v", err)
	}
	if old != "false" {
		t.Errorf("old value = %q, want %q", old, "false")
	}

	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "startup_notify") {
		t.Errorf("key still present after unset:\n%s", s)
	}
	if !strings.Contains(s, "# a comment that must survive edits") {
		t.Errorf("comment lost:\n%s", s)
	}
	if !strings.Contains(s, "compaction_notify = true") {
		t.Errorf("sibling key lost:\n%s", s)
	}
}

func TestUnsetInFileErrors(t *testing.T) {
	path := writeConfigEditFixture(t)

	if _, err := UnsetInFile(path, SetTarget{Section: "notify", Key: "not_a_key"}, 0o600); err == nil {
		t.Error("expected error unsetting a key that isn't set")
	}
	if _, err := UnsetInFile(path, SetTarget{Section: "nosuch", Key: "x"}, 0o600); err == nil {
		t.Error("expected error for a missing section")
	}
	if _, err := UnsetInFile(path, SetTarget{Section: "agents", AgentID: "ghost", Key: "x"}, 0o600); err == nil {
		t.Error("expected error for an unknown agent")
	}
}

func TestExplicitFileValues(t *testing.T) {
	path := writeConfigEditFixture(t)

	global, agents, err := ExplicitFileValues(path)
	if err != nil {
		t.Fatalf("ExplicitFileValues: %v", err)
	}

	if got := global["notify.startup_notify"]; got != "false" {
		t.Errorf("notify.startup_notify = %q, want false", got)
	}
	if got := global["data_dir"]; got != "/tmp/data" {
		t.Errorf("data_dir = %q, want /tmp/data", got)
	}
	if _, present := global["agents"]; present {
		t.Error("agents blocks must not leak into the global map")
	}

	clutch := agents["clutch"]
	if clutch == nil {
		t.Fatal("agent clutch missing")
	}
	if got := clutch["loop.max_tool_loops"]; got != "50" {
		t.Errorf("clutch loop.max_tool_loops = %q, want 50", got)
	}
	if _, present := clutch["id"]; present {
		t.Error("id must be stripped from agent values")
	}
	if got := clutch["name"]; got != "Clutch" {
		t.Errorf("clutch name = %q, want Clutch", got)
	}
	if _, ok := agents["spud"]; !ok {
		t.Error("second agent block missing")
	}
}

func TestUnsetInFileAgentBlock(t *testing.T) {
	path := writeConfigEditFixture(t)

	old, err := UnsetInFile(path, SetTarget{Section: "agents", AgentID: "clutch", Key: "name"}, 0o600)
	if err != nil {
		t.Fatalf("UnsetInFile agent key: %v", err)
	}
	if old != `"Clutch"` {
		t.Errorf("old value = %q, want %q", old, `"Clutch"`)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "Clutch") {
		t.Errorf("agent key still present:\n%s", data)
	}
}

func TestAgentKeyInSubTable(t *testing.T) {
	path := writeConfigEditFixture(t)

	// "loop.max_tool_loops" lives under the [agents.loop] sub-table header,
	// which TOML attributes to the preceding [[agents]] clutch block. Both
	// set and unset must find it there (NOT insert a duplicate inline key).
	old, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "clutch", Key: "loop.max_tool_loops"}, "75", 0o600)
	if err != nil {
		t.Fatalf("SetInFile sub-table key: %v", err)
	}
	if old != "50" {
		t.Errorf("old value = %q, want 50", old)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "max_tool_loops = 75") {
		t.Errorf("sub-table value not updated:\n%s", data)
	}
	if strings.Contains(string(data), "loop.max_tool_loops =") {
		t.Errorf("duplicate inline dotted key inserted:\n%s", data)
	}

	if _, err := UnsetInFile(path, SetTarget{Section: "agents", AgentID: "clutch", Key: "loop.max_tool_loops"}, 0o600); err != nil {
		t.Fatalf("UnsetInFile sub-table key: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "max_tool_loops") {
		t.Errorf("sub-table key still present after unset:\n%s", data)
	}

	// The file must still parse and still contain the second agent.
	_, agents, err := ExplicitFileValues(path)
	if err != nil {
		t.Fatalf("file no longer parses: %v", err)
	}
	if _, ok := agents["spud"]; !ok {
		t.Error("second agent lost")
	}
}
