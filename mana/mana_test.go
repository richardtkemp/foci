package mana

import (
	"strings"
	"testing"
	"time"

	"foci/anthropic"
)

func TestIsGood_InCredit(t *testing.T) {
	now := time.Now()
	// 2.5 hours into window, 50% mana remaining
	// expected_mana = 100 * (5h - 2.5h) / (5h - 30m) = 100 * 2.5h / 4.5h ≈ 55.6%
	// actual 50% < expected 55.6% → NOT good
	resetsAt := now.Add(2*time.Hour + 30*time.Minute) // 2.5h remaining → 2.5h since reset
	if IsGood(50, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good at 50% with 2.5h elapsed (below expected ~55.6%)")
	}

	// Same point in time but 70% mana — above the line
	if !IsGood(70, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good at 70% with 2.5h elapsed (above expected ~55.6%)")
	}
}

func TestIsGood_InvestPeriod(t *testing.T) {
	now := time.Now()
	// 10 minutes into window (within 30m invest interval)
	resetsAt := now.Add(4*time.Hour + 50*time.Minute)
	if IsGood(95, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good during invest period, even with 95% mana")
	}
}

func TestIsGood_NearReset(t *testing.T) {
	now := time.Now()
	// 2 minutes to reset, 5% mana
	// time_since_reset = 4h58m
	// expected_mana = 100 * (5h - 4h58m) / (5h - 30m) = 100 * 2m / 270m ≈ 0.74%
	// 5% > 0.74% → good
	resetsAt := now.Add(2 * time.Minute)
	if !IsGood(5, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good near reset (5% > expected ~0.74%)")
	}
}

func TestIsGood_JustAfterInvest(t *testing.T) {
	now := time.Now()
	// Exactly at invest interval boundary (30m into window)
	resetsAt := now.Add(4*time.Hour + 30*time.Minute)
	// expected_mana = 100 * (5h - 30m) / (5h - 30m) = 100%
	// Need > 100% which is impossible, so this should be false
	if IsGood(99, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good right at invest boundary (99% < expected 100%)")
	}

	// Slightly past invest (31m in)
	resetsAt = now.Add(4*time.Hour + 29*time.Minute) // 31m since reset
	// expected_mana = 100 * (5h - 31m) / (5h - 30m) = 100 * 269m / 270m ≈ 99.6%
	// 99% < 99.6% → not good, but 100% would be good
	if IsGood(99, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good at 99% just past invest (below expected ~99.6%)")
	}
	if !IsGood(100, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good at 100% just past invest (above expected ~99.6%)")
	}
}

func TestIsGood_ZeroReset(t *testing.T) {
	// No reset time = don't spend (no data = deny)
	if IsGood(50, time.Time{}, 30*time.Minute, time.Now()) {
		t.Error("expected mana NOT good when reset time is zero (no data)")
	}
}

func TestIsGood_StalenessGuard(t *testing.T) {
	// IsGood with zero reset → false (no data = don't spend)
	if IsGood(50, time.Time{}, 30*time.Minute, time.Now()) {
		t.Error("IsGood should return false for zero reset time")
	}

	// IsGood with valid reset and good mana → true
	now := time.Now()
	resetsAt := now.Add(2 * time.Minute) // near end of window
	if !IsGood(50, resetsAt, 30*time.Minute, now) {
		t.Error("IsGood should return true with valid data near end of window")
	}
}

func TestIsGood_MidWindow(t *testing.T) {
	now := time.Now()
	// Exactly halfway through window: 2.5h since reset, 2.5h to go
	// expected = 100 * (5h - 2.5h) / (5h - 30m) = 100 * 2.5h / 4.5h ≈ 55.6%
	resetsAt := now.Add(2*time.Hour + 30*time.Minute)

	// 60% > 55.6% → good
	if !IsGood(60, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good at 60% midway (above expected ~55.6%)")
	}

	// 40% < 55.6% → not good
	if IsGood(40, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good at 40% midway (below expected ~55.6%)")
	}
}

