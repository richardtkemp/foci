package main

import (
	"sync"

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
}

type usageClientEntry struct {
	client *anthropic.UsageClient
	once   sync.Once
}

// newUsageClientRegistry creates a registry that lazily initializes UsageClients.
func newUsageClientRegistry(cfg *config.Config, store *secrets.Store) *usageClientRegistry {
	return &usageClientRegistry{
		entries: make(map[string]*usageClientEntry),
		cfg:     cfg,
		store:   store,
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
		// Check if format has a custom resolver
		if resolver, ok := formatResolvers[epCfg.Format]; ok {
			client, err := resolver.ResolveUsageClient(endpointName, apiKeyName, r.store)
			if err != nil {
				log.Debugf("usage_registry", "no usage client for %q: %v", endpointName, err)
				return
			}
			entry.client = client
			return
		}

		// Fallback: simple resolution (shouldn't happen if resolver registered)
		log.Debugf("usage_registry", "no resolver for format %q", epCfg.Format)
	})

	return entry.client
}
