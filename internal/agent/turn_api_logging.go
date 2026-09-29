package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/provider"
	"foci/internal/ratelimit"
	"foci/internal/session"
)

// logAPIResponse logs usage, cost, and optionally the full request/response payload.
func (a *Agent) logAPIResponse(ts *TurnState, model string, start time.Time, duration time.Duration, req *provider.MessageRequest, resp *provider.MessageResponse, msgCount int) float64 {
	sessionKey := ts.SessionKey
	cost, _ := modelinfo.CostAsOf(model, time.Now(), resp.Usage.Tokens())

	a.logger().Infof("session=%s stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f",
		sessionKey, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens, cost)

	sessionFile := ""
	if a.Sessions != nil {
		if p, err := a.Sessions.SessionPath(sessionKey); err == nil {
			sessionFile = p
		}
	}
	// One response is one call, booked on its turn (#2111 P2: the direct API
	// was the first backend on the per-call ledger).
	turn := a.ledgerTurn(ts)
	if turn.TurnID == "" {
		turn.TurnID, turn.StartedAt = accounting.MintTurnID(sessionKey, accounting.KindCall, start), start
	}
	if err := accounting.Record(turn, accounting.APIResponse{
		ID: resp.ID, Kind: accounting.KindCall, Provider: a.SessionFormat(sessionKey), Model: model,
		Session: sessionKey, AgentID: turn.AgentID, TurnID: turn.TurnID,
		Start: start, Duration: duration, Tokens: resp.Usage.Tokens(), StopReason: resp.StopReason,
		SessionFile: sessionFile,
		SessionLine: msgCount + 2, // +2 for the user message and assistant response being appended
	}.Call()); err != nil {
		a.logger().Errorf("session=%s book API call: %v", sessionKey, err)
	}

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
			Timestamp:  start,
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
	// Prefer wire request (SDK-serialized) over internal request.
	var reqJSON json.RawMessage
	var perr *provider.APIError
	if errors.As(apiErr, &perr) && perr.WireRequest != nil {
		reqJSON = perr.WireRequest
	} else {
		reqJSON, _ = json.Marshal(req)
	}

	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.apiSeqNum++
	seqNum := sm.apiSeqNum
	a.metaMu.Unlock()

	sysTexts := make([]string, len(req.System))
	for i, b := range req.System {
		sysTexts[i] = b.Text
	}

	entry := log.PayloadEntry{
		Timestamp:  start,
		Session:    sessionKey,
		SeqNum:     seqNum,
		Model:      model,
		SystemHash: log.SystemHash(sysTexts),
		Request:    reqJSON,
		Error:      apiErr.Error(),
		DurationMS: duration.Milliseconds(),
	}

	if perr != nil {
		entry.StatusCode = perr.StatusCode
		entry.ResponseBody = json.RawMessage(perr.Body)
		entry.RequestID = perr.RequestID()
	}

	log.Payload(entry)
}

// recordAPITurnEnd closes a direct-API turn in the ledger. Its spend is final
// with its last response, so its activity closes when it ends (#2111 R8).
func (a *Agent) recordAPITurnEnd(ts *TurnState, stopReason string) {
	t := a.ledgerTurn(ts)
	if t.TurnID == "" {
		return
	}
	now := time.Now()
	t.EndedAt, t.ActivityClosedAt, t.StopReason, t.FinalModel = now, now, stopReason, ts.TurnModel
	if err := accounting.RecordTurn(t); err != nil {
		a.logger().Errorf("session=%s record turn end: %v", ts.SessionKey, err)
	}
}

// recordAPITurnActivity moves a direct-API turn's activity close to now: its
// post-turn compaction, booked on the turn, spends after the turn has ended.
func (a *Agent) recordAPITurnActivity(ts *TurnState) {
	t := a.ledgerTurn(ts)
	if t.TurnID == "" {
		return
	}
	t.ActivityClosedAt = time.Now()
	if err := accounting.RecordTurn(t); err != nil {
		a.logger().Errorf("session=%s record turn activity: %v", ts.SessionKey, err)
	}
}

// ledgerTurn is the ledger's record of a direct-API turn as it starts; the
// id is "" for a TurnState with no identity yet.
func (a *Agent) ledgerTurn(ts *TurnState) accounting.Turn {
	return accounting.Turn{
		TurnID: ts.RowID(), Session: ts.SessionKey, AgentID: session.AgentIDFromKey(ts.SessionKey),
		Backend: accounting.BackendAPI, Source: ledgerTurnSource(ts), Purpose: ts.Purpose,
		StartedAt: ts.StartedAt,
	}
}

// ledgerTurnSource says what started a turn, as the ledger's turns table
// records it.
func ledgerTurnSource(ts *TurnState) string {
	switch {
	case ts.Purpose != "":
		return accounting.SourceBatch
	case ts.Trigger == "keepalive":
		return accounting.SourceKeepalive
	case isUserTrigger(ts.Trigger):
		return accounting.SourceUser
	}
	return accounting.SourceAutonomous
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
		signal := ratelimit.Signal{
			Kind:       ratelimit.KindRequest,
			RetryAfter: time.Duration(apiErr.RetryAfterSeconds()) * time.Second,
			Detail:     apiErr.Body,
		}
		resetTime := a.engageRateLimit(endpoint, signal, !isUserTrigger(TriggerFromContext(ctx)))
		a.logger().Infof("session=%s API rate limit recorded for %s", sessionKey, endpoint)
		return &RateLimitedError{Until: resetTime}
	}
	if apiErr.IsOverloaded() {
		return fmt.Errorf("API is overloaded (HTTP %d) — try again shortly", apiErr.StatusCode)
	}
	if apiErr.IsRetryable() {
		a.logger().Debugf("session=%s server error detail: %s", sessionKey, err)
		return fmt.Errorf("API is temporarily unavailable (HTTP %d), try again in a few minutes", apiErr.StatusCode)
	}
	return fmt.Errorf("send message: %w", err)
}
