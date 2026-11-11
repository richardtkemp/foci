package main

import (
	"errors"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
)

// TestEndpointKeepaliveOffFor (#3020): the runner's per-session gate follows
// each session's endpoint (the agent default, or a /model override) and the
// LIVE endpoints config: an endpoint with keepalive.enabled = false turns
// keepalive off for its sessions only, and an endpoint edit applies on the
// next check without rebuilding the runner.
func TestEndpointKeepaliveOffFor(t *testing.T) {
	t.Parallel()
	acfg := config.AgentConfig{ID: "ka"}
	inst := newQuietSetupInstance(t, &config.Config{}, acfg, func(a *agent.Agent) {
		a.Endpoint = "anthropic"
	})
	// ka/c2 runs a local model; ka/c1 stays on the agent default.
	inst.ag.SetSessionModel("ka/c2", "qwen", "local", "openai", nil)

	var mu sync.Mutex
	off := false
	endpoints := func() map[string]config.EndpointConfig {
		mu.Lock()
		defer mu.Unlock()
		local := config.EndpointConfig{Format: "openai"}
		if off {
			local.Keepalive.Enabled = config.Ptr(false)
		}
		return map[string]config.EndpointConfig{
			"anthropic": {Format: "anthropic"},
			"local":     local,
		}
	}
	gate := endpointKeepaliveOffFor(inst, periodicParams{cfg: &config.Config{}, endpoints: endpoints})
	if gate == nil {
		t.Fatal("gate is nil for an API agent")
	}
	if gate("ka/c1") || gate("ka/c2") {
		t.Fatal("gate reports off before any endpoint turns keepalive off")
	}

	mu.Lock()
	off = true // the [endpoints] live applier swapped in the edited config
	mu.Unlock()
	if !gate("ka/c2") {
		t.Error("session on the local endpoint: gate = false, want true (keepalive off)")
	}
	if gate("ka/c1") {
		t.Error("session on the agent default endpoint: gate = true, want false")
	}

	// Agent default on the local endpoint: every session without its own
	// override is off.
	inst2 := newQuietSetupInstance(t, &config.Config{}, config.AgentConfig{ID: "kb"}, func(a *agent.Agent) {
		a.Endpoint = "local"
	})
	if !endpointKeepaliveOffFor(inst2, periodicParams{cfg: &config.Config{}, endpoints: endpoints})("kb/c1") {
		t.Error("agent default on the local endpoint: gate = false, want true")
	}
}

// TestEndpointKeepaliveOffFor_StaticFallback (#3020): without a live source
// the gate reads the startup config's endpoints.
func TestEndpointKeepaliveOffFor_StaticFallback(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Endpoints: map[string]config.EndpointConfig{
		"local": {Format: "openai", Keepalive: config.EndpointKeepaliveConfig{Enabled: config.Ptr(false)}},
	}}
	inst := newQuietSetupInstance(t, cfg, config.AgentConfig{ID: "kc"}, func(a *agent.Agent) {
		a.Endpoint = "local"
	})
	if !endpointKeepaliveOffFor(inst, periodicParams{cfg: cfg})("kc/c1") {
		t.Error("gate = false with keepalive.enabled = false in the startup config, want true")
	}
}

// TestEndpointKeepaliveOffFor_DelegatedNil (#3020): a delegated agent's
// backend runs its own model, not a foci [endpoints] entry, so it gets no
// endpoint gate.
func TestEndpointKeepaliveOffFor_DelegatedNil(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Endpoints: map[string]config.EndpointConfig{
		"anthropic": {Format: "anthropic", Keepalive: config.EndpointKeepaliveConfig{Enabled: config.Ptr(false)}},
	}}
	inst := newQuietSetupInstance(t, cfg, config.AgentConfig{ID: "kd"}, func(a *agent.Agent) {
		a.Endpoint = "anthropic"
		a.DelegatedManager = &agent.DelegatedManager{
			Spec:       delegator.Spec{CacheTTL: time.Hour},
			NewBackend: func() (delegator.Delegator, error) { return nil, errors.New("no backend in this test") },
		}
	})
	if endpointKeepaliveOffFor(inst, periodicParams{cfg: cfg}) != nil {
		t.Error("gate is non-nil for a delegated agent")
	}
}

// TestSameClientConfig_IgnoresKeepalive (#3020): the keepalive switch builds
// no client, so an edit to it (or a reload, which allocates a fresh *bool)
// must not rebuild the endpoint's client and drop its retry and cache state.
func TestSameClientConfig_IgnoresKeepalive(t *testing.T) {
	t.Parallel()
	a := config.EndpointConfig{Format: "openai", URL: "http://x", Keepalive: config.EndpointKeepaliveConfig{Enabled: config.Ptr(false)}}
	b := config.EndpointConfig{Format: "openai", URL: "http://x", Keepalive: config.EndpointKeepaliveConfig{Enabled: config.Ptr(false)}}
	c := config.EndpointConfig{Format: "openai", URL: "http://x"}
	if !sameClientConfig(a, b) {
		t.Error("two loads of the same keepalive.enabled: sameClientConfig = false, want true")
	}
	if !sameClientConfig(a, c) {
		t.Error("keepalive.enabled added: sameClientConfig = false, want true")
	}
	c.URL = "http://y"
	if sameClientConfig(a, c) {
		t.Error("control: a URL change must still rebuild the client")
	}
}
