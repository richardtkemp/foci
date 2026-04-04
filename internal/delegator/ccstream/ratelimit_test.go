package ccstream

import (
	"context"
	"testing"
	"time"
)

func TestRateLimitState_NilBeforeFirstEvent(t *testing.T) {
	// Proves that GetUsage returns (nil, nil) before any rate_limit_event is received.
	s := &RateLimitState{}
	w, err := s.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != nil {
		t.Fatalf("expected nil window before first event, got %+v", w)
	}
}

func TestRateLimitState_UpdateAndGet(t *testing.T) {
	// Proves that Update stores the info and GetUsage converts it correctly.
	s := &RateLimitState{}
	util := 0.42
	epoch := float64(1743800000)
	s.Update(&RateLimitInfo{
		Status:        "allowed_warning",
		Utilization:   &util,
		ResetsAt:      &epoch,
		RateLimitType: "five_hour",
	})

	w, err := s.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil window")
	}

	// Utilization passes through (0–1 scale).
	if w.Utilization == nil || *w.Utilization != 0.42 {
		t.Errorf("utilization = %v, want 0.42", w.Utilization)
	}

	// ResetsAt converted from unix epoch.
	if w.ResetsAt.IsZero() {
		t.Error("ResetsAt is zero")
	}
	if w.ResetsAt.Unix() != 1743800000 {
		t.Errorf("ResetsAt.Unix() = %d, want 1743800000", w.ResetsAt.Unix())
	}

	// Period mapped from rateLimitType.
	if w.Period != 5*time.Hour {
		t.Errorf("period = %v, want 5h", w.Period)
	}
}

func TestRateLimitState_AllowedNoUtilization(t *testing.T) {
	// Proves that "allowed" status (no utilization) returns a window with nil Utilization.
	s := &RateLimitState{}
	epoch := float64(1743800000)
	s.Update(&RateLimitInfo{
		Status:        "allowed",
		ResetsAt:      &epoch,
		RateLimitType: "five_hour",
	})

	w, err := s.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil window")
	}
	if w.Utilization != nil {
		t.Errorf("expected nil utilization for 'allowed', got %v", *w.Utilization)
	}
	if w.ResetsAt.IsZero() {
		t.Error("ResetsAt should be set even for 'allowed'")
	}
}

func TestRateLimitState_SevenDayPeriod(t *testing.T) {
	// Proves that "seven_day" rateLimitType maps to 168h period.
	s := &RateLimitState{}
	s.Update(&RateLimitInfo{
		Status:        "allowed",
		RateLimitType: "seven_day",
	})

	w, _ := s.GetUsage(context.Background())
	if w.Period != 7*24*time.Hour {
		t.Errorf("period = %v, want 168h", w.Period)
	}
}

func TestRateLimitState_UnknownTypeDefaultsPeriod(t *testing.T) {
	// Proves that an unknown rateLimitType defaults to 5h period.
	s := &RateLimitState{}
	s.Update(&RateLimitInfo{
		Status:        "allowed",
		RateLimitType: "unknown_type",
	})

	w, _ := s.GetUsage(context.Background())
	if w.Period != 5*time.Hour {
		t.Errorf("period = %v, want 5h (default)", w.Period)
	}
}

func TestRateLimitState_InvalidateNoOp(t *testing.T) {
	// Proves that Invalidate doesn't clear the cached data (push-based).
	s := &RateLimitState{}
	util := 0.5
	s.Update(&RateLimitInfo{Utilization: &util, RateLimitType: "five_hour"})
	s.Invalidate()

	w, _ := s.GetUsage(context.Background())
	if w == nil || w.Utilization == nil {
		t.Error("Invalidate should not clear push-based data")
	}
}
