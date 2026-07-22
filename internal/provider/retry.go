package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

)

// retryCallbacksKey is the context key for retry notification callbacks.
type retryCallbacksKey struct{}

// retryStateKey is the context key for tracking retry notification state.
type retryStateKey struct{}

// retryState tracks whether we've already notified for this retry sequence.
type retryState struct {
	mu       sync.Mutex
	notified bool
}

// RetryCallbacks holds callbacks for retry lifecycle events.
type RetryCallbacks struct {
	OnFirstRetry func(endpoint string) // called once on first retry in a sequence
	OnSuccess    func()                // called when a retry succeeds
}

// WithRetryCallbacks attaches retry callbacks to a context and initializes state.
func WithRetryCallbacks(ctx context.Context, cb *RetryCallbacks) context.Context {
	ctx = context.WithValue(ctx, retryCallbacksKey{}, cb)
	ctx = context.WithValue(ctx, retryStateKey{}, &retryState{})
	return ctx
}

// retryCallbacksFromContext extracts RetryCallbacks from context (nil if absent).
func retryCallbacksFromContext(ctx context.Context) *RetryCallbacks {
	cb, _ := ctx.Value(retryCallbacksKey{}).(*RetryCallbacks)
	return cb
}

// retryStateFromContext extracts retry state from context (nil if absent).
func retryStateFromContext(ctx context.Context) *retryState {
	s, _ := ctx.Value(retryStateKey{}).(*retryState)
	return s
}

// notifyFirstRetry calls OnFirstRetry callback once per retry sequence.
func notifyFirstRetry(ctx context.Context, endpoint string) {
	callbacks := retryCallbacksFromContext(ctx)
	if callbacks == nil || callbacks.OnFirstRetry == nil {
		return
	}

	state := retryStateFromContext(ctx)
	if state == nil {
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.notified {
		callbacks.OnFirstRetry(endpoint)
		state.notified = true
	}
}

// notifySuccess calls OnSuccess callback if we previously notified a retry.
func notifySuccess(ctx context.Context) {
	callbacks := retryCallbacksFromContext(ctx)
	if callbacks == nil || callbacks.OnSuccess == nil {
		return
	}

	state := retryStateFromContext(ctx)
	if state == nil {
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.notified {
		callbacks.OnSuccess()
	}
}

// endpointDescriber is an optional interface for clients that can identify
// their API endpoint by name (e.g. "Anthropic API", "OpenRouter").
// Used to display which API is busy during retry notifications.
type endpointDescriber interface {
	Endpoint() string
}

// endpointFromClient returns a human-readable endpoint name from a client.
// Falls back to "API" if the client doesn't implement endpointDescriber.
func endpointFromClient(client Client) string {
	if ed, ok := client.(endpointDescriber); ok {
		return ed.Endpoint()
	}
	return "API"
}

// EndpointNameFromURL extracts a human-readable name from an API base URL.
// Uses the penultimate domain label: "api.openrouter.ai" -> "Openrouter API".
func EndpointNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) >= 2 {
		name := parts[len(parts)-2]
		return strings.ToUpper(name[:1]) + name[1:] + " API"
	}
	return u.Host
}

// maxInlineRateLimitWait bounds how long a 429 is retried inline before the
// caller's rate-limit gate takes over. A Retry-After beyond this means the
// limit is long-lived (e.g. an account quota window), so blocking the turn is
// wrong — the gate defers and replays instead.
const maxInlineRateLimitWait = 30 * time.Second

// inlineRateLimitWait decides whether a 429 should be retried inline, and how
// long to wait first. A short (or absent) Retry-After retries inline, honoring
// the hint when it exceeds the current backoff; a Retry-After beyond the cap
// returns retry=false so the caller can fall back to the rate-limit gate.
func inlineRateLimitWait(apiErr *APIError, backoff time.Duration) (wait time.Duration, retry bool) {
	ra := time.Duration(apiErr.RetryAfterSeconds()) * time.Second
	if ra > maxInlineRateLimitWait {
		return 0, false
	}
	wait = backoff
	if ra > wait {
		wait = ra
	}
	if wait > maxInlineRateLimitWait {
		wait = maxInlineRateLimitWait
	}
	return wait, true
}

// retryableClient is a type-assertion interface for clients that support
// extended retry logic (currently only Anthropic).
type retryableClient interface {
	OnRetrySuccess()
	WaitForRecovery() <-chan struct{}
	RetryBaseDelay() time.Duration         // base delay for phase 1 retries
	OverloadBaseDelay() time.Duration      // base delay for phase 2 retries
	OverloadMaxDuration() time.Duration    // max duration for phase 2 overload (529) retries
	ServerErrorMaxDuration() time.Duration // max duration for phase 2 server error (5xx) retries
}

