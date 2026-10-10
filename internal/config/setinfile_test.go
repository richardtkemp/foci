package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestSetInFile_UpdateExistingKey(t *testing.T) {
	// Proves that SetInFile updates an existing key in place and returns the old
	// value, while preserving all other keys, comments, and sections unchanged.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `# comment
[agent_loop]
model = "old-model"
max_tool_loops = 25

[sessions]
dir = "/tmp/sessions"
`
	os.WriteFile(path, []byte(content), 0o644)

	old, err := SetInFile(path, SetTarget{Section: "agent_loop", Key: "model"}, `"new-model"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if old != `"old-model"` {
		t.Errorf("old value = %q, want %q", old, `"old-model"`)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if !strings.Contains(result, `model = "new-model"`) {
		t.Errorf("new value not found in output:\n%s", result)
	}
	if !strings.Contains(result, "# comment") {
		t.Error("comment was not preserved")
	}
	if !strings.Contains(result, "max_tool_loops = 25") {
		t.Error("adjacent key was not preserved")
	}
	if !strings.Contains(result, `dir = "/tmp/sessions"`) {
		t.Error("other section was not preserved")
	}
}

func TestSetInFile_ReplacesMultiLineArray(t *testing.T) {
	// Proves the multi-line-safe span replacement: a value spanning several lines
	// (a multi-line TOML array) is fully replaced by the single new line, with no
	// orphaned body lines left behind (the pre-fix bug corrupted the file).
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[permissions]
auto_approve = [          # composable rules
  "Bash:git *",
  "Read:*",
]
other_key = true
`
	os.WriteFile(path, []byte(content), 0o644)

	if _, err := SetInFile(path, SetTarget{Section: "permissions", Key: "auto_approve"}, `["Bash:ls *", "Read:/tmp/*"]`, 0640); err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if !strings.Contains(result, `auto_approve = ["Bash:ls *", "Read:/tmp/*"]`) {
		t.Errorf("new single-line array not found:\n%s", result)
	}
	for _, orphan := range []string{`"Bash:git *"`, `"Read:*"`} {
		if strings.Contains(result, orphan) {
			t.Errorf("orphaned old-array fragment %q left in file (corruption):\n%s", orphan, result)
		}
	}
	// No line should be a bare "]" (the old array's dangling close).
	for _, line := range strings.Split(result, "\n") {
		if strings.TrimSpace(line) == "]" {
			t.Errorf("orphaned bare ] line left in file (corruption):\n%s", result)
		}
	}
	if !strings.Contains(result, "other_key = true") {
		t.Errorf("adjacent key not preserved:\n%s", result)
	}
}

func TestFormatTOMLValue_StringList(t *testing.T) {
	got, err := FormatTOMLValue(`["a","b c","d"]`, FieldStringList)
	if err != nil {
		t.Fatalf("FormatTOMLValue: %v", err)
	}
	if want := `["a", "b c", "d"]`; got != want {
		t.Errorf("FormatTOMLValue = %q, want %q", got, want)
	}
	if _, err := FormatTOMLValue(`not json`, FieldStringList); err == nil {
		t.Error("FormatTOMLValue accepted non-JSON string list, want error")
	}
	if got, _ := FormatTOMLValue(`[]`, FieldStringList); got != "[]" {
		t.Errorf("empty list = %q, want []", got)
	}
}

func TestSetInFile_InsertNewKey(t *testing.T) {
	// Proves that SetInFile inserts a new key into an existing section, returns an
	// empty old value, and places the key within the correct section boundaries.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[agent_loop]
model = "haiku"

[sessions]
dir = "/tmp"
`
	os.WriteFile(path, []byte(content), 0o644)

	old, err := SetInFile(path, SetTarget{Section: "agent_loop", Key: "max_tool_loops"}, "50", 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if old != "" {
		t.Errorf("old value = %q, want empty (new key)", old)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if !strings.Contains(result, "max_tool_loops = 50") {
		t.Errorf("new key not found in output:\n%s", result)
	}
	// Must be in the agent_loop section, not after [sessions]
	agentLoopIdx := strings.Index(result, "[agent_loop]")
	sessionsIdx := strings.Index(result, "[sessions]")
	keyIdx := strings.Index(result, "max_tool_loops")
	if keyIdx < agentLoopIdx || keyIdx > sessionsIdx {
		t.Errorf("new key inserted outside [agent_loop] section")
	}
}

func TestSetInFile_CreateNewSection(t *testing.T) {
	// Proves that SetInFile creates a new section header and inserts the key/value
	// when the target section does not yet exist in the file.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[agent_loop]
model = "haiku"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "keepalive", Key: "enabled"}, "true", 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if !strings.Contains(result, "[keepalive]") {
		t.Errorf("new section not found in output:\n%s", result)
	}
	if !strings.Contains(result, "enabled = true") {
		t.Errorf("new key not found in output:\n%s", result)
	}
}

func TestSetInFile_NestedMapSection(t *testing.T) {
	// Regression test for MapFieldSpec's longest-prefix-match: this fixture
	// mirrors the real live foci.toml, which has a bare [groups] section
	// (group name → model) AND a separate explicit [groups.fallbacks]
	// section. LookupField must resolve "groups.fallbacks.stepfun" to
	// Section="groups.fallbacks" (not the shorter "groups" prefix) — getting
	// this backwards doesn't corrupt the file (BurntSushi/toml tolerates a
	// dotted key implicitly opening a table an explicit header later extends
	// — verified empirically), it just writes "fallbacks.stepfun = ..." into
	// the wrong (but still valid) place: [groups]'s own body instead of
	// [groups.fallbacks], confusing on read and in diffs.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[groups]
powerful = "qwen35"
fast = "stepfun"
cheap = "stepfun"

[groups.fallbacks]
stepfun = "minimax"
glmturbo = "minimax"
`
	os.WriteFile(path, []byte(content), 0o644)

	field, ok := LookupField("groups.fallbacks.stepfun")
	if !ok {
		t.Fatal("LookupField(\"groups.fallbacks.stepfun\") not found")
	}
	if field.Section != "groups.fallbacks" || field.Key != "stepfun" {
		t.Fatalf("field = %+v, want Section=groups.fallbacks Key=stepfun", field)
	}

	_, err := SetInFile(path, SetTarget{Section: field.Section, Key: field.Key}, `"qwen35"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if strings.Count(result, "[groups.fallbacks]") != 1 {
		t.Errorf("expected exactly one [groups.fallbacks] header, got:\n%s", result)
	}
	if strings.Contains(result, "fallbacks.stepfun") {
		t.Errorf("wrote a dotted fallbacks.stepfun key into [groups]'s own body — wrong (if not corrupt) placement:\n%s", result)
	}
	if !strings.Contains(result, `stepfun = "qwen35"`) {
		t.Errorf("updated value not found:\n%s", result)
	}
	// [groups] bare group defs must survive untouched.
	if !strings.Contains(result, `powerful = "qwen35"`) {
		t.Errorf("[groups] bare defs not preserved:\n%s", result)
	}
}

func TestSetInFile_NewSectionBeforeAgents(t *testing.T) {
	// Proves that a newly-created section is inserted before any [[agents]] blocks
	// to maintain the conventional ordering of the config file.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[agent_loop]
model = "haiku"

[[agents]]
id = "main"
model = "sonnet"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "keepalive", Key: "enabled"}, "true", 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	keepaliveIdx := strings.Index(result, "[keepalive]")
	agentsIdx := strings.Index(result, "[[agents]]")
	if keepaliveIdx > agentsIdx {
		t.Errorf("[keepalive] should appear before [[agents]] in output:\n%s", result)
	}
}

func TestSetInFile_AgentBlock(t *testing.T) {
	// Proves that SetInFile updates only the [[agents]] block with the matching ID,
	// leaving other agents' values unchanged, and returns the old value.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[agent_loop]
model = "haiku"

[[agents]]
id = "alpha"
model = "sonnet"

[[agents]]
id = "beta"
model = "haiku"
`
	os.WriteFile(path, []byte(content), 0o644)

	old, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "beta", Key: "model"}, `"opus"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if old != `"haiku"` {
		t.Errorf("old value = %q, want %q", old, `"haiku"`)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	// Alpha should be unchanged
	lines := strings.Split(result, "\n")
	alphaBlock := false
	for _, line := range lines {
		if strings.Contains(line, `id = "alpha"`) {
			alphaBlock = true
		}
		if alphaBlock && strings.HasPrefix(strings.TrimSpace(line), "model") {
			if !strings.Contains(line, `"sonnet"`) {
				t.Errorf("alpha's model was changed: %q", line)
			}
			break
		}
	}

	// Beta should be updated
	betaBlock := false
	for _, line := range lines {
		if strings.Contains(line, `id = "beta"`) {
			betaBlock = true
		}
		if betaBlock && strings.HasPrefix(strings.TrimSpace(line), "model") {
			if !strings.Contains(line, `"opus"`) {
				t.Errorf("beta's model was not updated: %q", line)
			}
			break
		}
	}
}

func TestSetInFile_AgentNotFound(t *testing.T) {
	// Proves that SetInFile returns an error mentioning the missing ID when no
	// [[agents]] block with the requested AgentID exists.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[[agents]]
id = "alpha"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "nonexistent", Key: "model"}, `"opus"`, 0640)
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q", err)
	}
}

