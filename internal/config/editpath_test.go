package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// editFixture is a config with comments around every shape the path editor
// must reach: a top-level scalar, a nested table, two agents (one with a
// sub-table and an array of platforms), a top-level array of tables, and a
// quoted key.
const editFixture = `# head comment
master_agent = "main"

# loop settings
[agent_loop]
max_tool_loops = 10 # trailing comment

[models."claude.fast"]
id = "claude-haiku-4-5"

# blocked paths
[[blocked_paths]]
path = "/etc"

[[blocked_paths]]
path = "/root"

[[agents]]
id = "main"
model = "opus" # main model

[agents.nudge]
enable = true

[[agents.platforms]]
id = "telegram"

[[agents]]
id = "other"
model = "haiku"
# tail comment
`

func mustPath(t *testing.T, s string) KeyPath {
	t.Helper()
	p, err := ParseKeyPath(s)
	if err != nil {
		t.Fatalf("ParseKeyPath(%q): %v", s, err)
	}
	return p
}

// TestPlanSetPath_Shapes proves the path editor reaches every shape the
// ticket lists — top-level scalar, nested table key, a key in one agent's
// block, a top-level array of tables, one agent's platforms, a quoted key —
// and that each result holds the value at the path, differs nowhere else
// (VerifyPathEdit), and keeps every comment of the file.
func TestPlanSetPath_Shapes(t *testing.T) {
	cases := []struct {
		path, value string
		wantLine    string // a line the result must contain
	}{
		{"master_agent", `"other"`, `master_agent = "other"`},
		{"agent_loop.max_tool_loops", "20", "max_tool_loops = 20"},
		{"agent_loop.streaming", "false", "streaming = false"},
		{"agents[main].model", `"sonnet"`, `model = "sonnet"`},
		{"agents[other].nudge.enable", "false", "enable = false"},
		{"agents[main].nudge.enable", "false", "enable = false"},
		// An array of tables keeps its [[...]] form (#2938).
		{"blocked_paths", `[{path = "/x"}, {path = "/y"}]`, `path = "/y"`},
		{"agents[main].platforms", `[{id = "app"}]`, `id = "app"`},
		{`models."claude.fast".id`, `"claude-sonnet"`, `id = "claude-sonnet"`},
		{"new_section.when", "2026-11-01T10:00:00Z", "when = 2026-11-01T10:00:00Z"},
		{"agent_loop.list", `["a", 1.5, true]`, `list = ["a", 1.5, true]`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			p := mustPath(t, tc.path)
			next, err := PlanSetPath(editFixture, p, tc.value)
			if err != nil {
				t.Fatalf("PlanSetPath: %v", err)
			}
			if !strings.Contains(next, tc.wantLine) {
				t.Errorf("result lacks %q:\n%s", tc.wantLine, next)
			}
			want, _ := DecodeTOMLValue(tc.value)
			if err := VerifyPathEdit(editFixture, next, p, want, false); err != nil {
				t.Errorf("VerifyPathEdit: %v", err)
			}
			for _, c := range []string{"# head comment", "# loop settings", "# trailing comment", "# blocked paths", "# main model", "# tail comment"} {
				if !strings.Contains(next, c) {
					t.Errorf("comment %q lost:\n%s", c, next)
				}
			}
		})
	}
}

// TestPlanSetPath_ArrayOfTablesReplacedWhole proves a set of a path written
// as [[blocked_paths]] blocks replaces every block: no old entry survives,
// and the new value keeps the [[...]] form (#2938).
func TestPlanSetPath_ArrayOfTablesReplacedWhole(t *testing.T) {
	next, err := PlanSetPath(editFixture, mustPath(t, "blocked_paths"), `[{path = "/x"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(next, "[[blocked_paths]]") != 1 || strings.Contains(next, `"/etc"`) || strings.Contains(next, `"/root"`) {
		t.Errorf("want one [[blocked_paths]] block holding /x:\n%s", next)
	}
}

// TestPlanUnsetPath proves unset removes every definition of the path (a
// key line, a whole sub-table, an array of tables) and nothing else, and that
// an absent path is refused.
func TestPlanUnsetPath(t *testing.T) {
	for _, path := range []string{"agent_loop.max_tool_loops", "agents[main].nudge", "agents[main].nudge.enable", "blocked_paths", "agents[main].platforms", `models."claude.fast"`} {
		t.Run(path, func(t *testing.T) {
			p := mustPath(t, path)
			next, err := PlanUnsetPath(editFixture, p)
			if err != nil {
				t.Fatalf("PlanUnsetPath: %v", err)
			}
			if _, ok, _ := LookupPath(next, p); ok {
				t.Errorf("%s still set", path)
			}
			if !strings.Contains(next, "# tail comment") || !strings.Contains(next, "# head comment") {
				t.Errorf("comments lost:\n%s", next)
			}
		})
	}
	if _, err := PlanUnsetPath(editFixture, mustPath(t, "agent_loop.nope")); err == nil {
		t.Error("unset of an absent key: want an error")
	}
}

// TestPlanSetPath_Refusals proves an unknown agent, a path inside an inline
// value, and a value that is not TOML are refused without a plan.
func TestPlanSetPath_Refusals(t *testing.T) {
	inline := "x = {a = 1}\n"
	cases := []struct {
		content, path, value, want string
	}{
		{editFixture, "agents[ghost].model", `"a"`, `agent "ghost" not found`},
		{inline, "x.a", "2", "inside the value"},
		{editFixture, "master_agent", "not toml", "not a TOML value"},
	}
	for _, tc := range cases {
		if _, err := PlanSetPath(tc.content, mustPath(t, tc.path), tc.value); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s = %s: err = %v, want %q", tc.path, tc.value, err, tc.want)
		}
	}
}

// TestParseKeyPath covers the path syntax: dotted, quoted, agent-scoped, and
// the malformed forms.
func TestParseKeyPath(t *testing.T) {
	p := mustPath(t, `agents[clutch].models."a.b".x`)
	if p.AgentID != "clutch" || strings.Join(p.Keys, "|") != "models|a.b|x" {
		t.Errorf("got %+v", p)
	}
	if p.String() != `agents[clutch].models."a.b".x` {
		t.Errorf("String() = %q", p.String())
	}
	for _, bad := range []string{"", "agents[]", "agents[x]", "agents.x", "a..b", `a."b`} {
		if _, err := ParseKeyPath(bad); err == nil {
			t.Errorf("ParseKeyPath(%q): want an error", bad)
		}
	}
}

