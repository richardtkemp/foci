package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/account"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/usagelog"

	_ "foci/internal/delegator/ccstream"
	_ "foci/internal/delegator/codex"
)

// TestUsagePaceFromLedger (#3269): the one codex login, which reports a 7d
// window only, goes over pace on the codex calls in the cost ledger. A Claude
// Code login has the calls whose transcript is in its config folder (#3291):
// with the same figures and calls in its folder it goes over pace too, and
// with calls in no folder it has no pace. At 99% used every login is over.
func TestUsagePaceFromLedger(t *testing.T) {
	for _, c := range []struct {
		name, backend, ledger, window string
		inFolder                      bool // the calls' transcript is in the login's folder
		want                          bool
	}{
		{"codex", "codex", accounting.BackendCodex, "codex/7d", false, true},
		{"claude-code in its folder", "claude-code", accounting.BackendCCStream, "seven_day", true, true},
		{"claude-code in no folder", "claude-code", accounting.BackendCCStream, "seven_day", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolateHostCC(t)
			spec, ok := delegator.SpecFor(c.backend)
			if !ok {
				t.Fatalf("no backend %q", c.backend)
			}
			st, err := usagelog.Open(filepath.Join(t.TempDir(), usagelog.FileName))
			if err != nil {
				t.Fatal(err)
			}
			setUsageHistory(st)
			ledger, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
			if err != nil {
				t.Fatal(err)
			}
			accounting.SetLive(ledger)
			t.Cleanup(func() {
				accounting.SetLive(nil)
				_ = ledger.Close()
				setUsageHistory(nil)
				_ = st.Close()
			})
			accts := agentAccounts(&config.Config{DataDir: t.TempDir()}, spec, "cam", nil)
			now := time.Now()
			n := 0
			file := ""
			if c.inFolder {
				if accts[0].Dir() == "" {
					t.Fatal("the Claude Code login has no folder")
				}
				file = filepath.Join(accts[0].Dir(), "projects", "p", "s.jsonl")
			}
			spendAt := func(at time.Time) {
				n++
				turn := accounting.Turn{TurnID: "T", Session: "cam/c1", AgentID: "cam", Backend: c.ledger,
					Source: accounting.SourceUser, StartedAt: now.Add(-20 * time.Hour)}
				if _, err := ledger.RecordCall(turn, accounting.Call{
					Key: fmt.Sprint("call", n), Backend: c.ledger, Model: "claude-opus-5", Session: "cam/c1", AgentID: "cam",
					TurnID: "T", Kind: accounting.KindCall, Finality: accounting.FinalityCompleted,
					ClassMethod: accounting.ClassMethodObserved, BilledAt: at, SessionFile: file,
					Tokens: modelinfo.Tokens{modelinfo.ClassInput: 100000},
				}, nil); err != nil {
					t.Fatal(err)
				}
			}
			figure := func(at time.Time, pct float64) {
				if err := st.Save(delegator.UsageReport{Source: "test", ReceivedAt: at, Backend: c.backend, Account: accts[0].Key(),
					AgentID: "cam", Windows: []delegator.UsageReportWindow{pctW(c.window, pct, now.Add(60*time.Hour))}}); err != nil {
					t.Fatal(err)
				}
			}
			// One call for each 1%, an hour apart; then 30 calls in the last hour.
			for i := range 10 {
				at := now.Add(time.Duration(i-15) * time.Hour)
				spendAt(at)
				figure(at.Add(time.Minute), float64(40+i))
			}
			for i := range 30 {
				spendAt(now.Add(-time.Duration(i+1) * time.Minute))
			}
			figure(now.Add(-time.Second), 79)
			if got := account.NewPool(nil, accts...).OverPace(now); got != c.want {
				t.Errorf("over pace = %v, want %v", got, c.want)
			}
			figure(now, 99)
			if got := account.NewPool(nil, accts...).OverPace(now); !got {
				t.Errorf("at 99%%: over pace = %v, want true", got)
			}
		})
	}
}
