package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestUsageReports_UpdateAndStartupRead is #3103 requirement 6 (H7) for
// codex: an account/rateLimits/updated notification and the start-up
// account/rateLimits/read each reach the usage sink once, with their source,
// each bucket's windows (with lengths) as reported, the limit-reached state,
// and the payload as received. A /usage read is not reported by the backend
// (the gateway stores its result), but carries the response in Raw.
func TestUsageReports_UpdateAndStartupRead(t *testing.T) {
	b := newStartableBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		return json.RawMessage(liveRateLimitsRead), nil
	})
	poolAs(t, t.Name(), b)
	isolateRateLimits(t, t.Name())
	var got []delegator.UsageReport
	b.SetUsageSink(func(r delegator.UsageReport) { got = append(got, r) })

	b.seedRateLimits()
	if len(got) != 1 {
		t.Fatalf("start-up read: %d reports, want 1", len(got))
	}
	r := got[0]
	if r.Source != delegator.UsageSourceCodexRead || string(r.Payload) != liveRateLimitsRead {
		t.Errorf("start-up report source %q payload %s", r.Source, r.Payload)
	}
	if len(r.Windows) != 2 {
		t.Fatalf("start-up windows = %+v, want codex/5h and codex/7d", r.Windows)
	}
	w5, w7 := r.Windows[0], r.Windows[1]
	if w5.Window != "codex/5h" || w5.Length != 5*time.Hour || *w5.Used != 100 || w5.UsedUnit != delegator.UsageUnitPercent ||
		!w5.ResetsAt.Equal(time.Unix(reset5h, 0)) || !w5.LimitReached || w5.Status != "workspace_member_credits_depleted" {
		t.Errorf("5h window = %+v", w5)
	}
	if w7.Window != "codex/7d" || w7.Length != 7*24*time.Hour || *w7.Used != 34 {
		t.Errorf("7d window = %+v", w7)
	}

	b.dispatch([]byte(livePremiumDepleted))
	if len(got) != 2 {
		t.Fatalf("after an update: %d reports, want 2", len(got))
	}
	r = got[1]
	if r.Source != delegator.UsageSourceCodexUpdate || !strings.Contains(string(r.Payload), `"limitId":"premium"`) {
		t.Errorf("update report source %q payload %s", r.Source, r.Payload)
	}
	if len(r.Windows) != 1 || r.Windows[0].Window != "premium/limit" || !r.Windows[0].LimitReached {
		t.Errorf("update windows = %+v, want the premium bucket's limit", r.Windows)
	}

	info, err := QueryUsage(context.Background(), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("/usage read reported by the backend: %d reports, want still 2", len(got))
	}
	if string(info.Raw) != liveRateLimitsRead {
		t.Errorf("/usage Raw = %s, want the response", info.Raw)
	}
	// The gateway's report of that read names the same windows.
	names := []string{}
	for _, w := range info.ReportWindows() {
		names = append(names, w.Window)
	}
	if strings.Join(names, ",") != "codex/5h,codex/7d,premium/limit" {
		t.Errorf("/usage report windows = %v", names)
	}
}