func TestSetInFile_AgentInsertKey(t *testing.T) {
	// Proves that SetInFile inserts a new key into the correct [[agents]] block
	// when the key does not yet exist in that agent's section.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[[agents]]
id = "main"
model = "sonnet"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "main", Key: "effort"}, `"high"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if !strings.Contains(result, `effort = "high"`) {
		t.Errorf("new key not found in output:\n%s", result)
	}
}

func TestSetInFile_AgentDottedKey_CreatesExplicitTable(t *testing.T) {
	// Proves a NEW dotted per-agent key gets its own [agents.<tablePath>]
	// table rather than an inline dotted key in the [[agents]] block's own
	// body — the "upgrade dotted keys to tables" behavior.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"

[[agents]]
id = "main"
model = "sonnet"
`), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "main", Key: "loop.max_tool_loops"}, "50", 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if strings.Contains(result, "loop.max_tool_loops") {
		t.Errorf("wrote an inline dotted key instead of upgrading to a table:\n%s", result)
	}
	if !strings.Contains(result, "[agents.loop]") {
		t.Errorf("expected a new [agents.loop] table:\n%s", result)
	}
	if !strings.Contains(result, "max_tool_loops = 50") {
		t.Errorf("leaf key/value not found:\n%s", result)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, result)
	}
	if cfg.Agents[0].Loop.MaxToolLoops == nil || *cfg.Agents[0].Loop.MaxToolLoops != 50 {
		t.Errorf("decoded Agents[0].Loop.MaxToolLoops = %v, want 50", cfg.Agents[0].Loop.MaxToolLoops)
	}
}

