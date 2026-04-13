package mana

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mockUsageClient is a test mock implementing UsageClient.
type mockUsageClient struct {
	window *UsageWindow
	err    error
}

func (m *mockUsageClient) GetUsage(_ context.Context) (*UsageWindow, error) { return m.window, m.err }
func (m *mockUsageClient) Invalidate()                                      {}
func (m *mockUsageClient) SetCacheTTL(time.Duration)                        {}

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
	// FromUtilization takes 0–1 fraction, returns mana percentage 0–100.
	tests := []struct {
		util float64
		want float64
	}{
		{0, 100},
		{0.5, 50},
		{1, 0},
		{1.1, 0},    // clamped
		{0.255, 74.5},
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

func TestFormatPercentNoUtilization(t *testing.T) {
	// Window with nil utilization.
	if got := FormatPercent(&UsageWindow{}); got != "" {
		t.Errorf("FormatPercent(empty) = %q, want empty", got)
	}
}

func TestFormatPercentValues(t *testing.T) {
	// Utilization is 0–1 fraction.
	tests := []struct {
		util float64
		want string
	}{
		{0, "100%"},
		{0.25, "75%"},
		{0.5, "50%"},
		{0.995, "0.5%"},
		{1, "0.0%"},
		{1.1, "0.0%"}, // clamped to 0
	}
	for _, tt := range tests {
		util := tt.util
		got := FormatPercent(&UsageWindow{Utilization: &util})
		if got != tt.want {
			t.Errorf("FormatPercent(util=%.3f) = %q, want %q", tt.util, got, tt.want)
		}
	}
}

func TestFormatResetNil(t *testing.T) {
	if got := FormatReset(nil); got != "" {
		t.Errorf("FormatReset(nil) = %q, want empty", got)
	}
}

func TestFormatResetZeroTime(t *testing.T) {
	// Window with zero ResetsAt.
	if got := FormatReset(&UsageWindow{}); got != "" {
		t.Errorf("FormatReset(zero) = %q, want empty", got)
	}
}

func TestFormatResetWithTime(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC()
	got := FormatReset(&UsageWindow{ResetsAt: future})
	if !strings.HasPrefix(got, "in ") || !strings.Contains(got, "h") {
		t.Errorf("FormatReset(2h) = %q, want 'in Xh' or 'in Xh Ym'", got)
	}
}

func TestFormatResetPast(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC()
	got := FormatReset(&UsageWindow{ResetsAt: past})
	if got != "now" {
		t.Errorf("FormatReset(past) = %q, want %q", got, "now")
	}
}

func TestFormatResetMinutes(t *testing.T) {
	future := time.Now().Add(45 * time.Minute).UTC()
	got := FormatReset(&UsageWindow{ResetsAt: future})
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

func TestNewMonitor(t *testing.T) {
	m := NewMonitor(nil)
	if m == nil {
		t.Fatal("NewMonitor returned nil")
	}
	if m.usageClient != nil {
		t.Error("usageClient should be nil")
	}
}

func TestIsGoodFor_NoClient(t *testing.T) {
	// No usage client means unknown mana — don't block operations.
	m := NewMonitor(nil)
	if !m.IsGoodFor(context.Background(), 30*time.Minute) {
		t.Error("IsGoodFor should return true with no client (unknown mana should not block)")
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

func TestIsGood_NegativeInvestInterval(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
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

func TestFromUtilization_EdgeCases(t *testing.T) {
	// Input is 0–1 fraction.
	tests := []struct {
		name string
		util float64
		want float64
	}{
		{"negative", -0.1, 110},
		{"large_negative", -1, 200},
		{"exactly_1", 1, 0},
		{"way_over", 2, -100},
		{"fractional", 0.3333, 66.67},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromUtilization(tt.util)
			expected := tt.want
			if expected < 0 {
				expected = 0
			}
			if diff := got - expected; diff < -0.01 || diff > 0.01 {
				t.Errorf("FromUtilization(%v) = %v, want ≈%v", tt.util, got, expected)
			}
		})
	}
}

func TestIsGood_InvestIntervalBoundary(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(4*time.Hour + 31*time.Minute)
	if IsGood(99, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good before invest interval ends")
	}
}

func TestParseResetTime_EarlyMorning(t *testing.T) {
	future := time.Now().Add(30 * time.Hour).UTC()
	isoTime := future.Format(time.RFC3339Nano)
	result := ParseResetTime(isoTime)
	if strings.HasPrefix(result, "in ") {
		t.Errorf("ParseResetTime(30h) should be absolute format, got %q", result)
	}
	if result == "" {
		t.Error("ParseResetTime(30h) should not be empty")
	}
}

func TestFormatPercent_EdgeCasesNearZero(t *testing.T) {
	// Utilization is 0–1 fraction.
	tests := []struct {
		util float64
		want string
	}{
		{0.999, "0.1%"},
		{0.9995, "0.0%"},
		{0.9999, "0.0%"},
	}
	for _, tt := range tests {
		util := tt.util
		got := FormatPercent(&UsageWindow{Utilization: &util})
		if got != tt.want {
			t.Errorf("FormatPercent(%.4f) = %q, want %q", tt.util, got, tt.want)
		}
	}
}

func TestIsGood_ZeroInvestInterval(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
	if !IsGood(70, resetsAt, 0, now) {
		t.Error("IsGood should handle zero invest interval")
	}
}

func TestIsGood_WindowEqualsInvestInterval(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(5 * time.Hour)
	if IsGood(99, resetsAt, 5*time.Hour, now) {
		t.Error("IsGood should return false during investing period")
	}
	resetsAt = now.Add(-5 * time.Minute)
	if !IsGood(1, resetsAt, 5*time.Hour, now) {
		t.Error("IsGood should return true when past invest interval with mana > 0")
	}
}

func TestMonitor_IsGoodFor_NoClient_AllowsOperations(t *testing.T) {
	// No usage client = unknown mana = don't block.
	m := NewMonitor(nil)
	if !m.IsGoodFor(context.Background(), 30*time.Minute) {
		t.Error("IsGoodFor with nil client should return true (unknown mana should not block)")
	}
	if !m.IsGoodFor(context.Background(), 0) {
		t.Error("IsGoodFor with nil client and zero interval should return true")
	}
}

func TestMonitor_IsGoodFor_ContextCancelled(t *testing.T) {
	// Nil client returns true regardless of context state (unknown = allow).
	m := NewMonitor(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := m.IsGoodFor(ctx, 30*time.Minute)
	if !result {
		t.Error("IsGoodFor with nil client should return true even with cancelled context")
	}
}

func TestParseResetTime_ExactBoundary(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"exactly 1 minute", 1 * time.Minute, "in 1m"},
		{"exactly 1 hour", 1 * time.Hour, "in 1h"},
		{"exactly 24 hours", 24 * time.Hour, "0am"},
		{"1m59s", 1*time.Minute + 59*time.Second, "in 1m"},
		{"59m59s", 59*time.Minute + 59*time.Second, "in 59m"},
		{"23h59m", 23*time.Hour + 59*time.Minute, "in 23h 59m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			future := time.Now().Add(tt.duration).UTC()
			isoTime := future.Format(time.RFC3339Nano)
			result := ParseResetTime(isoTime)
			if tt.duration < 24*time.Hour && !strings.HasPrefix(result, "in ") {
				t.Errorf("ParseResetTime(%v) = %q, want to start with 'in '", tt.duration, result)
			}
		})
	}
}

func TestIsGood_ExtremeValues(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour)
	tests := []struct {
		name     string
		mana     float64
		resetAt  time.Time
		interval time.Duration
		want     bool
	}{
		{"max positive mana", 999999.0, resetsAt, 30 * time.Minute, true},
		{"very small positive mana", 0.0001, resetsAt, 30 * time.Minute, false},
		{"huge invest interval", 50.0, resetsAt, 100 * time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsGood(tt.mana, tt.resetAt, tt.interval, now)
			if got != tt.want {
				t.Errorf("IsGood(%.1f, ...) = %v, want %v", tt.mana, got, tt.want)
			}
		})
	}
}

