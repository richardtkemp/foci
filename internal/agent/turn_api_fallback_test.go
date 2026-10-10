package agent

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// These tests pin #2246: after a model fallback, the turn's success path
// pairs with the tuple that SERVED — the fallback endpoint's gate is
// released, the primary's is not, and the ledger books the serving model and
// format — while the turn snapshot itself stays on the primary, so the next
// tool-loop iteration tries the primary first again. None run in parallel:
// each makes a ledger the live one.

// selfRetryingTestClient makes a scripted *testClient handle its own retries,
// so provider.sendWithRetry sends exactly once per attempt and a scripted 503
// falls straight through to the fallback chain instead of backing off.
type selfRetryingTestClient struct{ *testClient }

func (selfRetryingTestClient) HandlesOwnRetries() bool { return true }

// fb503Client scripts an always-503 primary.
func fb503Client() selfRetryingTestClient {
	return selfRetryingTestClient{newTestClientWithError(
		func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
			return nil, &provider.APIError{StatusCode: 503, Body: "unavailable"}
		})}
}

// fbEndTurnClient scripts a single end_turn response.
func fbEndTurnClient(id string) selfRetryingTestClient {
	return selfRetryingTestClient{newTestClient(func(_ *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID: id, Type: "message", Role: "assistant",
			Content:    provider.TextContent("served by fallback"),
			StopReason: "end_turn",
		}
	})}
}

// openAIFallbackFn falls back from the anthropic primary to openai/gpt-5.6
// on the openai endpoint and format.
func openAIFallbackFn(model string) (string, string, string, bool) {
	if model == "claude-haiku-4-5" {
		return "openai/gpt-5.6", "openai", "openai", true
	}
	return "", "", "", false
}

// fallbackAgent builds the anthropic-primary agent the cases below share.
func fallbackAgent(t *testing.T, primary provider.Client, cp provider.ClientProvider) *Agent {
	t.Helper()
	return &Agent{
		Client:         primary,
		Sessions:       session.NewStore(t.TempDir()),
		Tools:          tools.NewRegistry(),
		Bootstrap:      workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:          "claude-haiku-4-5",
		Endpoint:       "anthropic",
		Format:         "anthropic",
		FallbackFunc:   openAIFallbackFn,
		ClientProvider: cp,
	}
}

// TestFallbackTurnReleasesAndBooksServingEndpoint proves the gate pairing:
// with both endpoint gates closed, a turn whose primary 503s and whose
// fallback hop (own client) answers releases the OPENAI gate, leaves the
// ANTHROPIC gate closed — the primary just failed — and books the call as
// Provider "openai", Model "openai/gpt-5.6".
func TestFallbackTurnReleasesAndBooksServingEndpoint(t *testing.T) {
	ledger := openTestLedger(t)
	ag := fallbackAgent(t, fb503Client(), tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fbEndTurnClient("msg_fb_a"),
	}})

	// Both gates closed; a USER trigger may probe through the primary's.
	ag.getOrCreateRateLimitGate("anthropic").Close(time.Now().Add(1 * time.Hour))
	ag.getOrCreateRateLimitGate("openai").Close(time.Now().Add(1 * time.Hour))

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifb-a", "Hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	if limited, _ := ag.getOrCreateRateLimitGate("openai").IsLimited(); limited {
		t.Error(`"openai" gate still closed after the fallback endpoint served, want released`)
	}
	if limited, _ := ag.getOrCreateRateLimitGate("anthropic").IsLimited(); !limited {
		t.Error(`"anthropic" gate released by a fallback-served turn whose primary 503s, want still closed`)
	}
	calls := ledgerCalls(t, ledger)
	if len(calls) == 0 {
		t.Fatal("no calls booked in the ledger")
	}
	if last := calls[len(calls)-1]; last.Provider != "openai" || last.Model != "openai/gpt-5.6" {
		t.Errorf("booked Provider/Model = %q/%q, want openai / openai/gpt-5.6", last.Provider, last.Model)
	}
}

