package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// ---------------------------------------------------------------------------
// BackendConfig TOML decoding
// ---------------------------------------------------------------------------

// TestBackendConfig_Decode_Full verifies every field on the real
// config.BackendConfig type decodes correctly from TOML, including the env
// sub-table. Uses BurntSushi directly (not Load) to isolate the struct from
// the rest of the config system.
func TestBackendConfig_Decode_Full(t *testing.T) {
	tomlData := `
model = "opus"
idle_timeout = "2h"
skip_permissions = true
socket_path = "/tmp/agent.sock"
binary = "/opt/opencode/bin/opencode"
hostname = "0.0.0.0"
server_auth = "s3cret"
log_level = "DEBUG"
port = 40799
default_permission = "allow"

allowed_tools = ["Write(/tmp/**)", "Bash(git:*)"]

[env]
FOO = "bar"
BAZ = "qux"
CCSTUB_EXIT_CODE = "1"
`
	var bc BackendConfig
	if _, err := toml.Decode(tomlData, &bc); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Pointer fields: verify non-nil and correct value.
	checkStr(t, bc.Model, "opus", "model")
	checkStr(t, bc.IdleTimeout, "2h", "idle_timeout")
	checkStr(t, bc.SocketPath, "/tmp/agent.sock", "socket_path")
	checkStr(t, bc.Binary, "/opt/opencode/bin/opencode", "binary")
	checkStr(t, bc.Hostname, "0.0.0.0", "hostname")
	checkStr(t, bc.ServerAuth, "s3cret", "server_auth")
	checkStr(t, bc.LogLevel, "DEBUG", "log_level")
	checkStr(t, bc.DefaultPermission, "allow", "default_permission")

	if bc.SkipPermissions == nil || !*bc.SkipPermissions {
		t.Error("skip_permissions = nil/false, want true")
	}
	if bc.Port == nil || *bc.Port != 40799 {
		t.Errorf("port = %v, want 40799", bc.Port)
	}

	// AllowedTools decodes as []string from a TOML array.
	if len(bc.AllowedTools) != 2 {
		t.Fatalf("allowed_tools len = %d, want 2", len(bc.AllowedTools))
	}
	if bc.AllowedTools[0] != "Write(/tmp/**)" || bc.AllowedTools[1] != "Bash(git:*)" {
		t.Errorf("allowed_tools = %v, want [Write(/tmp/**), Bash(git:*)]", bc.AllowedTools)
	}

	// Env sub-table decodes as map[string]string.
	if len(bc.Env) != 3 {
		t.Fatalf("env len = %d, want 3", len(bc.Env))
	}
	if bc.Env["FOO"] != "bar" {
		t.Errorf("env[FOO] = %q, want bar", bc.Env["FOO"])
	}
	if bc.Env["BAZ"] != "qux" {
		t.Errorf("env[BAZ] = %q, want qux", bc.Env["BAZ"])
	}
	if bc.Env["CCSTUB_EXIT_CODE"] != "1" {
		t.Errorf("env[CCSTUB_EXIT_CODE] = %q, want 1", bc.Env["CCSTUB_EXIT_CODE"])
	}
}

// TestBackendConfig_Decode_Empty verifies that a backend_config block with
// only some fields set leaves the others nil — not zero-value strings. This
// is what the config cascade Merge[T] relies on (nil = inherit).
func TestBackendConfig_Decode_Partial(t *testing.T) {
	tomlData := `
model = "sonnet"
[env]
KEY = "val"
`
	var bc BackendConfig
	if _, err := toml.Decode(tomlData, &bc); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	checkStr(t, bc.Model, "sonnet", "model")
	if bc.Env["KEY"] != "val" {
		t.Errorf("env[KEY] = %q, want val", bc.Env["KEY"])
	}

	// Everything else must be nil/empty, not zero-value.
	if bc.SkipPermissions != nil {
		t.Errorf("skip_permissions = %v, want nil", bc.SkipPermissions)
	}
	if bc.Port != nil {
		t.Errorf("port = %v, want nil", bc.Port)
	}
	if len(bc.AllowedTools) != 0 {
		t.Errorf("allowed_tools = %v, want empty", bc.AllowedTools)
	}
}

