package compaction

import (
	"testing"
	"time"
)

func TestCalculateIdlePressure_NotIdle(t *testing.T) {
	// Verifies no adjustment when idle time
	// is below the idle threshold.
	adj, refresh := CalculateIdlePressure(0.8, 30*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 140000, 200000)
	if adj != 0.8 {
		t.Errorf("threshold = %f, want 0.8", adj)
	}
	if refresh {
		t.Error("isManaRefresh = true, want false")
	}
}

func TestCalculateIdlePressure_IdleRamp(t *testing.T) {
	// Verifies the linear ramp from idle threshold
	// to 2x idle threshold reduces the compaction threshold proportionally.
	base := 0.8
	idleThreshold := 45 * time.Minute
	pressureStart := "70%"
	pressureMax := 0.15
	tokens := 140000 // 70% of 200k — at pressure start
	limit := 200000

	// At idle threshold (45m): 0% pressure → base threshold
	adj, _ := CalculateIdlePressure(base, 45*time.Minute, idleThreshold,
		pressureStart, pressureMax, time.Time{}, 15*time.Minute, tokens, limit)
	if adj != 0.8 {
		t.Errorf("at 45m: threshold = %f, want 0.8", adj)
	}

	// At 50% through ramp (67.5m): 50% pressure → 0.725
	adj, _ = CalculateIdlePressure(base, 67*time.Minute+30*time.Second, idleThreshold,
		pressureStart, pressureMax, time.Time{}, 15*time.Minute, tokens, limit)
	if adj < 0.72 || adj > 0.73 {
		t.Errorf("at 67.5m: threshold = %f, want ~0.725", adj)
	}

	// At 100% pressure (90m): full reduction → 0.65
	adj, _ = CalculateIdlePressure(base, 90*time.Minute, idleThreshold,
		pressureStart, pressureMax, time.Time{}, 15*time.Minute, tokens, limit)
	if adj != 0.65 {
		t.Errorf("at 90m: threshold = %f, want 0.65", adj)
	}

	// Beyond 2x threshold (120m): clamped at max → still 0.65
	adj, _ = CalculateIdlePressure(base, 120*time.Minute, idleThreshold,
		pressureStart, pressureMax, time.Time{}, 15*time.Minute, tokens, limit)
	if adj != 0.65 {
		t.Errorf("at 120m: threshold = %f, want 0.65 (clamped)", adj)
	}
}

func TestCalculateIdlePressure_ManaRefreshMode(t *testing.T) {
	// Verifies aggressive threshold
	// and refresh flag when mana reset is imminent.
	resetsAt := time.Now().Add(10 * time.Minute)
	adj, refresh := CalculateIdlePressure(0.8, 30*time.Minute, 45*time.Minute,
		"70%", 0.15, resetsAt, 15*time.Minute, 100000, 200000)
	if adj != 0.4 {
		t.Errorf("threshold = %f, want 0.4", adj)
	}
	if !refresh {
		t.Error("isManaRefresh = false, want true")
	}
}

func TestCalculateIdlePressure_ManaRefreshPriority(t *testing.T) {
	// Verifies mana refresh mode
	// takes priority over idle pressure.
	resetsAt := time.Now().Add(5 * time.Minute)
	// Even though idle for 90m (would give max pressure), mana refresh wins
	adj, refresh := CalculateIdlePressure(0.8, 90*time.Minute, 45*time.Minute,
		"70%", 0.15, resetsAt, 15*time.Minute, 140000, 200000)
	if adj != 0.4 {
		t.Errorf("threshold = %f, want 0.4 (mana refresh priority)", adj)
	}
	if !refresh {
		t.Error("isManaRefresh = false, want true")
	}
}

func TestCalculateIdlePressure_BelowPressureStart(t *testing.T) {
	// 60% context usage, pressure starts at 70% → no adjustment even though idle
	adj, refresh := CalculateIdlePressure(0.8, 60*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 120000, 200000)
	if adj != 0.8 {
		t.Errorf("threshold = %f, want 0.8 (below pressure start)", adj)
	}
	if refresh {
		t.Error("isManaRefresh = true, want false")
	}
}

