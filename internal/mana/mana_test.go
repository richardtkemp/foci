package mana

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/anthropic"
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

func TestNewMonitor(t *testing.T) {
	// Monitor with nil client
	m := NewMonitor(nil, 5*time.Minute)
	if m == nil {
		t.Fatal("NewMonitor returned nil")
	}
	if m.usageClient != nil {
		t.Error("usageClient should be nil")
	}
	if m.stalenessTimeout != 5*time.Minute {
		t.Errorf("stalenessTimeout = %v, want 5m", m.stalenessTimeout)
	}
}

func TestIsGoodFor_NoClient(t *testing.T) {
	m := NewMonitor(nil, 5*time.Minute)
	// With no client, should always return true
	if !m.IsGoodFor(context.Background(), 30*time.Minute) {
		t.Error("IsGoodFor should return true with no client")
	}
}

func TestParseResetTime_ManyHours(t *testing.T) {
	future := time.Now().Add(5*time.Hour + 30*time.Minute).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(future)
	if !strings.Contains(result, "5h") && !strings.Contains(result, "4h") {
		t.Errorf("ParseResetTime(5h30m) = %q, should contain hours", result)
	}
	if !strings.HasPrefix(result, "in") {
		t.Errorf("ParseResetTime(5h30m) = %q, should start with 'in'", result)
	}
}

func TestParseResetTime_ExactHours(t *testing.T) {
	future := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := ParseResetTime(future)
	// Should have "in" and "h" (flexible about exact hour due to timing)
	if !strings.HasPrefix(result, "in ") || !strings.Contains(result, "h") {
		t.Errorf("ParseResetTime(3h) = %q, want format 'in Xh...'", result)
	}
}

func TestIsGood_NegativeInvestInterval(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
	// Negative invest interval should be treated as 0
	if !IsGood(50, resetsAt, -30*time.Minute, now) {
		t.Error("IsGood should handle negative invest interval")
	}
}

func TestIsGood_LargeMana(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
	if !IsGood(100, resetsAt, 30*time.Minute, now) {
		t.Error("IsGood should return true with 100% mana")
	}
}

func TestIsGood_ZeroMana(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
	if IsGood(0, resetsAt, 30*time.Minute, now) {
		t.Error("IsGood should return false with 0% mana in middle of window")
	}
}

// TestFromUtilization_EdgeCases tests edge cases for FromUtilization
func TestFromUtilization_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		util float64
		want float64
	}{
		{"negative", -10, 110},      // 100 - (-10) = 110, not clamped (not negative)
		{"large_negative", -100, 200}, // 100 - (-100) = 200, not clamped
		{"exactly_100", 100, 0},
		{"way_over", 200, -100},     // 100 - 200 = -100, clamped to 0
		{"fractional", 33.33, 66.67},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromUtilization(tt.util)
			expected := tt.want
			if expected < 0 {
				expected = 0 // clamped by the function
			}
			if got != expected {
				t.Errorf("FromUtilization(%v) = %v, want %v", tt.util, got, expected)
			}
		})
	}
}

// TestIsGood_InvestIntervalBoundary tests edge case at invest interval boundary
func TestIsGood_InvestIntervalBoundary(t *testing.T) {
	now := time.Now()
	// Just before invest interval ends
	resetsAt := now.Add(4*time.Hour + 31*time.Minute)
	// time_since_reset = 29m, which is < 30m invest interval
	if IsGood(99, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good before invest interval ends")
	}
}

// TestParseResetTime_EarlyMorning tests formatting of early morning times
func TestParseResetTime_EarlyMorning(t *testing.T) {
	// Create a time 30 hours in the future (will format as time)
	future := time.Now().Add(30 * time.Hour).UTC()
	isoTime := future.Format(time.RFC3339Nano)
	result := ParseResetTime(isoTime)

	// Should format as clock time (2pm) not relative
	if strings.HasPrefix(result, "in ") {
		t.Errorf("ParseResetTime(30h) should be absolute format, got %q", result)
	}
	if result == "" {
		t.Error("ParseResetTime(30h) should not be empty")
	}
}

// TestFormatPercent_EdgeCasesNearZero tests very small percentages
func TestFormatPercent_EdgeCasesNearZero(t *testing.T) {
	tests := []struct {
		util float64
		want string
	}{
		{99.9, "0.1%"},
		{99.95, "0.0%"},
		{99.99, "0.0%"},
	}

	for _, tt := range tests {
		util := tt.util
		got := FormatPercent(&anthropic.UsageResponse{
			FiveHour: &anthropic.UsageWindow{Utilization: &util},
		})
		if got != tt.want {
			t.Errorf("FormatPercent(%.2f) = %q, want %q", tt.util, got, tt.want)
		}
	}
}

// TestFormatUsage_ExtraUsagePresent tests formatting when extra usage is present
func TestFormatUsage_ExtraUsagePresent(t *testing.T) {
	result := FormatUsage(&anthropic.UsageResponse{
		ExtraUsage: &anthropic.ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 0.01,
		},
	})
	if !strings.Contains(result, "overage $0.01") {
		t.Errorf("FormatUsage with tiny overage = %q", result)
	}
}

// TestIsGood_ZeroInvestInterval tests with zero invest interval
func TestIsGood_ZeroInvestInterval(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
	// With zero invest interval, expected = 100 * (5h - 2h) / 5h = 60%
	// 70% > 60% → good
	if !IsGood(70, resetsAt, 0, now) {
		t.Error("IsGood should handle zero invest interval")
	}
}

// TestIsGood_WindowEqualsInvestInterval tests when window == invest interval
func TestIsGood_WindowEqualsInvestInterval(t *testing.T) {
	now := time.Now()
	// Window is 5 hours, set invest interval to 5 hours
	// If resetsAt is 5h from now, we're at start of window (0 elapsed)
	// We're in investing period since timeSinceReset < investInterval
	resetsAt := now.Add(5 * time.Hour)

	// At the very start (investing period), should return false even with mana
	if IsGood(99, resetsAt, 5*time.Hour, now) {
		t.Error("IsGood should return false during investing period")
	}

	// After the investing period ends (5h elapsed), denominator = 0, should return true if mana > 0
	resetsAt = now.Add(-5 * time.Minute) // 5h past the reset
	if !IsGood(1, resetsAt, 5*time.Hour, now) {
		t.Error("IsGood should return true when past invest interval with mana > 0")
	}
}

