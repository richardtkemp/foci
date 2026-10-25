package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
)

// These tests pin #2287's error-side rules: a 429 closes the gate of the
// endpoint that RETURNED it — the hop's, not the primary's — the ledger turn
// row's FinalModel names the model that served the turn's LAST response, and
// every user-visible naming of a closed gate (the returned error, the
// scheduler-facing reason) and of a failed request (the error payload log)
// names the endpoint or model that was actually involved. They reuse the
// #2246 fixtures (fallbackAgent, openAIFallbackFn, selfRetryingTestClient).
// None run in parallel: each makes a ledger or the payload log the live one.

// fb429Client scripts an always-429 hop client.
func fb429Client() selfRetryingTestClient {
	return selfRetryingTestClient{newTestClientWithError(
		func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
			return nil, &provider.APIError{StatusCode: 429, RetryAfter: "300", Body: "rate limited"}
		})}
}

// TestFallbackTurn429OnHopClosesHopEndpointGate proves the gate pairing on
// the failure side: a turn whose primary 529s and whose fallback hop (own
// client, different endpoint) answers 429 closes the FALLBACK endpoint's
// gate — the endpoint that limited — leaves the primary's open, and surfaces
// the usual *RateLimitedError to the user, naming the endpoint that limited.
func TestFallbackTurn429OnHopClosesHopEndpointGate(t *testing.T) {
	t.Parallel()
	ag := fallbackAgent(t, fbStatusClient(529), tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fb429Client(),
	}})

	ctx := WithTrigger(context.Background(), "user")
	_, err := ag.hmTest(ctx, "test/ifh-a", "Hello")
	if err == nil {
		t.Fatal("expected the hop's 429 to fail the turn")
	}
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if !strings.Contains(rlErr.Error(), "rate limited on openai") {
		t.Errorf("user-facing error should name the endpoint that limited, got: %s", rlErr.Error())
	}

	if limited, _ := ag.getOrCreateRateLimitGate("openai").IsLimited(); !limited {
		t.Error(`"openai" gate open after the fallback endpoint answered 429, want closed (it limited)`)
	}
	if limited, _ := ag.getOrCreateRateLimitGate("anthropic").IsLimited(); limited {
		t.Error(`"anthropic" gate closed by a fallback hop's 429, want open (the primary never answered 429)`)
	}
}

// TestFallbackTurn429OnPrimaryStillClosesPrimaryGate is the characterisation
// of the no-fallback arm: a 429 on the primary alone (a fallback is
// configured, but 429 is not fallback-eligible, so no hop runs) still closes
// only the primary's gate — the new hop pairing must not disturb it.
func TestFallbackTurn429OnPrimaryStillClosesPrimaryGate(t *testing.T) {
	t.Parallel()
	ag := fallbackAgent(t, fb429Client(), tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fbEndTurnClient("msg_fh_never"),
	}})

	ctx := WithTrigger(context.Background(), "user")
	_, err := ag.hmTest(ctx, "test/ifh-b", "Hello")
	if err == nil {
		t.Fatal("expected the primary's 429 to fail the turn")
	}
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}

	if limited, _ := ag.getOrCreateRateLimitGate("anthropic").IsLimited(); !limited {
		t.Error(`"anthropic" gate open after the primary answered 429, want closed`)
	}
	if limited, _ := ag.getOrCreateRateLimitGate("openai").IsLimited(); limited {
		t.Error(`"openai" gate closed though its endpoint was never contacted, want open`)
	}
}

// TestFallbackTurnRecordsServingModelAsFinalModel proves the ledger turn row
// names the model that served: a turn every response of which a fallback hop
// served records FinalModel = the hop's model, not the turn-start model.
func TestFallbackTurnRecordsServingModelAsFinalModel(t *testing.T) {
	ledger, path := openPathTestLedger(t)
	ag := fallbackAgent(t, fb503Client(), tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fbEndTurnClient("msg_fh_c"),
	}})

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifh-c", "Hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	if got := ledgerFinalModel(t, ledger, path, "test/ifh-c"); got != "openai/gpt-5.6" {
		t.Errorf("final_model = %q, want the serving model %q", got, "openai/gpt-5.6")
	}
}

