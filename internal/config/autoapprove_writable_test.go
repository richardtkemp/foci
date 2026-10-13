package config

import (
	"slices"
	"strings"
	"testing"

	"foci/internal/execguard"
	"foci/internal/log"
)

// testEnv builds an execEnv from explicit sets, so no test touches the host's
// real filesystem or PATH.
func testEnv(pathDirs []string, writable []string, executables []string) execguard.Env {
	w := make(map[string]bool, len(writable))
	for _, p := range writable {
		w[p] = true
	}
	x := make(map[string]bool, len(executables))
	for _, p := range executables {
		x[p] = true
	}
	return execguard.Env{
		CanWrite:     func(p string) bool { return w[p] },
		PathDirs:     pathDirs,
		IsExecutable: func(p string) bool { return x[p] },
		HomeDir:      "/home/foci",
	}
}

// readOnlyPath mimics a hardened host: no writable directory anywhere on PATH.
func readOnlyPath() execguard.Env {
	return testEnv(
		[]string{"/usr/local/bin", "/usr/bin"},
		nil,
		[]string{"/usr/bin/git", "/usr/bin/sqlite3", "/usr/local/bin/gh"},
	)
}

// shimmedPath mimics Dick's host: a writable ~/.local/bin ahead of /usr/bin.
func shimmedPath() execguard.Env {
	return testEnv(
		[]string{"/home/foci/.local/bin", "/usr/bin"},
		[]string{"/home/foci/.local/bin"},
		[]string{"/home/foci/.local/bin/sqlite3", "/usr/bin/git", "/usr/bin/sqlite3"},
	)
}

func TestCommandTokens(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want []string
	}{
		{"absolute path with glob arg", "Bash:/opt/bin/tool.py *", []string{"/opt/bin/tool.py"}},
		{"bare name", "Bash:sqlite3 *", []string{"sqlite3"}},
		{"foci shell function", "Bash:foci_*", []string{"foci_*"}},
		{"read rule names data not code", "Read:/home/rich/vault/*", nil},
		{"edit rule names data not code", "Edit:/tmp/*", nil},
		{"tool name with no pattern", "Bash", nil},
		{"empty pattern", "Bash:", nil},
		{"compound splits into segments", "Bash:cd /x && /opt/b.sh", []string{"cd", "/opt/b.sh"}},
		{"pipeline splits into segments", "Bash:cat /x | /opt/b.sh", []string{"cat", "/opt/b.sh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandTokens(tt.rule)
			if len(got) != len(tt.want) {
				t.Fatalf("commandTokens(%q) = %v, want %v", tt.rule, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("commandTokens(%q) = %v, want %v", tt.rule, got, tt.want)
				}
			}
		})
	}
}

func TestFilterWritableAutoApproveRules(t *testing.T) {
	rules := []string{
		"Bash:/usr/bin/sqlite3 -readonly", // read-only path: kept
		"Bash:/home/foci/scripts/x.py *",  // writable file: dropped
		"Bash:sqlite3 *",                  // PATH winner is the writable shim: dropped
		"Bash:git *",                      // winner is read-only /usr/bin/git: kept
		"Bash:cd *",                       // builtin: kept
		"Read:/home/foci/data/*",          // data path: kept
	}
	env := shimmedPath()
	env.CanWrite = func(p string) bool {
		return p == "/home/foci/.local/bin" ||
			p == "/home/foci/.local/bin/sqlite3" ||
			p == "/home/foci/scripts/x.py"
	}

	kept, dropped, _ := filterWritableAutoApproveRules(rules, env)

	wantKept := []string{"Bash:/usr/bin/sqlite3 -readonly", "Bash:git *", "Bash:cd *", "Read:/home/foci/data/*"}
	if len(kept) != len(wantKept) {
		t.Fatalf("kept = %v, want %v", kept, wantKept)
	}
	for i := range kept {
		if kept[i] != wantKept[i] {
			t.Fatalf("kept = %v, want %v", kept, wantKept)
		}
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %d entries, want 2: %+v", len(dropped), dropped)
	}
	for _, d := range dropped {
		if d.Rule == "" || d.Reason == "" || d.Path == "" {
			t.Errorf("dropped entry missing a field: %+v", d)
		}
	}
}