// retryWithBackoff performs standard exponential backoff retries for retryable errors.
// Returns (response, nil) on success, or (nil, lastError) on failure.
func retryWithBackoff(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	const maxRetries = 3
	backoff := 2 * time.Second

	// Use client-specific retry delay if available (for test mode support)
	if rc, ok := client.(retryableClient); ok {
		backoff = rc.RetryBaseDelay()
	}

	var lastErr error
	loopStart := time.Now()
	endpoint := endpointFromClient(client)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Notify on first retry only (across all retry phases)
			notifyFirstRetry(ctx, endpoint)

			providerLog.Warnf("retry after error: attempt=%d status=%s backoff=%s", attempt, lastErr.Error(), backoff.String())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		providerLog.Debugf("attempt_start: attempt=%d elapsed_total=%s", attempt, time.Since(loopStart))
		attemptStart := time.Now()
		resp, err := sendOnce(ctx, client, req, handler)
		attemptDur := time.Since(attemptStart)
		if err == nil {
			providerLog.Debugf("attempt_ok: attempt=%d duration=%s elapsed_total=%s", attempt, attemptDur, time.Since(loopStart))
			// Signal recovery if client supports it (Anthropic)
			if rc, ok := client.(retryableClient); ok {
				rc.OnRetrySuccess()
			}
			// Notify success if we retried
			notifySuccess(ctx)
			return resp, nil
		}
		lastErr = err
		providerLog.Debugf("attempt_fail: attempt=%d duration=%s error=%v elapsed_total=%s", attempt, attemptDur, err, time.Since(loopStart))

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return nil, err
		}

		// 429 rate limit: retry inline while the limit is short-lived
		// (honoring Retry-After). A long limit returns here so the caller's
		// rate-limit gate can defer and replay instead of blocking the turn.
		if apiErr.IsRateLimit() {
			wait, retry := inlineRateLimitWait(apiErr, backoff)
			if !retry {
				return nil, err
			}
			backoff = wait
			continue
		}

		if !apiErr.IsRetryable() {
			return nil, err
		}
	}

	providerLog.Debugf("exhausted_retries: elapsed_total=%s last_error=%v", time.Since(loopStart), lastErr)
	return nil, lastErr
}

// retryExtended handles extended retry logic for retryable server errors.
// Used for both 529 overload (long duration) and 5xx server errors (shorter duration).
// This function is only called for retryableClient implementations (type-asserted from Client).
func retryExtended(ctx context.Context, rc retryableClient, req *MessageRequest, handler *StreamHandler, maxDuration time.Duration) (*MessageResponse, error) {
	overloadBackoff := rc.OverloadBaseDelay()
	overloadStart := time.Now()
	recoverCh := rc.WaitForRecovery()
	var lastErr error
	endpoint := endpointFromClient(rc.(Client))

	for time.Since(overloadStart) < maxDuration {
		// Notify on first retry only (across all retry phases)
		notifyFirstRetry(ctx, endpoint)

		providerLog.Warnf("extended retry: backoff=%s elapsed=%s max=%s", overloadBackoff.String(), time.Since(overloadStart).String(), maxDuration.String())

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(overloadBackoff):
		case <-recoverCh:
			providerLog.Infof("recovery signal received, retrying immediately")
			recoverCh = rc.WaitForRecovery() // re-acquire for next iteration
		}

		resp, err := sendOnce(ctx, rc.(Client), req, handler)
		if err == nil {
			providerLog.Infof("recovered from extended retry: elapsed=%s", time.Since(overloadStart).String())
			rc.OnRetrySuccess()
			// Notify success if we retried
			notifySuccess(ctx)
			return resp, nil
		}
		lastErr = err

		var retryAPIErr *APIError
		if !errors.As(err, &retryAPIErr) || !retryAPIErr.IsRetryable() {
			return nil, err
		}

		overloadBackoff *= 2
	}

	providerLog.Warnf("extended retries exhausted: elapsed=%s", time.Since(overloadStart).String())
	return nil, lastErr
}

// sendOnce dispatches to streaming or non-streaming based on handler presence.
func sendOnce(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	if handler != nil {
		if sc, ok := client.(StreamingClient); ok {
			return sc.StreamMessage(ctx, req, handler)
		}
	}
	return client.SendMessage(ctx, req)
}