// TestMixedTurnRecordsLastServingModelAsFinalModel proves the LAST-response
// rule on a mixed turn: the first iteration is served by the primary
// (tool_use), the second falls back to the hop (end_turn) — FinalModel names
// the hop, the model of the turn's last response.
func TestMixedTurnRecordsLastServingModelAsFinalModel(t *testing.T) {
	ledger, path := openPathTestLedger(t)
	var calls atomic.Int32
	primary := selfRetryingTestClient{newTestClientWithError(
		func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
			if calls.Add(1) == 1 {
				return &provider.MessageResponse{
					ID: "msg_fh_d1", Type: "message", Role: "assistant",
					Content: []provider.ContentBlock{{
						Type: "tool_use", ID: "tu_fh_d", Name: "nonexistent_tool", Input: json.RawMessage(`{}`),
					}},
					StopReason: "tool_use",
				}, nil
			}
			return nil, &provider.APIError{StatusCode: 503, Body: "unavailable"}
		})}
	ag := fallbackAgent(t, primary, tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fbEndTurnClient("msg_fh_d2"),
	}})

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifh-d", "Hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	if got := ledgerFinalModel(t, ledger, path, "test/ifh-d"); got != "openai/gpt-5.6" {
		t.Errorf("final_model = %q, want the last response's model %q", got, "openai/gpt-5.6")
	}
}

// TestFailedTurnRecordsTurnModelAsFinalModel pins the no-response arm: a turn
// that errors before any response came back (primary 503, hop 429) keeps the
// turn-start model as FinalModel — before the change because it always wrote
// TurnModel, after it because LastServedModel is empty and falls back.
func TestFailedTurnRecordsTurnModelAsFinalModel(t *testing.T) {
	ledger, path := openPathTestLedger(t)
	ag := fallbackAgent(t, fb503Client(), tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fb429Client(),
	}})

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifh-e", "Hello"); err == nil {
		t.Fatal("expected the hop's 429 to fail the turn")
	}

	if got := ledgerFinalModel(t, ledger, path, "test/ifh-e"); got != "claude-haiku-4-5" {
		t.Errorf("final_model = %q, want the turn-start model %q (no response arrived)", got, "claude-haiku-4-5")
	}
}

// TestGateBlockedTurnErrorNamesEndpoint proves the other construction of the
// user-facing error names the endpoint too: a non-user turn refused by an
// already-closed gate gets the usual *RateLimitedError, and its text names
// the endpoint whose gate closed — the same rendering the 429 path gives.
func TestGateBlockedTurnErrorNamesEndpoint(t *testing.T) {
	t.Parallel()
	ag := fallbackAgent(t, fbEndTurnClient("msg_ifh_f"), tupleClientProvider{})
	ag.getOrCreateRateLimitGate("anthropic").Close(time.Now().Add(1 * time.Hour))

	// "keepalive" is a system trigger: the gate refuses it instead of letting
	// it through as a recovery probe.
	_, err := ag.hmTest(WithTrigger(context.Background(), "keepalive"), "test/ifh-f", "Hello")
	if err == nil {
		t.Fatal("expected the closed gate to refuse the system turn")
	}
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if !strings.Contains(rlErr.Error(), "rate limited on anthropic") {
		t.Errorf("gate-block error should name the endpoint whose gate closed, got: %s", rlErr.Error())
	}
}

// TestFailedTurnErrorPayloadNamesFailingModel proves the error payload log
// labels the failed request with the model being tried when it failed: a
// primary 503 whose fallback hop answers 429 logs the HOP's model and status,
// not the turn-start model — and the hop's plain-text error body survives in
// the entry (quoted), instead of silently dropping the whole record. Flips
// the global payload log on for the turn and off again in cleanup — safe
// because package tests run one at a time.
func TestFailedTurnErrorPayloadNamesFailingModel(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "payload.jsonl")
	if err := log.Init(log.Config{Level: "INFO", PayloadFile: payloadPath, FullPayload: true}); err != nil {
		t.Fatalf("init payload log: %v", err)
	}
	t.Cleanup(func() {
		if err := log.Init(log.Config{Level: "INFO"}); err != nil {
			t.Fatalf("restore payload log: %v", err)
		}
	})

	ag := fallbackAgent(t, fb503Client(), tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fb429Client(),
	}})

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifh-g", "Hello"); err == nil {
		t.Fatal("expected the hop's 429 to fail the turn")
	}

	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("payload log lines = %d, want exactly 1 (the turn's failed request): %q", len(lines), string(data))
	}
	var entry struct {
		Model        string          `json:"model"`
		StatusCode   int             `json:"status_code"`
		ResponseBody json.RawMessage `json:"response_body"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal payload entry: %v (line: %s)", err, lines[0])
	}
	if entry.Model != "openai/gpt-5.6" {
		t.Errorf("payload model = %q, want the failing hop's model %q", entry.Model, "openai/gpt-5.6")
	}
	if entry.StatusCode != 429 {
		t.Errorf("payload status_code = %d, want the hop's 429", entry.StatusCode)
	}
	var body string
	if err := json.Unmarshal(entry.ResponseBody, &body); err != nil || body != "rate limited" {
		t.Errorf("payload response_body = %s, want the hop's plain-text body \"rate limited\" quoted", entry.ResponseBody)
	}
}
