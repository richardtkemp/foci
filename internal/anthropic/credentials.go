package anthropic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/modelcaps"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// tokenHolder is a thread-safe, swappable credential string.
// Used with NewClient so credentials can be hot-reloaded
// without restarting.
type tokenHolder struct {
	mu    sync.RWMutex
	token string
}

// NewTokenHolder creates a new tokenHolder with an initial token.
func NewTokenHolder(token string) *tokenHolder {
	return &tokenHolder{token: token}
}

// Get returns the current token.
func (h *tokenHolder) Get() (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.token == "" {
		return "", fmt.Errorf("no credential configured")
	}
	return h.token, nil
}

// Set replaces the current token.
func (h *tokenHolder) Set(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = token
}

// AnthropicResolver implements provider.CredentialResolver for anthropic-format endpoints.
// Priority: API key → Claude Code credentials.
type AnthropicResolver struct {
	store       SecretsStore
	ccSrc       *CCTokenSource
	credHolders map[string]*tokenHolder
	mu          sync.Mutex
}

// NewResolver creates and initializes an AnthropicResolver.
func NewResolver(ctx context.Context, anthropicCfg *config.AnthropicConfig, store SecretsStore) (*AnthropicResolver, error) {
	const ccCredsFile = "~/.claude/.credentials.json"

	var ccSrc *CCTokenSource
	if src, err := NewCCTokenSource(ccCredsFile); err == nil {
		ccSrc = src
		anthropicLog.Infof("CC token source configured (%s, lazy reads)", ccCredsFile)
	}

	return &AnthropicResolver{
		store:       store,
		ccSrc:       ccSrc,
		credHolders: make(map[string]*tokenHolder),
	}, nil
}

// Close is a no-op retained for interface compatibility. The lazy token source
// has no background goroutines to stop.
func (r *AnthropicResolver) Close() {}

// ResolveClient implements provider.CredentialResolver.
// Priority: (1) API key, (2) Claude Code credentials.
func (r *AnthropicResolver) ResolveClient(ctx context.Context, endpointName, apiKeyName, baseURL string, httpTimeout time.Duration) (provider.Client, error) {
	// Priority 1: API key
	apiKey, ok := r.store.Get(apiKeyName)
	if ok && apiKey != "" {
		anthropicLog.Infof("using API key from secrets (endpoint %q)", endpointName)
		holder := NewTokenHolder(apiKey)
		r.mu.Lock()
		r.credHolders[endpointName] = holder
		r.mu.Unlock()
		c := NewClient(holder.Get, httpTimeout)
		if baseURL != "" {
			c.SetBaseURL(baseURL)
		}
		return c, nil
	}

	// Priority 2: Claude Code credentials (lazy disk reads, no polling)
	if r.ccSrc != nil {
		anthropicLog.Infof("using CC credentials from ~/.claude/.credentials.json (endpoint %q, lazy)", endpointName)
		c := NewClient(r.ccSrc.Token, httpTimeout)
		if baseURL != "" {
			c.SetBaseURL(baseURL)
		}
		return c, nil
	}

	return nil, fmt.Errorf("no Anthropic credentials found — run: foci auth")
}

// ModelCapsFetcher returns a modelcaps.Fetcher backed by Claude Code OAuth
// credentials, or nil if no CC token source is available (e.g. API-key-only
// deployments, where the static modelinfo registry remains the source). The
// returned fetcher GETs the live /v1/models catalogue on each call; the caller
// (internal/modelcaps) caches and refreshes it process-wide.
func (r *AnthropicResolver) ModelCapsFetcher(httpTimeout time.Duration) modelcaps.Fetcher {
	if r.ccSrc == nil {
		return nil
	}
	c := NewClient(r.ccSrc.Token, httpTimeout)
	return c.FetchModelCaps
}

// GetReloadFunc implements CredentialResolver.GetReloadFunc.
// Returns nil if using CC credentials (can't hot-reload), otherwise returns
// a function that reloads credentials from secrets.toml.
func (r *AnthropicResolver) GetReloadFunc(secretsPath string) func() error {
	// If using CC credentials (no tokenHolders), can't hot-reload
	r.mu.Lock()
	hasTokenHolders := len(r.credHolders) > 0
	r.mu.Unlock()

	if !hasTokenHolders && r.ccSrc != nil {
		// Using CC credentials, no hot-reload available
		return nil
	}

	return func() error {
		st, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("reload secrets.toml: %w", err)
		}

		token, _ := st.Get("anthropic.api_key")
		if token == "" {
			return fmt.Errorf("no api_key found in secrets.toml after reload")
		}

		// Update all cached tokenHolders
		r.mu.Lock()
		for name, holder := range r.credHolders {
			holder.Set(token)
			anthropicLog.Infof("hot-reloaded credentials for endpoint %q", name)
		}
		r.mu.Unlock()

		return nil
	}
}
