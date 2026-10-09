package tools

import (
	"context"

	"foci/internal/provider"
)

// Shared stubs for the fallback-booking tests (spawn, summary): a scripted
// client and a client provider, so a 503 primary reaches the fallback chain
// without provider-level retry backoff.

// fallbackProbeClient serves a scripted sequence of responses (or errors),
// one per call, and handles its own retries so a scripted 503 falls straight
// through to the fallback chain.
type fallbackProbeClient struct {
	responses []fallbackProbeResponse
	calls     []string // the model each call carried
}

type fallbackProbeResponse struct {
	text string
	err  error
}

func (m *fallbackProbeClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	m.calls = append(m.calls, req.Model)
	if len(m.responses) == 0 {
		panic("fallbackProbeClient: no more responses")
	}
	r := m.responses[0]
	m.responses = m.responses[1:]
	if r.err != nil {
		return nil, r.err
	}
	return &provider.MessageResponse{
		ID: "msg_probe", Type: "message", Role: "assistant",
		Content:    provider.TextContent(r.text),
		StopReason: "end_turn",
		Usage:      provider.Usage{InputTokens: 30, OutputTokens: 10},
	}, nil
}

func (m *fallbackProbeClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (m *fallbackProbeClient) IsCachingAvailable() bool { return false }

func (m *fallbackProbeClient) HandlesOwnRetries() bool { return true }

// probeClientProvider hands out one fixed client per endpoint/format pair.
type probeClientProvider struct{ clients map[string]provider.Client }

func (p probeClientProvider) GetClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

func (p probeClientProvider) PeekClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

func (p probeClientProvider) ResolveEndpointClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

// openAIFallbackFrom falls back to openai/gpt-5.6 on the openai endpoint and
// format from the given primary model.
func openAIFallbackFrom(primary string) provider.FallbackFunc {
	return func(model string) (string, string, string, bool) {
		if model == primary {
			return "openai/gpt-5.6", "openai", "openai", true
		}
		return "", "", "", false
	}
}