func TestIsGood_PastReset(t *testing.T) {
	now := time.Now()
	// Reset was 5 minutes ago (past)
	resetsAt := now.Add(-5 * time.Minute)
	// time_since_reset = 5h + 5m, clamped: expected ≈ negative → any mana is good
	if !IsGood(1, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good when past reset time")
	}
}

func TestWindow(t *testing.T) {
	if Window != 5*time.Hour {
		t.Errorf("Window = %v, want 5h", Window)
	}
}

func TestFromUtilization(t *testing.T) {
	tests := []struct {
		util float64
		want float64
	}{
		{0, 100},
		{50, 50},
		{100, 0},
		{110, 0},  // clamped
		{25.5, 74.5},
	}
	for _, tt := range tests {
		got := FromUtilization(tt.util)
		if got != tt.want {
			t.Errorf("FromUtilization(%v) = %v, want %v", tt.util, got, tt.want)
		}
	}
}

func TestFormatPercentNil(t *testing.T) {
	if got := FormatPercent(nil); got != "" {
		t.Errorf("FormatPercent(nil) = %q, want empty", got)
	}
}

func TestFormatPercentNoFiveHour(t *testing.T) {
	if got := FormatPercent(&anthropic.UsageResponse{}); got != "" {
		t.Errorf("FormatPercent(empty) = %q, want empty", got)
	}
}

func TestFormatPercentValues(t *testing.T) {
	tests := []struct {
		util float64
		want string
	}{
		{0, "100%"},
		{25, "75%"},
		{50, "50%"},
		{99.5, "0.5%"},
		{100, "0.0%"},
		{110, "0.0%"}, // clamped to 0
	}
	for _, tt := range tests {
		util := tt.util
		got := FormatPercent(&anthropic.UsageResponse{
			FiveHour: &anthropic.UsageWindow{Utilization: &util},
		})
		if got != tt.want {
			t.Errorf("FormatPercent(util=%.1f) = %q, want %q", tt.util, got, tt.want)
		}
	}
}

func TestFormatResetNil(t *testing.T) {
	if got := FormatReset(nil); got != "" {
		t.Errorf("FormatReset(nil) = %q, want empty", got)
	}
}

func TestFormatResetNoFiveHour(t *testing.T) {
	if got := FormatReset(&anthropic.UsageResponse{}); got != "" {
		t.Errorf("FormatReset(empty) = %q, want empty", got)
	}
}

func TestFormatResetNoResetsAt(t *testing.T) {
	util := 50.0
	if got := FormatReset(&anthropic.UsageResponse{FiveHour: &anthropic.UsageWindow{Utilization: &util}}); got != "" {
		t.Errorf("FormatReset(no ResetsAt) = %q, want empty", got)
	}
}

func TestFormatResetWithTime(t *testing.T) {
	util := 50.0
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	got := FormatReset(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
	})
	if !strings.HasPrefix(got, "in ") || !strings.Contains(got, "h") {
		t.Errorf("FormatReset(2h) = %q, want 'in Xh' or 'in Xh Ym'", got)
	}
}

func TestFormatResetPast(t *testing.T) {
	util := 50.0
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)
	got := FormatReset(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{
			Utilization: &util,
			ResetsAt:    &past,
		},
	})
	if got != "now" {
		t.Errorf("FormatReset(past) = %q, want %q", got, "now")
	}
}

func TestFormatResetMinutes(t *testing.T) {
	util := 50.0
	future := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339Nano)
	got := FormatReset(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
	})
	if !strings.HasPrefix(got, "in ") || !strings.HasSuffix(got, "m") {
		t.Errorf("FormatReset(45m) = %q, want 'in Xm'", got)
	}
}

func TestParseResetTimePast(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(past)
	if result != "now" {
		t.Errorf("ParseResetTime(past) = %q, want %q", result, "now")
	}
}