// TestVerifyPathEdit_RejectsSecondChange proves the guard refuses an edit
// that changes another key besides the path.
func TestVerifyPathEdit_RejectsSecondChange(t *testing.T) {
	next := strings.Replace(editFixture, "max_tool_loops = 10", "max_tool_loops = 20", 1)
	next = strings.Replace(next, `model = "haiku"`, `model = "opus"`, 1)
	if err := VerifyPathEdit(editFixture, next, mustPath(t, "agent_loop.max_tool_loops"), int64(20), false); err == nil {
		t.Error("want an error for an edit that changes a second key")
	}
}

// inodeOf returns the file's inode and mode.
func inodeOf(t *testing.T, path string) (os.FileInfo, uint64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info, info.Sys().(*syscall.Stat_t).Ino
}

// TestConfigWriters_KeepInodeAndMode proves every config writer edits the
// file in place (#479): after SetInFile, UnsetInFile and SetTableArray the
// file is the same inode with the same mode, even when the mode passed to the
// writer differs, and a failed edit leaves the file byte-identical.
func TestConfigWriters_KeepInodeAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(editFixture), 0o604); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o604); err != nil {
		t.Fatal(err)
	}
	before, _ := inodeOf(t, path)
	check := func(step string) {
		t.Helper()
		after, _ := inodeOf(t, path)
		if !os.SameFile(before, after) {
			t.Errorf("%s replaced the file (new inode)", step)
		}
		if after.Mode().Perm() != 0o604 {
			t.Errorf("%s changed the mode to %04o", step, after.Mode().Perm())
		}
	}
	if _, err := SetInFile(path, SetTarget{Section: "agent_loop", Key: "max_tool_loops"}, "11", 0o600); err != nil {
		t.Fatal(err)
	}
	check("SetInFile")
	if _, err := UnsetInFile(path, SetTarget{Section: "agent_loop", Key: "max_tool_loops"}, 0o600); err != nil {
		t.Fatal(err)
	}
	check("UnsetInFile")
	if _, err := SetTableArray(path, "blocked_paths", []map[string]any{{"path": "/z"}}, 0o600); err != nil {
		t.Fatal(err)
	}
	check("SetTableArray")

	data, _ := os.ReadFile(path)
	if _, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "ghost", Key: "model"}, `"x"`, 0o600); err == nil {
		t.Fatal("edit of an unknown agent: want an error")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Error("a failed edit changed the file")
	}
	check("failed SetInFile")
}

// TestWriteConfigFile_NewFileGetsMode proves the mode argument applies only
// when the file does not exist yet.
func TestWriteConfigFile_NewFileGetsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.toml")
	if err := WriteConfigFile(path, "a = 1\n", 0o640); err != nil {
		t.Fatal(err)
	}
	info, _ := inodeOf(t, path)
	if info.Mode().Perm() != 0o640&^currentUmask() {
		t.Errorf("new file mode %04o, want %04o", info.Mode().Perm(), 0o640&^currentUmask())
	}
}

// currentUmask reads the process umask without changing it for long.
func currentUmask() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}

// TestCheckConfigProtection proves the protection check passes only the
// exact shape (owner, group, mode 0664), names each fault and the fix
// command, and runs without root by injecting the wanted ids.
func TestCheckConfigProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("a = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	info, _ := inodeOf(t, path)
	st := info.Sys().(*syscall.Stat_t)
	uid, gid := st.Uid, st.Gid

	if err := checkConfigProtection(path, uid, gid); err != nil {
		t.Errorf("good file: %v", err)
	}
	if err := checkConfigProtection(path, uid+1, gid); err == nil || !strings.Contains(err.Error(), "owner is uid") || !strings.Contains(err.Error(), "sudo chown root:foci-secrets "+path) {
		t.Errorf("wrong uid: %v", err)
	}
	if err := checkConfigProtection(path, uid, gid+1); err == nil || !strings.Contains(err.Error(), "group is gid") {
		t.Errorf("wrong gid: %v", err)
	}
	for _, mode := range []os.FileMode{0o644, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkConfigProtection(path, uid, gid); err == nil || !strings.Contains(err.Error(), "want 0664") || !strings.Contains(err.Error(), "sudo chmod 0664 "+path) {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
}
