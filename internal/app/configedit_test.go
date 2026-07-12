package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/config"
	"foci/internal/platform"
)

const configEditTOML = `[notify]
startup_notify = false

[[agents]]
id = "clutch"
name = "Clutch"

[agents.loop]
max_tool_loops = 50
`

// newConfigEditHub builds a hub whose deps.Config points at a real temp
// foci.toml, the way main wires the loaded config into providers.
func newConfigEditHub(t *testing.T) (*Hub, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(configEditTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{
		SourcePath: path,
		FileMode:   "0600",
		Agents:     []config.AgentConfig{{ID: "clutch", Name: "Clutch"}},
	}}
	return h, path
}

// lastConfigSchema returns the most recent config.schema frame the client
// received (nil if none).
func lastConfigSchema(t *testing.T, c *wsClient) map[string]any {
	t.Helper()
	var out map[string]any
	for _, f := range drain(t, c) {
		if f.t == fap.TypeConfigSchema {
			out = f.d
		}
	}
	return out
}

// scopeByID digs one scope out of a decoded config.schema payload.
func scopeByID(t *testing.T, schema map[string]any, id string) map[string]any {
	t.Helper()
	scopes, _ := schema["scopes"].([]any)
	for _, s := range scopes {
		m, _ := s.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("scope %q missing from schema", id)
	return nil
}

func explicitList(scope map[string]any) []string {
	raw, _ := scope["explicit"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// TestConfigGet_SchemaShape proves config.get answers with the registry fields
// and per-scope values, splitting explicitly-set keys from defaults: the
// global scope flags notify.startup_notify (in the file), the agent scope
// flags loop.max_tool_loops (in its sub-table), and an unset registry field
// appears with explicit absent.
func TestConfigGet_SchemaShape(t *testing.T) {
	h, _ := newConfigEditHub(t)
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}

	h.handleConfigGet(c)

	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("config.get must answer with a config.schema frame")
	}
	if fields, _ := schema["fields"].([]any); len(fields) == 0 {
		t.Fatal("schema carries no fields")
	}
	if schema["restartRequired"] != true {
		t.Error("v1 schema must set restartRequired")
	}

	global := scopeByID(t, schema, "")
	gx := explicitList(global)
	if len(gx) != 1 || gx[0] != "notify.startup_notify" {
		t.Errorf("global explicit = %v, want [notify.startup_notify]", gx)
	}
	gv, _ := global["values"].(map[string]any)
	if gv["notify.startup_notify"] != "false" {
		t.Errorf("explicit global value = %v, want false", gv["notify.startup_notify"])
	}

	agent := scopeByID(t, schema, "clutch")
	if agent["label"] != "Clutch" {
		t.Errorf("agent label = %v, want Clutch", agent["label"])
	}
	ax := explicitList(agent)
	found := false
	for _, k := range ax {
		if k == "agent.loop.max_tool_loops" {
			found = true
		}
		if k == "agent.name" {
			continue // name is in the block; fine either way
		}
	}
	if !found {
		t.Errorf("agent explicit = %v, must include agent.loop.max_tool_loops (sub-table)", ax)
	}
	av, _ := agent["values"].(map[string]any)
	if av["agent.loop.max_tool_loops"] != "50" {
		t.Errorf("agent explicit value = %v, want 50", av["agent.loop.max_tool_loops"])
	}
}

// TestConfigPut_WritesAndBroadcasts proves a valid config.put edits the file
// and fans a fresh schema to every configEdit client (a plain client gets
// nothing), while an invalid put answers only the requester with error set
// and leaves the file untouched.
func TestConfigPut_WritesAndBroadcasts(t *testing.T) {
	h, path := newConfigEditHub(t)
	a := fakeClient()
	a.features = map[string]struct{}{featureConfigEdit: {}}
	b := fakeClient()
	b.features = map[string]struct{}{featureConfigEdit: {}}
	plain := fakeClient()
	h.clients[a] = struct{}{}
	h.clients[b] = struct{}{}
	h.clients[plain] = struct{}{}

	h.handleConfigPut(a, fap.ConfigPut{Section: "notify", Key: "startup_notify", Value: "true"})

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "startup_notify = true") {
		t.Fatalf("file not updated:\n%s", data)
	}
	for _, c := range []*wsClient{a, b} {
		schema := lastConfigSchema(t, c)
		if schema == nil {
			t.Fatal("capable clients must receive the post-edit broadcast")
		}
		gv, _ := scopeByID(t, schema, "")["values"].(map[string]any)
		if gv["notify.startup_notify"] != "true" {
			t.Errorf("broadcast value = %v, want true", gv["notify.startup_notify"])
		}
	}
	if len(drain(t, plain)) != 0 {
		t.Error("a client without configEdit must not receive config.schema")
	}

	// Invalid: unknown field. Only the requester hears back, with error set.
	h.handleConfigPut(a, fap.ConfigPut{Section: "notify", Key: "nope", Value: "1"})
	schema := lastConfigSchema(t, a)
	if schema == nil || schema["error"] == nil || schema["error"] == "" {
		t.Error("invalid put must answer the requester with error set")
	}
	if got := lastConfigSchema(t, b); got != nil {
		t.Error("invalid put must not broadcast")
	}
}

// TestConfigPut_AgentScopeAndUnset proves an agent-scoped put lands in the
// agent's block (here: its existing [agents.loop] sub-table, no duplicate
// key), and config.unset reverts the key to inherited/default — after which
// the schema no longer lists it as explicit.
func TestConfigPut_AgentScopeAndUnset(t *testing.T) {
	h, path := newConfigEditHub(t)
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}

	h.handleConfigPut(c, fap.ConfigPut{Scope: "clutch", Section: "agent", Key: "loop.max_tool_loops", Value: "75"})
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "max_tool_loops = 75") {
		t.Fatalf("agent sub-table not updated:\n%s", data)
	}
	if strings.Contains(string(data), "loop.max_tool_loops =") {
		t.Fatalf("duplicate inline dotted key written:\n%s", data)
	}

	// Scope/section mismatches are rejected.
	h.handleConfigPut(c, fap.ConfigPut{Scope: "clutch", Section: "notify", Key: "startup_notify", Value: "true"})
	if schema := lastConfigSchema(t, c); schema == nil || schema["error"] == nil {
		t.Error("agent scope with a global section must fail")
	}
	h.handleConfigPut(c, fap.ConfigPut{Scope: "ghost", Section: "agent", Key: "loop.max_tool_loops", Value: "1"})
	if schema := lastConfigSchema(t, c); schema == nil || schema["error"] == nil {
		t.Error("unknown agent must fail")
	}

	h.handleConfigUnset(c, fap.ConfigUnset{Scope: "clutch", Section: "agent", Key: "loop.max_tool_loops"})
	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("unset must broadcast a fresh schema")
	}
	for _, k := range explicitList(scopeByID(t, schema, "clutch")) {
		if k == "agent.loop.max_tool_loops" {
			t.Error("unset key still listed as explicit")
		}
	}
}
