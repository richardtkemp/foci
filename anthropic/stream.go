package anthropic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"foci/provider"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

// Compile-time check: Client implements provider.StreamingClient.
var _ provider.StreamingClient = (*Client)(nil)

// StreamMessage sends a streaming message request and returns the accumulated response.
// Delta callbacks in handler are invoked as content arrives. The full response is
// returned once the stream completes.
//
// Uses the same two-phase retry logic as SendMessage:
//   - Phase 1: retries pre-stream errors (connection failures, 5xx before any data).
//   - Phase 2: extended 529 overload retries with cross-goroutine recovery.
//   - Mid-stream errors are NOT retried (deltas already emitted to caller).
//
// Requires useSDK=true. Returns an error if called with useSDK=false.
func (c *Client) StreamMessage(ctx context.Context, req *MessageRequest, handler *provider.StreamHandler) (*MessageResponse, error) {
	if !c.useSDK {
		return nil, fmt.Errorf("streaming requires SDK transport (use_sdk = true)")
	}

	// Phase 1: standard retries for all retryable errors.
	const maxRetries = 3
	backoff := c.retryBaseDelay
	if backoff == 0 {
		backoff = 2 * time.Second
	}

	var lastErr error
	loopStart := time.Now()
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			slog.Warn("anthropic: stream retrying after error", "attempt", attempt, "status", lastErr.Error(), "backoff", backoff.String())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		slog.Debug("anthropic: stream_attempt_start", "attempt", attempt, "elapsed_total", time.Since(loopStart))
		attemptStart := time.Now()
		resp, err := c.streamOnce(ctx, req, handler)
		attemptDur := time.Since(attemptStart)
		if err == nil {
			slog.Debug("anthropic: stream_attempt_ok", "attempt", attempt, "duration", attemptDur, "elapsed_total", time.Since(loopStart))
			c.signalRecovery()
			return resp, nil
		}
		lastErr = err
		slog.Debug("anthropic: stream_attempt_fail", "attempt", attempt, "duration", attemptDur, "error", err, "elapsed_total", time.Since(loopStart))

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return nil, err
		}

		if !apiErr.IsRetryable() {
			return nil, err
		}
	}

	slog.Debug("anthropic: stream_exhausted_retries", "elapsed_total", time.Since(loopStart), "last_error", lastErr)

	// Phase 2: extended overload retries (529 only).
	var apiErr *APIError
	if !errors.As(lastErr, &apiErr) || !apiErr.IsOverloaded() {
		return nil, lastErr
	}

	overloadBackoff := c.overloadBaseDelay()
	maxDuration := c.overloadMaxDuration()
	overloadStart := time.Now()
	recoverCh := c.enterOverload()

	for time.Since(overloadStart) < maxDuration {
		slog.Warn("anthropic: stream overload retry", "backoff", overloadBackoff.String(), "elapsed", time.Since(overloadStart).String(), "max", maxDuration.String())

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(overloadBackoff):
		case <-recoverCh:
			slog.Info("anthropic: stream overload recovery signal received, retrying immediately")
			recoverCh = c.enterOverload()
		}

		resp, err := c.streamOnce(ctx, req, handler)
		if err == nil {
			slog.Info("anthropic: stream recovered from overload", "elapsed", time.Since(overloadStart).String())
			c.signalRecovery()
			return resp, nil
		}
		lastErr = err

		var retryAPIErr *APIError
		if !errors.As(err, &retryAPIErr) || !retryAPIErr.IsRetryable() {
			return nil, err
		}

		overloadBackoff *= 2
	}

	slog.Warn("anthropic: stream overload retries exhausted", "elapsed", time.Since(overloadStart).String())
	return nil, lastErr
}

// streamOnce performs a single streaming request. Returns the accumulated response.
// Errors that occur before any deltas are emitted are retryable (pre-stream).
// Errors after deltas have been emitted are returned as-is (mid-stream, not retryable).
func (c *Client) streamOnce(ctx context.Context, req *MessageRequest, handler *provider.StreamHandler) (*MessageResponse, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	params := buildSDKParams(req)
	sc := c.ensureSDKClient()

	slog.Debug("anthropic: stream_call_start", "model", req.Model)

	stream := sc.Messages.NewStreaming(ctx, params, sdkRequestOptions(token)...)

	var msg sdk.Message
	deltasEmitted := false

	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			slog.Warn("anthropic: stream accumulate error", "error", err)
		}

		// Fire delta callbacks.
		if event.Type == "content_block_delta" {
			switch event.Delta.Type {
			case "text_delta":
				if handler != nil && handler.OnTextDelta != nil && event.Delta.Text != "" {
					deltasEmitted = true
					handler.OnTextDelta(event.Delta.Text)
				}
			case "thinking_delta":
				if handler != nil && handler.OnThinkingDelta != nil && event.Delta.Thinking != "" {
					deltasEmitted = true
					handler.OnThinkingDelta(event.Delta.Thinking)
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		sdkErr := classifySDKError(err)
		if deltasEmitted {
			// Mid-stream error: deltas already emitted, can't retry.
			// Wrap so callers know it's a stream error.
			return nil, fmt.Errorf("mid-stream error (deltas already emitted): %w", sdkErr)
		}
		return nil, sdkErr
	}

	return responseFromSDK(&msg), nil
}
