package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/delegator/pretool"
)

// TestLivePreToolRules proves a pretool rule edit reaches the next CC launch
// without a restart (#2033): each call re-reads the file, and a file that no
// longer loads keeps the last good rules instead of dropping them.
func TestLivePreToolRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	write := func(body string) {
		t.Helper()
		cfg := "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n[[agents]]\nid = \"a\"\nbackend = \"claude-code\"\n" + body
		if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names := func(rules []pretool.Rule) (out []string) {
		for _, r := range rules {
			out = append(out, r.Name)
		}
		return out
	}
	has := func(rules []pretool.Rule, name string) bool {
		for _, n := range names(rules) {
			if n == name {
				return true
			}
		}
		return false
	}

	write("")
	source := livePreToolRules("a", path, resolvePreToolRules("a", nil, nil))
	if got := source(); has(got, "no_add_all") || len(got) != len(pretool.Defaults) {
		t.Fatalf("initial rules = %v", names(got))
	}

	write(`[[agents.backend_config.pretool_rules]]
name = "no_add_all"
tool = "Bash"
command = 'git add -A'
reason = "stage paths"
`)
	if got := source(); !has(got, "no_add_all") {
		t.Fatalf("edit not picked up: %v", names(got))
	}

	write("this is = = not toml")
	if got := source(); !has(got, "no_add_all") {
		t.Errorf("broken file dropped the last good rules: %v", names(got))
	}
}
