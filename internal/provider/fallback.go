package provider

import (
	"context"
	"errors"
	"strings"
)

// FallbackFunc resolves a model to its fallback.
// Returns the fallback model (canonical developer/model_id), endpoint, format,
// and ok=true if a fallback exists.
type FallbackFunc func(model string) (fbModel, endpoint, format string, ok bool)

// maxFallbackDepth is the maximum number of fallback hops per request.
const maxFallbackDepth = 3

// IsFallbackEligible returns true if the error indicates a transient server
// issue that warrants trying a fallback model.
//
// Eligible: context.DeadlineExceeded, 529 overload, 5xx server errors.
// Not eligible: 401 auth, 400 bad request, 429 rate limit, non-API errors.
func IsFallbackEligible(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	// IsRetryable covers 500, 502, 503, 529
	return apiErr.IsRetryable()
}

// stripUnsupportedParams checks if a 400 error indicates that
// Thinking, Output (effort), or Speed params are unsupported by the model.
// If so, it strips the offending params from req and returns true.
func stripUnsupportedParams(req *MessageRequest, apiErr *APIError, logf func(string, ...any)) bool {
	if req.Thinking == nil && req.Output == nil && req.Speed == "" {
		return false
	}
	body := strings.ToLower(apiErr.Body)
	stripped := false
	if req.Thinking != nil && strings.Contains(body, "thinking") {
		if logf != nil {
			logf("model %s rejected thinking param, retrying without it", req.Model)
		}
		req.Thinking = nil
		stripped = true
	}
	if req.Output != nil && (strings.Contains(body, "effort") || strings.Contains(body, "output")) {
		if logf != nil {
			logf("model %s rejected effort param, retrying without it", req.Model)
		}
		req.Output = nil
		stripped = true
	}
	if req.Speed != "" && strings.Contains(body, "speed") {
		if logf != nil {
			logf("model %s rejected speed param, retrying without it", req.Model)
		}
		req.Speed = ""
		stripped = true
	}
	return stripped
}

// walkFallback walks the fallback chain trying alternate models after a
// fallback-eligible error. It does NOT retry the primary model — the caller
// has already done that.
//
// Returns the successful response/nil error, or the last error from the chain.
// On success, req.Model reflects the model that succeeded.
func walkFallback(
	ctx context.Context,
	client Client,
	req *MessageRequest,
	handler *StreamHandler,
	fallbackFn FallbackFunc,
	clientProvider ClientProvider,
	logf func(string, ...any),
	originalErr error,
) (*MessageResponse, error) {
	fbModel := req.Model
	var lastErr error
	for depth := 0; depth < maxFallbackDepth; depth++ {
		fbCanonical, endpoint, format, ok := fallbackFn(fbModel)
		if !ok {
			break
		}

		if logf != nil {
			logf("fallback: %s failed, trying %s", fbModel, fbCanonical)
		}

		fbClient := client
		if clientProvider != nil {
			if c := clientProvider.GetClient(endpoint, format); c != nil {
				fbClient = c
			}
		}

		req.Model = fbCanonical
		resp, err := sendWithRetry(ctx, fbClient, req, handler)
		if err == nil {
			if logf != nil {
				logf("fallback succeeded on %s", fbCanonical)
			}
			return resp, nil
		}
		lastErr = err
		if !IsFallbackEligible(err) {
			break // non-transient error, stop trying
		}
		fbModel = fbCanonical
	}

	// Return the last fallback error, or the original error if no fallbacks
	// were attempted (no fallback model configured for this model).
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, originalErr
}

// Send sends a request with automatic error recovery:
//
//  1. Send the request with retries (exponential backoff + extended retry).
//  2. On 400 errors, strip unsupported params (Thinking, Output, Speed) and retry.
//  3. On transient errors (529, 5xx, deadline exceeded), walk the fallback chain.
//
// If fallbackFn is nil, step 3 is skipped (no fallback models configured).
// clientProvider resolves clients for fallback endpoint:format pairs; nil = reuse caller's client.
// logf receives diagnostic messages; nil = silent.
//
// On success from a fallback model, req.Model reflects that model. The caller
// should restore the original model if needed for subsequent iterations.
func Send(
	ctx context.Context,
	client Client,
	req *MessageRequest,
	handler *StreamHandler,
	fallbackFn FallbackFunc,
	clientProvider ClientProvider,
	logf func(string, ...any),
) (*MessageResponse, error) {
	resp, err := sendWithRetry(ctx, client, req, handler)
	if err == nil {
		return resp, nil
	}

	// Step 2: strip unsupported params on 400 and retry once.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 400 {
		if stripUnsupportedParams(req, apiErr, logf) {
			resp, err = sendWithRetry(ctx, client, req, handler)
			if err == nil {
				return resp, nil
			}
		}
	}

	// Step 3: walk fallback chain on transient errors.
	if fallbackFn != nil && IsFallbackEligible(err) {
		return walkFallback(ctx, client, req, handler, fallbackFn, clientProvider, logf, err)
	}

	return resp, err
}