func TestFilterWritableAutoApproveRules_NoRules(t *testing.T) {
	kept, dropped, unlocatable := filterWritableAutoApproveRules(nil, readOnlyPath())
	if len(kept) != 0 || len(dropped) != 0 || len(unlocatable) != 0 {
		t.Fatalf("kept=%v dropped=%v unlocatable=%v, want all empty", kept, dropped, unlocatable)
	}
}

func TestDropWritableAutoApproveRules_GlobalAndPerAgent(t *testing.T) {
	cfg := &Config{}
	cfg.Permissions.AutoApprove = []string{"Bash:/bad/g.sh *", "Bash:/good/g.sh *"}
	cfg.Agents = []AgentConfig{
		{ID: "clutch", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/bad/a.sh *", "Bash:cd *"}}},
		{ID: "scout", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/good/s.sh *"}}},
	}
	env := testEnv(nil, []string{"/bad/g.sh", "/bad/a.sh"}, nil)

	cfg.DropSubstitutableAutoApproveRules(env)

	if got := cfg.Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:/good/g.sh *" {
		t.Errorf("global AutoApprove = %v, want [Bash:/good/g.sh *]", got)
	}
	if got := cfg.Agents[0].Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:cd *" {
		t.Errorf("clutch AutoApprove = %v, want [Bash:cd *]", got)
	}
	if got := cfg.Agents[1].Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:/good/s.sh *" {
		t.Errorf("scout AutoApprove = %v, want [Bash:/good/s.sh *]", got)
	}
}

// #1900 fix direction (c): "the guard cannot find this command" must be a
// distinct, reportable outcome. Substitutable answers false for such an entry,
// identically to "checked and safe" — and that silence is what let a
// PATH-divergence hide for a month.
func TestFilterWritableAutoApproveRules_ReportsCommandsItCannotLocate(t *testing.T) {
	rules := []string{
		"Bash:git *",       // on PATH, read-only: kept, located
		"Bash:mds *",       // on NO directory of this PATH: kept, but reported
		"Bash:cd *",        // builtin: the shell never searches PATH, not a finding
		"Bash:foci_* ",     // glob: names no single file, not a finding
		"Bash:/opt/x.sh *", // explicit path: resolved directly, not a bare name
		"Read:/data/*",     // not a Bash entry at all
	}

	kept, dropped, unlocatable := filterWritableAutoApproveRules(rules, readOnlyPath())

	if len(kept) != len(rules) {
		t.Fatalf("kept %d of %d — an unlocatable command must not DROP the entry, only report it: %v", len(kept), len(rules), kept)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %+v, want none", dropped)
	}
	if len(unlocatable) != 1 {
		t.Fatalf("unlocatable = %+v, want exactly one (mds). Builtins, globs and explicit paths are not 'missing from PATH'.", unlocatable)
	}
	if unlocatable[0].Command != "mds" || unlocatable[0].Rule != "Bash:mds *" {
		t.Errorf("unlocatable[0] = %+v, want {Rule:\"Bash:mds *\" Command:\"mds\"}", unlocatable[0])
	}
}

