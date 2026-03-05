package gemini

import (
	"fmt"
	"net/http"
	"strings"

	"foci/internal/provider"

	"google.golang.org/genai"
)

// classifyError maps Gemini SDK errors to provider.APIError for
// consistent error handling in the agent loop.
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	msg := err.Error()

	// Check for common HTTP status patterns in error messages
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "RESOURCE_EXHAUSTED"):
		return &provider.APIError{
			StatusCode: http.StatusTooManyRequests,
			Body:    fmt.Sprintf("gemini: rate limited: %v", err),
		}
	case strings.Contains(msg, "500") || strings.Contains(msg, "INTERNAL"):
		return &provider.APIError{
			StatusCode: http.StatusInternalServerError,
			Body:    fmt.Sprintf("gemini: server error: %v", err),
		}
	case strings.Contains(msg, "503") || strings.Contains(msg, "UNAVAILABLE"):
		return &provider.APIError{
			StatusCode: http.StatusServiceUnavailable,
			Body:    fmt.Sprintf("gemini: service unavailable: %v", err),
		}
	case strings.Contains(msg, "400") || strings.Contains(msg, "INVALID_ARGUMENT"):
		return &provider.APIError{
			StatusCode: http.StatusBadRequest,
			Body:    fmt.Sprintf("gemini: bad request: %v", err),
		}
	case strings.Contains(msg, "401") || strings.Contains(msg, "UNAUTHENTICATED"):
		return &provider.APIError{
			StatusCode: http.StatusUnauthorized,
			Body:    fmt.Sprintf("gemini: unauthorized: %v", err),
		}
	case strings.Contains(msg, "403") || strings.Contains(msg, "PERMISSION_DENIED"):
		return &provider.APIError{
			StatusCode: http.StatusForbidden,
			Body:    fmt.Sprintf("gemini: forbidden: %v", err),
		}
	}

	// Check for safety-related errors
	if isSafetyError(err) {
		return &provider.APIError{
			StatusCode: http.StatusBadRequest,
			Body:    fmt.Sprintf("gemini: content filtered: %v", err),
		}
	}

	// Default: wrap as server error
	return fmt.Errorf("gemini: %w", err)
}

// isSafetyError checks if the error is related to Gemini safety filtering.
func isSafetyError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SAFETY") ||
		strings.Contains(msg, "safety") ||
		strings.Contains(msg, "RECITATION") ||
		strings.Contains(msg, "blocked")
}

// Ensure the genai import is used (needed for FinishReason constants in translate.go).
var _ = genai.FinishReasonStop
