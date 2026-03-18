package agent

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"foci/internal/provider"
)

func TestIsFallbackEligible_DeadlineExceeded(t *testing.T) {
	// Proves that context.DeadlineExceeded triggers fallback, since it
	// indicates the primary model timed out.
	if !isFallbackEligible(context.DeadlineExceeded) {
		t.Error("expected DeadlineExceeded to be fallback-eligible")
	}
}

func TestIsFallbackEligible_WrappedDeadlineExceeded(t *testing.T) {
	// Proves that a wrapped DeadlineExceeded is still detected via errors.Is.
	err := fmt.Errorf("request failed: %w", context.DeadlineExceeded)
	if !isFallbackEligible(err) {
		t.Error("expected wrapped DeadlineExceeded to be fallback-eligible")
	}
}

func TestIsFallbackEligible_Overloaded529(t *testing.T) {
	// Proves that 529 (Anthropic overloaded) triggers fallback.
	err := &provider.APIError{StatusCode: 529}
	if !isFallbackEligible(err) {
		t.Error("expected 529 to be fallback-eligible")
	}
}

func TestIsFallbackEligible_ServerErrors(t *testing.T) {
	// Proves that 5xx server errors (500, 502, 503) trigger fallback.
	for _, code := range []int{500, 502, 503} {
		err := &provider.APIError{StatusCode: code}
		if !isFallbackEligible(err) {
			t.Errorf("expected %d to be fallback-eligible", code)
		}
	}
}

func TestIsFallbackEligible_NotEligible(t *testing.T) {
	// Proves that client errors (400, 401, 429) and non-API errors
	// do NOT trigger fallback.
	cases := []struct {
		name string
		err  error
	}{
		{"400 bad request", &provider.APIError{StatusCode: http.StatusBadRequest}},
		{"401 unauthorized", &provider.APIError{StatusCode: http.StatusUnauthorized}},
		{"429 rate limit", &provider.APIError{StatusCode: http.StatusTooManyRequests}},
		{"generic error", fmt.Errorf("connection refused")},
		{"context cancelled", context.Canceled},
	}
	for _, tc := range cases {
		if isFallbackEligible(tc.err) {
			t.Errorf("%s: expected NOT fallback-eligible", tc.name)
		}
	}
}
