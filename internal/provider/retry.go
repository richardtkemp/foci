package provider

import (
	"context"
	"errors"
	"log/slog"
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
	endpoint := "API" // generic endpoint name (clients could expose baseURL if needed)

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
	endpoint := "API"

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
			// For streaming, we need to call the underlying streamOnce method.
			// However, we can't access private methods. The StreamingClient.StreamMessage
			// already includes retry logic in Anthropic's case, so we need to call
			// a method that does a single attempt.
			//
			// PROBLEM: We can't call streamOnce directly from here because it's private.
			// We need to restructure this.
			//
			// Actually, looking at the plan again, the idea is that SendMessage/StreamMessage
			// in anthropic/client.go will be simplified to just call sendOnce/streamOnce.
			// Then provider.Send() will handle the retry logic.
			//
			// But we have a chicken-and-egg problem: we're in provider.Send() trying to
			// call the client, but the client needs to expose a non-retrying version.
			//
			// Let me re-read the plan... The plan says:
			// "Simplify SendMessage/StreamMessage - remove all retry logic"
			// "Just marshal and call sendOnce (no retry here)"
			//
			// So the client methods will be simplified to call sendOnce/streamOnce directly,
			// without retry. Then provider.Send() wraps them with retry.
			//
			// But that creates a problem: if client.SendMessage() calls sendOnce directly,
			// and provider.Send() calls client.SendMessage(), we're not adding retry - we're
			// just calling through.
			//
			// Actually, I think the issue is that we need TWO layers:
			// 1. sendOnce/streamOnce - single attempt (private methods in anthropic)
			// 2. SendMessage/StreamMessage - exposed to provider, calls sendOnce/streamOnce
			// 3. provider.Send() - wraps SendMessage/StreamMessage with retry
			//
			// But the current SendMessage already calls sendOnce internally. If we remove
			// retry from SendMessage, it becomes just a wrapper around sendOnce.
			//
			// Wait, I think I misunderstood the architecture. Let me re-think...
			//
			// Current flow:
			// - agent calls provider.Send(client, req, handler)
			// - provider.Send() calls client.SendMessage() or client.StreamMessage()
			// - client.SendMessage() has retry logic and calls sendOnce() repeatedly
			//
			// New flow should be:
			// - agent calls provider.Send(client, req, handler)
			// - provider.Send() has retry logic and calls client.SendMessage() repeatedly
			// - client.SendMessage() just calls sendOnce() once (no retry)
			//
			// So the change is:
			// - Move retry loop from client.SendMessage() to provider.Send()
			// - client.SendMessage() becomes a thin wrapper around sendOnce()
			//
			// This makes sense! So in provider.Send(), we'll call client.SendMessage()
			// or client.StreamMessage() (which now do single attempts), and wrap them
			// in retry logic here.
			//
			// So sendOnce() here in provider should just call client.SendMessage() or
			// client.StreamMessage() directly. Those methods will no longer retry.

			return sc.StreamMessage(ctx, req, handler)
		}
	}
	return client.SendMessage(ctx, req)
}
