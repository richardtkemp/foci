package autoapprove

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A path rule written through a symlinked directory must match a file under
// it. The candidate path is resolved through symlinks before matching, so the
// rule's literal prefix must be too. This is the everyday case on macOS, where
// /tmp and /home are symlinks; the test builds its own symlink so it runs the
// same on Linux.
func TestPathRuleThroughSymlinkedDirMatches(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rules := parseAutoApproveRules([]string{"Edit:" + link + "/*"})
	for _, path := range []string{filepath.Join(link, "f.go"), filepath.Join(real, "f.go"), filepath.Join(link, "new.go")} {
		in, _ := json.Marshal(map[string]string{"file_path": path})
		if !matchAutoApprove(rules, "Edit", in) {
			t.Errorf("rule %q did not match %q (rule pattern after parse: %q)", "Edit:"+link+"/*", path, rules[0].Pattern)
		}
	}
	// The glob tail is kept as written, and a path outside still does not match.
	outside, _ := json.Marshal(map[string]string{"file_path": filepath.Join(t.TempDir(), "x.go")})
	if matchAutoApprove(rules, "Edit", outside) {
		t.Error("rule matched a path outside the symlinked dir")
	}
}

func TestCanonicalPathPatternLeavesNonPathsAlone(t *testing.T) {
	for _, p := range []string{"relative/*", "*.go", "/**"} {
		if got := canonicalPathPattern(p); got != p {
			t.Errorf("canonicalPathPattern(%q) = %q, want unchanged", p, got)
		}
	}
	// Bash patterns are never path-canonicalised.
	if r := parseAutoApproveRule("Bash:ls /tmp/*"); r.Pattern != "ls /tmp/*" {
		t.Errorf("Bash pattern changed: %q", r.Pattern)
	}
}
