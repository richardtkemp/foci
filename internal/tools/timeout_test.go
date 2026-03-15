package tools

import (
	"testing"
	"time"
)

// TestResolveTimeoutDefault verifies that zero or negative input returns the configured default.
func TestResolveTimeoutDefault(t *testing.T) {
	cfg := TimeoutConfig{DefaultSec: 30}

	for _, input := range []int{0, -1, -100} {
		got := ResolveTimeout(input, cfg)
		if got != 30*time.Second {
			t.Errorf("ResolveTimeout(%d) = %v, want 30s", input, got)
		}
	}
}

// TestResolveTimeoutPassthrough verifies that valid values within bounds are returned as-is.
func TestResolveTimeoutPassthrough(t *testing.T) {
	cfg := TimeoutConfig{DefaultSec: 30, MaxSec: 300}

	got := ResolveTimeout(60, cfg)
	if got != 60*time.Second {
		t.Errorf("ResolveTimeout(60) = %v, want 60s", got)
	}
}

// TestResolveTimeoutClamped verifies that values exceeding MaxSec are clamped.
func TestResolveTimeoutClamped(t *testing.T) {
	cfg := TimeoutConfig{DefaultSec: 30, MaxSec: 300}

	got := ResolveTimeout(600, cfg)
	if got != 300*time.Second {
		t.Errorf("ResolveTimeout(600) = %v, want 300s (clamped)", got)
	}
}

// TestResolveTimeoutUnlimitedMax verifies that MaxSec=0 means no upper bound.
func TestResolveTimeoutUnlimitedMax(t *testing.T) {
	cfg := TimeoutConfig{DefaultSec: 30, MaxSec: 0}

	got := ResolveTimeout(9999, cfg)
	if got != 9999*time.Second {
		t.Errorf("ResolveTimeout(9999) = %v, want 9999s (unlimited max)", got)
	}
}

// TestResolveTimeoutExactMax verifies that a value equal to MaxSec is not clamped.
func TestResolveTimeoutExactMax(t *testing.T) {
	cfg := TimeoutConfig{DefaultSec: 30, MaxSec: 300}

	got := ResolveTimeout(300, cfg)
	if got != 300*time.Second {
		t.Errorf("ResolveTimeout(300) = %v, want 300s (exact max)", got)
	}
}
