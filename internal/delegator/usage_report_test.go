package delegator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// specFakeUsage implements UsageReporter, as a usage-capable backend must.
type specFakeUsage struct{ Delegator }

func (*specFakeUsage) SetUsageSink(UsageSink) {}

func usageSpec() Spec {
	s := validSpec()
	s.UsageQuery = func(context.Context, UsageTarget) (*UsageInfo, error) { return nil, nil }
	s.Caps[CapUsageQuery] = Yes()
	return s
}

// TestSpecValidate_UsageNeedsReports is #3103 requirement 8 (H9): a backend
// that declares usage support must hand its usage figures to the store
// through the interface. Declaring usage_query without usage_reports fails,
// declaring usage_reports without implementing UsageReporter fails, and a
// backend that does both passes. A backend with no usage query need not.
func TestSpecValidate_UsageNeedsReports(t *testing.T) {
	s := usageSpec()
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "usage_query, which requires usage_reports") {
		t.Errorf("usage without reports: Validate() = %v, want the requires error", err)
	}

	s = usageSpec()
	s.Caps[CapUsageReports] = Yes()
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "declares usage_reports but") {
		t.Errorf("reports without the interface: Validate() = %v, want the interface error", err)
	}

	s = usageSpec()
	s.Caps[CapUsageReports] = Yes()
	s.Prototype = (*specFakeUsage)(nil)
	if err := s.Validate(); err != nil {
		t.Errorf("a usage backend with reports: Validate() = %v, want nil", err)
	}

	if err := validSpec().Validate(); err != nil {
		t.Errorf("a backend with no usage query: Validate() = %v, want nil", err)
	}
}

func TestUsageWindowLength(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"five_hour": 5 * time.Hour, "seven_day": 7 * 24 * time.Hour, "seven_day_opus": 7 * 24 * time.Hour,
		"codex/5h": 5 * time.Hour, "premium/7d": 7 * 24 * time.Hour, "codex/90m": 90 * time.Minute,
		"overage": 0, "premium/limit": 0, "codex/window": 0,
	} {
		if got := UsageWindowLength(in); got != want {
			t.Errorf("UsageWindowLength(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestReportWindows: a Claude Code read reports the windows it carried; a
// codex allowance reports each bucket's windows by "<bucket>/<length>".
func TestReportWindows(t *testing.T) {
	r := time.Unix(1794610800, 0)
	cc := (&UsageInfo{FiveHour: UsageWindow{Percent: 30, ResetsAt: r, Reported: true}}).ReportWindows()
	if len(cc) != 1 || cc[0].Window != "five_hour" || *cc[0].Used != 30 || cc[0].UsedUnit != UsageUnitPercent || !cc[0].ResetsAt.Equal(r) {
		t.Errorf("CC windows = %+v", cc)
	}
	cx := (&UsageInfo{Allowance: &Allowance{Buckets: []AllowanceBucket{
		{Windows: []AllowanceWindow{{Label: "5h", Percent: 34}, {Label: "7d", Percent: 12}}},
		{Name: "premium", LimitReached: "workspace out of credits"},
	}}}).ReportWindows()
	if len(cx) != 3 || cx[0].Window != "codex/5h" || cx[0].Length != 5*time.Hour || cx[1].Window != "codex/7d" || cx[2].Window != "premium/limit" || !cx[2].LimitReached {
		t.Errorf("codex windows = %+v", cx)
	}
}