// TestFallbackTurnReusedClientBooksHopModelOnCallerFormat proves the
// reused-client rule end to end: with ClientProvider returning nil for the
// fallback pair, the hop reuses the primary's client, so the ledger books the
// HOP model on the CALLER's format, and the release lands on the CALLER's
// endpoint gate — that client served — leaving the never-contacted fallback
// endpoint's gate closed.
func TestFallbackTurnReusedClientBooksHopModelOnCallerFormat(t *testing.T) {
	ledger := openTestLedger(t)
	var calls atomic.Int32
	shared := selfRetryingTestClient{newTestClientWithError(
		func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
			if calls.Add(1) == 1 {
				return nil, &provider.APIError{StatusCode: 503, Body: "unavailable"}
			}
			return &provider.MessageResponse{
				ID: "msg_fb_b", Type: "message", Role: "assistant",
				Content:    provider.TextContent("hop on the caller's client"),
				StopReason: "end_turn",
			}, nil
		})}
	ag := fallbackAgent(t, shared, tupleClientProvider{})

	// Both gates closed; a USER trigger may probe through the primary's.
	ag.getOrCreateRateLimitGate("anthropic").Close(time.Now().Add(1 * time.Hour))
	ag.getOrCreateRateLimitGate("openai").Close(time.Now().Add(1 * time.Hour))

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifb-b", "Hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	if limited, _ := ag.getOrCreateRateLimitGate("anthropic").IsLimited(); limited {
		t.Error(`"anthropic" gate still closed after the caller's client served the hop, want released`)
	}
	if limited, _ := ag.getOrCreateRateLimitGate("openai").IsLimited(); !limited {
		t.Error(`"openai" gate released by a reused-client hop, want still closed (no openai client served)`)
	}
	rows := ledgerCalls(t, ledger)
	if len(rows) == 0 {
		t.Fatal("no calls booked in the ledger")
	}
	if last := rows[len(rows)-1]; last.Model != "openai/gpt-5.6" || last.Provider != "anthropic" {
		t.Errorf("booked Provider/Model = %q/%q, want anthropic / openai/gpt-5.6 (caller format, hop model)",
			last.Provider, last.Model)
	}
}

// TestFallbackTurnThenPrimaryBooksBothTuplesInOrder proves the snapshot does
// not change: the next tool-loop iteration tries the PRIMARY first again. The
// first iteration falls back (the openai hop serves a tool_use for an
// unregistered tool), the second is served by the primary; the ledger books
// the two calls in that order.
func TestFallbackTurnThenPrimaryBooksBothTuplesInOrder(t *testing.T) {
	ledger := openTestLedger(t)
	var primaryCalls atomic.Int32
	var primaryModels []string
	primary := selfRetryingTestClient{newTestClientWithError(
		func(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			primaryModels = append(primaryModels, req.Model)
			if primaryCalls.Add(1) == 1 {
				return nil, &provider.APIError{StatusCode: 503, Body: "unavailable"}
			}
			return &provider.MessageResponse{
				ID: "msg_p2", Type: "message", Role: "assistant",
				Content:    provider.TextContent("primary recovered"),
				StopReason: "end_turn",
			}, nil
		})}
	var fbCalls atomic.Int32
	var fbModels []string
	fbClient := selfRetryingTestClient{newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		fbModels = append(fbModels, req.Model)
		fbCalls.Add(1)
		return &provider.MessageResponse{
			ID: "msg_fb_c", Type: "message", Role: "assistant",
			Content: []provider.ContentBlock{{
				Type: "tool_use", ID: "tu_fb1", Name: "nonexistent_tool", Input: json.RawMessage(`{}`),
			}},
			StopReason: "tool_use",
		}
	})}
	ag := fallbackAgent(t, primary, tupleClientProvider{clients: map[string]provider.Client{
		"openai/openai": fbClient,
	}})

	ctx := WithTrigger(context.Background(), "user")
	if _, err := ag.hmTest(ctx, "test/ifb-c", "Hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	calls := ledgerCalls(t, ledger)
	if len(calls) != 2 {
		t.Fatalf("ledger calls = %d, want one per iteration: %+v", len(calls), calls)
	}
	if calls[0].Provider != "openai" || calls[0].Model != "openai/gpt-5.6" {
		t.Errorf("iteration 1 booked Provider/Model = %q/%q, want openai / openai/gpt-5.6",
			calls[0].Provider, calls[0].Model)
	}
	if calls[1].Provider != "anthropic" || calls[1].Model != "claude-haiku-4-5" {
		t.Errorf("iteration 2 booked Provider/Model = %q/%q, want the primary tuple anthropic / claude-haiku-4-5",
			calls[1].Provider, calls[1].Model)
	}
	// The second iteration reached the primary FIRST: it saw exactly the 503
	// and its end_turn, and the fallback client served exactly one hop.
	if got := primaryCalls.Load(); got != 2 {
		t.Errorf("primary client calls = %d, want 2 (503 then end_turn — the primary is retried first)", got)
	}
	if got := fbCalls.Load(); got != 1 {
		t.Errorf("fallback client calls = %d, want 1 (only the first iteration fell back)", got)
	}
	// The snapshot model is what each request carried: the hop model is not
	// sticky on the primary client after a fallback-served iteration.
	if want := []string{"claude-haiku-4-5", "claude-haiku-4-5"}; !slices.Equal(primaryModels, want) {
		t.Errorf("models sent to the primary client = %v, want %v (iteration 2 must retry the primary model)", primaryModels, want)
	}
	if want := []string{"openai/gpt-5.6"}; !slices.Equal(fbModels, want) {
		t.Errorf("models sent to the fallback client = %v, want %v", fbModels, want)
	}
}
