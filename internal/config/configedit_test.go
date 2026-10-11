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

func TestMapObjectEntries(t *testing.T) {
	flat := map[string]string{
		"models.powerful.model":       "anthropic/claude-sonnet-4-20250514",
		"models.powerful.context":     "200000",
		"models.cheap.model":          "openrouter/qwen",
		"endpoints.openrouter.format": "openai",
		"logging.level":               "INFO",
	}
	got := MapObjectEntries("models", flat)
	if len(got) != 2 {
		t.Fatalf("MapObjectEntries(models) = %d entries, want 2", len(got))
	}
	if got["powerful"]["model"] != "anthropic/claude-sonnet-4-20250514" {
		t.Errorf("powerful.model = %q", got["powerful"]["model"])
	}
	if got["powerful"]["context"] != "200000" {
		t.Errorf("powerful.context = %q", got["powerful"]["context"])
	}
	if got["cheap"]["model"] != "openrouter/qwen" {
		t.Errorf("cheap.model = %q", got["cheap"]["model"])
	}

	gotEP := MapObjectEntries("endpoints", flat)
	if len(gotEP) != 1 || gotEP["openrouter"]["format"] != "openai" {
		t.Errorf("endpoints = %v, want openrouter.format=openai", gotEP)
	}

	// Section with no entries returns empty.
	if got := MapObjectEntries("nonexistent", flat); len(got) != 0 {
		t.Errorf("nonexistent = %v, want empty", got)
	}
}

// TestResolveAgentValue pins the ONE agent-value ladder shared by /config get
// and the app config editor — an explicit [[agents]] value, then the
// inherited file global, then the running value, then the built-in default —
// including the source string each rung reports and the two edges where the
// duplicated ladders it replaces differed: a present-but-empty global falls
// through, and the running rung sits between the file and the default.
func TestResolveAgentValue(t *testing.T) {
	interval, ok := LookupField("agent.keepalive.interval")
	if !ok {
		t.Fatal("agent.keepalive.interval missing from the registry")
	}
	systemFiles, ok := LookupField("agent.system.system_files")
	if !ok {
		t.Fatal("agent.system.system_files missing from the registry")
	}
	if _, mapped := agentGlobalSections()["system"]; mapped {
		t.Fatal("test premise broken: system must have no global-section mapping")
	}

	running9s := func() string { return "9s" }
	cases := []struct {
		name    string
		field   ConfigField
		global  map[string]string
		agents  map[string]map[string]string
		running func() string
		wantVal string
		wantSrc string
	}{
		{
			name:    "explicit agent value wins",
			field:   interval,
			global:  map[string]string{"keepalive.interval": "30m"},
			agents:  map[string]map[string]string{"main": {"keepalive.interval": "10m"}},
			running: running9s,
			wantVal: "10m",
			wantSrc: "set for main in foci.toml",
		},
		{
			name:    "explicit empty agent value is still explicit",
			field:   interval,
			agents:  map[string]map[string]string{"main": {"keepalive.interval": ""}},
			running: running9s,
			wantVal: "",
			wantSrc: "set for main in foci.toml",
		},
		{
			name:    "inherited global value",
			field:   interval,
			global:  map[string]string{"keepalive.interval": "30m"},
			running: running9s,
			wantVal: "30m",
			wantSrc: "inherited from [keepalive] in foci.toml",
		},
		{
			name:    "present-but-empty global falls through to running",
			field:   interval,
			global:  map[string]string{"keepalive.interval": ""},
			running: running9s,
			wantVal: "9s",
			wantSrc: "running config (not set in foci.toml)",
		},
		{
			name:    "running used when neither file rung is set",
			field:   interval,
			running: running9s,
			wantVal: "9s",
			wantSrc: "running config (not set in foci.toml)",
		},
		{
			name:    "nil running fn and nil maps land on the default",
			field:   interval,
			wantVal: "55m",
			wantSrc: "built-in default",
		},
		{
			name:    "empty running value falls to the default",
			field:   interval,
			running: func() string { return "" },
			wantVal: "55m",
			wantSrc: "built-in default",
		},
		{
			name:    "key with no global mapping ignores a same-named global key",
			field:   systemFiles,
			global:  map[string]string{"system.system_files": `["x.md"]`},
			running: running9s,
			wantVal: "9s",
			wantSrc: "running config (not set in foci.toml)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, src := ResolveAgentValue(tc.global, tc.agents, "main", tc.field, tc.running)
			if val != tc.wantVal || src != tc.wantSrc {
				t.Errorf("ResolveAgentValue = (%q, %q), want (%q, %q)", val, src, tc.wantVal, tc.wantSrc)
			}
		})
	}
}

// TestAgentRunningValue pins the ONE running rung both ResolveAgentValue
// callers feed in (/config get and the app config editor, #2294 review
// round 1): the mapped global section's running value first — override
// fields carry their defaults only on the global struct, so the agent's
// row alone reads "" — else the agent's own running row. A key whose
// prefix maps to no global section reads only the agent's row.
func TestAgentRunningValue(t *testing.T) {
	loaded := &Config{
		// As Load() would leave it: global defaults filled in.
		Sessions:  SessionsConfig{MaxSystemPromptFile: 20000},
		Keepalive: KeepaliveConfig{Interval: ptrStr("55m")},
	}
	bare := &Config{Sessions: SessionsConfig{MaxSystemPromptFile: 20000}}
	row := AgentConfig{ID: "a", Keepalive: KeepaliveConfig{Interval: ptrStr("42m")}}

	cases := []struct {
		name string
		cfg  *Config
		a    AgentConfig
		key  string
		want string
	}{
		{
			name: "override field reads the global default the agent row lacks",
			cfg:  loaded,
			a:    AgentConfig{ID: "a"},
			key:  "sessions.max_system_prompt_chars_file",
			want: "20000",
		},
		{
			name: "mapped running global wins over the agent row (the order both callers share)",
			cfg:  loaded,
			a:    row,
			key:  "keepalive.interval",
			want: "55m",
		},
		{
			name: "agent row feeds the rung when the mapped global runs empty",
			cfg:  bare,
			a:    row,
			key:  "keepalive.interval",
			want: "42m",
		},
		{
			name: "empty when the mapped global and the row are both empty",
			cfg:  bare,
			a:    AgentConfig{ID: "a"},
			key:  "keepalive.interval",
			want: "",
		},
		{
			name: "unmapped key reads only the agent row, ignoring a set global",
			cfg:  &Config{System: SystemConfig{SystemFiles: []string{"global.md"}}},
			a:    AgentConfig{ID: "a", System: SystemConfig{SystemFiles: []string{"row.md"}}},
			key:  "system.system_files",
			want: "[row.md]",
		},
		{
			// A nil slice renders "[]" (reflectToDisplay's %v) — non-empty,
			// so an unset slice row still feeds the rung, as it always has
			// for the editor.
			name: "unset slice row renders its empty form",
			cfg:  &Config{System: SystemConfig{SystemFiles: []string{"global.md"}}},
			a:    AgentConfig{ID: "a"},
			key:  "system.system_files",
			want: "[]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentRunningValue(tc.cfg, tc.a, tc.key); got != tc.want {
				t.Errorf("AgentRunningValue(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}