func TestSetInFile_AgentDottedKey_ReusesExistingTable(t *testing.T) {
	// Proves a new leaf under an ALREADY-EXISTING [agents.<tablePath>] table
	// (just missing this specific leaf) is inserted into that table, not
	// duplicated as an inline key or a second header.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"

[[agents]]
id = "main"
model = "sonnet"

[agents.groups.calls]
existing-site = "fast"
`), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "main", Key: "groups.calls.new-site"}, `"cheap"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if strings.Count(result, "[agents.groups.calls]") != 1 {
		t.Errorf("expected exactly one [agents.groups.calls] header:\n%s", result)
	}
	if strings.Contains(result, "groups.calls.new-site") {
		t.Errorf("wrote an inline dotted key instead of reusing the existing table:\n%s", result)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, result)
	}
	want := map[string]string{"existing-site": "fast", "new-site": "cheap"}
	got := cfg.Agents[0].Groups.Calls
	if len(got) != len(want) || got["existing-site"] != "fast" || got["new-site"] != "cheap" {
		t.Errorf("decoded Agents[0].Groups.Calls = %+v, want %+v", got, want)
	}
}

func TestSetInFile_AgentDottedKey_LongestTableMatchWins(t *testing.T) {
	// When both [agents.groups] and [agents.groups.calls] exist, a new
	// "groups.calls.X" key must land in the more specific groups.calls
	// table, not the shorter groups table.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"

[[agents]]
id = "main"
model = "sonnet"

[agents.groups]
myteam = "anthropic/claude-haiku-4-5"

[agents.groups.calls]
existing-site = "fast"
`), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "main", Key: "groups.calls.new-site"}, `"cheap"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("Load: %v\n%s", err, data)
	}
	if cfg.Agents[0].Groups.Calls["new-site"] != "cheap" {
		t.Errorf("Agents[0].Groups.Calls[new-site] = %q, want cheap", cfg.Agents[0].Groups.Calls["new-site"])
	}
	// The bare [agents.groups] table must survive untouched.
	if cfg.Agents[0].Groups.Groups["myteam"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("Agents[0].Groups.Groups[myteam] corrupted: %+v", cfg.Agents[0].Groups.Groups)
	}
}

func TestSetInFile_AgentFlatKey_StillUsesInlineForm(t *testing.T) {
	// A non-dotted key has no table to "upgrade" to — must keep using the
	// existing flat inline-key behavior unchanged.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`[[agents]]
id = "main"
model = "sonnet"
`), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "main", Key: "workspace"}, `"/tmp/x"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)
	if strings.Contains(result, "[agents.workspace]") {
		t.Errorf("flat key wrongly upgraded to a table:\n%s", result)
	}
	if !strings.Contains(result, `workspace = "/tmp/x"`) {
		t.Errorf("value not found:\n%s", result)
	}
}

