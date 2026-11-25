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
// Code login with the same figures and calls has no pace: its calls cannot be
// told from another login's.
func TestUsagePaceFromLedger(t *testing.T) {
	for _, c := range []struct {
		backend, ledger, window string
		want                    bool
	}{
		{"codex", accounting.BackendCodex, "codex/7d", true},
		{"claude-code", accounting.BackendCCStream, "seven_day", false},
	} {
		t.Run(c.backend, func(t *testing.T) {
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
			spendAt := func(at time.Time) {
				n++
				turn := accounting.Turn{TurnID: "T", Session: "cam/c1", AgentID: "cam", Backend: c.ledger,
					Source: accounting.SourceUser, StartedAt: now.Add(-20 * time.Hour)}
				if _, err := ledger.RecordCall(turn, accounting.Call{
					Key: fmt.Sprint("call", n), Backend: c.ledger, Model: "claude-opus-5", Session: "cam/c1", AgentID: "cam",
					TurnID: "T", Kind: accounting.KindCall, Finality: accounting.FinalityCompleted,
					ClassMethod: accounting.ClassMethodObserved, BilledAt: at,
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
			// At 99% too: that rule is for a login with dollars only.
			figure(now, 99)
			if got := account.NewPool(nil, accts...).OverPace(now); got != c.want {
				t.Errorf("at 99%%: over pace = %v, want %v", got, c.want)
			}
		})
	}
}