func TestFromUtilization_RoundingEdges(t *testing.T) {
	// Input is 0–1 fraction.
	tests := []struct {
		util     float64
		expected float64
	}{
		{0.005, 99.5},
		{0.499999, 50.0001},
		{0.505, 49.5},
		{0.999999, 0.0001},
	}
	for _, tt := range tests {
		got := FromUtilization(tt.util)
		if diff := got - tt.expected; diff < -0.001 || diff > 0.001 {
			t.Errorf("FromUtilization(%.6f) = %.4f, want ≈ %.4f", tt.util, got, tt.expected)
		}
	}
}

func TestMonitor_IsGoodFor_WithMock(t *testing.T) {
	// Uses a mock client that returns 70% utilization (30% mana), 2h until reset.
	util := 0.70 // 30% mana
	future := time.Now().Add(2 * time.Hour).UTC()
	client := &mockUsageClient{
		window: &UsageWindow{
			Utilization: &util,
			ResetsAt:    future,
			Period:      5 * time.Hour,
		},
	}

	m := NewMonitor(client)

	// 30% mana, 3h into window, invest=30m → expected ~44.4% → 30% < 44.4% → not good
	result := m.IsGoodFor(context.Background(), 30*time.Minute)
	if result {
		t.Error("IsGoodFor should return false with 30% mana (below expected ~44.4%)")
	}
}

func TestManaAndReset(t *testing.T) {
	// Nil client returns empty.
	pct, reset, good := ManaAndReset(nil, 30*time.Minute)
	if pct != "" || reset != "" || good {
		t.Errorf("ManaAndReset(nil) = (%q, %q, %v), want empty", pct, reset, good)
	}
}

func TestManaAndReset_WithMock(t *testing.T) {
	// 25% utilization → 75% mana, 2h until reset.
	util := 0.25
	future := time.Now().Add(2 * time.Hour).UTC()
	client := &mockUsageClient{
		window: &UsageWindow{
			Utilization: &util,
			ResetsAt:    future,
			Period:      5 * time.Hour,
		},
	}

	pct, reset, _ := ManaAndReset(client, 30*time.Minute)
	if pct != "75%" {
		t.Errorf("pct = %q, want 75%%", pct)
	}
	if !strings.HasPrefix(reset, "in ") {
		t.Errorf("reset = %q, want 'in ...'", reset)
	}
}
