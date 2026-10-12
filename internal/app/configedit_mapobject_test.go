package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/platform"
)

// Frame-level tests for the mapObject write path ([models.*], [endpoints.*]):
// put/unset through handleConfigPut/handleConfigUnset, the style of
// configedit_test.go. Fixtures that the whole-file check must vet are
// loadable (they pass config.Load), so the check's refusals below are real
// refusals, not pre-broken files sailing through.

// loadableConfigTOML is a minimal foci.toml that passes Validate: no agents
// (so no groups requirement) and a groups entry that resolves through the
// models map.
const loadableConfigTOML = `data_dir = "/tmp"

[groups]
powerful = "powerful"

[models.powerful]
model = "anthropic/claude-sonnet-4-20250514"
`

// newMapObjectHub writes fixture to a temp foci.toml and returns a hub wired
// to it plus the client and the path.
func newMapObjectHub(t *testing.T, fixture string) (*Hub, *wsClient, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{SourcePath: path, FileMode: "0600"}}
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}
	return h, c, path
}

// readFileOrFatal returns the fixture's bytes.
func readFileOrFatal(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// putErr sends one config.put and returns the error string from the reply
// ("" when the put succeeded and broadcast).
func putErr(t *testing.T, h *Hub, c *wsClient, put fap.ConfigPut) string {
	t.Helper()
	h.handleConfigPut(c, put)
	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("put must answer (error reply or broadcast)")
	}
	errStr, _ := schema["error"].(string)
	return errStr
}

// unsetErr is putErr for config.unset.
func unsetErr(t *testing.T, h *Hub, c *wsClient, unset fap.ConfigUnset) string {
	t.Helper()
	h.handleConfigUnset(c, unset)
	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("unset must answer (error reply or broadcast)")
	}
	errStr, _ := schema["error"].(string)
	return errStr
}

// TestConfigPut_MapObjectCreateEntryLoads proves a models create lands in
// the file, the result still loads (config.Load succeeds), the entry is in
// cfg.Models, and the broadcast schema carries it.
func TestConfigPut_MapObjectCreateEntryLoads(t *testing.T) {
	h, c, path := newMapObjectHub(t, "data_dir = \"/tmp\"\n")

	h.handleConfigPut(c, fap.ConfigPut{Section: "models", Key: "fast", Value: `{"model": "openai/gpt-5"}`})

	data := readFileOrFatal(t, path)
	if !strings.Contains(string(data), "[models.fast]") || !strings.Contains(string(data), `model = "openai/gpt-5"`) {
		t.Fatalf("entry not written:\n%s", data)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatalf("file no longer loads: %v\n%s", err, data)
	}
	if got := cfg.Models["fast"].Model; got != "openai/gpt-5" {
		t.Errorf("cfg.Models[fast].Model = %q, want openai/gpt-5", got)
	}

	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("put must broadcast a fresh schema")
	}
	gv, _ := scopeByID(t, schema, "")["values"].(map[string]any)
	raw, _ := gv["models"].(string)
	if !strings.Contains(raw, "fast") {
		t.Errorf("broadcast models value = %q, want the new entry", raw)
	}
}

