package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestObjectFields(t *testing.T) {
	specs := ObjectFields()
	byName := map[string]ObjectFieldSpec{}
	for _, s := range specs {
		byName[s.Section] = s
	}
	for _, want := range []struct {
		section string
		keys    []string
	}{
		{"message_transforms", []string{"find", "replace"}},
		{"blocked_paths", []string{"path", "rebuke"}},
		{"memory.sources", []string{"name", "dir", "weight"}},
	} {
		spec, ok := byName[want.section]
		if !ok {
			t.Errorf("ObjectFields missing section %q", want.section)
			continue
		}
		got := make([]string, len(spec.Fields))
		for i, f := range spec.Fields {
			got[i] = f.Key
		}
		if !reflect.DeepEqual(got, want.keys) {
			t.Errorf("%s sub-fields = %v, want %v", want.section, got, want.keys)
		}
	}
	// weight must be a float sub-field so the writer emits a bare number.
	ms := byName["memory.sources"]
	for _, f := range ms.Fields {
		if f.Key == "weight" && f.Type != FieldFloat {
			t.Errorf("memory.sources.weight type = %v, want FieldFloat", f.Type)
		}
	}
}

func TestObjectFieldSpecFor(t *testing.T) {
	if _, ok := ObjectFieldSpecFor("message_transforms"); !ok {
		t.Error("ObjectFieldSpecFor(message_transforms) = false")
	}
	if _, ok := ObjectFieldSpecFor("MESSAGE_TRANSFORMS"); !ok {
		t.Error("ObjectFieldSpecFor is not case-insensitive")
	}
	if _, ok := ObjectFieldSpecFor("groups"); ok {
		t.Error("ObjectFieldSpecFor(groups) should be false — that's a map, not an object-list")
	}
}

func TestParseObjectListValue(t *testing.T) {
	mt, _ := ObjectFieldSpecFor("message_transforms")
	ms, _ := ObjectFieldSpecFor("memory.sources")

	// Strings pass through; the whole list is preserved in order.
	entries, err := ParseObjectListValue(mt, `[{"find":"a","replace":"b"},{"find":"c","replace":"d"}]`)
	if err != nil {
		t.Fatalf("ParseObjectListValue: %v", err)
	}
	if len(entries) != 2 || entries[0]["find"] != "a" || entries[1]["replace"] != "d" {
		t.Errorf("entries = %v", entries)
	}

	// A float sub-field decodes to float64 (so SetTableArray emits a bare number).
	fe, err := ParseObjectListValue(ms, `[{"name":"code","dir":"/x","weight":0.5}]`)
	if err != nil {
		t.Fatalf("ParseObjectListValue(memory.sources): %v", err)
	}
	if w, ok := fe[0]["weight"].(float64); !ok || w != 0.5 {
		t.Errorf("weight = %#v, want float64 0.5", fe[0]["weight"])
	}

	// Unknown sub-field is rejected.
	if _, err := ParseObjectListValue(mt, `[{"find":"a","bogus":"b"}]`); err == nil {
		t.Error("expected error for unknown sub-field")
	}
	// Type mismatch (number where a string is expected) is rejected.
	if _, err := ParseObjectListValue(mt, `[{"find":5}]`); err == nil {
		t.Error("expected error for type mismatch")
	}
	// Malformed JSON is rejected.
	if _, err := ParseObjectListValue(mt, `not json`); err == nil {
		t.Error("expected error for malformed JSON")
	}
	// Empty array is valid and yields zero entries.
	empty, err := ParseObjectListValue(mt, `[]`)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty list = %v, err = %v", empty, err)
	}
}

func TestTableArrayEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	toml := `data_dir = "/tmp"

[[message_transforms]]
find = "foo"
replace = "bar"
extra = "ignored"

[memory]
search_backend = "bleve"

[[memory.sources]]
name = "code"
dir = "/src"
weight = 0.8
`
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	mt, _ := ObjectFieldSpecFor("message_transforms")
	got, err := TableArrayEntries(path, mt)
	if err != nil {
		t.Fatalf("TableArrayEntries: %v", err)
	}
	if len(got) != 1 || got[0]["find"] != "foo" || got[0]["replace"] != "bar" {
		t.Errorf("message_transforms entries = %v", got)
	}
	if _, leaked := got[0]["extra"]; leaked {
		t.Error("stray sub-key 'extra' leaked — only declared sub-fields should appear")
	}

	// Dotted nested section.
	ms, _ := ObjectFieldSpecFor("memory.sources")
	src, err := TableArrayEntries(path, ms)
	if err != nil {
		t.Fatalf("TableArrayEntries(memory.sources): %v", err)
	}
	if len(src) != 1 || src[0]["name"] != "code" {
		t.Errorf("memory.sources entries = %v", src)
	}
	if w, ok := src[0]["weight"].(float64); !ok || w != 0.8 {
		t.Errorf("weight = %#v, want float64 0.8", src[0]["weight"])
	}

	// Absent section yields an empty, non-nil slice (no error).
	empty := path + ".missing"
	if err := os.WriteFile(empty, []byte("data_dir = \"/tmp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got2, err := TableArrayEntries(empty, mt)
	if err != nil {
		t.Fatalf("TableArrayEntries(absent): %v", err)
	}
	if got2 == nil || len(got2) != 0 {
		t.Errorf("absent section = %#v, want empty non-nil slice", got2)
	}
}

// TestObjectListRoundTrip proves the full editor path: parse the JSON the client
// sends, write it with SetTableArray, read it back with TableArrayEntries, and
// recover the same data.
func TestObjectListRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("data_dir = \"/tmp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ms, _ := ObjectFieldSpecFor("memory.sources")

	entries, err := ParseObjectListValue(ms, `[{"name":"a","dir":"/a","weight":1},{"name":"b","dir":"/b","weight":0.25}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := SetTableArray(path, "memory.sources", entries, 0o600); err != nil {
		t.Fatalf("SetTableArray: %v", err)
	}
	got, err := TableArrayEntries(path, ms)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("round-trip entries = %v, want 2", got)
	}
	if got[0]["name"] != "a" || got[1]["name"] != "b" {
		t.Errorf("names not preserved in order: %v", got)
	}
	if w, _ := got[1]["weight"].(float64); w != 0.25 {
		t.Errorf("weight not preserved: %#v", got[1]["weight"])
	}
	// data_dir must survive the write untouched.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `data_dir = "/tmp"`) {
		t.Errorf("SetTableArray clobbered unrelated content:\n%s", data)
	}
}
