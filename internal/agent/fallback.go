package agent

import (
	"context"
	"errors"

	"foci/internal/provider"
)

// isFallbackEligible returns true if the error indicates a transient server
// issue that warrants trying a fallback model.
//
// Eligible: context.DeadlineExceeded, 529 overload, 5xx server errors.
// Not eligible: 401 auth, 400 bad request, 429 rate limit, non-API errors.
func isFallbackEligible(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	// IsRetryable covers 500, 502, 503, 529
	return apiErr.IsRetryable()
}
