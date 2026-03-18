package provider

import (
	"context"
	"errors"
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
) (*MessageResponse, error) {
	fbModel := req.Model
	var resp *MessageResponse
	var err error
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
		resp, err = Send(ctx, fbClient, req, handler)
		if err == nil {
			if logf != nil {
				logf("fallback succeeded on %s", fbCanonical)
			}
			return resp, nil
		}
		if !IsFallbackEligible(err) {
			break // non-transient error, stop trying
		}
		fbModel = fbCanonical
	}

	return resp, err
}

// SendWithFallback sends a request using Send, and on fallback-eligible errors
// walks the fallback chain (up to 3 hops) trying alternate models.
//
// If fallbackFn is nil, this degrades to a plain Send call.
// clientProvider is used to resolve clients for fallback endpoint:format pairs;
// if nil, the original client is reused for all fallback attempts.
// logf receives diagnostic messages about fallback attempts.
//
// On success, req.Model will reflect the model that succeeded. The caller
// should restore the original model if needed for subsequent iterations.
func SendWithFallback(
	ctx context.Context,
	client Client,
	req *MessageRequest,
	handler *StreamHandler,
	fallbackFn FallbackFunc,
	clientProvider ClientProvider,
	logf func(string, ...any),
) (*MessageResponse, error) {
	resp, err := Send(ctx, client, req, handler)
	if err == nil || fallbackFn == nil || !IsFallbackEligible(err) {
		return resp, err
	}

	return walkFallback(ctx, client, req, handler, fallbackFn, clientProvider, logf)
}

// WalkFallback walks the fallback chain after an already-failed Send.
// Use this when the caller has already called Send and wants to try fallbacks
// without re-sending to the primary model.
//
// If fallbackFn is nil or the error is not fallback-eligible, returns the
// original error unchanged.
func WalkFallback(
	ctx context.Context,
	client Client,
	req *MessageRequest,
	handler *StreamHandler,
	primaryErr error,
	fallbackFn FallbackFunc,
	clientProvider ClientProvider,
	logf func(string, ...any),
) (*MessageResponse, error) {
	if fallbackFn == nil || !IsFallbackEligible(primaryErr) {
		return nil, primaryErr
	}

	return walkFallback(ctx, client, req, handler, fallbackFn, clientProvider, logf)
}