// TestBackendConfig_Decode_NoUndecoded verifies that a full backend_config
// block (including the env sub-table) produces NO undecoded keys. Before the
// struct change, map[string]any left env descendants as undecoded; the typed
// struct must consume them fully.
func TestBackendConfig_Decode_NoUndecoded(t *testing.T) {
	tomlData := `
[[agents]]
id = "test"

[agents.backend_config]
model = "opus"

[agents.backend_config.env]
FOO = "bar"
BAZ = "qux"
`
	var cfg struct {
		Agents []AgentConfig `toml:"agents"`
	}
	md, err := toml.Decode(tomlData, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) != 0 {
		var paths []string
		for _, k := range undecoded {
			paths = append(paths, strings.Join(k, "."))
		}
		t.Errorf("undecoded keys: %v", paths)
	}
}

// ---------------------------------------------------------------------------
// BackendConfig.ToMap
// ---------------------------------------------------------------------------

// TestBackendConfig_ToMap_Full verifies every populated field appears in the
// output map with the correct key name and Go type. This is the bridge
// between the typed struct and the delegator's map[string]any interface —
// a missing or misnamed key here silently drops config that a backend needs.
func TestBackendConfig_ToMap_Full(t *testing.T) {
	str := func(s string) *string { return &s }
	b := func(v bool) *bool { return &v }
	i := func(v int) *int { return &v }

	bc := BackendConfig{
		Model:             str("opus"),
		AllowedTools:      []string{"Write(/tmp/**)", "Bash(git:*)"},
		IdleTimeout:       str("2h"),
		SkipPermissions:   b(true),
		SocketPath:        str("/tmp/sock"),
		Env:               map[string]string{"FOO": "bar"},
		Binary:            str("/opt/oc"),
		Hostname:          str("127.0.0.1"),
		ServerAuth:        str("pw"),
		LogLevel:          str("DEBUG"),
		Port:              i(40799),
		DefaultPermission: str("ask"),
	}
	m := bc.ToMap()

	// String fields.
	for key, want := range map[string]string{
		"model": "opus", "idle_timeout": "2h",
		"socket_path": "/tmp/sock", "binary": "/opt/oc", "hostname": "127.0.0.1",
		"server_auth": "pw", "log_level": "DEBUG", "default_permission": "ask",
	} {
		got, ok := m[key]
		if !ok {
			t.Errorf("ToMap: key %q missing", key)
			continue
		}
		if got != want {
			t.Errorf("ToMap[%q] = %v, want %q", key, got, want)
		}
	}

	// AllowedTools is comma-joined (CC backends read it as a string).
	if got, ok := m["allowed_tools"]; !ok {
		t.Error("ToMap: allowed_tools missing")
	} else if got != "Write(/tmp/**),Bash(git:*)" {
		t.Errorf("ToMap[allowed_tools] = %v, want comma-joined string", got)
	}

	// Env preserved as map[string]string.
	if got, ok := m["env"]; !ok {
		t.Error("ToMap: env missing")
	} else if env, ok := got.(map[string]string); !ok || env["FOO"] != "bar" {
		t.Errorf("ToMap[env] = %v, want map[string]string{FOO:bar}", got)
	}

	// Bool field.
	if got, ok := m["skip_permissions"]; !ok {
		t.Error("ToMap: skip_permissions missing")
	} else if got != true {
		t.Errorf("ToMap[skip_permissions] = %v, want true", got)
	}

	// Int field.
	if got, ok := m["port"]; !ok {
		t.Error("ToMap: port missing")
	} else if got != 40799 {
		t.Errorf("ToMap[port] = %v, want 40799", got)
	}

	// Verify the map has exactly the expected key set.
	wantKeys := map[string]bool{
		"model": true, "allowed_tools": true,
		"idle_timeout": true, "skip_permissions": true, "socket_path": true,
		"env": true, "binary": true, "hostname": true,
		"server_auth": true, "log_level": true, "port": true, "default_permission": true,
	}
	if len(m) != len(wantKeys) {
		t.Errorf("ToMap has %d keys, want %d. Extra: %v", len(m), len(wantKeys), diffKeys(m, wantKeys))
	}
	for k := range m {
		if !wantKeys[k] {
			t.Errorf("ToMap has unexpected key %q", k)
		}
	}
}

