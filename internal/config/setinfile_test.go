package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Verifies updating an existing key preserves the rest of the file.
func TestSetInFile_UpdateExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `# comment
[defaults]
model = "old-model"
max_tool_loops = 25

[sessions]
dir = "/tmp/sessions"
`
	os.WriteFile(path, []byte(content), 0o644)

	old, err := SetInFile(path, SetTarget{Section: "defaults", Key: "model"}, `"new-model"`)
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

// Verifies inserting a new key into an existing section.
func TestSetInFile_InsertNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[defaults]
model = "haiku"

[sessions]
dir = "/tmp"
`
	os.WriteFile(path, []byte(content), 0o644)

	old, err := SetInFile(path, SetTarget{Section: "defaults", Key: "max_tool_loops"}, "50")
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
	// Must be in the defaults section, not after [sessions]
	defaultsIdx := strings.Index(result, "[defaults]")
	sessionsIdx := strings.Index(result, "[sessions]")
	keyIdx := strings.Index(result, "max_tool_loops")
	if keyIdx < defaultsIdx || keyIdx > sessionsIdx {
		t.Errorf("new key inserted outside [defaults] section")
	}
}

// Verifies creating a new section when it doesn't exist.
func TestSetInFile_CreateNewSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[defaults]
model = "haiku"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "keepalive", Key: "enabled"}, "true")
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

// Verifies new sections are inserted before [[agents]] blocks.
func TestSetInFile_NewSectionBeforeAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[defaults]
model = "haiku"

[[agents]]
id = "main"
model = "sonnet"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "keepalive", Key: "enabled"}, "true")
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

// Verifies targeting the correct [[agents]] block by ID.
func TestSetInFile_AgentBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[defaults]
model = "haiku"

[[agents]]
id = "alpha"
model = "sonnet"

[[agents]]
id = "beta"
model = "haiku"
`
	os.WriteFile(path, []byte(content), 0o644)

	old, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "beta", Key: "model"}, `"opus"`)
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

// Verifies error when agent ID is not found.
func TestSetInFile_AgentNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[[agents]]
id = "alpha"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "nonexistent", Key: "model"}, `"opus"`)
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q", err)
	}
}

// Verifies inserting a new key into an agent block.
func TestSetInFile_AgentInsertKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[[agents]]
id = "main"
model = "sonnet"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "main", Key: "effort"}, `"high"`)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	if !strings.Contains(result, `effort = "high"`) {
		t.Errorf("new key not found in output:\n%s", result)
	}
}

// Verifies FormatTOMLValue for each field type.
func TestFormatTOMLValue(t *testing.T) {
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

// Verifies comments surrounding the edited section are preserved.
func TestSetInFile_PreserveComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `# Top-level comment

[defaults]
# Model configuration
model = "haiku"
# Tool loops
max_tool_loops = 25

# Sessions section
[sessions]
dir = "/tmp"
`
	os.WriteFile(path, []byte(content), 0o644)

	_, err := SetInFile(path, SetTarget{Section: "defaults", Key: "model"}, `"opus"`)
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

// Verifies round-trip: set a value, then Load() the result and check the field.
func TestSetInFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	content := `[defaults]
model = "anthropic/claude-haiku-4-5-20251001"

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

	_, err := SetInFile(path, SetTarget{Section: "defaults", Key: "model"}, `"anthropic/claude-sonnet-4-5-20250929"`)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after set: %v", err)
	}
	if cfg.Defaults.Model != "anthropic/claude-sonnet-4-5-20250929" {
		t.Errorf("model = %q after round-trip", cfg.Defaults.Model)
	}
}
