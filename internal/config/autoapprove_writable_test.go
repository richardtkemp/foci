package config

import (
	"testing"

	"foci/internal/execguard"
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

	kept, dropped := filterWritableAutoApproveRules(rules, env)

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
	kept, dropped := filterWritableAutoApproveRules(nil, readOnlyPath())
	if len(kept) != 0 || len(dropped) != 0 {
		t.Fatalf("kept=%v dropped=%v, want both empty", kept, dropped)
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

	cfg.dropWritableAutoApproveRules(env)

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
