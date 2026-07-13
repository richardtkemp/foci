package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// This file is the CONTRACT for SetTableArray — the array-of-tables ([[section]])
// writer that backs Phase 3 of the app config editor (message_transforms,
// blocked_paths, memory.sources). SetTableArray does not exist yet; implement it
// (and any helpers) so these tests pass WITHOUT editing this file.
//
// SetTableArray replaces the [[section]] array-of-tables blocks in the TOML file
// at `path` with one block per entry, preserving every other line (other
// sections, scalar keys, blank lines, comments). Each entry maps a sub-field
// name to its value; values are formatted by Go type — string -> quoted, float64
// -> bare number, int/int64 -> bare integer, bool -> true/false. `section` may be
// dotted ("memory.sources" -> [[memory.sources]]). Passing zero entries removes
// the section's blocks entirely. It returns the number of blocks written.
//
//   func SetTableArray(path, section string, entries []map[string]any, mode os.FileMode) (int, error)

func p3write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func p3reparse(t *testing.T, path string) Config {
	t.Helper()
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		t.Fatalf("reparse produced invalid TOML: %v", err)
	}
	return cfg
}

func p3read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetTableArray_InsertWhenAbsent(t *testing.T) {
	path := p3write(t, "[keepalive]\nenabled = true\n")
	n, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "a", "replace": "b"}}, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("wrote %d blocks, want 1", n)
	}
	cfg := p3reparse(t, path)
	if len(cfg.MessageTransforms) != 1 || cfg.MessageTransforms[0].Find != "a" || cfg.MessageTransforms[0].Replace != "b" {
		t.Errorf("MessageTransforms = %+v, want [{a b}]", cfg.MessageTransforms)
	}
	if raw := p3read(t, path); !strings.Contains(raw, "[keepalive]") || !strings.Contains(raw, "enabled = true") {
		t.Errorf("unrelated content not preserved:\n%s", raw)
	}
}

func TestSetTableArray_ReplacesExisting(t *testing.T) {
	path := p3write(t, `[[message_transforms]]
find = "old1"
replace = "r1"

[[message_transforms]]
find = "old2"
replace = "r2"

[keepalive]
enabled = true
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "new", "replace": "z"}}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.MessageTransforms) != 1 || cfg.MessageTransforms[0].Find != "new" {
		t.Errorf("MessageTransforms = %+v, want exactly [{new z}]", cfg.MessageTransforms)
	}
	if raw := p3read(t, path); strings.Count(raw, "[[message_transforms]]") != 1 {
		t.Errorf("want exactly one [[message_transforms]] header:\n%s", raw)
	}
	if raw := p3read(t, path); strings.Contains(raw, "old1") || strings.Contains(raw, "old2") {
		t.Errorf("old block content orphaned:\n%s", raw)
	}
	if raw := p3read(t, path); !strings.Contains(raw, "[keepalive]") {
		t.Errorf("unrelated section lost:\n%s", raw)
	}
}

func TestSetTableArray_RemovesWhenEmpty(t *testing.T) {
	path := p3write(t, `# top comment
[[message_transforms]]
find = "x"
replace = "y"

[keepalive]
enabled = true
`)
	if _, err := SetTableArray(path, "message_transforms", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.MessageTransforms) != 0 {
		t.Errorf("MessageTransforms = %+v, want empty", cfg.MessageTransforms)
	}
	raw := p3read(t, path)
	if strings.Contains(raw, "[[message_transforms]]") {
		t.Errorf("block not removed:\n%s", raw)
	}
	if !strings.Contains(raw, "[keepalive]") || !strings.Contains(raw, "# top comment") {
		t.Errorf("unrelated content/comment lost:\n%s", raw)
	}
}

func TestSetTableArray_TypedValues(t *testing.T) {
	path := p3write(t, "[memory]\nsearch_backend = \"bleve\"\n")
	if _, err := SetTableArray(path, "memory.sources",
		[]map[string]any{{"name": "code", "dir": "/x", "weight": 0.5}}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.Memory.Sources) != 1 {
		t.Fatalf("Sources = %+v, want one entry", cfg.Memory.Sources)
	}
	s := cfg.Memory.Sources[0]
	if s.Name != "code" || s.Dir != "/x" || s.Weight != 0.5 {
		t.Errorf("Source = %+v, want {code /x 0.5}", s)
	}
	raw := p3read(t, path)
	if !strings.Contains(raw, `name = "code"`) {
		t.Errorf("string not quoted:\n%s", raw)
	}
	if !strings.Contains(raw, "weight = 0.5") || strings.Contains(raw, `weight = "0.5"`) {
		t.Errorf("float not written bare:\n%s", raw)
	}
}

func TestSetTableArray_SpecialCharsInValues(t *testing.T) {
	// message_transforms values are regexes/replacements: quotes, backslashes,
	// '#', and $-refs must survive verbatim (proper TOML quoting, not corruption).
	path := p3write(t, "[keepalive]\nenabled = true\n")
	find := `a"b\c#d`
	replace := `$1 -> "x"`
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": find, "replace": replace}}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.MessageTransforms) != 1 {
		t.Fatalf("MessageTransforms = %+v, want one entry", cfg.MessageTransforms)
	}
	if cfg.MessageTransforms[0].Find != find || cfg.MessageTransforms[0].Replace != replace {
		t.Errorf("round-trip corrupted special chars: got {%q %q}, want {%q %q}",
			cfg.MessageTransforms[0].Find, cfg.MessageTransforms[0].Replace, find, replace)
	}
}

