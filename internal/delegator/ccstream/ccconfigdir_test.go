package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ccProjectsRel is CC's transcript dir relative to HOME on a single-home
// install (no FOCI_HOME, no CLAUDE_CONFIG_DIR): ~/.claude/projects.
const ccProjectsRel = ".claude/projects"

// #2580: when foci runs with a separate install root, CC runs with
// CLAUDE_CONFIG_DIR=<install root>/.claude and writes its transcripts there.
// Every reader foci has of CC's on-disk state must look in that folder, not
// in $HOME/.claude: the transcript path (cost booking, session sweeps), the
// subagent transcripts, forks and cleanup, and the retention setting.
func TestCCStateFollowsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	ccDir := filepath.Join(t.TempDir(), "foci", ".claude")
	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", ccDir)

	const workDir = "/Users/u/foci/cam"
	slugDir := filepath.Join(ccDir, "projects", projectSlug(workDir))

	b := &Backend{sessionID: "sess-1", workDir: workDir}
	if got, want := b.SessionFilePath(), filepath.Join(slugDir, "sess-1.jsonl"); got != want {
		t.Errorf("SessionFilePath = %q, want %q", got, want)
	}
	if got, err := ccTranscriptPath(workDir, "sess-1"); err != nil || got != filepath.Join(slugDir, "sess-1.jsonl") {
		t.Errorf("ccTranscriptPath = %q, %v", got, err)
	}
	if got, want := b.subagentFilePath("a1", ".jsonl"), filepath.Join(slugDir, "sess-1", "subagents", "agent-a1.jsonl"); got != want {
		t.Errorf("subagentFilePath = %q, want %q", got, want)
	}

	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(slugDir, "parent.jsonl")
	if err := os.WriteFile(parent, []byte(`{"sessionId":"parent","type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "parent", WorkDir: workDir})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	forked := filepath.Join(slugDir, res.SessionID+".jsonl")
	if _, err := os.Stat(forked); err != nil {
		t.Fatalf("fork not written under CLAUDE_CONFIG_DIR: %v", err)
	}
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: res.SessionID, WorkDir: workDir}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if _, err := os.Stat(forked); !os.IsNotExist(err) {
		t.Errorf("CleanupSession left %s (stat err %v)", forked, err)
	}

	if err := os.WriteFile(filepath.Join(ccDir, "settings.json"), []byte(`{"cleanupPeriodDays": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CleanupPeriod(); got != 7*24*time.Hour {
		t.Errorf("CleanupPeriod = %v, want 7 days from CLAUDE_CONFIG_DIR/settings.json", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("something wrote under $HOME/.claude: %v", err)
	}
}