func TestFormatTOMLValue(t *testing.T) {
	// Proves that FormatTOMLValue correctly formats values for string, int, float,
	// bool (including yes/no/1/0 aliases), and duration field types, and returns
	// an error for invalid values.
	tests := []struct {
		value   string
		ft      FieldType
		want    string
		wantErr bool
	}{
		{"hello", FieldString, `"hello"`, false},
		{`"already-quoted"`, FieldString, `"already-quoted"`, false},
		{"42", FieldInt, "42", false},
		{"abc", FieldInt, "", true},
		{"3.14", FieldFloat, "3.14", false},
		{"nope", FieldFloat, "", true},
		{"true", FieldBool, "true", false},
		{"false", FieldBool, "false", false},
		{"yes", FieldBool, "true", false},
		{"no", FieldBool, "false", false},
		{"1", FieldBool, "true", false},
		{"0", FieldBool, "false", false},
		{"maybe", FieldBool, "", true},
		{"5m", FieldDuration, `"5m"`, false},
		// Durations are type-checked like every other non-string type:
		// the runtime parses them with time.ParseDuration, so junk is
		// refused here rather than after the next reload. The
		// already-quoted passthrough is checked on its inner text;
		// "0" (a documented disable value) and "" ("empty = default"
		// fields) stay valid.
		{`"5m"`, FieldDuration, `"5m"`, false},
		{"1h30m", FieldDuration, `"1h30m"`, false},
		{"0", FieldDuration, `"0"`, false},
		{"", FieldDuration, `""`, false},
		{"banana", FieldDuration, "", true},
		{`"banana"`, FieldDuration, "", true},
	}

	for _, tt := range tests {
		got, err := FormatTOMLValue(tt.value, tt.ft)
		if tt.wantErr {
			if err == nil {
				t.Errorf("FormatTOMLValue(%q, %d): expected error", tt.value, tt.ft)
			}
			continue
		}
		if err != nil {
			t.Errorf("FormatTOMLValue(%q, %d): %v", tt.value, tt.ft, err)
			continue
		}
		if got != tt.want {
			t.Errorf("FormatTOMLValue(%q, %d) = %q, want %q", tt.value, tt.ft, got, tt.want)
		}
	}
}

// decodeSingleStringRoundTrip parses "v = <formatted>" with the same TOML
// parser Load uses and returns the single string value key v holds — the
// contract every FieldString result must satisfy.
func decodeSingleStringRoundTrip(t *testing.T, formatted string) string {
	t.Helper()
	var m map[string]any
	if err := tomlParser.Unmarshal([]byte("v = "+formatted), &m); err != nil {
		t.Fatalf("v = %s does not parse as TOML: %v", formatted, err)
	}
	if len(m) != 1 {
		t.Fatalf("v = %s decoded to %d keys (%v), want exactly one key v", formatted, len(m), m)
	}
	s, ok := m["v"].(string)
	if !ok {
		t.Fatalf("v = %s decoded to %T, want string", formatted, m["v"])
	}
	return s
}

// TestFormatTOMLValueStringRefusesValuesThatAreNotOneTOMLString proves a
// FieldString value that is not exactly one well-formed TOML string is refused
// rather than written into foci.toml: trailing garbage after the closing quote,
// a backslash that unterminates the string, a second key smuggled after a
// newline, and invalid UTF-8 (which TOML cannot hold at all).
func TestFormatTOMLValueStringRefusesValuesThatAreNotOneTOMLString(t *testing.T) {
	for _, v := range []string{
		`"x" y "z"`,              // trailing garbage after the closing quote
		`"a\"`,                   // the backslash escapes the quote: string never terminates
		"\"x\"\nbackend = \"y\"", // a second key after a newline
		"\"a\xffb\"",             // invalid UTF-8 (a real 0xFF byte) inside the quotes
	} {
		if _, err := FormatTOMLValue(v, FieldString); err == nil {
			t.Errorf("FormatTOMLValue(%q, FieldString) accepted a value that is not one TOML string, want error", v)
		}
	}
}

// TestFormatTOMLValueStringEncodesControlCharacters proves FieldString encodes
// control characters as escapes TOML actually has (\uXXXX for U+0007/U+000B,
// which Go's %q writes as the TOML-invalid \a and \v), that the one-character
// value `"` is encoded as the string `"` rather than passed through as an
// unterminated quote, and that each result decodes back to the exact input.
func TestFormatTOMLValueStringEncodesControlCharacters(t *testing.T) {
	for _, v := range []string{"a\x07b", "a\x0bb", `"`} {
		got, err := FormatTOMLValue(v, FieldString)
		if err != nil {
			t.Fatalf("FormatTOMLValue(%q, FieldString): %v", v, err)
		}
		if back := decodeSingleStringRoundTrip(t, got); back != v {
			t.Errorf("FormatTOMLValue(%q) = %s, which decodes back to %q — not the exact input", v, got, back)
		}
	}
}

// TestFormatTOMLValueStringQuotedValueKeepsDecodedString proves an
// already-quoted value keeps only its decoded TOML meaning: `"x" # "y"` (a
// quoted string with a trailing comment) becomes exactly `"x"`, not a line
// carrying the comment into foci.toml.
func TestFormatTOMLValueStringQuotedValueKeepsDecodedString(t *testing.T) {
	got, err := FormatTOMLValue(`"x" # "y"`, FieldString)
	if err != nil {
		t.Fatalf("FormatTOMLValue: %v", err)
	}
	if got != `"x"` {
		t.Errorf("FormatTOMLValue(`\"x\" # \"y\"`) = %s, want \"x\"", got)
	}
}

