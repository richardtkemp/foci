package main

import (
	"sync"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
)

// Compile-time verification that usageClientRegistry implements provider.UsageClientProvider
var _ provider.UsageClientProvider = (*usageClientRegistry)(nil)

// usageClientRegistry lazily creates UsageClient instances per API key.
// Key format: "format:api_key_secret_name"
type usageClientRegistry struct {
	mu      sync.Mutex
	entries map[string]*usageClientEntry

	cfg *config.Config
}

type usageClientEntry struct {
	client provider.UsageClient
	once   sync.Once
}

// newUsageClientRegistry creates a registry that lazily initializes UsageClients.
func newUsageClientRegistry(cfg *config.Config) *usageClientRegistry {
	return &usageClientRegistry{
		entries: make(map[string]*usageClientEntry),
		cfg:     cfg,
	}
}

// GetUsageClient returns a UsageClient for the given endpoint, or nil if unavailable.
// Creates and caches by format:api_key pair.
func (r *usageClientRegistry) GetUsageClient(endpointName string) provider.UsageClient {
	if endpointName == "" {
		return nil
	}

	epCfg, ok := r.cfg.Endpoints[endpointName]
	if !ok {
		return nil
	}

	// Only formats with a registered resolver can provide usage clients.
	resolver, ok := formatResolvers[epCfg.Format]
	if !ok {
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
		client, err := resolver.ResolveUsageClient(endpointName, apiKeyName)
		if err != nil {
			log.Debugf("usage_registry", "no usage client for %q: %v", endpointName, err)
			return
		}
		entry.client = client
	})

	return entry.client
}
