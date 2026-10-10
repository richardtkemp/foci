package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// summariserWireClient records every request it serves. ProviderRouting and
// RoutingFor are json:"-" fields, invisible to a wire-capture server, so
// this stub observes them on the request object. HandlesOwnRetries keeps
// sendWithRetry a single pass.
type summariserWireClient struct {
	reqs []*provider.MessageRequest
}

func (c *summariserWireClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	c.reqs = append(c.reqs, req)
	return &provider.MessageResponse{
		ID: "msg_1", Type: "message", Role: "assistant",
		Content: provider.TextContent("a summary"), StopReason: "end_turn",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (c *summariserWireClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (c *summariserWireClient) IsCachingAvailable() bool { return false }

// HandlesOwnRetries makes sendWithRetry skip its retry loop.
func (c *summariserWireClient) HandlesOwnRetries() bool { return true }

// summariserWireClientProvider returns the capture client for every
// endpoint:format pair, standing in for the gateway's lazy client registry.
type summariserWireClientProvider struct {
	client provider.Client
}

func (p *summariserWireClientProvider) GetClient(string, string) provider.Client { return p.client }
func (p *summariserWireClientProvider) PeekClient(string, string) provider.Client {
	return p.client
}
func (p *summariserWireClientProvider) ResolveEndpointClient(string, string) provider.Client {
	return p.client
}

// TestConfigureAPI_SummariserCarriesRouting proves the production wiring in
// configureAPI (agents.go): the APISummariser is chained with
// WithRoutingFor over the same modelDefaultsFn the agent itself uses, so a
// summary executed through the registered summary tool carries the cheap
// model's own [models.*.provider] table (and its fallback hops resolve
// theirs) — end to end, not just via a unit test of the setter.
func TestConfigureAPI_SummariserCarriesRouting(t *testing.T) {
	routingCheap := &provider.ProviderRouting{Order: []string{"prov-cheap"}}

	client := &summariserWireClient{}
	p := minimalSetupParams(t, "test")
	p.acfg.Workspace = t.TempDir()
	p.ctx = context.Background()
	p.cfg.Agents = []config.AgentConfig{{ID: "test"}} // an API-backed agent: groups may resolve
	p.cfg.Groups.Groups = map[string]string{
		"powerful": "anthropic/claude-opus-4-6",
		"cheap":    "openrouter/cheap-model",
	}
	p.cfg.Models = map[string]config.ModelConfig{
		"cheap": {
			Model:    "openrouter/cheap-model",
			Provider: routingCheap,
		},
	}
	p.clientProvider = &summariserWireClientProvider{client: client}
	store, err := secrets.Load(filepath.Join(t.TempDir(), "secrets.toml"))
	if err != nil {
		t.Fatalf("secrets.Load: %v", err)
	}
	p.store = store

	// The same preamble setupAgent runs: shared resolution, the agent
	// shell, and configureUniversal (which sets ag.ModelDefaultsFn).
	shared := resolveSharedSetup(p)
	ag := shared.newAgent()
	configureUniversal(ag, shared.p, nil)
	fp, ok := configureAPI(ag, shared.p, shared, nil)
	if !ok {
		t.Fatal("configureAPI failed")
	}

	summary := fp.registry.Get("summary")
	if summary == nil {
		t.Fatal("summary tool not registered")
	}
	file := filepath.Join(p.acfg.Workspace, "notes.txt")
	if err := os.WriteFile(file, []byte("some content worth summarising\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]string{"file": file, "prompt": "what is this?"})
	if _, err := summary.Execute(context.Background(), params); err != nil {
		t.Fatalf("summary execute: %v", err)
	}

	if len(client.reqs) != 1 {
		t.Fatalf("summary sent %d requests, want 1", len(client.reqs))
	}
	req := client.reqs[0]
	if req.Model != "openrouter/cheap-model" {
		t.Errorf("summary model = %q, want openrouter/cheap-model", req.Model)
	}
	if req.ProviderRouting != routingCheap {
		t.Errorf("ProviderRouting = %+v, want the cheap model's [models.cheap.provider] table", req.ProviderRouting)
	}
	if req.RoutingFor == nil {
		t.Fatal("RoutingFor not set on summary request")
	}
	if got := req.RoutingFor("openrouter/cheap-model"); got != routingCheap {
		t.Errorf("RoutingFor(cheap) = %+v, want the cheap model's table", got)
	}
}
