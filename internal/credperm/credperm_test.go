package credperm

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeCred(t *testing.T, home, rel string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile's mode is umask-filtered
		t.Fatal(err)
	}
	return p
}

func self() uint32 { return uint32(os.Geteuid()) } //nolint:gosec // test-only

func TestScan_OwnerOnlyFileIsQuiet(t *testing.T) {
	home := t.TempDir()
	writeCred(t, home, Files[0], 0o600)

	if got := scan(home, self(), "foci"); len(got) != 0 {
		t.Fatalf("scan = %q, want no warnings for a 0600 file", got)
	}
}

func TestScan_MissingFilesAreQuiet(t *testing.T) {
	if got := scan(t.TempDir(), self(), "foci"); len(got) != 0 {
		t.Fatalf("scan = %q, want no warnings when no backend is logged in", got)
	}
}

// The WARN must name the file and carry a command that, run verbatim, fixes
// it — and the fix must actually make the warning go away.
func TestScan_GroupReadableWarnsWithWorkingFix(t *testing.T) {
	home := t.TempDir()
	p := writeCred(t, home, Files[2], 0o640)

	got := scan(home, self(), "foci")
	if len(got) != 1 {
		t.Fatalf("scan = %q, want exactly one warning", got)
	}
	if !strings.Contains(got[0], p) {
		t.Fatalf("warning %q does not name %s", got[0], p)
	}
	_, fix, ok := strings.Cut(got[0], "Fix: ")
	if !ok {
		t.Fatalf("warning %q has no Fix: command", got[0])
	}
	if out, err := exec.Command("/bin/sh", "-c", fix).CombinedOutput(); err != nil {
		t.Fatalf("running fix %q: %v\n%s", fix, err, out)
	}
	if again := scan(home, self(), "foci"); len(again) != 0 {
		t.Fatalf("after running the fix, scan = %q, want none", again)
	}
}

func TestAssess(t *testing.T) {
	const p = "/h/it's.json"
	cases := []struct {
		name    string
		st      fileState
		wantFix string // "" = protected
	}{
		{"owner only", fileState{uid: 7, perm: 0o600}, ""},
		{"read-only owner", fileState{uid: 7, perm: 0o400}, ""},
		{"group read", fileState{uid: 7, perm: 0o640}, `chmod 600 '/h/it'\''s.json'`},
		{"world read", fileState{uid: 7, perm: 0o604}, `chmod 600 '/h/it'\''s.json'`},
		{"extended ACL", fileState{uid: 7, perm: 0o600, hasACL: true}, `setfacl -b '/h/it'\''s.json' && chmod 600 '/h/it'\''s.json'`},
		{"foreign owner", fileState{uid: 0, perm: 0o600}, `sudo chown foci '/h/it'\''s.json' && chmod 600 '/h/it'\''s.json'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			problem, fix := assess(p, c.st, 7, "foci")
			if fix != c.wantFix {
				t.Fatalf("fix = %q, want %q", fix, c.wantFix)
			}
			if (problem == "") != (c.wantFix == "") {
				t.Fatalf("problem = %q inconsistent with fix %q", problem, fix)
			}
		})
	}
}

// Files and the Makefile's BACKEND_CRED_FILES must agree, or the installer
// protects a different set of files than startup checks.
func TestFilesMatchMakefile(t *testing.T) {
	f, err := os.Open("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "BACKEND_CRED_FILES") {
			continue
		}
		_, val, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparseable Makefile line %q", line)
		}
		got := strings.Join(strings.Fields(val), " ")
		if want := strings.Join(Files, " "); got != want {
			t.Fatalf("Makefile BACKEND_CRED_FILES = %q, want %q (credperm.Files)", got, want)
		}
		return
	}
	t.Fatal("BACKEND_CRED_FILES not found in Makefile")
}

// A 0600 mode with an extended ACL entry still leaks: chmod alone does not
// remove a named-user entry. Exercises the real xattr probe in statFile.
func TestScan_ExtendedACLWarnsWithWorkingFix(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not installed")
	}
	home := t.TempDir()
	p := writeCred(t, home, Files[0], 0o600)
	if out, err := exec.Command("setfacl", "-m", "u:root:r", p).CombinedOutput(); err != nil {
		t.Skipf("filesystem does not support ACLs: %v\n%s", err, out)
	}

	got := scan(home, self(), "foci")
	if len(got) != 1 || !strings.Contains(got[0], "ACL") {
		t.Fatalf("scan = %q, want one ACL warning", got)
	}
	_, fix, _ := strings.Cut(got[0], "Fix: ")
	if out, err := exec.Command("/bin/sh", "-c", fix).CombinedOutput(); err != nil {
		t.Fatalf("running fix %q: %v\n%s", fix, err, out)
	}
	if again := scan(home, self(), "foci"); len(again) != 0 {
		t.Fatalf("after running the fix, scan = %q, want none", again)
	}
}
