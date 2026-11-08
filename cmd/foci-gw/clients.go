package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/gemini"
	oai "foci/internal/openai"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// Compile-time verification that clientRegistry implements provider.ClientProvider
var _ provider.ClientProvider = (*clientRegistry)(nil)

// clientRegistry lazily creates provider clients on first use per endpoint:format pair.
//
// Each pair gets ONE provider.SwappableClient handle, which callers keep for
// the life of the process (agents, sessions, spawns, commands). A live
// [endpoints.*] edit (Reconfigure, #2980) rebuilds the client of every pair
// whose endpoint changed and swaps it behind the handle, so the next call
// reaches the new url, api_key or http_timeout without a restart. A call in
// flight keeps the client it pinned (provider.Pin).
type clientRegistry struct {
	mu      sync.Mutex
	entries map[string]*clientEntry

	cfg   *config.Config // guarded by mu; swapped by Reconfigure
	store *secrets.Store
	ctx   context.Context
}

type clientEntry struct {
	mu       sync.Mutex
	built    bool                      // a build was attempted (it may have failed)
	live     *provider.SwappableClient // nil when the build failed
	endpoint config.EndpointConfig     // the config the current client was built from
}

// gwClients is the process-wide registry, set in main (same pattern as
// gwLiveApply) so the [endpoints] live applier can reach it.
var gwClients *clientRegistry

func newClientRegistry(cfg *config.Config, store *secrets.Store, ctx context.Context) *clientRegistry {
	return &clientRegistry{
		entries: make(map[string]*clientEntry),
		cfg:     cfg,
		store:   store,
		ctx:     ctx,
	}
}

// config returns the registry's current config.
func (r *clientRegistry) config() *config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// GetClient returns the client for an endpoint:format pair, initializing it on first use.
// A failed build returns nil until a config edit lets Reconfigure retry it.
func (r *clientRegistry) GetClient(endpointName, format string) provider.Client {
	key := endpointName + ":" + format
	r.mu.Lock()
	entry, ok := r.entries[key]
	if !ok {
		entry = &clientEntry{}
		r.entries[key] = entry
	}
	r.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.built {
		entry.built = true
		cfg := r.config()
		if c := r.build(cfg, endpointName, format); c != nil {
			entry.live = provider.NewSwappableClient(c)
			entry.endpoint = cfg.Endpoints[endpointName]
		}
	}
	if entry.live == nil {
		return nil // never a typed-nil interface: callers test == nil
	}
	return entry.live
}

// Reconfigure adopts fresh's [endpoints] (#2980). For every pair already
// built, a changed endpoint gets a new client, swapped behind the pair's
// handle; an unchanged one keeps its client (and its retry and cache state).
// A pair whose first build failed is reset, so the next GetClient retries it
// against fresh. A rebuild that fails keeps the old client and logs an
// error: the edit must not leave a working endpoint with no client.
//
// The old client is not closed: calls in flight still use it. A replaced
// Gemini client's server-side caches therefore expire by their TTL rather
// than being deleted at shutdown.
func (r *clientRegistry) Reconfigure(fresh *config.Config) {
	r.mu.Lock()
	r.cfg = fresh
	keys := make([]string, 0, len(r.entries))
	entries := make([]*clientEntry, 0, len(r.entries))
	for k, e := range r.entries {
		keys = append(keys, k)
		entries = append(entries, e)
	}
	r.mu.Unlock()

	for i, e := range entries {
		endpointName, format, _ := strings.Cut(keys[i], ":")
		r.reconfigureEntry(e, fresh, endpointName, format)
	}
}

func (r *clientRegistry) reconfigureEntry(e *clientEntry, fresh *config.Config, endpointName, format string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.built {
		return // the first GetClient builds it, reading fresh
	}
	if e.live == nil {
		e.built = false // retry on the next GetClient
		return
	}
	epCfg, exists := fresh.Endpoints[endpointName]
	if !exists {
		mainLog.Warnf("endpoint %q was removed from config; its %s client keeps the old settings until a restart", endpointName, format)
		return
	}
	if epCfg == e.endpoint {
		return
	}
	c := r.build(fresh, endpointName, format)
	if c == nil {
		mainLog.Errorf("endpoint %q changed but its %s client could not be rebuilt; keeping the old client", endpointName, format)
		return
	}
	e.live.Swap(c)
	e.endpoint = epCfg
	mainLog.Infof("endpoint %q (%s) reconfigured live: url=%s http_timeout=%s",
		endpointName, format, epCfg.URLForFormat(format), parseDurationDefault(epCfg.HTTPTimeout, 120*time.Second))
}

