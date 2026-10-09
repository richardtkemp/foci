package compaction

import (
	"context"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/provider"
	"foci/internal/session"
)

// fbStubClient serves a scripted sequence of responses (or errors), one per
// call, and handles its own retries so a scripted 503 falls straight through
// to the fallback chain without provider-level backoff.
type fbStubClient struct {
	responses []fbStubResponse
}

type fbStubResponse struct {
	text string
	err  error
}

func (m *fbStubClient) SendMessage(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
	if len(m.responses) == 0 {
		panic("fbStubClient: no more responses")
	}
	r := m.responses[0]
	m.responses = m.responses[1:]
	if r.err != nil {
		return nil, r.err
	}
	return &provider.MessageResponse{
		ID: "msg_fb", Type: "message", Role: "assistant",
		Content:    provider.TextContent(r.text),
		StopReason: "end_turn",
		Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
	}, nil
}

func (m *fbStubClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (m *fbStubClient) IsCachingAvailable() bool { return false }

func (m *fbStubClient) HandlesOwnRetries() bool { return true }

// fbStubProvider hands out one fixed client per endpoint/format pair.
type fbStubProvider struct{ clients map[string]provider.Client }

func (p fbStubProvider) GetClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

func (p fbStubProvider) PeekClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

func (p fbStubProvider) ResolveEndpointClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

// TestCompactBooksServedFallbackTuple proves a compaction summary a fallback
// hop served is booked on the SERVING tuple — Provider and Model name the
// hop's format and canonical model — not the requested primary.
func TestCompactBooksServedFallbackTuple(t *testing.T) {
	ledger := openTestLedger(t)
	store := session.NewStore(t.TempDir())
	sessionKey := "test/ifb"
	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, 0.8)
	c.FallbackFunc = func(model string) (string, string, string, bool) {
		if model == "anthropic/claude-haiku-4-5" {
			return "openai/gpt-5.6", "openai", "openai", true
		}
		return "", "", "", false
	}
	c.ClientProvider = fbStubProvider{clients: map[string]provider.Client{
		"openai/openai": &fbStubClient{responses: []fbStubResponse{{text: "Summary from the fallback."}}},
	}}
	primary := &fbStubClient{responses: []fbStubResponse{
		{err: &provider.APIError{StatusCode: 503, Body: "unavailable"}},
	}}

	if _, err := c.Compact(context.Background(), primary, sessionKey,
		"anthropic/claude-haiku-4-5", "anthropic", nil, "", "", false); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	calls, err := ledger.Calls(time.Time{})
	if err != nil || len(calls) != 1 {
		t.Fatalf("ledger calls = %d (%v), want the one compaction call", len(calls), err)
	}
	got := calls[0]
	if got.Kind != accounting.KindCompaction {
		t.Errorf("call kind = %q, want %q", got.Kind, accounting.KindCompaction)
	}
	if got.Provider != "openai" || got.Model != "openai/gpt-5.6" {
		t.Errorf("booked Provider/Model = %q/%q, want openai / openai/gpt-5.6", got.Provider, got.Model)
	}
}
