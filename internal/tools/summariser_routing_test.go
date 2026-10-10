package tools

import (
	"context"
	"testing"

	"foci/internal/provider"
)

// summariserCaptureClient records the one request Summarise sends.
// ProviderRouting/RoutingFor are json:"-" fields, invisible to the
// httptest wire-capture route the other summary tests use, so this stub
// observes them on the request object instead. HandlesOwnRetries makes
// sendWithRetry a single pass.
type summariserCaptureClient struct {
	req *provider.MessageRequest
}

func (c *summariserCaptureClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	c.req = req
	return &provider.MessageResponse{
		ID: "msg_1", Type: "message", Role: "assistant",
		Content: provider.TextContent("summary"), StopReason: "end_turn",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (c *summariserCaptureClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (c *summariserCaptureClient) IsCachingAvailable() bool { return false }

// HandlesOwnRetries makes sendWithRetry skip its retry loop.
func (c *summariserCaptureClient) HandlesOwnRetries() bool { return true }

func TestAPISummariser_WithRoutingForPutsRoutingOnRequest(t *testing.T) {
	// Proves WithRoutingFor wires the per-model [models.*.provider] lookup
	// onto the summary request: the cheap model's own table rides on it,
	// and the lookup answers for other models too (a fallback hop lands on
	// the hop model's own routing).
	routingCheap := &provider.ProviderRouting{Order: []string{"prov-cheap"}}
	routingOther := &provider.ProviderRouting{Order: []string{"prov-other"}}
	client := &summariserCaptureClient{}
	gr := testGroupResolver("openrouter/cheap-model")
	s := NewAPISummariser(client, nil, gr, nil, func() int { return 0 }).
		WithRoutingFor(func(model string) *provider.ProviderRouting {
			switch model {
			case "openrouter/cheap-model":
				return routingCheap
			case "openrouter/other":
				return routingOther
			default:
				return nil
			}
		})

	if _, err := s.Summarise(context.Background(), []byte("content"), "sum it", "f.txt"); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if client.req == nil {
		t.Fatal("no request captured")
	}
	if client.req.ProviderRouting != routingCheap {
		t.Errorf("ProviderRouting = %+v, want the cheap model's table", client.req.ProviderRouting)
	}
	if client.req.RoutingFor == nil {
		t.Fatal("RoutingFor not set on request")
	}
	if got := client.req.RoutingFor("openrouter/other"); got != routingOther {
		t.Errorf("RoutingFor(other) = %+v, want the other model's table", got)
	}
}

func TestAPISummariser_WithoutRoutingForSendsNil(t *testing.T) {
	// Proves the unchanged case: without WithRoutingFor the summary request
	// carries no routing at all — today's behaviour for deployments with no
	// [models.*] config.
	client := &summariserCaptureClient{}
	gr := testGroupResolver("openrouter/cheap-model")
	s := NewAPISummariser(client, nil, gr, nil, func() int { return 0 })

	if _, err := s.Summarise(context.Background(), []byte("content"), "sum it", "f.txt"); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if client.req == nil {
		t.Fatal("no request captured")
	}
	if client.req.ProviderRouting != nil {
		t.Errorf("ProviderRouting = %+v, want nil", client.req.ProviderRouting)
	}
	if client.req.RoutingFor != nil {
		t.Error("RoutingFor set on request, want nil")
	}
}