func TestSetTableArray_ScatteredBlocks(t *testing.T) {
	// TOML allows same-named [[blocks]] separated by other sections; ALL must be
	// found and replaced, not just a contiguous run.
	path := p3write(t, `[[message_transforms]]
find = "a"
replace = "1"

[keepalive]
enabled = true

[[message_transforms]]
find = "b"
replace = "2"
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "only", "replace": "z"}}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.MessageTransforms) != 1 || cfg.MessageTransforms[0].Find != "only" {
		t.Errorf("scattered blocks not consolidated: %+v", cfg.MessageTransforms)
	}
	raw := p3read(t, path)
	if strings.Count(raw, "[[message_transforms]]") != 1 {
		t.Errorf("want one [[message_transforms]] header after replace:\n%s", raw)
	}
	if !strings.Contains(raw, "[keepalive]") {
		t.Errorf("interleaved section lost:\n%s", raw)
	}
}

func TestSetTableArray_HeaderAnchoring(t *testing.T) {
	// Writing "message_transforms" must not touch a different section whose name
	// merely has it as a prefix.
	path := p3write(t, `[[message_transforms]]
find = "a"
replace = "1"

[[message_transforms_v2]]
find = "keep"
replace = "me"
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "new", "replace": "z"}}, 0o644); err != nil {
		t.Fatal(err)
	}
	raw := p3read(t, path)
	if !strings.Contains(raw, "[[message_transforms_v2]]") || !strings.Contains(raw, `find = "keep"`) {
		t.Errorf("prefix-named section was clobbered:\n%s", raw)
	}
	if strings.Count(raw, "[[message_transforms]]") != 1 {
		t.Errorf("want exactly one [[message_transforms]] block:\n%s", raw)
	}
}

func TestSetTableArray_PreservesEntryOrder(t *testing.T) {
	path := p3write(t, "")
	entries := []map[string]any{
		{"find": "first", "replace": "1"},
		{"find": "second", "replace": "2"},
		{"find": "third", "replace": "3"},
	}
	if _, err := SetTableArray(path, "message_transforms", entries, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	got := make([]string, len(cfg.MessageTransforms))
	for i, mt := range cfg.MessageTransforms {
		got[i] = mt.Find
	}
	if strings.Join(got, ",") != "first,second,third" {
		t.Errorf("order not preserved: %v", got)
	}
}

func TestSetTableArray_EmptyStringValueIsNotRemoval(t *testing.T) {
	// An entry with an empty string value writes `key = ""`, distinct from
	// removing the section (which is len(entries)==0).
	path := p3write(t, "")
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "x", "replace": ""}}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.MessageTransforms) != 1 || cfg.MessageTransforms[0].Find != "x" || cfg.MessageTransforms[0].Replace != "" {
		t.Errorf("empty-string value mishandled: %+v", cfg.MessageTransforms)
	}
	if raw := p3read(t, path); !strings.Contains(raw, `replace = ""`) {
		t.Errorf("empty string not written as \"\":\n%s", raw)
	}
}

