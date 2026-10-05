package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// writeTOML writes content to a fresh foci.toml in a temp dir and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// decodeTOML parses the file, failing the test if it is no longer valid TOML
// (the #2188 symptom: an orphaned multi-line tail reads as a broken header).
func decodeTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("config no longer parses: %v\n%s", err, data)
	}
	return raw
}

func agentByID(t *testing.T, raw map[string]any, id string) map[string]any {
	t.Helper()
	agents, _ := raw["agents"].([]map[string]any)
	for _, a := range agents {
		if a["id"] == id {
			return a
		}
	}
	t.Fatalf("agent %q not in decoded config", id)
	return nil
}

// #2188: /config set on a key whose existing value is a multi-line string
// (triple-quoted, basic or literal) must replace the whole string, not just
// its first line.
func TestSetInFile_ReplacesMultiLineString(t *testing.T) {
	cases := []struct {
		name, value, wantOld string
	}{
		{
			name:    "basic",
			value:   "\"\"\"{default}\n[deploy] ${/path/script}\"\"\"",
			wantOld: "\"\"\"{default}\n[deploy] ${/path/script}\"\"\"",
		},
		{
			name:    "literal",
			value:   "'''{default}\n[deploy] # not a comment\n'''",
			wantOld: "'''{default}\n[deploy] # not a comment\n'''",
		},
		{
			// Escaped quotes and a quote run at the close (content quote + delimiter).
			name:    "basic with escapes and trailing content quote",
			value:   "\"\"\"a \\\"\"\" b\n[x]\nend\"\"\"\" # trailing comment",
			wantOld: "\"\"\"a \\\"\"\" b\n[x]\nend\"\"\"\"",
		},
	}
	for _, tc := range cases {
		t.Run("agent/"+tc.name, func(t *testing.T) {
			path := writeTOML(t, `[[agents]]
id = "clutch"
display.statusline = `+tc.value+`
model = "opus"

[[agents]]
id = "other"
`)
			old, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "clutch", Key: "display.statusline"}, `"x"`, 0o640)
			if err != nil {
				t.Fatalf("SetInFile: %v", err)
			}
			if old != tc.wantOld {
				t.Errorf("old = %q, want %q", old, tc.wantOld)
			}
			a := agentByID(t, decodeTOML(t, path), "clutch")
			if got := a["display"].(map[string]any)["statusline"]; got != "x" {
				t.Errorf("statusline = %v, want x", got)
			}
			if a["model"] != "opus" {
				t.Errorf("following key lost: model = %v", a["model"])
			}
		})
		t.Run("section/"+tc.name, func(t *testing.T) {
			path := writeTOML(t, `[display]
statusline = `+tc.value+`
other = 1

[sessions]
dir = "/tmp"
`)
			old, err := SetInFile(path, SetTarget{Section: "display", Key: "statusline"}, `"x"`, 0o640)
			if err != nil {
				t.Fatalf("SetInFile: %v", err)
			}
			if old != tc.wantOld {
				t.Errorf("old = %q, want %q", old, tc.wantOld)
			}
			d := decodeTOML(t, path)["display"].(map[string]any)
			if d["statusline"] != "x" || d["other"] != int64(1) {
				t.Errorf("display = %v, want statusline=x other=1", d)
			}
		})
		t.Run("unset/"+tc.name, func(t *testing.T) {
			path := writeTOML(t, `[display]
statusline = `+tc.value+`
other = 1
`)
			old, err := UnsetInFile(path, SetTarget{Section: "display", Key: "statusline"}, 0o640)
			if err != nil {
				t.Fatalf("UnsetInFile: %v", err)
			}
			if old != tc.wantOld {
				t.Errorf("old = %q, want %q", old, tc.wantOld)
			}
			d := decodeTOML(t, path)["display"].(map[string]any)
			if _, ok := d["statusline"]; ok || d["other"] != int64(1) {
				t.Errorf("display = %v, want only other=1", d)
			}
		})
	}
}

// A multi-line array whose strings hold brackets and '#' (in both quote kinds)
// must still be spanned to its real close, not to a bracket inside a string.
func TestSetInFile_ReplacesArrayWithBracketsInStrings(t *testing.T) {
	path := writeTOML(t, `[permissions]
auto_approve = [
  'Bash:echo #[',
  "Read:#]",
  """x
]""",
]
other_key = true
`)
	if _, err := SetInFile(path, SetTarget{Section: "permissions", Key: "auto_approve"}, `["a"]`, 0o640); err != nil {
		t.Fatalf("SetInFile: %v", err)
	}
	p := decodeTOML(t, path)["permissions"].(map[string]any)
	if p["other_key"] != true {
		t.Errorf("permissions = %v, want other_key=true kept", p)
	}
	if got, _ := p["auto_approve"].([]any); len(got) != 1 || got[0] != "a" {
		t.Errorf("auto_approve = %v, want [a]", p["auto_approve"])
	}
}

// An unterminated value must be refused, not "fixed" by eating the lines that follow.
func TestSetInFile_UnterminatedMultiLineStringFails(t *testing.T) {
	content := `[display]
statusline = """never closed
other = 1
`
	path := writeTOML(t, content)
	if _, err := SetInFile(path, SetTarget{Section: "display", Key: "statusline"}, `"x"`, 0o640); err == nil {
		t.Fatal("SetInFile accepted an unterminated value, want error")
	}
	if data, _ := os.ReadFile(path); string(data) != content {
		t.Errorf("file changed despite error:\n%s", data)
	}
}
