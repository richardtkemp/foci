package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writableSet builds a predicate that reports true for exactly the given paths.
func writableSet(paths ...string) writablePredicate {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return func(path string) bool { return set[path] }
}

func nothingWritable(string) bool { return false }

func TestAutoApproveCommandPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	tests := []struct {
		name string
		rule string
		want []string
	}{
		{"absolute path with glob arg", "Bash:/opt/bin/tool.py *", []string{"/opt/bin/tool.py"}},
		{"absolute path with flag", "Bash:/usr/bin/sqlite3 -readonly", []string{"/usr/bin/sqlite3"}},
		{"bare command is out of scope", "Bash:sqlite3 *", nil},
		{"foci shell function is out of scope", "Bash:foci_*", nil},
		{"relative path is out of scope", "Bash:./run.sh", nil},
		{"tilde is expanded", "Bash:~/bin/x.sh *", []string{filepath.Join(home, "bin/x.sh")}},
		{"read rule names data not code", "Read:/home/rich/vault/*", nil},
		{"edit rule names data not code", "Edit:/tmp/*", nil},
		{"write rule names data not code", "Write:/home/rich/vault/*", nil},
		{"tool name with no pattern", "Bash", nil},
		{"empty pattern", "Bash:", nil},
		{"compound entry checks every segment", "Bash:cd /x && /opt/bin/b.sh", []string{"/opt/bin/b.sh"}},
		{"compound entry with two paths", "Bash:/opt/a.sh; /opt/b.sh", []string{"/opt/a.sh", "/opt/b.sh"}},
		{"pipeline segments", "Bash:/opt/a.sh | /opt/b.sh", []string{"/opt/a.sh", "/opt/b.sh"}},
		{"path is cleaned", "Bash:/opt/bin/../bin/tool *", []string{"/opt/bin/tool"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := autoApproveCommandPaths(tt.rule)
			if len(got) != len(tt.want) {
				t.Fatalf("autoApproveCommandPaths(%q) = %v, want %v", tt.rule, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("autoApproveCommandPaths(%q) = %v, want %v", tt.rule, got, tt.want)
				}
			}
		})
	}
}

func TestExecutablePathIsWritable_StrictIncludesParentDirectory(t *testing.T) {
	// The strict rule: a read-only file inside a writable directory still fails,
	// because the directory permits unlink-and-replace.
	writable, reason := executablePathIsWritable("/opt/bin/tool", writableSet("/opt/bin"))
	if !writable {
		t.Fatal("file in a writable directory must be treated as writable")
	}
	if reason == "" {
		t.Fatal("want a non-empty reason naming the directory")
	}

	writable, _ = executablePathIsWritable("/opt/bin/tool", writableSet("/opt/bin/tool"))
	if !writable {
		t.Fatal("a directly writable file must be treated as writable")
	}

	writable, _ = executablePathIsWritable("/opt/bin/tool", nothingWritable)
	if writable {
		t.Fatal("a read-only file in a read-only directory must pass")
	}
}

func TestFilterWritableAutoApproveRules(t *testing.T) {
	rules := []string{
		"Bash:/usr/bin/sqlite3 -readonly", // root-owned: kept
		"Bash:/home/foci/scripts/x.py *",  // foci-writable file: dropped
		"Bash:/opt/ro/tool *",             // read-only file, writable dir: dropped (strict)
		"Bash:sqlite3 *",                  // bare name: out of scope, kept
		"Read:/home/foci/data/*",          // data path: kept
	}
	canWrite := writableSet("/home/foci/scripts/x.py", "/opt/ro")

	kept, dropped := filterWritableAutoApproveRules(rules, canWrite)

	wantKept := []string{"Bash:/usr/bin/sqlite3 -readonly", "Bash:sqlite3 *", "Read:/home/foci/data/*"}
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
	if dropped[0].Path != "/home/foci/scripts/x.py" {
		t.Errorf("dropped[0].Path = %q", dropped[0].Path)
	}
	if dropped[1].Path != "/opt/ro/tool" {
		t.Errorf("dropped[1].Path = %q", dropped[1].Path)
	}
	for _, d := range dropped {
		if d.Rule == "" || d.Reason == "" {
			t.Errorf("dropped entry missing Rule/Reason: %+v", d)
		}
	}
}

func TestFilterWritableAutoApproveRules_NoRules(t *testing.T) {
	kept, dropped := filterWritableAutoApproveRules(nil, nothingWritable)
	if len(kept) != 0 || len(dropped) != 0 {
		t.Fatalf("kept=%v dropped=%v, want both empty", kept, dropped)
	}
}

func TestDropWritableAutoApproveRules_GlobalAndPerAgent(t *testing.T) {
	cfg := &Config{}
	cfg.Permissions.AutoApprove = []string{"Bash:/bad/g.sh *", "Bash:/good/g.sh *"}
	cfg.Agents = []AgentConfig{
		{ID: "clutch", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/bad/a.sh *", "Bash:git *"}}},
		{ID: "scout", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/good/s.sh *"}}},
	}

	cfg.dropWritableAutoApproveRules(writableSet("/bad/g.sh", "/bad/a.sh"))

	if got := cfg.Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:/good/g.sh *" {
		t.Errorf("global AutoApprove = %v, want [Bash:/good/g.sh *]", got)
	}
	if got := cfg.Agents[0].Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:git *" {
		t.Errorf("clutch AutoApprove = %v, want [Bash:git *]", got)
	}
	if got := cfg.Agents[1].Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:/good/s.sh *" {
		t.Errorf("scout AutoApprove = %v, want [Bash:/good/s.sh *]", got)
	}
}

// processCanWrite is the real predicate; exercise it against the filesystem so
// the injected-predicate tests above are not the only coverage.
func TestProcessCanWrite_RealFilesystem(t *testing.T) {
	dir := t.TempDir()
	writablePath := filepath.Join(dir, "writable")
	if err := os.WriteFile(writablePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !processCanWrite(writablePath) {
		t.Error("a 0600 file we own must report writable")
	}
	if !processCanWrite(dir) {
		t.Error("a temp dir we own must report writable")
	}

	readonlyPath := filepath.Join(dir, "readonly")
	if err := os.WriteFile(readonlyPath, []byte("x"), 0o400); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root — access(2) reports writable regardless of mode")
	}
	if processCanWrite(readonlyPath) {
		t.Error("a 0400 file must report not writable")
	}
	if processCanWrite(filepath.Join(dir, "does-not-exist")) {
		t.Error("a missing path must report not writable")
	}
}
