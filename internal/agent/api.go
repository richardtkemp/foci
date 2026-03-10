package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
)

// logAPIResponse logs usage, cost, and optionally the full request/response payload.
func (a *Agent) logAPIResponse(sessionKey, model string, start time.Time, duration time.Duration, req *provider.MessageRequest, resp *provider.MessageResponse, msgCount int) float64 {
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	a.logger().Infof("stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f",
		resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens, cost)

	sessionFile := ""
	if a.Sessions != nil {
		if p, err := a.Sessions.SessionPath(sessionKey); err == nil {
			sessionFile = p
		}
	}
	log.API(log.APIEntry{
		Timestamp:   start.UTC(),
		Session:     sessionKey,
		Model:       model,
		Input:       resp.Usage.InputTokens,
		Output:      resp.Usage.OutputTokens,
		CacheRead:   resp.Usage.CacheReadInputTokens,
		CacheWrite:  resp.Usage.CacheCreationInputTokens,
		CostUSD:     cost,
		DurationMS:  duration.Milliseconds(),
		StopReason:  resp.StopReason,
		CallType:    "conversation",
		SessionFile: sessionFile,
		SessionLine: msgCount + 2, // +2 for the user message and assistant response being appended
	})

	if log.PayloadEnabled() {
		reqJSON, _ := json.Marshal(req)
		respJSON, _ := json.Marshal(resp)
		log.Payload(log.PayloadEntry{
			Timestamp:  start.UTC(),
			Session:    sessionKey,
			Model:      model,
			Request:    reqJSON,
			Response:   respJSON,
			DurationMS: duration.Milliseconds(),
		})
	}

	return cost
}

// classifyAPIError maps API errors to user-friendly messages, notifying
// rate limit and server error callbacks as appropriate.
func (a *Agent) classifyAPIError(ctx context.Context, err error, sessionKey string, endpoint string, duration time.Duration) error {
	if ctx.Err() != nil {
		a.logger().Debugf("api_call_ctx_cancelled session=%s ctx_err=%v duration=%s", sessionKey, ctx.Err(), duration)
		return ctx.Err()
	}
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("send message: %w", err)
	}
	if apiErr.IsRateLimit() {
		gate := a.getOrCreateRateLimitGate(endpoint)
		resetTime := gate.ComputeResetTime(apiErr.RetryAfterSeconds())
		gate.Close(resetTime)

		a.logger().Infof("rate limit gate (%s) closed until %s", endpoint, resetTime.Format(time.Kitchen))
		if a.RateLimitFunc != nil && !isUserTrigger(TriggerFromContext(ctx)) {
			a.RateLimitFunc(apiErr.RetryAfterSeconds())
		}
		return &RateLimitedError{Until: resetTime}
	}
	if apiErr.IsOverloaded() {
		return fmt.Errorf("API is overloaded — try again shortly")
	}
	if apiErr.IsRetryable() {
		a.logger().Debugf("server error detail: %s", err)
		if a.RateLimitFunc != nil {
			a.RateLimitFunc(0)
		}
		return fmt.Errorf("API is temporarily unavailable, try again in a few minutes")
	}
	return fmt.Errorf("send message: %w", err)
}