func TestCalculateIdlePressure_DecimalPressureStart(t *testing.T) {
	// Above 0.7 threshold → pressure applies
	adj, _ := CalculateIdlePressure(0.8, 90*time.Minute, 45*time.Minute,
		"0.7", 0.15, time.Time{}, 15*time.Minute, 150000, 200000)
	if adj != 0.65 {
		t.Errorf("threshold = %f, want 0.65", adj)
	}

	// Below 0.7 threshold → no pressure
	adj, _ = CalculateIdlePressure(0.8, 90*time.Minute, 45*time.Minute,
		"0.7", 0.15, time.Time{}, 15*time.Minute, 120000, 200000)
	if adj != 0.8 {
		t.Errorf("threshold = %f, want 0.8 (below decimal pressure start)", adj)
	}
}

func TestCalculateIdlePressure_ManaResetPast(t *testing.T) {
	// Verifies no mana refresh mode
	// when the reset time is in the past.
	resetsAt := time.Now().Add(-5 * time.Minute) // in the past
	adj, refresh := CalculateIdlePressure(0.8, 30*time.Minute, 45*time.Minute,
		"70%", 0.15, resetsAt, 15*time.Minute, 100000, 200000)
	if adj != 0.8 {
		t.Errorf("threshold = %f, want 0.8 (past reset)", adj)
	}
	if refresh {
		t.Error("isManaRefresh = true for past reset, want false")
	}
}

func TestCalculateIdlePressure_ManaResetFarFuture(t *testing.T) {
	// Verifies no mana refresh mode
	// when reset is far in the future (beyond the threshold).
	resetsAt := time.Now().Add(2 * time.Hour) // well beyond 15m threshold
	adj, refresh := CalculateIdlePressure(0.8, 30*time.Minute, 45*time.Minute,
		"70%", 0.15, resetsAt, 15*time.Minute, 100000, 200000)
	if adj != 0.8 {
		t.Errorf("threshold = %f, want 0.8 (far future reset)", adj)
	}
	if refresh {
		t.Error("isManaRefresh = true for far future reset, want false")
	}
}

func TestCalculateIdlePressure_ZeroContextLimit(t *testing.T) {
	// Verifies no panic with zero context limit.
	adj, refresh := CalculateIdlePressure(0.8, 60*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 0, 0)
	// With zero context limit, pressure start check is skipped → ramp applies
	if adj > 0.8 {
		t.Errorf("threshold = %f, should be <= 0.8", adj)
	}
	if refresh {
		t.Error("isManaRefresh = true, want false")
	}
}

func TestParsePressureStart(t *testing.T) {
	// Verifies the pressure start parsing for both formats.
	tests := []struct {
		input    string
		fallback float64
		want     float64
	}{
		{"70%", 0.5, 0.70},
		{"50%", 0.5, 0.50},
		{"0.6", 0.5, 0.60},
		{"0.85", 0.5, 0.85},
		{"", 0.5, 0.50},
		{"invalid", 0.5, 0.50},
	}
	for _, tt := range tests {
		got := parsePressureStart(tt.input, tt.fallback)
		if got != tt.want {
			t.Errorf("parsePressureStart(%q, %f) = %f, want %f", tt.input, tt.fallback, got, tt.want)
		}
	}
}

func TestCompactorGettersSetters(t *testing.T) {
	// Verifies that Threshold returns the compaction ratio set at
	// construction, that PreserveMessages reflects the value from WithConfig, and that
	// SetPreserveMessages can update the preserve count independently at runtime.
	c := NewCompactor(nil, 0.8).WithConfig(4096, 4, 25)

	if c.Threshold() != 0.8 {
		t.Errorf("Threshold() = %f, want 0.8", c.Threshold())
	}
	if c.PreserveMessages() != 25 {
		t.Errorf("PreserveMessages() = %d, want 25", c.PreserveMessages())
	}

	c.SetPreserveMessages(100)
	if c.PreserveMessages() != 100 {
		t.Errorf("after SetPreserveMessages(100): PreserveMessages() = %d, want 100", c.PreserveMessages())
	}
}