// TestFormatTOMLValueStringListEncodesControlCharacters proves every
// FieldStringList item is encoded with TOML-legal escapes, so a list holding
// U+0007 round-trips instead of writing \a (which the next Load rejects).
func TestFormatTOMLValueStringListEncodesControlCharacters(t *testing.T) {
	got, err := FormatTOMLValue(`["a\u0007b","c"]`, FieldStringList)
	if err != nil {
		t.Fatalf("FormatTOMLValue: %v", err)
	}
	var m map[string]any
	if err := tomlParser.Unmarshal([]byte("v = "+got), &m); err != nil {
		t.Fatalf("v = %s does not parse as TOML: %v", got, err)
	}
	items, ok := m["v"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("v = %s decoded to %#v, want a 2-item array", got, m["v"])
	}
	if items[0] != "a\x07b" || items[1] != "c" {
		t.Errorf("v = %s decoded to [%#v %#v], want [a\\x07b c]", got, items[0], items[1])
	}
}

// TestFormatTOMLValueStringAcceptedRoundTrip is a characterisation test: plain
// values, already-quoted values, embedded quotes and TOML escapes in quoted
// values all decode back to exactly the intended string today, and the encoder
// change must keep that.
func TestFormatTOMLValueStringAcceptedRoundTrip(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{
		{"hello", "hello"},
		{`"already-quoted"`, "already-quoted"},
		{`say "hi"`, `say "hi"`},
		{`"a\tb"`, "a\tb"}, // a quoted value using a TOML escape
	} {
		got, err := FormatTOMLValue(tt.raw, FieldString)
		if err != nil {
			t.Fatalf("FormatTOMLValue(%q, FieldString): %v", tt.raw, err)
		}
		if back := decodeSingleStringRoundTrip(t, got); back != tt.want {
			t.Errorf("FormatTOMLValue(%q) = %s, which decodes back to %q, want %q", tt.raw, got, back, tt.want)
		}
	}
}

func TestSetInFile_PreserveComments(t *testing.T) {
	// Proves that SetInFile preserves all comments (top-level, inline, and section
	// comments) when updating a key, leaving the surrounding text untouched.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `# Top-level comment

[agent_loop]
# Model configuration
model = "haiku"
# Tool loops
max_tool_loops = 25

# Sessions section
[sessions]
dir = "/tmp"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agent_loop", Key: "model"}, `"opus"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	for _, want := range []string{
		"# Top-level comment",
		"# Model configuration",
		"# Tool loops",
		"max_tool_loops = 25",
		"# Sessions section",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("missing preserved content %q in output:\n%s", want, result)
		}
	}
}

