package ccstream

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// realGetUsageResponse is a real get_usage control_response payload (the
// `response.response` field), captured live 2026-07-17 against a
// comfortably-below-threshold account (seven_day well under any warning
// level) — the exact case the passive rate_limit_event stream can't surface,
// which is why QueryUsage exists. Trimmed of fields foci doesn't model.
const realGetUsageResponse = `{
  "session": {"total_cost_usd": 0.0065691, "total_api_duration_ms": 4653, "total_duration_ms": 22263, "model_usage": {}},
  "subscription_type": "max",
  "rate_limits_available": true,
  "rate_limits": {
    "five_hour": {"utilization": 94, "resets_at": "2026-07-17T20:29:59.639022+00:00"},
    "seven_day": {"utilization": 43, "resets_at": "2026-07-19T22:59:59.639046+00:00"},
    "limits": [
      {"kind": "session", "group": "session", "percent": 94, "severity": "critical", "is_active": true}
    ]
  },
  "behaviors": {
    "day": {"request_count": 4152, "session_count": 97, "behaviors": [{"key": "long_context", "pct": 89, "count": 2838}, {"key": "cron", "pct": 38, "count": 2}]},
    "week": {"request_count": 26660, "session_count": 464, "behaviors": [{"key": "long_context", "pct": 85, "count": 16990}]}
  }
}`

func TestParseUsagePayload(t *testing.T) {
	info, err := parseUsagePayload(json.RawMessage(realGetUsageResponse))
	if err != nil {
		t.Fatalf("parseUsagePayload: %v", err)
	}
	if info.SubscriptionType != "max" {
		t.Errorf("SubscriptionType = %q, want %q", info.SubscriptionType, "max")
	}
	if info.FiveHour.Percent != 94 {
		t.Errorf("FiveHour.Percent = %d, want 94", info.FiveHour.Percent)
	}
	wantReset := time.Date(2026, 7, 17, 20, 29, 59, 639022000, time.UTC)
	if !info.FiveHour.ResetsAt.Equal(wantReset) {
		t.Errorf("FiveHour.ResetsAt = %v, want %v", info.FiveHour.ResetsAt, wantReset)
	}
	// The case this whole feature exists for: a comfortably-below-threshold
	// percentage must come through, not just near-limit values.
	if info.SevenDay.Percent != 43 {
		t.Errorf("SevenDay.Percent = %d, want 43 (must work well within 'allowed' range)", info.SevenDay.Percent)
	}
	if info.SessionCostUSD != 0.0065691 {
		t.Errorf("SessionCostUSD = %v, want 0.0065691", info.SessionCostUSD)
	}
	if info.Day.RequestCount != 4152 || info.Day.SessionCount != 97 {
		t.Errorf("Day = %+v, want RequestCount=4152 SessionCount=97", info.Day)
	}
	if len(info.Day.Top) != 2 || info.Day.Top[0].Key != "long_context" || info.Day.Top[0].Pct != 89 {
		t.Errorf("Day.Top = %+v, want first item long_context/89", info.Day.Top)
	}
	if len(info.Week.Top) != 1 || info.Week.Top[0].Count != 16990 {
		t.Errorf("Week.Top = %+v, want one item with Count=16990", info.Week.Top)
	}
	if len(info.Raw) == 0 {
		t.Error("Raw is empty, want the original payload preserved")
	}
}

func TestParseUsagePayload_MissingResetsAt(t *testing.T) {
	// A window with no/unparseable resets_at must not error the whole parse —
	// ResetsAt just stays the zero Time (formatUsage renders "unknown").
	raw := `{"subscription_type":"max","rate_limits":{"five_hour":{"utilization":5},"seven_day":{"utilization":5,"resets_at":"not-a-time"}}}`
	info, err := parseUsagePayload(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parseUsagePayload: %v", err)
	}
	if !info.FiveHour.ResetsAt.IsZero() {
		t.Errorf("FiveHour.ResetsAt = %v, want zero (no resets_at supplied)", info.FiveHour.ResetsAt)
	}
	if !info.SevenDay.ResetsAt.IsZero() {
		t.Errorf("SevenDay.ResetsAt = %v, want zero (unparseable resets_at)", info.SevenDay.ResetsAt)
	}
}

func TestParseUsagePayload_MalformedJSON(t *testing.T) {
	if _, err := parseUsagePayload(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
}

// TestWaitForControlResponseRaw_Success verifies the happy path: a
// control_response matching reqID with subtype=success returns its payload.
func TestWaitForControlResponseRaw_Success(t *testing.T) {
	resCh := make(chan json.RawMessage, 1)
	readerDone := make(chan struct{})
	resCh <- json.RawMessage(`{"type":"control_response","response":{"subtype":"success","request_id":"req-1","response":{"ok":true}}}`)

	raw, err := waitForControlResponseRaw(context.Background(), resCh, readerDone, "req-1")
	if err != nil {
		t.Fatalf("waitForControlResponseRaw: %v", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("raw = %s, want {\"ok\":true}", raw)
	}
}

// TestWaitForControlResponseRaw_IgnoresMismatchedRequestID proves a response
// for a DIFFERENT in-flight request (e.g. the initialize handshake's own ack,
// arriving after get_usage was sent) is skipped rather than misread as the
// answer — the bug this whole wait-by-reqID design exists to avoid.
func TestWaitForControlResponseRaw_IgnoresMismatchedRequestID(t *testing.T) {
	resCh := make(chan json.RawMessage, 2)
	readerDone := make(chan struct{})
	resCh <- json.RawMessage(`{"type":"control_response","response":{"subtype":"success","request_id":"other-req","response":{"wrong":true}}}`)
	resCh <- json.RawMessage(`{"type":"control_response","response":{"subtype":"success","request_id":"req-1","response":{"right":true}}}`)

	raw, err := waitForControlResponseRaw(context.Background(), resCh, readerDone, "req-1")
	if err != nil {
		t.Fatalf("waitForControlResponseRaw: %v", err)
	}
	if string(raw) != `{"right":true}` {
		t.Errorf("raw = %s, want the req-1 response, not the mismatched one", raw)
	}
}

func TestWaitForControlResponseRaw_ErrorSubtype(t *testing.T) {
	resCh := make(chan json.RawMessage, 1)
	readerDone := make(chan struct{})
	resCh <- json.RawMessage(`{"type":"control_response","response":{"subtype":"error","request_id":"req-1","error":"boom"}}`)

	_, err := waitForControlResponseRaw(context.Background(), resCh, readerDone, "req-1")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want %q", err, "boom")
	}
}

func TestWaitForControlResponseRaw_ReaderDone(t *testing.T) {
	resCh := make(chan json.RawMessage)
	readerDone := make(chan struct{})
	close(readerDone) // claude exited without ever responding

	_, err := waitForControlResponseRaw(context.Background(), resCh, readerDone, "req-1")
	if err == nil {
		t.Fatal("expected an error when the reader goroutine exits first, got nil")
	}
}

func TestWaitForControlResponseRaw_ContextCancelled(t *testing.T) {
	resCh := make(chan json.RawMessage)
	readerDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForControlResponseRaw(ctx, resCh, readerDone, "req-1")
	if err == nil {
		t.Fatal("expected ctx.Err(), got nil")
	}
}