// build creates the client for an endpoint:format pair from cfg, or returns
// nil (after logging why) when it cannot.
func (r *clientRegistry) build(cfg *config.Config, endpointName, format string) provider.Client {
	epCfg, exists := cfg.Endpoints[endpointName]
	if !exists {
		mainLog.Errorf("endpoint %q not found in config", endpointName)
		return nil
	}

	// Resolve API key name (used for both custom resolvers and simple resolution)
	apiKeyName := epCfg.APIKey
	if apiKeyName == "" {
		apiKeyName = endpointName + ".api_key"
	}

	// Resolve base URL and timeout for endpoint
	baseURL := epCfg.URLForFormat(format)
	httpTimeout := parseDurationDefault(epCfg.HTTPTimeout, 120*time.Second)

	// Check if format has a custom resolver (e.g., anthropic)
	if resolver, ok := formatResolvers[format]; ok {
		client, err := resolver.ResolveClient(r.ctx, endpointName, apiKeyName, baseURL, httpTimeout)
		if err != nil {
			mainLog.Errorf("resolve %s client for endpoint %q: %v", format, endpointName, err)
			return nil
		}
		return client
	}

	// Default: simple API key resolution for formats without custom resolver

	switch format {
	case "gemini":
		if endpointName != "gemini" {
			mainLog.Errorf("gemini format on non-gemini endpoint %q not supported", endpointName)
			return nil
		}
		// Built-in gemini endpoint
		apiKey, _ := r.store.Get("gemini.api_key")
		if apiKey == "" {
			mainLog.Errorf("gemini.api_key not found in secrets — gemini endpoint unavailable")
			return nil
		}
		opts := []gemini.Option{gemini.WithHTTPTimeout(httpTimeout)}
		cacheTTLStr := modelCacheTTLForEndpoint(cfg.Models, endpointName, "1h")
		if cacheTTLStr != "0" {
			if cacheTTL, err := time.ParseDuration(cacheTTLStr); err == nil && cacheTTL > 0 {
				opts = append(opts, gemini.WithCacheTTL(cacheTTL))
			}
		}
		gc, err := gemini.NewClient(r.ctx, apiKey, opts...)
		if err != nil {
			mainLog.Errorf("create gemini client: %v", err)
			return nil
		}
		mainLog.Infof("gemini client ready (cache_ttl=%s)", cacheTTLStr)
		return gc

	case "openai":
		apiKey, _ := r.store.Get(apiKeyName)
		if apiKey == "" {
			mainLog.Errorf("%s not found in secrets — endpoint %q (openai format) unavailable", apiKeyName, endpointName)
			return nil
		}
		opts := []oai.Option{oai.WithHTTPTimeout(httpTimeout), oai.WithInstructionRole(epCfg.InstructionRole)}
		url := epCfg.URLForFormat("openai")
		if url != "" {
			opts = append(opts, oai.WithBaseURL(url))
		}
		mainLog.Infof("openai client ready for endpoint %q (url=%s instruction_role=%s)", endpointName, url, epCfg.EffectiveInstructionRole())
		return oai.NewClient(apiKey, opts...)
	}
	return nil
}

// PeekClient returns the client for an endpoint:format pair without initializing it.
func (r *clientRegistry) PeekClient(endpointName, format string) provider.Client {
	key := endpointName + ":" + format
	r.mu.Lock()
	entry, ok := r.entries[key]
	r.mu.Unlock()
	if !ok || entry == nil {
		return nil
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.live == nil {
		return nil
	}
	return entry.live
}

// ResolveEndpointClient resolves the client for an endpoint+format pair.
// Falls back to openai format if the endpoint doesn't support the given format.
func (r *clientRegistry) ResolveEndpointClient(endpointName, format string) provider.Client {
	epCfg, ok := r.config().Endpoints[endpointName]
	if ok && !epCfg.SupportsFormat(format) {
		format = "openai" // universal fallback
	}
	return r.GetClient(endpointName, format)
}

// modelCacheTTLForEndpoint finds the cache_ttl from the first model that
// resolves to the given endpoint. Returns fallback if no model specifies one.
func modelCacheTTLForEndpoint(models map[string]config.ModelConfig, endpoint, fallback string) string {
	for _, mc := range models {
		if mc.CacheTTL == "" {
			continue
		}
		rm, err := config.ResolveModel(mc.Model, "", models)
		if err != nil {
			continue
		}
		if rm.Endpoint == endpoint {
			return mc.CacheTTL
		}
	}
	return fallback
}