func TestSetInFile_RoundTrip(t *testing.T) {
	// Proves that a value written by SetInFile can be read back correctly by Load,
	// confirming that the file output is valid TOML with the expected field value.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[sessions]
dir = "` + filepath.Join(dir, "sessions") + `"

[logging]
event_file = "` + filepath.Join(dir, "foci.log") + `"
api_file = "` + filepath.Join(dir, "api.jsonl") + `"

[http]
port = 8080
`
	os.WriteFile(path, []byte(content), 0o644)
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	_, err := SetInFile(path, SetTarget{Section: "groups", Key: "powerful"}, `"anthropic/claude-sonnet-4-5-20250929"`, 0640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load after set: %v", err)
	}
	if cfg.Groups.Groups["powerful"] != "anthropic/claude-sonnet-4-5-20250929" {
		t.Errorf("groups.powerful = %q after round-trip", cfg.Groups.Groups["powerful"])
	}
}

// setDirectRoundTrip runs a /config-set-shaped write through LookupField →
// SetInFile → Load, the same path ConfigSetDirect takes, and returns the
// reloaded config for the caller to assert against. t.Fatal's on any error.
func setDirectRoundTrip(t *testing.T, path, settPath, rawValue string) *Config {
	t.Helper()
	field, ok := LookupField(settPath)
	if !ok {
		t.Fatalf("LookupField(%q) not found", settPath)
	}
	formatted, err := FormatTOMLValue(rawValue, field.Type)
	if err != nil {
		t.Fatalf("FormatTOMLValue: %v", err)
	}
	if _, err := SetInFile(path, SetTarget{Section: field.Section, Key: field.Key}, formatted, 0640); err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("Load after set: %v\n--- resulting file ---\n%s", err, data)
	}
	return cfg
}

func TestSetInFile_MapSections_RoundTrip(t *testing.T) {
	// Diverse add/update scenarios for every registered map section, each
	// proven by an actual Load() round-trip (not just string-matching the
	// written bytes) — the only way to be sure the output is valid,
	// semantically-correct TOML and not just plausible-looking text.
	t.Run("new group added to existing [groups] with calls+fallbacks after it", func(t *testing.T) {
		// Mirrors the real live foci.toml shape: bare [groups] followed by
		// separate [groups.calls] and [groups.fallbacks] tables. Adding a new
		// bare group name must land inside [groups]'s own bounds, not spill
		// into or past the following sub-tables.
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"
fast = "anthropic/claude-haiku-4-5"

[groups.calls]
summarize-file = "cheap"

[groups.fallbacks]
"anthropic/claude-haiku-4-5" = "anthropic/claude-opus-4-6"
`), 0o644)

		cfg := setDirectRoundTrip(t, path, "groups.myteam", "openrouter/some-model")
		if cfg.Groups.Groups["myteam"] != "openrouter/some-model" {
			t.Errorf("groups.myteam = %q", cfg.Groups.Groups["myteam"])
		}
		// Existing entries in all three tables must survive untouched.
		if cfg.Groups.Groups["powerful"] != "anthropic/claude-opus-4-6" || cfg.Groups.Groups["fast"] != "anthropic/claude-haiku-4-5" {
			t.Errorf("bare groups corrupted: %+v", cfg.Groups.Groups)
		}
		if cfg.Groups.Calls["summarize-file"] != "cheap" {
			t.Errorf("groups.calls corrupted: %+v", cfg.Groups.Calls)
		}
		if cfg.Groups.Fallbacks["anthropic/claude-haiku-4-5"] != "anthropic/claude-opus-4-6" {
			t.Errorf("groups.fallbacks corrupted: %+v", cfg.Groups.Fallbacks)
		}
	})

	t.Run("update existing fallback without disturbing bare groups before it", func(t *testing.T) {
		// Fallback keys/values are validated as models (validateFallbacks),
		// which accepts a [models.*] ALIAS as well as a raw developer/model_id
		// string — and aliases are the realistic case here: a raw model_id
		// contains "/" (and often ".") which matchMapField refuses as a key
		// (see bareTOMLKeyRe), so real [groups.fallbacks] keys set live
		// through /config set have to be aliases. Mirrors the live foci.toml,
		// whose [groups.fallbacks] entries are all short alias names.
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[models.opus]
model = "anthropic/claude-opus-4-6"

[models.haiku]
model = "anthropic/claude-haiku-4-5"

[models.gemini]
model = "google/gemini-2.5-flash"

[groups]
powerful = "opus"
fast = "haiku"
cheap = "haiku"

[groups.fallbacks]
haiku = "opus"
`), 0o644)

		cfg := setDirectRoundTrip(t, path, "groups.fallbacks.haiku", "gemini")
		if cfg.Groups.Fallbacks["haiku"] != "gemini" {
			t.Errorf("groups.fallbacks.haiku = %q, want gemini", cfg.Groups.Fallbacks["haiku"])
		}
		if cfg.Groups.Groups["powerful"] != "opus" || cfg.Groups.Groups["cheap"] != "haiku" {
			t.Errorf("bare groups corrupted by a fallbacks-section edit: %+v", cfg.Groups.Groups)
		}
	})

	t.Run("groups.calls created from scratch when only bare [groups] exists", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"
`), 0o644)

		cfg := setDirectRoundTrip(t, path, "groups.calls.summarize-file", "cheap")
		if cfg.Groups.Calls["summarize-file"] != "cheap" {
			t.Errorf("groups.calls.summarize-file = %q", cfg.Groups.Calls["summarize-file"])
		}
		if cfg.Groups.Groups["powerful"] != "anthropic/claude-opus-4-6" {
			t.Errorf("bare groups corrupted: %+v", cfg.Groups.Groups)
		}
	})

	t.Run("system.webhooks created from scratch when [system] does not exist at all", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"
`), 0o644)

		cfg := setDirectRoundTrip(t, path, "system.webhooks.deploy", "deploy.md")
		if cfg.System.Webhooks["deploy"] != "deploy.md" {
			t.Errorf("system.webhooks.deploy = %q", cfg.System.Webhooks["deploy"])
		}
	})

	t.Run("system.webhooks created from scratch alongside an existing [system] with other scalar keys", func(t *testing.T) {
		// [system] and [system.webhooks] are DISTINCT registry sections
		// (setInSection matches section headers literally) — proves adding
		// webhooks doesn't touch [system]'s own unrelated keys.
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[system]
system_files = ["notes.md"]
`), 0o644)

		cfg := setDirectRoundTrip(t, path, "system.webhooks.deploy", "deploy.md")
		if cfg.System.Webhooks["deploy"] != "deploy.md" {
			t.Errorf("system.webhooks.deploy = %q", cfg.System.Webhooks["deploy"])
		}
		if len(cfg.System.SystemFiles) != 1 || cfg.System.SystemFiles[0] != "notes.md" {
			t.Errorf("[system]'s own key corrupted: got %+v", cfg.System.SystemFiles)
		}
	})

	t.Run("update existing webhook and add a second one", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[system.webhooks]
deploy = "old-deploy.md"
`), 0o644)

		cfg := setDirectRoundTrip(t, path, "system.webhooks.deploy", "new-deploy.md")
		if cfg.System.Webhooks["deploy"] != "new-deploy.md" {
			t.Errorf("system.webhooks.deploy = %q, want new-deploy.md", cfg.System.Webhooks["deploy"])
		}

		cfg2 := setDirectRoundTrip(t, path, "system.webhooks.alert", "alert.md")
		if cfg2.System.Webhooks["alert"] != "alert.md" {
			t.Errorf("system.webhooks.alert = %q", cfg2.System.Webhooks["alert"])
		}
		if cfg2.System.Webhooks["deploy"] != "new-deploy.md" {
			t.Errorf("earlier update lost after adding a second key: %+v", cfg2.System.Webhooks)
		}
	})

	t.Run("value containing special TOML characters round-trips", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte("[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n"), 0o644)

		cfg := setDirectRoundTrip(t, path, "system.webhooks.deploy", `prompts/deploy "v2".md`)
		if cfg.System.Webhooks["deploy"] != `prompts/deploy "v2".md` {
			t.Errorf("system.webhooks.deploy = %q", cfg.System.Webhooks["deploy"])
		}
	})
}

func TestSetInFile_AgentMapSection_RoundTrip(t *testing.T) {
	// End-to-end per-agent map-field write (#1231): LookupField → the
	// "agent"→"agents"+AgentID remapping ConfigSetDirect does → SetInFile →
	// a real Load(). Proves the whole chain, not just the pieces individually.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-opus-4-6"

[[agents]]
id = "alpha"
workspace = "`+filepath.Join(dir, "alpha")+`"

[[agents]]
id = "beta"
workspace = "`+filepath.Join(dir, "beta")+`"
`), 0o644)

	set := func(agentID, settPath, rawValue string) *Config {
		t.Helper()
		field, ok := LookupField(settPath)
		if !ok {
			t.Fatalf("LookupField(%q) not found", settPath)
		}
		if field.Section != "agent" {
			t.Fatalf("LookupField(%q).Section = %q, want agent", settPath, field.Section)
		}
		formatted, err := FormatTOMLValue(rawValue, field.Type)
		if err != nil {
			t.Fatalf("FormatTOMLValue: %v", err)
		}
		if _, err := SetInFile(path, SetTarget{Section: "agents", AgentID: agentID, Key: field.Key}, formatted, 0640); err != nil {
			t.Fatalf("SetInFile: %v", err)
		}
		cfg, err := Load(path, nil)
		if err != nil {
			data, _ := os.ReadFile(path)
			t.Fatalf("Load: %v\n--- resulting file ---\n%s", err, data)
		}
		return cfg
	}

	cfg := set("alpha", "agent.groups.myteam", "anthropic/claude-haiku-4-5")
	if cfg.Agents[0].ID != "alpha" || cfg.Agents[0].Groups.Groups["myteam"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("Agents[0] (alpha).Groups.Groups = %+v", cfg.Agents[0].Groups.Groups)
	}
	// Beta is untouched by alpha's write.
	if len(cfg.Agents[1].Groups.Groups) != 0 {
		t.Errorf("Agents[1] (beta).Groups.Groups = %+v, want empty", cfg.Agents[1].Groups.Groups)
	}

	cfg = set("beta", "agent.system.webhooks.deploy", "deploy.md")
	if cfg.Agents[1].System.Webhooks["deploy"] != "deploy.md" {
		t.Errorf("Agents[1] (beta).System.Webhooks = %+v", cfg.Agents[1].System.Webhooks)
	}
	// Alpha's earlier group write must survive beta's independent edit.
	if cfg.Agents[0].Groups.Groups["myteam"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("alpha's earlier write was lost: %+v", cfg.Agents[0].Groups.Groups)
	}

	// Resolve() merge: alpha keeps its own override AND inherits the global default.
	rc := Resolve(cfg, cfg.Agents[0])
	if rc.Groups.Groups["myteam"] != "anthropic/claude-haiku-4-5" || rc.Groups.Groups["powerful"] != "anthropic/claude-opus-4-6" {
		t.Errorf("resolved Groups.Groups = %+v", rc.Groups.Groups)
	}
}

