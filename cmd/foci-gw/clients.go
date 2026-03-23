package main

import (
	"context"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/gemini"
	"foci/internal/log"
	oai "foci/internal/openai"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// Compile-time verification that clientRegistry implements provider.ClientProvider
var _ provider.ClientProvider = (*clientRegistry)(nil)

// clientRegistry lazily creates provider clients on first use per endpoint:format pair.
type clientRegistry struct {
	mu      sync.Mutex
	entries map[string]*clientEntry

	cfg *config.Config
	store *secrets.Store
	ctx   context.Context
}

type clientEntry struct {
	client provider.Client
	once   sync.Once
}

func newClientRegistry(cfg *config.Config, store *secrets.Store, ctx context.Context) *clientRegistry {
	return &clientRegistry{
		entries: make(map[string]*clientEntry),
		cfg:     cfg,
		store:   store,
		ctx:     ctx,
	}
}

// GetClient returns the client for an endpoint:format pair, initializing it on first use.
func (r *clientRegistry) GetClient(endpointName, format string) provider.Client {
	key := endpointName + ":" + format
	r.mu.Lock()
	entry, ok := r.entries[key]
	if !ok {
		entry = &clientEntry{}
		r.entries[key] = entry
	}
	r.mu.Unlock()

	entry.once.Do(func() {
		epCfg, exists := r.cfg.Endpoints[endpointName]
		if !exists {
			log.Errorf("main", "endpoint %q not found in config", endpointName)
			return
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
				log.Errorf("main", "resolve %s client for endpoint %q: %v", format, endpointName, err)
				return
			}
			entry.client = client
			return
		}

		// Default: simple API key resolution for formats without custom resolver

		switch format {
		case "gemini":
			if endpointName == "gemini" {
				// Built-in gemini endpoint
				apiKey, _ := r.store.Get("gemini.api_key")
				if apiKey == "" {
					log.Errorf("main", "gemini.api_key not found in secrets — gemini endpoint unavailable")
					return
				}
				opts := []gemini.Option{gemini.WithHTTPTimeout(httpTimeout)}
				cacheTTLStr := modelCacheTTLForEndpoint(r.cfg.Models, endpointName, "1h")
				if cacheTTLStr != "0" {
					if cacheTTL, err := time.ParseDuration(cacheTTLStr); err == nil && cacheTTL > 0 {
						opts = append(opts, gemini.WithCacheTTL(cacheTTL))
					}
				}
				gc, err := gemini.NewClient(r.ctx, apiKey, opts...)
				if err != nil {
					log.Errorf("main", "create gemini client: %v", err)
					return
				}
				entry.client = gc
				log.Infof("main", "gemini client ready (cache_ttl=%s)", cacheTTLStr)
			} else {
				log.Errorf("main", "gemini format on non-gemini endpoint %q not supported", endpointName)
			}

		case "openai":
			apiKey, _ := r.store.Get(apiKeyName)
			if apiKey == "" {
				log.Errorf("main", "%s not found in secrets — endpoint %q (openai format) unavailable", apiKeyName, endpointName)
				return
			}
			opts := []oai.Option{oai.WithHTTPTimeout(httpTimeout)}
			url := epCfg.URLForFormat("openai")
			if url != "" {
				opts = append(opts, oai.WithBaseURL(url))
			}
			entry.client = oai.NewClient(apiKey, opts...)
			log.Infof("main", "openai client ready for endpoint %q (url=%s)", endpointName, url)
		}
	})

	return entry.client
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
	return entry.client
}

// ResolveEndpointClient resolves the client for an endpoint+format pair.
// Falls back to openai format if the endpoint doesn't support the given format.
func (r *clientRegistry) ResolveEndpointClient(endpointName, format string) provider.Client {
	epCfg, ok := r.cfg.Endpoints[endpointName]
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
