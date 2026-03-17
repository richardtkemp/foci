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

	a.logger().Infof("session=%s stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f",
		sessionKey, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens, cost)

	sessionFile := ""
	if a.Sessions != nil {
		if p, err := a.Sessions.SessionPath(sessionKey); err == nil {
			sessionFile = p
		}
	}
	log.API(log.APIEntry{
		Timestamp:   start.UTC(),
		Provider:    a.SessionFormat(sessionKey),
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
		reqJSON := resp.WireRequest
		if reqJSON == nil {
			reqJSON, _ = json.Marshal(req)
		}
		respJSON, _ := json.Marshal(resp)

		// Increment per-session sequence number.
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.apiSeqNum++
		seqNum := sm.apiSeqNum
		a.metaMu.Unlock()

		// Hash system block texts for cache-bust detection.
		sysTexts := make([]string, len(req.System))
		for i, b := range req.System {
			sysTexts[i] = b.Text
		}

		log.Payload(log.PayloadEntry{
			Timestamp:  start.UTC(),
			Session:    sessionKey,
			SeqNum:     seqNum,
			Model:      model,
			SystemHash: log.SystemHash(sysTexts),
			Request:    reqJSON,
			Response:   respJSON,
			DurationMS: duration.Milliseconds(),
		})
	}

	return cost
}

// logErrorPayload logs the full request payload when an API call fails.
// Requires full_payload = true in config.
func (a *Agent) logErrorPayload(sessionKey, model string, start time.Time, duration time.Duration, req *provider.MessageRequest, apiErr error) {
	if !log.PayloadEnabled() {
		return
	}
	reqJSON, _ := json.Marshal(req)


	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.apiSeqNum++
	seqNum := sm.apiSeqNum
	a.metaMu.Unlock()

	sysTexts := make([]string, len(req.System))
	for i, b := range req.System {
		sysTexts[i] = b.Text
	}

	log.Payload(log.PayloadEntry{
		Timestamp:  start.UTC(),
		Session:    sessionKey,
		SeqNum:     seqNum,
		Model:      model,
		SystemHash: log.SystemHash(sysTexts),
		Request:    reqJSON,
		Error:      apiErr.Error(),
		DurationMS: duration.Milliseconds(),
	})
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

		a.logger().Infof("session=%s rate limit gate (%s) closed until %s", sessionKey, endpoint, resetTime.Format(time.Kitchen))
		if !isUserTrigger(TriggerFromContext(ctx)) {
			for _, fn := range a.RateLimitFunc {
				fn(resetTime)
			}
		}
		return &RateLimitedError{Until: resetTime}
	}
	if apiErr.IsOverloaded() {
		return fmt.Errorf("API is overloaded — try again shortly")
	}
	if apiErr.IsRetryable() {
		a.logger().Debugf("session=%s server error detail: %s", sessionKey, err)
		return fmt.Errorf("API is temporarily unavailable, try again in a few minutes")
	}
	return fmt.Errorf("send message: %w", err)
}