// TestBackendConfig_ToMap_Empty verifies that a zero-value BackendConfig (all
// nil pointers, empty slices/maps) produces an empty map — no keys with zero
// values that could override a backend's defaults.
func TestBackendConfig_ToMap_Empty(t *testing.T) {
	bc := BackendConfig{}
	m := bc.ToMap()
	if len(m) != 0 {
		t.Errorf("ToMap of empty BackendConfig = %v, want empty map", m)
	}
}

// TestBackendConfig_ToMap_NilOmitted verifies that nil pointers and empty
// collections are omitted while populated fields appear. The key invariant:
// a nil pointer means "not set" and must NOT produce a key with a zero value.
func TestBackendConfig_ToMap_NilOmitted(t *testing.T) {
	model := "sonnet"
	bc := BackendConfig{
		Model: &model,
		// Everything else nil/empty.
	}
	m := bc.ToMap()

	if len(m) != 1 {
		t.Fatalf("ToMap len = %d, want 1 (only model). Got: %v", len(m), m)
	}
	if m["model"] != "sonnet" {
		t.Errorf("ToMap[model] = %v, want sonnet", m["model"])
	}
}

// TestBackendConfig_ToMap_EnvNil verifies that a nil Env map does not produce
// an "env" key. The DelegatedManager.Get merge allocates as needed when Env
// is nil, but an empty map key would be a harmless-but-confusing artifact.
func TestBackendConfig_ToMap_EnvNil(t *testing.T) {
	bc := BackendConfig{Model: ptrStr("opus")}
	m := bc.ToMap()
	if _, ok := m["env"]; ok {
		t.Error("ToMap: env key present for nil Env, want omitted")
	}
}

// ---------------------------------------------------------------------------
// MergedAllowedTools with []string (the new BackendConfig.AllowedTools type)
// ---------------------------------------------------------------------------

// TestMergedAllowedTools_StringSlicePerAgent verifies that MergedAllowedTools
// accepts []string — the type BackendConfig.AllowedTools produces. Previously
// tested with nil, string, and []any, but not []string which is now the
// canonical type flowing from the struct.
func TestMergedAllowedTools_StringSlicePerAgent(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Write(/tmp/**)"}}
	got := c.MergedAllowedTools([]string{"Bash(git:*)", "Read"})
	want := "Write(/tmp/**),Bash(git:*),Read"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Stale UnknownKeys test update
// ---------------------------------------------------------------------------

// The old TestUnknownKeysDetected test included a [agents.backend_config.env]
// fixture with a comment "backend_config.* descendants are free-form and must
// NOT be flagged." That rationale is stale — backend_config is now a typed
// struct, and nothing is undecoded because the decoder knows every field.
// The env fixture still serves its original purpose (proving backend_config
// keys don't appear in unknown keys), but now it's because the struct fully
// decodes, not because of a hardcoded skip. The test itself doesn't need
// changing — this comment block documents the rationale shift.

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func checkStr(t *testing.T, got *string, want, name string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %q", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %q, want %q", name, *got, want)
	}
}

func ptrStr(s string) *string { return &s }

func diffKeys(got map[string]any, want map[string]bool) []string {
	var extra []string
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	return extra
}
