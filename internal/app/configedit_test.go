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

// TestConfigGet_EmitsMapSections proves map sections (groups) are advertised
// as a "map"-typed field with the current entries carried as a JSON object.
func TestConfigGet_EmitsMapSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	os.WriteFile(path, []byte("[groups]\npowerful = \"opus\"\nfast = \"haiku\"\n"), 0o600)
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{SourcePath: path, FileMode: "0600"}}
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}

	h.handleConfigGet(c)
	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("no config.schema frame")
	}

	fields, _ := schema["fields"].([]any)
	foundMap := false
	for _, fe := range fields {
		f, _ := fe.(map[string]any)
		if f["section"] == "groups" && f["type"] == "map" {
			foundMap = true
		}
	}
	if !foundMap {
		t.Error(`no {section:"groups", valueType:"map"} descriptor emitted`)
	}

	global := scopeByID(t, schema, "")
	gv, _ := global["values"].(map[string]any)
	got, _ := gv["groups"].(string)
	if !strings.Contains(got, `"powerful":"opus"`) || !strings.Contains(got, `"fast":"haiku"`) {
		t.Errorf("groups map value = %q, want a JSON object with powerful/fast", got)
	}
}

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
	if _, ok := schema["restartRequired"]; ok {
		t.Error("schema-level restartRequired was removed; per-field needsRestart carries it now")
	}
	if fields, _ := schema["fields"].([]any); len(fields) > 0 {
		f0, _ := fields[0].(map[string]any)
		if _, ok := f0["needsRestart"]; !ok {
			t.Error("each field must carry per-field needsRestart metadata")
		}
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

// TestConfigGet_EmitsObjectSections proves []struct sections (message_transforms)
// are advertised as an "object[]"-typed descriptor carrying its sub-field shapes
// in "fields", with the current entries as a JSON array at Values[section].
func TestConfigGet_EmitsObjectSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	os.WriteFile(path, []byte("[[message_transforms]]\nfind = \"foo\"\nreplace = \"bar\"\n"), 0o600)
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{SourcePath: path, FileMode: "0600"}}
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}

	h.handleConfigGet(c)
	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("no config.schema frame")
	}

	fields, _ := schema["fields"].([]any)
	var desc map[string]any
	for _, fe := range fields {
		f, _ := fe.(map[string]any)
		if f["section"] == "message_transforms" && f["type"] == "object[]" {
			desc = f
		}
	}
	if desc == nil {
		t.Fatal(`no {section:"message_transforms", type:"object[]"} descriptor emitted`)
	}
	sub, _ := desc["fields"].([]any)
	if len(sub) != 2 {
		t.Errorf("object[] descriptor sub-fields = %d, want 2", len(sub))
	}
	if desc["needsRestart"] != true {
		t.Error("object[] sections are restart-required; needsRestart should be true")
	}

	global := scopeByID(t, schema, "")
	gv, _ := global["values"].(map[string]any)
	got, _ := gv["message_transforms"].(string)
	if !strings.Contains(got, `"find":"foo"`) || !strings.Contains(got, `"replace":"bar"`) {
		t.Errorf("message_transforms value = %q, want a JSON array with find/replace", got)
	}
}

// TestConfigPut_ObjectListWholeReplace proves a put with an empty key and a JSON
// array value replaces the whole [[section]] list, and unset clears it.
func TestConfigPut_ObjectListWholeReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	os.WriteFile(path, []byte("data_dir = \"/tmp\"\n\n[[message_transforms]]\nfind = \"old\"\nreplace = \"x\"\n"), 0o600)
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{SourcePath: path, FileMode: "0600"}}
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}

	h.handleConfigPut(c, fap.ConfigPut{Section: "message_transforms", Key: "", Value: `[{"find":"a","replace":"1"},{"find":"b","replace":"2"}]`})
	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, `find = "old"`) {
		t.Errorf("old block not replaced:\n%s", s)
	}
	if !strings.Contains(s, `find = "a"`) || !strings.Contains(s, `find = "b"`) {
		t.Errorf("new blocks not written:\n%s", s)
	}
	if !strings.Contains(s, `data_dir = "/tmp"`) {
		t.Errorf("unrelated content clobbered:\n%s", s)
	}

	// A bad value (unknown sub-field) is rejected and answers only the requester.
	h.handleConfigPut(c, fap.ConfigPut{Section: "message_transforms", Key: "", Value: `[{"nope":"x"}]`})
	if schema := lastConfigSchema(t, c); schema == nil || schema["error"] == nil || schema["error"] == "" {
		t.Error("invalid object-list put must answer with error set")
	}

	// Unset clears every block.
	h.handleConfigUnset(c, fap.ConfigUnset{Section: "message_transforms", Key: ""})
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "[[message_transforms]]") {
		t.Errorf("unset should remove all blocks:\n%s", data)
	}
}
