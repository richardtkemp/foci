package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/account"
	"foci/internal/delegator"
	"foci/internal/session"
)

// acctMock is a backend whose files are in the folder of the account it runs
// on, as Claude Code's are (<config folder>/projects/...).
type acctMock struct {
	*mockBackendDM
	acct delegator.AccountSpec
}

func (m *acctMock) SetAccount(a delegator.AccountSpec) { m.acct = a }

func (m *acctMock) SessionFilePath() string {
	return filepath.Join(m.acct.Dir, "projects", "sid.jsonl")
}

// TestSessionAccountSurvivesRestart_PruneKeepsRow (#3186 P2/P3, the #3104 F1
// risk): a session created on an account that is not the pool's first is
// still on that account after a restart (a new manager and pool over the same
// state.db): its backend resolves its transcript in that account's folder, the
// index row names the file that exists, and startup's PruneOrphans keeps the
// row. Resolved through the first account instead, the backend would name a
// file in another folder, and the session would lose its transcript.
func TestSessionAccountSurvivesRestart_PruneKeepsRow(t *testing.T) {
	t.Parallel()
	idx := newTestSessionIndex(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	mk := func(key, dir string) *account.Account {
		return account.New(account.Options{Backend: "claude-code", Spec: delegator.AccountSpec{
			Key: key, Dir: dir, Env: map[string]string{"CLAUDE_CONFIG_DIR": dir},
		}})
	}
	const key = "test-agent/c1"
	transcript := filepath.Join(dirB, "projects", "sid.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The session was created on account b (stored with it).
	if err := idx.SetSessionMetadata(key, session.MetaKeyAccount, "b"); err != nil {
		t.Fatal(err)
	}

	// The restart: a new manager whose pool lists a first.
	mgr, _ := newTestManager(t, idx)
	mgr.NewBackend = func() (delegator.Delegator, error) {
		return &acctMock{mockBackendDM: &mockBackendDM{running: true}}, nil
	}
	mgr.Accounts = account.NewPool(idx, mk("a", dirA), mk("b", dirB))
	a := &Agent{AgentID: "test-agent", SessionIndex: idx, DelegatedManager: mgr}

	if got := a.AccountFor(key).Key(); got != "b" {
		t.Fatalf("session on account %q after the restart, want b", got)
	}
	if env := mgr.LaunchEnv(key); env["CLAUDE_CONFIG_DIR"] != dirB {
		t.Errorf("launch env %v, want account b's folder", env)
	}
	if _, err := mgr.Get(context.Background(), key); err != nil {
		t.Fatalf("Get: %v", err)
	}
	a.recordTurnActivity(recordTurnActivityTS(key, "user"))
	e, err := idx.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if e.FilePath != transcript || !strings.HasPrefix(e.FilePath, dirB) {
		t.Fatalf("row names %q, want the transcript in account b's folder %q", e.FilePath, transcript)
	}
	if n := idx.PruneOrphans(); n != 0 {
		t.Errorf("sweep pruned %d rows; the session's transcript exists", n)
	}
	if _, err := idx.Get(key); err != nil {
		t.Errorf("row gone after the sweep: %v", err)
	}
}
