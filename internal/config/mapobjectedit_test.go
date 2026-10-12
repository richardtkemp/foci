package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The mapObject planners' tests: entry create/merge/delete keep the file as
// written (comments, sibling keys and [S.<entry>.*] sub-tables byte-for-byte),
// placement follows the section's existing entries, and every refusal in the
// spec (requirement 2's list plus the non-table-entry forms) names its rule.

// mapObjectSpecFor returns the registered spec for a section, failing the test
// when the section is not registered.
func mapObjectSpecFor(t *testing.T, section string) MapObjectFieldSpec {
	t.Helper()
	spec, ok := MapObjectFieldSpecFor(section)
	if !ok {
		t.Fatalf("section %q missing from mapObjectFieldSpecs", section)
	}
	return spec
}

// writeMapObjectFixture writes fixture to a temp foci.toml and returns its path.
func writeMapObjectFixture(t *testing.T, fixture string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseMapObjectValue proves the put-value parser's refusals: malformed
// JSON, a non-string value, an empty object, an unknown sub-field, and values
// that do not fit their sub-field type — plus the happy path.
func TestParseMapObjectValue(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "malformed json", value: `{model`, wantErr: "invalid entry value"},
		{name: "array not object", value: `["model"]`, wantErr: "invalid entry value"},
		{name: "empty object", value: `{}`, wantErr: "at least one sub-field"},
		{name: "unknown sub-field", value: `{"model": "a/b", "nope": "x"}`, wantErr: `unknown field "nope"`},
		{name: "non-string value", value: `{"context": 200}`, wantErr: `field "context": expected a string`},
		{name: "bad bool", value: `{"enable_keepalive": "maybe"}`, wantErr: "invalid bool"},
		{name: "bad duration", value: `{"cache_ttl": "banana"}`, wantErr: "invalid duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMapObjectValue(spec, tc.value)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseMapObjectValue(%s) error = %v, want it to mention %q", tc.value, err, tc.wantErr)
			}
		})
	}

	settings, err := ParseMapObjectValue(spec, `{"model": "anthropic/claude-sonnet-4-20250514", "cache_ttl": "5m", "enable_keepalive": "true"}`)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if settings["model"] != "anthropic/claude-sonnet-4-20250514" || settings["cache_ttl"] != "5m" || settings["enable_keepalive"] != "true" {
		t.Errorf("settings = %v", settings)
	}
}

