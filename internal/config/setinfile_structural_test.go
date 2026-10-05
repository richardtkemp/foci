package config

import (
	"os"
	"strings"
	"testing"
)

// #2191: a line inside a multi-line value that looks like structure (a table
// header, a key line, a commented key, an agent's id line) must not be read
// as structure. Each case puts the look-alike BEFORE the target key, so a
// line-based reader ends the section early or matches the wrong line.

const statuslineWithHeader = `"""{default}
[deploy]
"""`

func TestSetInFile_SectionHeaderInsideStringIsContent(t *testing.T) {
	path := writeTOML(t, `[display]
statusline = `+statuslineWithHeader+`
other = 1
`)
	old, err := SetInFile(path, SetTarget{Section: "display", Key: "other"}, "2", 0o640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if old != "1" {
		t.Errorf("old = %q, want 1", old)
	}
	d := decodeTOML(t, path)["display"].(map[string]any)
	if d["other"] != int64(2) || d["statusline"] != "{default}\n[deploy]\n" {
		t.Errorf("display = %q, want other=2 and statusline untouched", d)
	}
}

func TestSetInFile_AgentBlockHeaderInsideStringIsContent(t *testing.T) {
	path := writeTOML(t, `[[agents]]
id = "clutch"
display.statusline = `+statuslineWithHeader+`
model = "opus"
`)
	old, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "clutch", Key: "model"}, `"sonnet"`, 0o640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if old != `"opus"` {
		t.Errorf("old = %q, want \"opus\"", old)
	}
	a := agentByID(t, decodeTOML(t, path), "clutch")
	if a["model"] != "sonnet" || a["display"].(map[string]any)["statusline"] != "{default}\n[deploy]\n" {
		t.Errorf("agent = %q, want model=sonnet and statusline untouched", a)
	}
}

func TestUnsetInFile_HeaderInsideStringIsContent(t *testing.T) {
	path := writeTOML(t, `[display]
statusline = `+statuslineWithHeader+`
other = 1
`)
	if _, err := UnsetInFile(path, SetTarget{Section: "display", Key: "other"}, 0o640); err != nil {
		t.Fatalf("UnsetInFile: %v", err)
	}
	d := decodeTOML(t, path)["display"].(map[string]any)
	if _, ok := d["other"]; ok || d["statusline"] != "{default}\n[deploy]\n" {
		t.Errorf("display = %q, want other gone and statusline untouched", d)
	}
}

// A nested array row "[ "a" ]" on its own line also matches the header regex.
func TestSetInFile_NestedArrayRowIsContent(t *testing.T) {
	path := writeTOML(t, `[x]
grid = [
  [ "a" ]
]
other = 1
`)
	if _, err := SetInFile(path, SetTarget{Section: "x", Key: "other"}, "2", 0o640); err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if got := decodeTOML(t, path)["x"].(map[string]any)["other"]; got != int64(2) {
		t.Errorf("other = %v, want 2", got)
	}
}

// A "key = ..." line inside a string is not the key.
func TestSetInFile_KeyLineInsideStringIsContent(t *testing.T) {
	const prompt = "\"\"\"\nother = 9\n\"\"\""
	path := writeTOML(t, `[x]
prompt = `+prompt+`
other = 1
`)
	old, err := SetInFile(path, SetTarget{Section: "x", Key: "other"}, "2", 0o640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	if old != "1" {
		t.Errorf("old = %q, want 1", old)
	}
	x := decodeTOML(t, path)["x"].(map[string]any)
	if x["other"] != int64(2) || x["prompt"] != "other = 9\n" {
		t.Errorf("x = %q, want other=2 and prompt untouched", x)
	}
}

// A "# key = ..." line inside a string must not be uncommented.
func TestSetInFile_CommentedKeyInsideStringIsContent(t *testing.T) {
	path := writeTOML(t, `[x]
prompt = """
# other = 9
"""
`)
	if _, err := SetInFile(path, SetTarget{Section: "x", Key: "other"}, "2", 0o640); err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	x := decodeTOML(t, path)["x"].(map[string]any)
	if x["other"] != int64(2) || x["prompt"] != "# other = 9\n" {
		t.Errorf("x = %q, want other=2 and prompt untouched", x)
	}
}

// An earlier agent whose prompt contains another agent's id line must not be
// taken for that agent.
func TestSetInFile_AgentIDInsideStringIsContent(t *testing.T) {
	path := writeTOML(t, `[[agents]]
id = "other"
prompt = """
id = "clutch"
"""
model = "a"

[[agents]]
id = "clutch"
model = "b"
`)
	if _, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "clutch", Key: "model"}, `"c"`, 0o640); err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	raw := decodeTOML(t, path)
	if m := agentByID(t, raw, "clutch")["model"]; m != "c" {
		t.Errorf("clutch model = %v, want c", m)
	}
	if m := agentByID(t, raw, "other")["model"]; m != "a" {
		t.Errorf("other model = %v, want a (untouched)", m)
	}
}

// SetTableArray must not treat a "[[section]]" line inside a string as a block.
func TestSetTableArray_HeaderInsideStringIsContent(t *testing.T) {
	path := writeTOML(t, `[x]
note = """
[[message_transforms]]
pattern = "keep me"
"""
`)
	if _, err := SetTableArray(path, "message_transforms",
		[]map[string]any{{"pattern": "p", "replacement": "r"}}, 0o640); err != nil {
		t.Fatalf("SetTableArray: %v", err)
	}
	raw := decodeTOML(t, path)
	if note := raw["x"].(map[string]any)["note"]; note != "[[message_transforms]]\npattern = \"keep me\"\n" {
		t.Errorf("note = %q, want untouched", note)
	}
	if mt, _ := raw["message_transforms"].([]map[string]any); len(mt) != 1 || mt[0]["pattern"] != "p" {
		data, _ := os.ReadFile(path)
		t.Errorf("message_transforms = %v, want one block\n%s", raw["message_transforms"], data)
	}
}

// An unterminated value keeps the old line-based reading for the rest of the
// file rather than hiding every later section (which would make the writer
// append a duplicate [b]).
func TestSetInFile_UnterminatedValueStillFindsLaterSection(t *testing.T) {
	path := writeTOML(t, `[a]
s = """never closed

[b]
k = 1
`)
	old, err := SetInFile(path, SetTarget{Section: "b", Key: "k"}, "2", 0o640)
	if err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	data, _ := os.ReadFile(path)
	if old != "1" || strings.Count(string(data), "[b]") != 1 || !strings.Contains(string(data), "k = 2") {
		t.Errorf("old = %q, want 1 and k set in place:\n%s", old, data)
	}
}
