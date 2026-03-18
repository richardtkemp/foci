package compaction

import (
	"testing"
	"time"
)

func TestManaResetImminent(t *testing.T) {
	// Verifies boundary conditions: within threshold (true), beyond threshold (false),
	// past reset (false), zero time (false), zero threshold (false), and exact
	// boundary (false, strict <).
	tests := []struct {
		name      string
		resetTime time.Time
		threshold time.Duration
		want      bool
	}{
		{
			name:      "within threshold",
			resetTime: time.Now().Add(3 * time.Minute),
			threshold: 5 * time.Minute,
			want:      true,
		},
		{
			name:      "beyond threshold",
			resetTime: time.Now().Add(2 * time.Hour),
			threshold: 5 * time.Minute,
			want:      false,
		},
		{
			name:      "past reset",
			resetTime: time.Now().Add(-5 * time.Minute),
			threshold: 5 * time.Minute,
			want:      false,
		},
		{
			name:      "zero time",
			resetTime: time.Time{},
			threshold: 5 * time.Minute,
			want:      false,
		},
		{
			name:      "zero threshold disables check",
			resetTime: time.Now().Add(3 * time.Minute),
			threshold: 0,
			want:      false,
		},
		{
			name:      "exact boundary not triggered",
			resetTime: time.Now().Add(5*time.Minute + time.Second),
			threshold: 5 * time.Minute,
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ManaResetImminent(tt.resetTime, tt.threshold); got != tt.want {
				t.Errorf("ManaResetImminent = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCompactorGettersSetters verifies Threshold, PreserveMessages, and SetPreserveMessages.
func TestCompactorGettersSetters(t *testing.T) {
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