// TestConfigPut_MapObjectEditSubFieldPreservesFile proves a merge-put changes
// only the named key line: the entry's comment, its other keys and its
// [models.X.provider] sub-table stay byte-for-byte.
func TestConfigPut_MapObjectEditSubFieldPreservesFile(t *testing.T) {
	fixture := "data_dir = \"/tmp\"\n\n[models.powerful]\n# entry comment that must survive\nmodel = \"anthropic/claude-sonnet-4-20250514\"\ncontext = \"200000\"\n\n[models.powerful.provider]\norder = [\"deepinfra\"]\n"
	h, c, path := newMapObjectHub(t, fixture)

	if errStr := putErr(t, h, c, fap.ConfigPut{Section: "models", Key: "powerful", Value: `{"context": "262k"}`}); errStr != "" {
		t.Fatalf("merge put refused: %s", errStr)
	}

	got := string(readFileOrFatal(t, path))
	want := strings.Replace(fixture, "context = \"200000\"", "context = \"262k\"", 1)
	if got != want {
		t.Errorf("file:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestConfigUnset_MapObjectSubField proves "<entry>.<sub>" removes exactly
// that key line.
func TestConfigUnset_MapObjectSubField(t *testing.T) {
	fixture := "data_dir = \"/tmp\"\n\n[models.powerful]\nmodel = \"anthropic/claude-sonnet-4-20250514\"\ncontext = \"200000\"\n"
	h, c, path := newMapObjectHub(t, fixture)

	if errStr := unsetErr(t, h, c, fap.ConfigUnset{Section: "models", Key: "powerful.context"}); errStr != "" {
		t.Fatalf("sub-field unset refused: %s", errStr)
	}

	got := string(readFileOrFatal(t, path))
	want := strings.Replace(fixture, "context = \"200000\"\n", "", 1)
	if got != want {
		t.Errorf("file:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestConfigUnset_MapObjectEntryWithSubTable proves deleting a whole entry
// removes both its table and its provider sub-table, keeps the comment after
// the entry's last key, consumes the separating blanks and leaves the
// neighbouring entry intact — byte-for-byte.
func TestConfigUnset_MapObjectEntryWithSubTable(t *testing.T) {
	fixture := "data_dir = \"/tmp\"\n\n[models.a]\nmodel = \"anthropic/claude-sonnet-4-20250514\"\n\n[models.a.provider]\norder = [\"deepinfra\"]\n# routing note that must stay\n\n[models.b]\nmodel = \"openai/gpt-5\"\n"
	h, c, path := newMapObjectHub(t, fixture)

	if errStr := unsetErr(t, h, c, fap.ConfigUnset{Section: "models", Key: "a"}); errStr != "" {
		t.Fatalf("entry unset refused: %s", errStr)
	}

	got := string(readFileOrFatal(t, path))
	want := "data_dir = \"/tmp\"\n\n# routing note that must stay\n\n[models.b]\nmodel = \"openai/gpt-5\"\n"
	if got != want {
		t.Errorf("file:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestConfigPut_MapObjectRefusals proves every spec refusal answers only the
// requester with the rule named, and writes nothing (file byte-unchanged).
func TestConfigPut_MapObjectRefusals(t *testing.T) {
	cases := []struct {
		name    string
		put     fap.ConfigPut
		wantErr string
	}{
		{
			name:    "dotted entry name",
			put:     fap.ConfigPut{Section: "models", Key: "glm-4.6", Value: `{"model": "zai/glm-4.6"}`},
			wantErr: "bare TOML keys",
		},
		{
			name:    "spaced entry name",
			put:     fap.ConfigPut{Section: "models", Key: "my model", Value: `{"model": "openai/gpt-5"}`},
			wantErr: "bare TOML keys",
		},
		{
			name:    "malformed JSON",
			put:     fap.ConfigPut{Section: "models", Key: "new", Value: `{model`},
			wantErr: "invalid entry value",
		},
		{
			name:    "unknown sub-field",
			put:     fap.ConfigPut{Section: "models", Key: "new", Value: `{"model": "openai/gpt-5", "nope": "x"}`},
			wantErr: `unknown field "nope"`,
		},
		{
			name:    "bad bool",
			put:     fap.ConfigPut{Section: "models", Key: "powerful", Value: `{"enable_keepalive": "maybe"}`},
			wantErr: "invalid bool",
		},
		{
			name:    "bad duration",
			put:     fap.ConfigPut{Section: "models", Key: "powerful", Value: `{"cache_ttl": "banana"}`},
			wantErr: "invalid duration",
		},
		{
			name:    "create without model",
			put:     fap.ConfigPut{Section: "models", Key: "new", Value: `{"context": "262k"}`},
			wantErr: "[models.new] model is required",
		},
		{
			name:    "empty object",
			put:     fap.ConfigPut{Section: "models", Key: "new", Value: `{}`},
			wantErr: "at least one sub-field",
		},
		{
			name:    "per-agent scope refused",
			put:     fap.ConfigPut{Scope: "clutch", Section: "models", Key: "new", Value: `{"model": "openai/gpt-5"}`},
			wantErr: "no per-agent scope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c, path := newMapObjectHub(t, loadableConfigTOML)
			before := readFileOrFatal(t, path)
			if errStr := putErr(t, h, c, tc.put); errStr == "" || !strings.Contains(errStr, tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", errStr, tc.wantErr)
			}
			if after := readFileOrFatal(t, path); string(after) != string(before) {
				t.Errorf("refused put changed the file:\n%s", after)
			}
		})
	}
}

// TestConfigPut_EndpointsRefusals is the endpoints twin of the refusal
// table: a bad duration and malformed JSON, nothing written.
func TestConfigPut_EndpointsRefusals(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "bad http_timeout duration", value: `{"format": "openai", "http_timeout": "banana"}`, wantErr: "invalid duration"},
		{name: "malformed JSON", value: `[{"format":`, wantErr: "invalid entry value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c, path := newMapObjectHub(t, "data_dir = \"/tmp\"\n")
			before := readFileOrFatal(t, path)
			if errStr := putErr(t, h, c, fap.ConfigPut{Section: "endpoints", Key: "relay", Value: tc.value}); errStr == "" || !strings.Contains(errStr, tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", errStr, tc.wantErr)
			}
			if after := readFileOrFatal(t, path); string(after) != string(before) {
				t.Errorf("refused put changed the file:\n%s", after)
			}
		})
	}
}

// TestConfigPut_MapObjectNonTableEntryRefused proves an entry the file holds
// as an inline table or dotted keys (not a [S.<name>] table) is refused for
// put AND unset with the pointer at editing foci.toml by hand, and nothing
// is written. The partial put (no model — the entry already has one) must
// get the SAME refusal, not "model is required": the not-a-table check runs
// before the create rules.
func TestConfigPut_MapObjectNonTableEntryRefused(t *testing.T) {
	for _, fixture := range []string{
		"models = { a = { model = \"x\" } }\n",
		"[models]\na.model = \"x\"\n",
	} {
		h, c, path := newMapObjectHub(t, fixture)
		before := readFileOrFatal(t, path)
		for _, value := range []string{`{"model": "openai/gpt-5"}`, `{"context": "262k"}`} {
			if errStr := putErr(t, h, c, fap.ConfigPut{Section: "models", Key: "a", Value: value}); errStr == "" || !strings.Contains(errStr, "not as a [models.a] table") {
				t.Errorf("fixture %q, put %s: error = %q, want the not-a-table refusal", fixture, value, errStr)
			}
		}
		if errStr := unsetErr(t, h, c, fap.ConfigUnset{Section: "models", Key: "a"}); errStr == "" || !strings.Contains(errStr, "not as a [models.a] table") {
			t.Errorf("fixture %q: unset error = %q, want the not-a-table refusal", fixture, errStr)
		}
		if after := readFileOrFatal(t, path); string(after) != string(before) {
			t.Errorf("fixture %q: refused frames changed the file:\n%s", fixture, after)
		}
	}
}

// TestConfigPut_MapObjectCommentedHeaderEditable proves an entry whose
// [S.<name>] header carries a trailing comment — listed by the schema reader
// like any other — is editable through the frames: a merge-put edits its key
// line in place (header comment and the commented provider sub-table stay
// byte-for-byte) instead of being refused with a false error, and deleting
// the entry removes both its tables.
func TestConfigPut_MapObjectCommentedHeaderEditable(t *testing.T) {
	fixture := "data_dir = \"/tmp\"\n\n[models.powerful] # main model\nmodel = \"anthropic/claude-sonnet-4-20250514\"\ncontext = \"200000\"\n\n[models.powerful.provider] # routing\norder = [\"deepinfra\"]\n"
	h, c, path := newMapObjectHub(t, fixture)

	if errStr := putErr(t, h, c, fap.ConfigPut{Section: "models", Key: "powerful", Value: `{"context": "262k"}`}); errStr != "" {
		t.Fatalf("merge put on a commented-header entry refused: %s", errStr)
	}
	if got, want := string(readFileOrFatal(t, path)), strings.Replace(fixture, "context = \"200000\"", "context = \"262k\"", 1); got != want {
		t.Errorf("file after put:\ngot:\n%s\nwant:\n%s", got, want)
	}

	if errStr := unsetErr(t, h, c, fap.ConfigUnset{Section: "models", Key: "powerful"}); errStr != "" {
		t.Fatalf("entry unset refused: %s", errStr)
	}
	if got, want := string(readFileOrFatal(t, path)), "data_dir = \"/tmp\"\n"; got != want {
		t.Errorf("file after delete:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if _, err := config.Load(path, nil); err != nil {
		t.Fatalf("file no longer loads: %v", err)
	}
}

// TestConfigEdit_WholeFileCheckRefuses proves the whole-file check (requirement
// 4) across write paths: deleting a model [groups] powerful names, putting
// groups.powerful = "nosuch" through the existing map path, and creating an
// endpoint with a bad format are all refused with the load error and leave
// the file byte-unchanged — while a file that ALREADY fails to load stays
// editable (the check refuses only newly-breaking edits, so a broken
// foci.toml can be repaired through the editor).
func TestConfigEdit_WholeFileCheckRefuses(t *testing.T) {
	t.Run("delete of a model groups names", func(t *testing.T) {
		h, c, path := newMapObjectHub(t, loadableConfigTOML)
		before := readFileOrFatal(t, path)
		errStr := unsetErr(t, h, c, fap.ConfigUnset{Section: "models", Key: "powerful"})
		if errStr == "" || !strings.Contains(errStr, "invalid config") || !strings.Contains(errStr, "powerful") {
			t.Fatalf("error = %q, want the load error naming powerful", errStr)
		}
		if after := readFileOrFatal(t, path); string(after) != string(before) {
			t.Errorf("refused unset changed the file:\n%s", after)
		}
	})

	t.Run("groups.powerful = nosuch via the map path", func(t *testing.T) {
		h, c, path := newMapObjectHub(t, loadableConfigTOML)
		before := readFileOrFatal(t, path)
		errStr := putErr(t, h, c, fap.ConfigPut{Section: "groups", Key: "powerful", Value: "nosuch"})
		if errStr == "" || !strings.Contains(errStr, "invalid config") || !strings.Contains(errStr, "nosuch") {
			t.Fatalf("error = %q, want the load error naming nosuch", errStr)
		}
		if after := readFileOrFatal(t, path); string(after) != string(before) {
			t.Errorf("refused put changed the file:\n%s", after)
		}
	})

	t.Run("endpoint with a bad format", func(t *testing.T) {
		h, c, path := newMapObjectHub(t, "data_dir = \"/tmp\"\n")
		before := readFileOrFatal(t, path)
		errStr := putErr(t, h, c, fap.ConfigPut{Section: "endpoints", Key: "relay", Value: `{"format": "banana"}`})
		if errStr == "" || !strings.Contains(errStr, "banana") {
			t.Fatalf("error = %q, want the load error naming banana", errStr)
		}
		if after := readFileOrFatal(t, path); string(after) != string(before) {
			t.Errorf("refused put changed the file:\n%s", after)
		}
	})

	t.Run("already-broken file stays editable", func(t *testing.T) {
		h, c, path := newMapObjectHub(t, "[groups]\npowerful = \"nosuchmodel\"\n\n[notify]\nstartup_notify = false\n")
		if errStr := putErr(t, h, c, fap.ConfigPut{Section: "notify", Key: "startup_notify", Value: "true"}); errStr != "" {
			t.Fatalf("an unloadable file must stay editable, got: %s", errStr)
		}
		if after := string(readFileOrFatal(t, path)); !strings.Contains(after, "startup_notify = true") {
			t.Errorf("edit not written:\n%s", after)
		}
	})
}

// TestConfigGet_MapObjectEntriesReportedRight proves the reported entries
// (requirement 5): one JSON object per entry name with dotted quoted names
// intact, no sub-table keys leaked in, Explicit lists the section, and the
// descriptor is mapObject with needsRestart true (requirement 6).
func TestConfigGet_MapObjectEntriesReportedRight(t *testing.T) {
	h, c, _ := newMapObjectHub(t, "[models.powerful]\nmodel = \"anthropic/claude-sonnet-4-20250514\"\n\n[models.powerful.provider]\norder = [\"deepinfra\"]\nallow_fallbacks = false\n\n[models.\"glm-4.6\"]\nmodel = \"zai/glm-4.6\"\n")

	h.handleConfigGet(c)
	schema := lastConfigSchema(t, c)
	if schema == nil {
		t.Fatal("no config.schema frame")
	}

	fields, _ := schema["fields"].([]any)
	var desc map[string]any
	for _, fe := range fields {
		f, _ := fe.(map[string]any)
		if f["section"] == "models" && f["type"] == "mapObject" {
			desc = f
		}
	}
	if desc == nil {
		t.Fatal(`no {section:"models", type:"mapObject"} descriptor emitted`)
	}
	if desc["needsRestart"] != true {
		t.Error("mapObject descriptors must keep needsRestart=true (restart-only)")
	}

	global := scopeByID(t, schema, "")
	gv, _ := global["values"].(map[string]any)
	raw, _ := gv["models"].(string)
	var entries map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("models value is not a JSON object-of-objects: %v (%q)", err, raw)
	}
	if entries["glm-4.6"]["model"] != "zai/glm-4.6" {
		t.Errorf("dotted quoted entry name mangled: %v", entries)
	}
	powerful := entries["powerful"]
	if powerful["model"] != "anthropic/claude-sonnet-4-20250514" {
		t.Errorf("powerful.model = %v", powerful["model"])
	}
	for k := range powerful {
		if strings.HasPrefix(k, "provider") {
			t.Errorf("sub-table leaked into the report as %q", k)
		}
	}
	for _, k := range explicitList(global) {
		if k == "models" {
			return // explicit lists the section — pass
		}
	}
	t.Error(`Explicit must list "models" when the section has entries`)
}

// TestConfigPut_MapObjectRoutesToApplyLive proves a mapObject edit is handed
// to ApplyLive with the section and an empty key, like object lists: no
// applier exists for models, so this is what logs takes-effect-on-restart.
func TestConfigPut_MapObjectRoutesToApplyLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("[models.a]\nmodel = \"openai/gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestHub()
	type call struct{ section, key string }
	var calls []call
	h.deps = platform.ProviderDeps{
		Config: &config.Config{SourcePath: path, FileMode: "0600"},
		ApplyLive: func(section, key string) (bool, error) {
			calls = append(calls, call{section, key})
			return false, nil
		},
	}
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}

	h.handleConfigPut(c, fap.ConfigPut{Section: "models", Key: "a", Value: `{"context": "262k"}`})
	h.handleConfigUnset(c, fap.ConfigUnset{Section: "models", Key: "a"})

	want := []call{{"models", ""}, {"models", ""}}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("ApplyLive calls = %+v, want %+v (put then unset)", calls, want)
	}
}