func TestParseResetTimeLessThanMinute(t *testing.T) {
	soon := time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(soon)
	if result != "in <1m" {
		t.Errorf("ParseResetTime(30s) = %q, want %q", result, "in <1m")
	}
}

func TestParseResetTimeMinutes(t *testing.T) {
	future := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(future)
	if !strings.HasPrefix(result, "in ") || !strings.HasSuffix(result, "m") {
		t.Errorf("ParseResetTime(45m) = %q, want 'in Xm'", result)
	}
}

func TestParseResetTimeHours(t *testing.T) {
	future := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(future)
	if !strings.HasPrefix(result, "in ") || !strings.Contains(result, "h") {
		t.Errorf("ParseResetTime(3h) = %q, want 'in Xh' or 'in Xh Ym'", result)
	}
}

func TestParseResetTimeMoreThan24h(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(future)
	if strings.HasPrefix(result, "in ") {
		t.Errorf("ParseResetTime(48h) = %q, should not be relative", result)
	}
	if result == "" {
		t.Error("ParseResetTime(48h) returned empty string")
	}
}

func TestParseResetTimeInvalid(t *testing.T) {
	result := ParseResetTime("not-a-timestamp")
	if result != "" {
		t.Errorf("ParseResetTime(invalid) = %q, want empty", result)
	}
}

func TestFormatUsageNil(t *testing.T) {
	result := FormatUsage(nil)
	if result != "No usage data" {
		t.Errorf("FormatUsage(nil) = %q", result)
	}
}

func TestFormatUsageEmpty(t *testing.T) {
	result := FormatUsage(&anthropic.UsageResponse{})
	if result != "No active usage limits" {
		t.Errorf("FormatUsage(empty) = %q", result)
	}
}

func TestFormatUsagePercentage(t *testing.T) {
	util := 42.0
	result := FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{Utilization: &util},
	})
	if !strings.Contains(result, "42% used") {
		t.Errorf("result = %q, want '42%% used'", result)
	}

	util = 0.3
	result = FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{Utilization: &util},
	})
	if !strings.Contains(result, "0.3% used") {
		t.Errorf("result = %q, want '0.3%% used'", result)
	}
}

func TestFormatUsageResetTime(t *testing.T) {
	util := 50.0
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
	})
	if !strings.Contains(result, "resets") {
		t.Errorf("result = %q, want 'resets'", result)
	}
}

func TestFormatUsageOverage(t *testing.T) {
	util := 80.0
	result := FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{Utilization: &util},
		ExtraUsage: &anthropic.ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 1.50,
		},
	})
	if !strings.Contains(result, "overage $1.50") {
		t.Errorf("result = %q, want 'overage $1.50'", result)
	}
}

func TestFormatUsageOverageDisabled(t *testing.T) {
	util := 80.0
	result := FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{Utilization: &util},
		ExtraUsage: &anthropic.ExtraUsage{
			IsEnabled:   false,
			UsedCredits: 5.0,
		},
	})
	if strings.Contains(result, "overage") {
		t.Errorf("result = %q, should not show overage when disabled", result)
	}
}

func TestFormatUsageOverageZero(t *testing.T) {
	util := 80.0
	result := FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{Utilization: &util},
		ExtraUsage: &anthropic.ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 0.0,
		},
	})
	if strings.Contains(result, "overage") {
		t.Errorf("result = %q, should not show overage when zero", result)
	}
}

func TestFormatUsageAllFields(t *testing.T) {
	util := 75.0
	future := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	result := FormatUsage(&anthropic.UsageResponse{
		FiveHour: &anthropic.UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
		ExtraUsage: &anthropic.ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 2.75,
		},
	})
	if !strings.Contains(result, "75% used") {
		t.Errorf("result missing utilization: %q", result)
	}
	if !strings.Contains(result, "resets") {
		t.Errorf("result missing reset time: %q", result)
	}
	if !strings.Contains(result, "overage $2.75") {
		t.Errorf("result missing overage: %q", result)
	}
}