// TestPlanSetMapObjectEntry_Placement pins where a created [S.<entry>] table
// lands: after the section's existing entries including their sub-tables (a
// trailing comment stays with its entry), after a bare [S] table when the
// section has no entries, before the entry's first sub-table when only those
// exist, and where a brand-new section goes (before [[agents]], else EOF)
// when the section has no table at all.
func TestPlanSetMapObjectEntry_Placement(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	settings := map[string]string{"model": "openai/gpt-5"}
	cases := []struct {
		name    string
		fixture string
	}{
		{
			name:    "after existing entries and their sub-tables, trailing comment stays",
			fixture: "[models.a]\nmodel = \"x\"\n\n[models.a.provider]\norder = [\"p\"]\n\n[models.b]\nmodel = \"y\"\n# b comment\n",
		},
		{
			name:    "after a bare [models] table",
			fixture: "[models]\n# group of models\n",
		},
		{
			name:    "before the entry's first sub-table when only those exist",
			fixture: "[models.x.provider]\norder = [\"p\"]\n",
		},
		{
			name:    "new section goes before [[agents]]",
			fixture: "data_dir = \"/tmp\"\n\n[[agents]]\nid = \"clutch\"\n",
		},
		{
			name:    "new section goes at EOF",
			fixture: "data_dir = \"/tmp\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanSetMapObjectEntry(tc.fixture, spec, "c", settings)
			if err != nil {
				t.Fatalf("PlanSetMapObjectEntry: %v", err)
			}
			if !strings.Contains(got, "[models.c]\nmodel = \"openai/gpt-5\"") {
				t.Errorf("created table missing:\n%s", got)
			}
			if _, err := decodeTOMLTree(got); err != nil {
				t.Errorf("result does not parse: %v\n%s", err, got)
			}
		})
	}

	// Byte-exact placement: after [models.b] and its trailing comment, with
	// the separator spacing landing after the new table.
	got, err := PlanSetMapObjectEntry("[models.a]\nmodel = \"x\"\n\n[models.a.provider]\norder = [\"p\"]\n\n[models.b]\nmodel = \"y\"\n# b comment\n", spec, "c", settings)
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry: %v", err)
	}
	want := "[models.a]\nmodel = \"x\"\n\n[models.a.provider]\norder = [\"p\"]\n\n[models.b]\nmodel = \"y\"\n# b comment\n\n[models.c]\nmodel = \"openai/gpt-5\"\n"
	if got != want {
		t.Errorf("placement:\ngot:\n%s\nwant:\n%s", got, want)
	}

	// Before the first sub-table when the entry exists only through it.
	got, err = PlanSetMapObjectEntry("[models.x.provider]\norder = [\"p\"]\n", spec, "x", settings)
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry subtable-only: %v", err)
	}
	want = "[models.x]\nmodel = \"openai/gpt-5\"\n[models.x.provider]\norder = [\"p\"]\n"
	if got != want {
		t.Errorf("subtable-only placement:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestPlanSetMapObjectEntry_MergePreservesFile proves a merge-put changes only
// the named key lines inside [S.<entry>]: an existing sub-field's line is
// replaced, a new one is inserted into the entry's own body, and the entry's
// comments, other keys and [S.<entry>.provider] sub-table stay byte-for-byte.
func TestPlanSetMapObjectEntry_MergePreservesFile(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "data_dir = \"/tmp\"\n\n[models.powerful]\n# entry comment that must survive\nmodel = \"anthropic/claude-sonnet-4-20250514\"\ncontext = \"200000\"\n\n[models.powerful.provider]\norder = [\"deepinfra\"]\n\n[notify]\nstartup_notify = false\n"

	got, err := PlanSetMapObjectEntry(fixture, spec, "powerful", map[string]string{"context": "262k"})
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry: %v", err)
	}
	want := strings.Replace(fixture, "context = \"200000\"", "context = \"262k\"", 1)
	if got != want {
		t.Errorf("merge replace:\ngot:\n%s\nwant:\n%s", got, want)
	}

	got, err = PlanSetMapObjectEntry(fixture, spec, "powerful", map[string]string{"cache_ttl": "5m"})
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry insert: %v", err)
	}
	want = strings.Replace(fixture, "context = \"200000\"", "context = \"200000\"\ncache_ttl = \"5m\"", 1)
	if got != want {
		t.Errorf("merge insert:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestPlanSetMapObjectEntry_Refusals proves every refused put writes nothing
// and names its rule: a non-bare entry name, an unknown sub-field, a
// type-misfit value, a create with no sub-fields, a models create without a
// non-empty model, and an entry the file holds as an inline table or dotted
// keys rather than a [S.<entry>] table.
func TestPlanSetMapObjectEntry_Refusals(t *testing.T) {
	models := mapObjectSpecFor(t, "models")
	cases := []struct {
		name     string
		fixture  string
		entry    string
		settings map[string]string
		wantErr  string
	}{
		{
			name:     "dotted entry name",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "glm-4.6",
			settings: map[string]string{"model": "zai/glm-4.6"},
			wantErr:  "bare TOML keys",
		},
		{
			name:     "spaced entry name",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "my model",
			settings: map[string]string{"model": "openai/gpt-5"},
			wantErr:  "bare TOML keys",
		},
		{
			name:     "unknown sub-field",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "a",
			settings: map[string]string{"nope": "x"},
			wantErr:  `unknown field "nope"`,
		},
		{
			name:     "type-misfit value",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "a",
			settings: map[string]string{"cache_ttl": "banana"},
			wantErr:  "invalid duration",
		},
		{
			name:     "no sub-fields",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "b",
			settings: map[string]string{},
			wantErr:  "at least one sub-field",
		},
		{
			name:     "create without the required model",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "b",
			settings: map[string]string{"context": "262k"},
			wantErr:  "[models.b] model is required",
		},
		{
			name:     "create with empty model",
			fixture:  "[models.a]\nmodel = \"x\"\n",
			entry:    "b",
			settings: map[string]string{"model": ""},
			wantErr:  "[models.b] model is required",
		},
		{
			name:     "inline-table entry",
			fixture:  "models = { a = { model = \"x\" } }\n",
			entry:    "a",
			settings: map[string]string{"model": "openai/gpt-5"},
			wantErr:  "not as a [models.a] table",
		},
		{
			// The not-a-table refusal must fire BEFORE the create rules:
			// a partial put (no model) on an inline entry already HAS its
			// model — "model is required" would be untrue.
			name:     "inline-table entry, partial put without model",
			fixture:  "models = { a = { model = \"x\" } }\n",
			entry:    "a",
			settings: map[string]string{"context": "262k"},
			wantErr:  "not as a [models.a] table",
		},
		{
			name:     "dotted-key entry under [models]",
			fixture:  "[models]\na.model = \"x\"\n",
			entry:    "a",
			settings: map[string]string{"model": "openai/gpt-5"},
			wantErr:  "not as a [models.a] table",
		},
		{
			name:     "dotted-key entry under [models], partial put without model",
			fixture:  "[models]\na.model = \"x\"\n",
			entry:    "a",
			settings: map[string]string{"context": "262k"},
			wantErr:  "not as a [models.a] table",
		},
		{
			// Dotted keys plus a [S.<entry>.*] sub-table: the sub-table
			// header must not hide the dotted form (it would be "created"
			// as a duplicate [models.z] table before the sub-table).
			name:     "dotted-key entry with a provider sub-table",
			fixture:  "[models]\nz.model = \"x\"\n\n[models.z.provider]\norder = [\"p\"]\n",
			entry:    "z",
			settings: map[string]string{"model": "openai/gpt-5"},
			wantErr:  "not as a [models.z] table",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanSetMapObjectEntry(tc.fixture, models, tc.entry, tc.settings)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestPlanUnsetMapObject_SubField proves "<entry>.<sub>" removes exactly that
// key line from the entry's table, leaving comments, sibling keys and
// sub-tables byte-for-byte, and refuses what it cannot address: an unknown
// sub-field, a sub-field that is not set, and a sub-field on an entry that
// exists only through sub-tables.
func TestPlanUnsetMapObject_SubField(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "[models.powerful]\n# comment stays\nmodel = \"anthropic/claude-sonnet-4-20250514\"\ncontext = \"200000\"\n\n[models.powerful.provider]\norder = [\"deepinfra\"]\n"

	got, err := PlanUnsetMapObject(fixture, spec, "powerful.context")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject: %v", err)
	}
	want := strings.Replace(fixture, "context = \"200000\"\n", "", 1)
	if got != want {
		t.Errorf("sub-field removal:\ngot:\n%s\nwant:\n%s", got, want)
	}

	cases := []struct {
		name    string
		fixture string
		key     string
		wantErr string
	}{
		{name: "unknown sub-field", fixture: fixture, key: "powerful.nope", wantErr: `unknown field "nope"`},
		{name: "sub-field not set", fixture: fixture, key: "powerful.cache_ttl", wantErr: "is not set in the config file"},
		{
			name:    "sub-field on an entry that exists only as sub-tables",
			fixture: "[models.x.provider]\norder = [\"p\"]\n",
			key:     "x.model",
			wantErr: "is not set in the config file",
		},
		{
			name:    "sub-field on a dotted-key entry with a provider sub-table",
			fixture: "[models]\nz.model = \"x\"\n\n[models.z.provider]\norder = [\"p\"]\n",
			key:     "z.model",
			wantErr: "not as a [models.z] table",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanUnsetMapObject(tc.fixture, spec, tc.key)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestPlanUnsetMapObject_Entry proves deleting a whole entry removes its
// [S.<entry>] table and every [S.<entry>.*] sub-table, consumes the blank
// lines that separated them, keeps comment lines that sit after a block's
// last key (blockRemovalEnd's rule), and leaves neighbouring entries intact —
// and that absent or non-table entries are refused with the right message.
func TestPlanUnsetMapObject_Entry(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "data_dir = \"/tmp\"\n\n[models.a]\nmodel = \"anthropic/claude-sonnet-4-20250514\"\n\n[models.a.provider]\norder = [\"deepinfra\"]\n# routing note that must stay\n\n[models.b]\nmodel = \"openai/gpt-5\"\n"

	got, err := PlanUnsetMapObject(fixture, spec, "a")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject: %v", err)
	}
	want := "data_dir = \"/tmp\"\n\n# routing note that must stay\n\n[models.b]\nmodel = \"openai/gpt-5\"\n"
	if got != want {
		t.Errorf("entry deletion:\ngot:\n%s\nwant:\n%s", got, want)
	}

	// Entry that exists only through sub-tables: those are its presence,
	// so deleting the entry removes them.
	got, err = PlanUnsetMapObject("[models.x.provider]\norder = [\"p\"]\n\n[models.y]\nmodel = \"q\"\n", spec, "x")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject subtable-only: %v", err)
	}
	if strings.Contains(got, "[models.x.provider]") || !strings.Contains(got, "[models.y]") {
		t.Errorf("subtable-only deletion left the wrong lines:\n%s", got)
	}

	cases := []struct {
		name    string
		fixture string
		key     string
		wantErr string
	}{
		{name: "absent entry", fixture: "[models.a]\nmodel = \"x\"\n", key: "b", wantErr: "models.b is not set"},
		{
			name:    "inline-table entry",
			fixture: "models = { a = { model = \"x\" } }\n",
			key:     "a",
			wantErr: "not as a [models.a] table",
		},
		{
			name:    "dotted-key entry under [models]",
			fixture: "[models]\na.model = \"x\"\n",
			key:     "a",
			wantErr: "not as a [models.a] table",
		},
		{
			// Deleting only the sub-table would leave z.model behind
			// and report success.
			name:    "dotted-key entry with a provider sub-table",
			fixture: "[models]\nz.model = \"x\"\n\n[models.z.provider]\norder = [\"p\"]\n",
			key:     "z",
			wantErr: "not as a [models.z] table",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanUnsetMapObject(tc.fixture, spec, tc.key)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestPlanUnsetMapObject_DottedNameEntry pins review-note behaviour: a frame
// key like models.glm-4.6 splits into entry "glm-4" + sub "6", and must never
// act on (or report missing) entry glm-4 — when the full key names an
// existing dotted quoted entry table, the refusal says the name is not a bare
// TOML key and points at editing foci.toml by hand.
func TestPlanUnsetMapObject_DottedNameEntry(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "[models.powerful]\nmodel = \"x\"\n\n[models.\"glm-4.6\"]\nmodel = \"zai/glm-4.6\"\n"

	_, err := PlanUnsetMapObject(fixture, spec, "glm-4.6")
	if err == nil || !strings.Contains(err.Error(), "bare TOML keys") {
		t.Fatalf("error = %v, want the bare-TOML-key refusal", err)
	}
	// The split-at-first-dot sub-field path must not mis-fire either.
	_, err = PlanUnsetMapObject(fixture, spec, "glm-4.6.model")
	if err == nil || !strings.Contains(err.Error(), "bare TOML keys") {
		t.Fatalf("sub-field split error = %v, want the bare-TOML-key refusal", err)
	}
}

// TestPlanSetMapObjectEntry_CommentedHeader proves an entry whose [S.<name>]
// header carries a trailing comment — a form the TOML reader reports and
// tableArrayHeaderRe accepts — is found by the write side: a merge edits its
// key lines in place, the body bound stops at a commented next header (a new
// key lands in the entry's own body, never after the commented neighbour's
// keys), and every comment stays byte-for-byte.
func TestPlanSetMapObjectEntry_CommentedHeader(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "[models.a] # main model\nmodel = \"x\"\n\n[models.a.provider] # routing\norder = [\"p\"]\n\n[models.b] # other\nmodel = \"y\"\n"

	got, err := PlanSetMapObjectEntry(fixture, spec, "a", map[string]string{"model": "z"})
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry replace: %v", err)
	}
	want := strings.Replace(fixture, "model = \"x\"", "model = \"z\"", 1)
	if got != want {
		t.Errorf("replace:\ngot:\n%s\nwant:\n%s", got, want)
	}

	got, err = PlanSetMapObjectEntry(fixture, spec, "a", map[string]string{"context": "262k"})
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry insert: %v", err)
	}
	want = strings.Replace(fixture, "model = \"x\"", "model = \"x\"\ncontext = \"262k\"", 1)
	if got != want {
		t.Errorf("insert:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestPlanUnsetMapObject_CommentedHeader proves deletion finds an entry whose
// header carries a trailing comment: the entry's table and its own commented
// sub-table are removed (consuming the separating blanks) while the commented
// neighbour stays byte-for-byte — and that entryTableNamed's comment
// tolerance holds, so a frame key that flat-splits a commented dotted quoted
// entry name gets the bare-key refusal, not a bogus unknown-field error.
func TestPlanUnsetMapObject_CommentedHeader(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "[models.a] # main model\nmodel = \"x\"\n\n[models.a.provider] # routing\norder = [\"p\"]\n\n[models.b] # other\nmodel = \"y\"\n"

	got, err := PlanUnsetMapObject(fixture, spec, "a")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject: %v", err)
	}
	if want := "[models.b] # other\nmodel = \"y\"\n"; got != want {
		t.Errorf("deletion:\ngot:\n%s\nwant:\n%s", got, want)
	}

	dotted := "[models.powerful]\nmodel = \"x\"\n\n[models.\"glm-4.6\"] # z-ai\nmodel = \"zai/glm-4.6\"\n"
	_, err = PlanUnsetMapObject(dotted, spec, "glm-4.6")
	if err == nil || !strings.Contains(err.Error(), "bare TOML keys") {
		t.Fatalf("dotted error = %v, want the bare-TOML-key refusal", err)
	}
}

// TestMapObjectSpacedHeaders proves the write side reads table headers with
// whitespace inside the brackets and around the dots ([ models . g ], valid
// TOML that the reader reports as entry g): a merge edits g's key lines, a
// whole-entry delete removes g's table and its spaced sub-table, and a new
// entry is anchored after a spaced header's block rather than where a
// brand-new section goes.
func TestMapObjectSpacedHeaders(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	fixture := "[ models.g ]\nmodel = \"x\"\n\n[ models . g . provider ]\norder = [\"p\"]\n\n[notify]\nstartup_notify = false\n"

	got, err := PlanSetMapObjectEntry(fixture, spec, "c", map[string]string{"model": "openai/gpt-5"})
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry create: %v", err)
	}
	want := "[ models.g ]\nmodel = \"x\"\n\n[ models . g . provider ]\norder = [\"p\"]\n\n[models.c]\nmodel = \"openai/gpt-5\"\n\n[notify]\nstartup_notify = false\n"
	if got != want {
		t.Errorf("create placement:\ngot:\n%s\nwant:\n%s", got, want)
	}

	got, err = PlanSetMapObjectEntry(fixture, spec, "g", map[string]string{"model": "z"})
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry merge: %v", err)
	}
	if want := strings.Replace(fixture, "model = \"x\"", "model = \"z\"", 1); got != want {
		t.Errorf("merge:\ngot:\n%s\nwant:\n%s", got, want)
	}

	got, err = PlanUnsetMapObject(fixture, spec, "g")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject entry: %v", err)
	}
	if want := "[notify]\nstartup_notify = false\n"; got != want {
		t.Errorf("entry deletion:\ngot:\n%s\nwant:\n%s", got, want)
	}

	got, err = PlanUnsetMapObject(fixture, spec, "g.model")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject sub-field: %v", err)
	}
	if want := strings.Replace(fixture, "model = \"x\"\n", "", 1); got != want {
		t.Errorf("sub-field removal:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestMapObjectFileEntries pins the tree-based entry reader the schema
// reports from: dotted quoted entry names stay one key, sub-tables such as
// provider never leak in as sub-fields, only registered sub-fields are
// reported, an entry with none set still appears, and an absent section
// yields an empty map.
func TestMapObjectFileEntries(t *testing.T) {
	spec := mapObjectSpecFor(t, "models")
	path := writeMapObjectFixture(t, "[models.powerful]\nmodel = \"anthropic/claude-sonnet-4-20250514\"\ncontext = \"200000\"\n\n[models.powerful.provider]\norder = [\"deepinfra\"]\nallow_fallbacks = false\n\n[models.\"glm-4.6\"]\nmodel = \"zai/glm-4.6\"\nextra_unknown = \"skipped\"\n")

	got, err := MapObjectFileEntries(path, spec)
	if err != nil {
		t.Fatalf("MapObjectFileEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %v, want powerful and glm-4.6", got)
	}
	powerful := got["powerful"]
	if powerful["model"] != "anthropic/claude-sonnet-4-20250514" || powerful["context"] != "200000" {
		t.Errorf("powerful = %v", powerful)
	}
	for k := range powerful {
		if strings.HasPrefix(k, "provider") {
			t.Errorf("sub-table leaked in as sub-field %q", k)
		}
	}
	glm, ok := got["glm-4.6"]
	if !ok {
		t.Fatal("dotted quoted entry name mangled")
	}
	if glm["model"] != "zai/glm-4.6" {
		t.Errorf("glm-4.6 = %v", glm)
	}
	if _, leaked := glm["extra_unknown"]; leaked {
		t.Error("unregistered sub-field reported")
	}

	empty, err := MapObjectFileEntries(path, MapObjectFieldSpec{Section: "endpoints", Fields: spec.Fields})
	if err != nil {
		t.Fatalf("absent section: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("absent section = %v, want empty", empty)
	}
}