// TestScanAutoApproveRules_ScopesFieldsAndPurity proves the -check-config
// preview sees exactly what the startup drop would do, and that seeing it
// costs nothing: ScanAutoApproveRules reports every scope in drop order
// (global block first, then agents in config order), carries the fields the
// renderer needs (the dropped entry's Command for the summary line, and the
// Path/Reason pair execguard gave), and neither logs nor mutates the config.
func TestScanAutoApproveRules_ScopesFieldsAndPurity(t *testing.T) {
	cfg := &Config{}
	cfg.Permissions.AutoApprove = []string{"Bash:/bad/g.sh *", "Bash:gone *"}
	cfg.Agents = []AgentConfig{
		{ID: "clutch", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/bad/a.sh *"}}},
		{ID: "scout", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/good/s.sh *"}}},
	}
	env := testEnv(nil, []string{"/bad/g.sh", "/bad/a.sh"}, nil)

	var logged []string
	remove := log.AddTap(log.WARN, func(level log.Level, component, msg string) {
		if strings.Contains(msg, "[permissions]") {
			logged = append(logged, msg)
		}
	})
	defer remove()

	got := cfg.ScanAutoApproveRules(env)

	if len(logged) != 0 {
		t.Errorf("ScanAutoApproveRules logged %d line(s); a report must not log: %v", len(logged), logged)
	}
	if len(got) != 3 {
		t.Fatalf("got %d scopes, want 3 (global + 2 agents): %+v", len(got), got)
	}
	if !got[0].Global || got[0].AgentID != "" {
		t.Errorf("scope[0] = {Global:%v AgentID:%q}, want the global block", got[0].Global, got[0].AgentID)
	}
	if got[1].Global || got[1].AgentID != "clutch" || got[2].AgentID != "scout" {
		t.Errorf("agent scopes mislabelled or out of config order: %+v %+v", got[1], got[2])
	}
	if len(got[0].Dropped) != 1 {
		t.Fatalf("global Dropped = %+v, want the one substitutable entry", got[0].Dropped)
	}
	d := got[0].Dropped[0]
	if d.Rule != "Bash:/bad/g.sh *" || d.Command != "/bad/g.sh" || d.Path != "/bad/g.sh" || d.Reason == "" {
		t.Errorf("dropped entry missing a field: %+v", d)
	}
	if len(got[0].Unlocatable) != 1 || got[0].Unlocatable[0].Command != "gone" || got[0].Unlocatable[0].Rule != "Bash:gone *" {
		t.Errorf("global Unlocatable = %+v, want the one bare command no PATH directory provides", got[0].Unlocatable)
	}
	if len(got[1].Dropped) != 1 || got[1].Dropped[0].Command != "/bad/a.sh" {
		t.Errorf("clutch Dropped = %+v, want its own substitutable entry", got[1].Dropped)
	}
	if len(got[2].Dropped) != 0 || len(got[2].Unlocatable) != 0 {
		t.Errorf("scout findings = %+v %+v, want none: a safe entry is not reported", got[2].Dropped, got[2].Unlocatable)
	}

	if want := []string{"Bash:/bad/g.sh *", "Bash:gone *"}; !slices.Equal(cfg.Permissions.AutoApprove, want) {
		t.Errorf("scan mutated the global list: %v, want %v", cfg.Permissions.AutoApprove, want)
	}
	if want := []string{"Bash:/bad/a.sh *"}; !slices.Equal(cfg.Agents[0].Permissions.AutoApprove, want) {
		t.Errorf("scan mutated the agent list: %v, want %v", cfg.Agents[0].Permissions.AutoApprove, want)
	}
}

// TestDropWritableAutoApproveRules_AgentNamedGlobalLabels is a
// characterisation test (listed in .factory/red-gate-exempt.txt: the labels
// are unchanged on both sides of #1897). It pins requirement 4's startup
// half: the global block is labelled `global`, and an agent whose id happens
// to be "global" is labelled as that agent — `agent "global"` — so the drop
// WARN's scopes never collide.
func TestDropWritableAutoApproveRules_AgentNamedGlobalLabels(t *testing.T) {
	var lines []string
	remove := log.AddTap(log.WARN, func(level log.Level, component, msg string) {
		if strings.Contains(msg, "[permissions]") {
			lines = append(lines, msg)
		}
	})
	defer remove()

	cfg := &Config{}
	cfg.Permissions.AutoApprove = []string{"Bash:/bad/g.sh *"}
	cfg.Agents = []AgentConfig{
		{ID: "global", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/bad/a.sh *"}}},
	}
	cfg.DropSubstitutableAutoApproveRules(testEnv(nil, []string{"/bad/g.sh", "/bad/a.sh"}, nil))

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `[permissions] global: ignoring auto_approve entry "Bash:/bad/g.sh *"`) {
		t.Errorf("global block not labelled global:\n%s", joined)
	}
	if !strings.Contains(joined, `[permissions] agent "global": ignoring auto_approve entry "Bash:/bad/a.sh *"`) {
		t.Errorf("agent named global not labelled as that agent:\n%s", joined)
	}
}
