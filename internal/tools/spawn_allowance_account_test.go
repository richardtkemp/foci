package tools

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// TestSpawnUsageReader_PerAccount (H5, requirement 4): Claude Code agents on
// different accounts never read each other's usage, whichever registered
// first; agents on one account share one cached read.
func TestSpawnUsageReader_PerAccount(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"alpha-first", "beta-first"} {
		t.Run(order, func(t *testing.T) {
			acctA := "test-3104-" + order + ":file:/host/.claude/.credentials.json"
			acctB := "test-3104-" + order + ":file:/tmp/ccB/.credentials.json"
			usage := map[string]int{"": 10, "/tmp/ccB": 99}
			var mu sync.Mutex
			reads := map[string]int{}
			q := func(_ context.Context, tg delegator.UsageTarget) (*delegator.UsageInfo, error) {
				dir := tg.Env["CLAUDE_CONFIG_DIR"]
				mu.Lock()
				reads[dir]++
				mu.Unlock()
				return &delegator.UsageInfo{Backend: "Claude Code", RateLimitsAvailable: true,
					FiveHour: delegator.UsageWindow{Percent: usage[dir], Reported: true}}, nil
			}
			envB := map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/ccB"}
			register := map[string]func() UsageReader{
				"alpha": func() UsageReader {
					return SpawnUsageReader("claude-code", delegator.UsageTarget{AgentID: "alpha"}, q, acctA, nil)
				},
				"beta": func() UsageReader {
					return SpawnUsageReader("claude-code", delegator.UsageTarget{AgentID: "beta", Env: envB}, q, acctB, nil)
				},
			}
			first, second := "alpha", "beta"
			if order == "beta-first" {
				first, second = second, first
			}
			readers := map[string]UsageReader{first: register[first](), second: register[second]()}
			beta2 := SpawnUsageReader("claude-code", delegator.UsageTarget{AgentID: "beta2", Env: envB}, q, acctB, nil)

			pct := func(r UsageReader) string {
				got := r(context.Background())
				if got.Err != nil || got.Info == nil {
					return fmt.Sprintf("err=%v pending=%v", got.Err, got.Pending)
				}
				return fmt.Sprint(got.Info.FiveHour.Percent)
			}
			if got := pct(readers["beta"]); got != "99" {
				t.Errorf("beta's reserve check read %s%%, want B's 99%%", got)
			}
			if got := pct(readers["alpha"]); got != "10" {
				t.Errorf("alpha's reserve check read %s%%, want A's 10%%", got)
			}
			if got := pct(beta2); got != "99" {
				t.Errorf("beta2's reserve check read %s%%, want B's 99%%", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if reads["/tmp/ccB"] != 1 || reads[""] != 1 {
				t.Errorf("reads per account = %v, want one for B (beta and beta2 share it) and one for A", reads)
			}
		})
	}
}