func TestSetTableArray_PreservesFollowingSectionKeys(t *testing.T) {
	// A block in the middle: the section AFTER it (and its keys) must survive a
	// replace untouched — the removal must stop at the next header, not eat it.
	path := p3write(t, `[[message_transforms]]
find = "x"
replace = "y"

[permissions]
auto_approve_common_safe_write = true
foo = "bar"
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "new", "replace": "z"}}, 0o644); err != nil {
		t.Fatal(err)
	}
	raw := p3read(t, path)
	for _, want := range []string{"[permissions]", "auto_approve_common_safe_write = true", `foo = "bar"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("clobbered following section content %q:\n%s", want, raw)
		}
	}
}

func TestSetTableArray_PreservesAdjacentTableArray(t *testing.T) {
	// A DIFFERENT array-of-tables immediately after (no blank line) must fully
	// survive replacing this one.
	path := p3write(t, `[[message_transforms]]
find = "x"
replace = "y"
[[blocked_paths]]
path = "/etc"
rebuke = "no"
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "new", "replace": "z"}}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := p3reparse(t, path)
	if len(cfg.BlockedPaths) != 1 || cfg.BlockedPaths[0].Path != "/etc" || cfg.BlockedPaths[0].Rebuke != "no" {
		t.Errorf("adjacent [[blocked_paths]] clobbered: %+v", cfg.BlockedPaths)
	}
}

func TestSetTableArray_RemovePreservesSurroundingSections(t *testing.T) {
	// Removing the section must leave every OTHER section byte-intact.
	path := p3write(t, `[alpha]
a = 1

[[message_transforms]]
find = "x"
replace = "y"

[beta]
b = 2
`)
	if _, err := SetTableArray(path, "message_transforms", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	raw := p3read(t, path)
	if strings.Contains(raw, "message_transforms") {
		t.Errorf("block not removed:\n%s", raw)
	}
	for _, want := range []string{"[alpha]", "a = 1", "[beta]", "b = 2"} {
		if !strings.Contains(raw, want) {
			t.Errorf("removal clobbered surrounding section %q:\n%s", want, raw)
		}
	}
	if _, err := toml.Decode(raw, &Config{}); err != nil {
		t.Errorf("removal produced invalid TOML: %v\n%s", err, raw)
	}
}

func TestSetTableArray_PreservesTrailingComment(t *testing.T) {
	// A comment after the last block (no section follows) must not be eaten by
	// the block scan running to EOF — it is unrelated content.
	path := p3write(t, `[[message_transforms]]
find = "x"
replace = "y"

# unrelated trailing note
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"find": "new", "replace": "z"}}, 0o644); err != nil {
		t.Fatal(err)
	}
	if raw := p3read(t, path); !strings.Contains(raw, "# unrelated trailing note") {
		t.Errorf("trailing comment clobbered:\n%s", raw)
	}
}

func TestSetTableArray_RoundTripStable(t *testing.T) {
	path := p3write(t, "[keepalive]\nenabled = true\n")
	entries := []map[string]any{
		{"find": "a", "replace": "b"},
		{"find": "c", "replace": "d"},
	}
	if _, err := SetTableArray(path, "message_transforms", entries, 0o644); err != nil {
		t.Fatal(err)
	}
	first := p3read(t, path)
	if _, err := SetTableArray(path, "message_transforms", entries, 0o644); err != nil {
		t.Fatal(err)
	}
	if second := p3read(t, path); second != first {
		t.Errorf("write is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if cfg := p3reparse(t, path); len(cfg.MessageTransforms) != 2 {
		t.Errorf("MessageTransforms = %+v, want 2 entries", cfg.MessageTransforms)
	}
}
