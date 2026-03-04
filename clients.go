package main

import (
	"context"
	"sync"
	"time"

	"foci/anthropic"
	"foci/config"
	"foci/gemini"
	"foci/log"
	oai "foci/openai"
	"foci/provider"
	"foci/secrets"
)

// clientRegistry lazily creates provider clients on first use per endpoint:format pair.
type clientRegistry struct {
	mu      sync.Mutex
	entries map[string]*clientEntry

	cfg             *config.Config
	store           *secrets.Store
	anthropicClient *anthropic.Client
	ctx             context.Context
}

type clientEntry struct {
	client provider.Client
	once   sync.Once
}

func newClientRegistry(cfg *config.Config, store *secrets.Store, anthropicClient *anthropic.Client, ctx context.Context) *clientRegistry {
	return &clientRegistry{
		entries:         make(map[string]*clientEntry),
		cfg:             cfg,
		store:           store,
		anthropicClient: anthropicClient,
		ctx:             ctx,
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

		// Resolve API key from secrets store
		apiKeyName := epCfg.APIKey
		if apiKeyName == "" {
			apiKeyName = endpointName + ".api_key"
		}

		switch format {
		case "anthropic":
			// Built-in anthropic endpoint uses resolveCredentials (setup-token, API key, CC creds).
			// Other endpoints using anthropic format use simple API key auth.
			if endpointName == "anthropic" {
				entry.client = r.anthropicClient
				return
			}
			apiKey, _ := r.store.Get(apiKeyName)
			if apiKey == "" {
				log.Errorf("main", "%s not found in secrets — endpoint %q (anthropic format) unavailable", apiKeyName, endpointName)
				return
			}
			httpTimeout := parseDurationDefault(epCfg.HTTPTimeout, parseDurationDefault(r.cfg.Anthropic.HTTPTimeout, 600*time.Second))
			holder := &tokenHolder{token: apiKey}
			c := anthropic.NewClientWithTokenFunc(holder.Get, httpTimeout)
			url := epCfg.URLForFormat("anthropic")
			if url != "" {
				c.SetBaseURL(url)
			}
			c.SetUseSDK(r.cfg.Anthropic.UseSDK)
			entry.client = c
			log.Infof("main", "anthropic client ready for endpoint %q (url=%s)", endpointName, url)

		case "gemini":
			if endpointName == "gemini" {
				// Built-in gemini endpoint
				apiKey, _ := r.store.Get("gemini.api_key")
				if apiKey == "" {
					log.Errorf("main", "gemini.api_key not found in secrets — gemini endpoint unavailable")
					return
				}
				httpTimeout, err := time.ParseDuration(r.cfg.Gemini.HTTPTimeout)
				if err != nil {
					httpTimeout = 120 * time.Second
				}
				opts := []gemini.Option{gemini.WithHTTPTimeout(httpTimeout)}
				if r.cfg.Gemini.CacheTTL != "0" {
					if cacheTTL, err := time.ParseDuration(r.cfg.Gemini.CacheTTL); err == nil && cacheTTL > 0 {
						opts = append(opts, gemini.WithCacheTTL(cacheTTL))
					}
				}
				gc, err := gemini.NewClient(r.ctx, apiKey, opts...)
				if err != nil {
					log.Errorf("main", "create gemini client: %v", err)
					return
				}
				entry.client = gc
				log.Infof("main", "gemini client ready (cache_ttl=%s)", r.cfg.Gemini.CacheTTL)
			} else {
				log.Errorf("main", "gemini format on non-gemini endpoint %q not supported", endpointName)
			}

		case "openai":
			apiKey, _ := r.store.Get(apiKeyName)
			if apiKey == "" {
				log.Errorf("main", "%s not found in secrets — endpoint %q (openai format) unavailable", apiKeyName, endpointName)
				return
			}
			httpTimeout := parseDurationDefault(epCfg.HTTPTimeout, parseDurationDefault(r.cfg.OpenAI.HTTPTimeout, 120*time.Second))
			opts := []oai.Option{oai.WithHTTPTimeout(httpTimeout)}
			url := epCfg.URLForFormat("openai")
			if url == "" && endpointName == "openai" {
				url = r.cfg.OpenAI.BaseURL
			}
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

// ResolveEndpointClient resolves the client for an endpoint+modelID pair.
// Infers wire format from model name, falls back to openai if endpoint doesn't support it.
// Note: modelID can be either a bare model ID or developer/model_id format.
func (r *clientRegistry) ResolveEndpointClient(endpointName, modelID string) provider.Client {
	// Extract bare model ID if it's in developer/model_id format
	_, bareModelID := config.SplitDeveloperModel(modelID)

	// Infer wire format from the model ID
	format := config.InferFormat(bareModelID)
	epCfg, ok := r.cfg.Endpoints[endpointName]
	if ok && !epCfg.SupportsFormat(format) {
		format = "openai" // universal fallback
	}
	return r.GetClient(endpointName, format)
}
