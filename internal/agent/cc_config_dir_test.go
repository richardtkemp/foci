package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestIndexFilePath_KeepsExistingPathUntilNewFileExists (#3104 requirement 6):
// after an agent's CC config folder changes, its backend names a transcript
// path in the new folder before anything is written there. The turn's index
// write keeps the row's existing path (whose file still exists) instead, so
// the startup orphan sweep does not purge the chat; once the new file exists
// it is recorded. With no running backend (a turn's entry after a restart)
// the row keeps its path too. A row whose own file is really gone is still
// pruned.
func TestIndexFilePath_KeepsExistingPathUntilNewFileExists(t *testing.T) {
	t.Parallel()
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)
	a := &Agent{AgentID: "test-agent", SessionIndex: idx, DelegatedManager: mgr}
	const key = "test-agent/c1"
	if err := idx.SetSessionMetadata(key, "cc_resume_id", "old"); err != nil {
		t.Fatal(err)
	}
	idx.RecordBackendResume(key, "old")

	oldDir, newDir := t.TempDir(), t.TempDir()
	oldPath := filepath.Join(oldDir, "old.jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Turn entry with no backend running yet: nothing named, nothing stored.
	a.recordTurnActivity(recordTurnActivityTS(key, "user"))
	if e, err := idx.Get(key); err != nil || e.FilePath != "" {
		t.Fatalf("row %+v, %v; want a row with no path", e, err)
	}

	if _, err := mgr.Get(context.Background(), key); err != nil {
		t.Fatalf("Get: %v", err)
	}
	setPath := func(p string) {
		(*mocks)[0].mu.Lock()
		(*mocks)[0].sessionFilePath = p
		(*mocks)[0].mu.Unlock()
	}
	setPath(oldPath)
	a.recordTurnActivity(recordTurnActivityTS(key, "user"))
	if e, _ := idx.Get(key); e.FilePath != oldPath {
		t.Fatalf("existing file not recorded: %q", e.FilePath)
	}

	// The folder moves: the backend names a path in the new folder that does
	// not exist yet. The row keeps the old path, at a turn's entry and on a
	// mid-turn round alike, and the sweep keeps the row.
	newPath := filepath.Join(newDir, "old.jsonl")
	setPath(newPath)
	a.recordTurnActivity(recordTurnActivityTS(key, "user"))
	a.touchTurnActivity(key, "user")
	if e, _ := idx.Get(key); e.FilePath != oldPath {
		t.Errorf("row names %q, want the existing %q until the new file exists", e.FilePath, oldPath)
	}
	if n := idx.PruneOrphans(); n != 0 {
		t.Errorf("sweep pruned %d rows; the chat's transcript still exists", n)
	}
	if v, _ := idx.GetSessionMetadata(key, "cc_resume_id"); v != "old" {
		t.Errorf("resume id = %q after the sweep, want kept", v)
	}

	// No backend named (a restart): the row keeps its path.
	setPath("")
	a.recordTurnActivity(recordTurnActivityTS(key, "user"))
	if e, _ := idx.Get(key); e.FilePath != oldPath {
		t.Errorf("row names %q after a turn with no named path, want %q kept", e.FilePath, oldPath)
	}

	// Once the backend has written its file, that file is recorded.
	fresh := filepath.Join(newDir, "fresh.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setPath(fresh)
	a.touchTurnActivity(key, "user")
	if e, _ := idx.Get(key); e.FilePath != fresh {
		t.Errorf("row names %q, want the new existing file %q", e.FilePath, fresh)
	}

	// A file that is really deleted is still pruned.
	if err := os.Remove(fresh); err != nil {
		t.Fatal(err)
	}
	if n := idx.PruneOrphans(); n != 1 {
		t.Errorf("sweep pruned %d rows, want 1 for the deleted transcript", n)
	}
}

// TestSpecHooksGetAgentEnv (#3104, F13): the Spec hooks that read a backend's
// on-disk state with no running backend get the agent's backend_config.env,
// which can move that state (CLAUDE_CONFIG_DIR): the transcript check at
// restart (consumedBeforeRestart) and the resume-missed notice's retention.
func TestSpecHooksGetAgentEnv(t *testing.T) {
	t.Parallel()
	mgr, _, _ := newDeliveryManager(t)
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/agent/cc"}
	mgr.StartOpts.Env = env
	var checkedEnv, retentionEnv map[string]string
	mgr.Spec.TranscriptChecker = func(e map[string]string, _, _, _ string) (bool, error) {
		checkedEnv = e
		return true, nil
	}
	mgr.Spec.ResumeRetention = func(e map[string]string) time.Duration {
		retentionEnv = e
		return time.Hour
	}
	if !mgr.consumedBeforeRestart(deliverySK, delegator.PendingInput{ID: "x", WorkDir: "/w", SessionID: "s1"}) {
		t.Fatal("consumedBeforeRestart = false with a checker that finds the input")
	}
	if checkedEnv["CLAUDE_CONFIG_DIR"] != "/agent/cc" {
		t.Errorf("TranscriptChecker got env %v, want the agent's", checkedEnv)
	}
	if got := mgr.resumeRetention(); got != time.Hour || retentionEnv["CLAUDE_CONFIG_DIR"] != "/agent/cc" {
		t.Errorf("resumeRetention = %v with env %v, want the agent's env", got, retentionEnv)
	}
}
