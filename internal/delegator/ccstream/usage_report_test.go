package ccstream

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestRateLimitEvent_EveryTypeReachesUsageSink is #3103 requirement 6 (H7)
// for Claude Code: every rate_limit_event, a five_hour one and one of a type
// other than five_hour/seven_day, reaches the usage sink with its window,
// utilization as a fraction, reset, status and the line as received. An
// "allowed" event with no utilization is reported too, with no use.
func TestRateLimitEvent_EveryTypeReachesUsageSink(t *testing.T) {
	lines := []string{
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"five_hour","utilization":0.42,"resetsAt":1794610800,"surpassedThreshold":0.4}}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day_opus","utilization":1,"resetsAt":1794862800,"overageStatus":"rejected"}}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","resetsAt":1794610800}}`,
	}
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	var got []delegator.UsageReport
	b.SetUsageSink(func(r delegator.UsageReport) { got = append(got, r) })
	NewReader(strings.NewReader(strings.Join(lines, "\n")+"\n"), b).Run(context.Background())

	if len(got) != 3 {
		t.Fatalf("sink got %d reports, want 3: %+v", len(got), got)
	}
	for i, r := range got {
		if r.Source != delegator.UsageSourceCCEvent {
			t.Errorf("report %d source = %q", i, r.Source)
		}
		if string(r.Payload) != lines[i] {
			t.Errorf("report %d payload = %s, want the line as received", i, r.Payload)
		}
		if len(r.Windows) != 1 {
			t.Fatalf("report %d windows = %+v", i, r.Windows)
		}
	}
	w := got[0].Windows[0]
	if w.Window != "five_hour" || w.Length != 5*time.Hour || w.Used == nil || *w.Used != 0.42 || w.UsedUnit != delegator.UsageUnitFraction ||
		!w.ResetsAt.Equal(time.Unix(1794610800, 0)) || w.Status != "allowed_warning" || w.LimitReached {
		t.Errorf("five_hour window = %+v", w)
	}
	w = got[1].Windows[0]
	if w.Window != "seven_day_opus" || w.Length != 7*24*time.Hour || !w.LimitReached || w.Status != "rejected" {
		t.Errorf("seven_day_opus window = %+v", w)
	}
	if w := got[2].Windows[0]; w.Used != nil || w.Status != "allowed" {
		t.Errorf("allowed window = %+v, want no use", w)
	}
}

// TestBackendDeclaresUsageReports (#3103 requirement 8): the Claude Code
// backend hands its usage figures to the host through the interface.
func TestBackendDeclaresUsageReports(t *testing.T) {
	if _, ok := any((*Backend)(nil)).(delegator.UsageReporter); !ok {
		t.Fatal("*Backend does not implement delegator.UsageReporter")
	}
	if !spec.Supports(delegator.CapUsageReports) {
		t.Fatal("the claude-code Spec does not declare usage_reports")
	}
}