func TestLookupField_MapSections_RejectsNonBareKey(t *testing.T) {
	// A group/call-site/hook name containing a "." would be written as an
	// unquoted TOML dotted key (implicit nested tables, not the flat entry
	// intended); "/" and other non-bare-key characters simply fail to parse
	// as a bare key at all. Both are refused rather than risking a corrupt or
	// unparseable file — see matchMapField/bareTOMLKeyRe's doc.
	for _, path := range []string{
		"groups.calls.foo.bar",
		"groups.my.team",
		"system.webhooks.v1.deploy",
		"groups.fallbacks.anthropic/claude-haiku-4-5", // raw model_id, not an alias
		"groups.my team",                              // space
	} {
		if _, ok := LookupField(path); ok {
			t.Errorf("LookupField(%q) should be refused (not a bare TOML key), but matched", path)
		}
	}
}

func TestSetInFile_MapSection_PreExistingInlineTable_FailsLoudly(t *testing.T) {
	// Documents the known limitation noted on MapFieldSpec: SetInFile always
	// writes an EXPANDED [Section] table, never an inline one. If a section
	// already exists in inline-table form (webhooks = { ... }, the form
	// docs/CONFIG.md shows), writing a new key creates a SECOND, conflicting
	// definition of the same table path. This must fail LOUDLY at Load
	// (a config that refuses to (re)start is safe; one that silently
	// corrupts is not) rather than silently picking one definition.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`[system]
webhooks = { deploy = "deploy.md" }
`), 0o644)

	field, ok := LookupField("system.webhooks.alert")
	if !ok {
		t.Fatal("LookupField(system.webhooks.alert) not found")
	}
	if _, err := SetInFile(path, SetTarget{Section: field.Section, Key: field.Key}, `"alert.md"`, 0640); err != nil {
		t.Fatalf("SetInFile itself should still succeed (it's a pure text edit): %v", err)
	}

	if _, err := Load(path, nil); err == nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("Load unexpectedly succeeded on a file with both inline and expanded [system.webhooks] — silent corruption risk:\n%s", data)
	}
}

// TestFormatTOMLValueScheduleRefusesBadValues proves the FieldSchedule case
// holds schedule values to the same acceptance rule as the load-time walk:
// neither a "HH:MM" clock time nor a positive Go duration is refused BEFORE
// any file write, quoted or not — including the non-positive durations "0s",
// "0" and "-5m" the run-time scheduler would reject on every tick.
func TestFormatTOMLValueScheduleRefusesBadValues(t *testing.T) {
	for _, v := range []string{"banana", "25:00", "12:60", "0s", "0", "-5m", `"banana"`} {
		if _, err := FormatTOMLValue(v, FieldSchedule); err == nil {
			t.Errorf("FormatTOMLValue(%q, FieldSchedule) accepted a value that is not a clock time or positive duration, want error", v)
		}
	}
	if got, err := FormatTOMLValue("", FieldSchedule); err != nil || got != `""` {
		t.Errorf("FormatTOMLValue(\"\", FieldSchedule) = %q, %v; want %q, nil (empty = never)", got, err, `""`)
	}
}

// TestFormatTOMLValueLongPollTimeoutRowRefusesBadValue proves the registry
// row /config set and the app editor actually use for long_poll_timeout —
// not just the FieldType constant in isolation — refuses a value the Telegram
// bot would silently ignore at run time.
func TestFormatTOMLValueLongPollTimeoutRowRefusesBadValue(t *testing.T) {
	f, ok := LookupField("platforms.telegram.long_poll_timeout")
	if !ok {
		t.Fatal("LookupField(platforms.telegram.long_poll_timeout) returned false")
	}
	if _, err := FormatTOMLValue("banana", f.Type); err == nil {
		t.Errorf("FormatTOMLValue(banana, %v) accepted an invalid duration, want error", f.Type)
	}
}

// TestFormatTOMLValueScheduleRoundTrips is a characterisation test: the
// FieldSchedule case outputs exactly the quoting contract FieldDuration has
// always produced — a quoted string, with an already-quoted value passed
// through — so the documented clock form (04:00) and interval form ("20h")
// round-trip through /config set and the app editor, and each result decodes
// back with the real TOML parser to the same string.
func TestFormatTOMLValueScheduleRoundTrips(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"04:00", `"04:00"`},
		{`"20h"`, `"20h"`},
		{"", `""`},
	}
	for _, tt := range tests {
		got, err := FormatTOMLValue(tt.value, FieldSchedule)
		if err != nil {
			t.Fatalf("FormatTOMLValue(%q, FieldSchedule): %v", tt.value, err)
		}
		if got != tt.want {
			t.Errorf("FormatTOMLValue(%q, FieldSchedule) = %s, want %s", tt.value, got, tt.want)
		}
		if back := decodeSingleStringRoundTrip(t, got); back != strings.Trim(tt.value, `"`) {
			t.Errorf("v = %s decodes back to %q, want %q", got, back, strings.Trim(tt.value, `"`))
		}
	}
}
