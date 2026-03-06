package main

import (
	"sync"
	"time"

	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
)

// supportsUsageAPI returns whether a provider format supports usage/quota tracking.
func supportsUsageAPI(format string) bool {
	switch format {
	case "anthropic":
		return true
	case "gemini", "openai":
		return false
	default:
		return false
	}
}

// usageClientRegistry lazily creates UsageClient instances per API key.
// Key format: "format:api_key_secret_name"
type usageClientRegistry struct {
	mu      sync.Mutex
	entries map[string]*usageClientEntry

	cfg   *config.Config
	store *secrets.Store
	ccSrc *anthropic.CCTokenSource
}

type usageClientEntry struct {
	client *anthropic.UsageClient
	once   sync.Once
}

// newUsageClientRegistry creates a registry that lazily initializes UsageClients.
func newUsageClientRegistry(cfg *config.Config, store *secrets.Store, ccSrc *anthropic.CCTokenSource) *usageClientRegistry {
	return &usageClientRegistry{
		entries: make(map[string]*usageClientEntry),
		cfg:     cfg,
		store:   store,
		ccSrc:   ccSrc,
	}
}

// GetUsageClient returns a UsageClient for the given endpoint, or nil if unavailable.
// Creates and caches by format:api_key pair.
func (r *usageClientRegistry) GetUsageClient(endpointName string) *anthropic.UsageClient {
	if endpointName == "" {
		endpointName = "anthropic"
	}

	epCfg, ok := r.cfg.Endpoints[endpointName]
	if !ok {
		// Default to anthropic endpoint with standard config
		epCfg = config.EndpointConfig{
			Format: "anthropic",
			APIKey: "anthropic.api_key",
		}
	}

	// Check if format supports usage API (extensible, not hardcoded)
	if !supportsUsageAPI(epCfg.Format) {
		return nil
	}

	// Resolve API key secret name
	apiKeyName := epCfg.APIKey
	if apiKeyName == "" {
		apiKeyName = endpointName + ".api_key"
	}

	// Key: format:api_key_secret_name
	key := epCfg.Format + ":" + apiKeyName

	// Lazy init with sync.Once
	r.mu.Lock()
	entry, ok := r.entries[key]
	if !ok {
		entry = &usageClientEntry{}
		r.entries[key] = entry
	}
	r.mu.Unlock()

	entry.once.Do(func() {
		// Priority 1: setup-token (OAuth)
		if setupToken, ok := r.store.Get("anthropic.setup_token"); ok {
			holder := &tokenHolder{token: setupToken}
			entry.client = anthropic.NewUsageClientWithFunc(holder.Get)
			if ttl, err := time.ParseDuration(r.cfg.Anthropic.UsageCacheTTL); err == nil && ttl > 0 {
				entry.client.SetCacheTTL(ttl)
			}
			log.Infof("usage_registry", "created client for %s (via setup_token)", key)
			return
		}

		// Priority 2: Endpoint-specific API key
		if apiKey, ok := r.store.Get(apiKeyName); ok {
			holder := &tokenHolder{token: apiKey}
			entry.client = anthropic.NewUsageClientWithFunc(holder.Get)
			if ttl, err := time.ParseDuration(r.cfg.Anthropic.UsageCacheTTL); err == nil && ttl > 0 {
				entry.client.SetCacheTTL(ttl)
			}
			log.Infof("usage_registry", "created client for %s (via %s)", key, apiKeyName)
			return
		}

		// Priority 3: Claude Code credentials (fallback)
		if r.ccSrc != nil {
			entry.client = anthropic.NewUsageClientWithFunc(r.ccSrc.Token)
			if ttl, err := time.ParseDuration(r.cfg.Anthropic.UsageCacheTTL); err == nil && ttl > 0 {
				entry.client.SetCacheTTL(ttl)
			}
			log.Infof("usage_registry", "created client for %s (via CC credentials)", key)
		}
	})

	return entry.client
}

// InvalidateAll clears all cached usage clients (for token hot-reload).
func (r *usageClientRegistry) InvalidateAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make(map[string]*usageClientEntry)
	log.Infof("usage_registry", "invalidated all clients")
}
