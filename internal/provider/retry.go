package provider

import (
	"context"
	"errors"
	"log/slog"
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
// Uses the penultimate domain label: "api.openrouter.ai" -> "Openrouter".
func EndpointNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) >= 2 {
		name := parts[len(parts)-2]
		return strings.ToUpper(name[:1]) + name[1:]
	}
	return u.Host
}

// retryableClient is a type-assertion interface for clients that support
// extended overload retry logic (currently only Anthropic).
type retryableClient interface {
	OnRetrySuccess()
	WaitForRecovery() <-chan struct{}
	RetryBaseDelay() time.Duration       // base delay for phase 1 retries
	OverloadBaseDelay() time.Duration    // base delay for phase 2 retries
	OverloadMaxDuration() time.Duration  // max duration for phase 2 retries
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

			slog.Warn("provider: retry after error", "attempt", attempt, "status", lastErr.Error(), "backoff", backoff.String())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		slog.Debug("provider: attempt_start", "attempt", attempt, "elapsed_total", time.Since(loopStart))
		attemptStart := time.Now()
		resp, err := sendOnce(ctx, client, req, handler)
		attemptDur := time.Since(attemptStart)
		if err == nil {
			slog.Debug("provider: attempt_ok", "attempt", attempt, "duration", attemptDur, "elapsed_total", time.Since(loopStart))
			// Signal recovery if client supports it (Anthropic)
			if rc, ok := client.(retryableClient); ok {
				rc.OnRetrySuccess()
			}
			// Notify success if we retried
			notifySuccess(ctx)
			return resp, nil
		}
		lastErr = err
		slog.Debug("provider: attempt_fail", "attempt", attempt, "duration", attemptDur, "error", err, "elapsed_total", time.Since(loopStart))

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return nil, err
		}

		if !apiErr.IsRetryable() {
			return nil, err
		}
	}

	slog.Debug("provider: exhausted_retries", "elapsed_total", time.Since(loopStart), "last_error", lastErr)
	return nil, lastErr
}

// retryWithOverload handles extended overload retry logic for Anthropic's 529 errors.
// This function is only called for retryableClient implementations (type-asserted from Client).
func retryWithOverload(ctx context.Context, rc retryableClient, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	// Extended overload retry: ~2h production, scaled in tests
	overloadBackoff := rc.OverloadBaseDelay()
	maxDuration := rc.OverloadMaxDuration()
	overloadStart := time.Now()
	recoverCh := rc.WaitForRecovery()
	var lastErr error
	endpoint := endpointFromClient(rc.(Client))

	for time.Since(overloadStart) < maxDuration {
		// Notify on first retry only (across all retry phases)
		notifyFirstRetry(ctx, endpoint)

		slog.Warn("provider: overload retry", "backoff", overloadBackoff.String(), "elapsed", time.Since(overloadStart).String(), "max", maxDuration.String())

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(overloadBackoff):
		case <-recoverCh:
			slog.Info("provider: recovery signal received, retrying immediately")
			recoverCh = rc.WaitForRecovery() // re-acquire for next iteration
		}

		resp, err := sendOnce(ctx, rc.(Client), req, handler)
		if err == nil {
			slog.Info("provider: recovered from overload", "elapsed", time.Since(overloadStart).String())
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

	slog.Warn("provider: overload retries exhausted", "elapsed", time.Since(overloadStart).String())
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
