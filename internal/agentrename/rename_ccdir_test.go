package agentrename

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/fociroot"
)

// TestRenameMoveWorkspace_AgentsOwnCCFolder (#3104 H7): an agent whose
// backend_config.env sets its own CLAUDE_CONFIG_DIR has its CC project folder
// moved inside THAT folder's projects/ when --move-workspace changes the
// workspace slug, and the host folder is not touched. (TestRenameMoveWorkspace
// is the host-folder arm: an agent with no override still moves within the
// host folder.)
func TestRenameMoveWorkspace_AgentsOwnCCFolder(t *testing.T) {
	f := newFixture(t)
	ccB := filepath.Join(f.root, "ccB")
	cfg, err := os.ReadFile(f.cfgPath)
	must(t, err)
	const anchor = "name = \"Bob\"\n"
	if !strings.Contains(string(cfg), anchor) {
		t.Fatalf("fixture config has no %q", anchor)
	}
	withEnv := strings.Replace(string(cfg), anchor, anchor+"\n[agents.backend_config.env]\nCLAUDE_CONFIG_DIR = \""+ccB+"\"\n", 1)
	writeFile(t, f.cfgPath, []byte(withEnv))

	oldWS, newWS := filepath.Join(f.root, "bob"), filepath.Join(f.root, "rob")
	ownOld := filepath.Join(ccB, "projects", fociroot.CCProjectSlug(oldWS), "u2.jsonl")
	ownNew := filepath.Join(ccB, "projects", fociroot.CCProjectSlug(newWS), "u2.jsonl")
	writeFile(t, ownOld, []byte(`{"cwd":"x"}`+"\n"))
	hostBob := filepath.Join(f.root, ".claude", "projects", fociroot.CCProjectSlug(oldWS), "u1.jsonl")
	hostBefore := hashTree(t, filepath.Join(f.root, ".claude"))

	out, err := f.run(Options{Move: true})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	if _, err := os.Stat(ownNew); err != nil {
		t.Errorf("bob's CC project folder not moved inside its own folder: %v\n%s", err, out)
	}
	if _, err := os.Stat(ownOld); !os.IsNotExist(err) {
		t.Errorf("old project folder still in bob's own folder (stat err %v)", err)
	}
	if _, err := os.Stat(hostBob); err != nil {
		t.Errorf("the host folder's transcript moved: %v", err)
	}
	diffHashes(t, hostBefore, hashTree(t, filepath.Join(f.root, ".claude")))
}
