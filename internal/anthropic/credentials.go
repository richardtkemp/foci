package anthropic

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
)

// CredentialResolver interface allows format-specific packages to implement
// custom credential resolution logic. The main package checks if a format has
// a custom resolver and delegates to it; otherwise falls back to simple API key.
type CredentialResolver interface {
	// ResolveClient returns a configured Client for the given endpoint.
	// apiKeyName is the secret name for the API key (e.g., "anthropic.api_key").
	// baseURL is the endpoint base URL (empty = SDK default).
	// Returns error if credentials cannot be resolved.
	ResolveClient(ctx context.Context, endpointName string, apiKeyName string, baseURL string, store SecretsStore) (*Client, error)

	// ResolveUsageClient returns a configured UsageClient for the given endpoint,
	// or nil if the format doesn't support usage API or credentials cannot be resolved.
	ResolveUsageClient(endpointName string, apiKeyName string, store SecretsStore) (*UsageClient, error)

	// GetReloadFunc returns a function that reloads credentials from disk,
	// or nil if hot-reload is not supported.
	GetReloadFunc(secretsPath string) func() error
}

// tokenHolder is a thread-safe, swappable credential string.
// Used with NewClientWithTokenFunc so credentials can be hot-reloaded
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

// AnthropicResolver implements CredentialResolver for anthropic-format endpoints.
// It handles the 3-tier priority: setup-token (OAuth) → API key → Claude Code credentials.
type AnthropicResolver struct {
	ccSrc        *CCTokenSource
	credHolders  map[string]*tokenHolder
	mu           sync.Mutex
	httpTimeout  time.Duration
	usageCacheTTL time.Duration
}

// NewResolver creates and initializes an AnthropicResolver.
// Initializes the shared CCTokenSource and sets up expiry callback to trigger token refresh.
func NewResolver(ctx context.Context, anthropicCfg *config.AnthropicConfig, store SecretsStore) (*AnthropicResolver, error) {
	httpTimeout, err := time.ParseDuration(anthropicCfg.HTTPTimeout)
	if err != nil {
		log.Warnf("anthropic", "invalid http_timeout, using default: %v", err)
		httpTimeout = 600 * time.Second
	}

	usageCacheTTL, err := time.ParseDuration(anthropicCfg.UsageCacheTTL)
	if err != nil {
		log.Warnf("anthropic", "invalid usage_cache_ttl, using default: %v", err)
		usageCacheTTL = 10 * time.Minute
	}

	ccPollInterval, err := time.ParseDuration(anthropicCfg.CCCredentialsPollInterval)
	if err != nil {
		log.Warnf("anthropic", "invalid cc_credentials_poll_interval, using default: %v", err)
		ccPollInterval = 30 * time.Second
	}

	const ccCredsFile = "~/.claude/.credentials.json"

	var ccSrc *CCTokenSource
	if src, err := NewCCTokenSource(ccCredsFile, ccPollInterval); err == nil {
		src.OnExpired(func() {
			log.Warnf("anthropic", "CC credentials expired — starting claude to refresh")
			go startClaudeForRefresh()
		})
		src.Start(ctx)
		ccSrc = src
		log.Infof("anthropic", "CC token source configured (%s, poll %s)", ccCredsFile, ccPollInterval)
	}

	return &AnthropicResolver{
		ccSrc:         ccSrc,
		credHolders:   make(map[string]*tokenHolder),
		httpTimeout:   httpTimeout,
		usageCacheTTL: usageCacheTTL,
	}, nil
}

// ResolveClient implements CredentialResolver.ResolveClient.
// Priority: (1) setup-token, (2) api_key, (3) Claude Code credentials.
func (r *AnthropicResolver) ResolveClient(ctx context.Context, endpointName string, apiKeyName string, baseURL string, store SecretsStore) (*Client, error) {
	// Priority 1: setup-token (OAuth)
	setupToken, ok := store.Get("anthropic.setup_token")
	if ok && setupToken != "" {
		log.Infof("anthropic", "using setup-token from secrets (endpoint %q)", endpointName)
		holder := NewTokenHolder(setupToken)
		r.mu.Lock()
		r.credHolders[endpointName] = holder
		r.mu.Unlock()
		c := NewClientWithTokenFunc(holder.Get, r.httpTimeout)
		if baseURL != "" {
			c.SetBaseURL(baseURL)
		}
		return c, nil
	}

	// Priority 2: API key
	apiKey, ok := store.Get(apiKeyName)
	if ok && apiKey != "" {
		log.Infof("anthropic", "using API key from secrets (endpoint %q)", endpointName)
		holder := NewTokenHolder(apiKey)
		r.mu.Lock()
		r.credHolders[endpointName] = holder
		r.mu.Unlock()
		c := NewClientWithTokenFunc(holder.Get, r.httpTimeout)
		if baseURL != "" {
			c.SetBaseURL(baseURL)
		}
		return c, nil
	}

	// Priority 3: Claude Code credentials
	if r.ccSrc != nil {
		log.Infof("anthropic", "using CC credentials from ~/.claude/.credentials.json (endpoint %q, passive)", endpointName)
		c := NewClientWithTokenFunc(r.ccSrc.Token, r.httpTimeout)
		if baseURL != "" {
			c.SetBaseURL(baseURL)
		}
		return c, nil
	}

	return nil, fmt.Errorf("no Anthropic credentials found — run: foci auth")
}

// ResolveUsageClient implements CredentialResolver.ResolveUsageClient.
// Priority: (1) setup-token, (2) api_key, (3) Claude Code credentials.
func (r *AnthropicResolver) ResolveUsageClient(endpointName string, apiKeyName string, store SecretsStore) (*UsageClient, error) {
	// Priority 1: setup-token (OAuth)
	if setupToken, ok := store.Get("anthropic.setup_token"); ok && setupToken != "" {
		holder := NewTokenHolder(setupToken)
		client := NewUsageClientWithFunc(holder.Get)
		client.SetCacheTTL(r.usageCacheTTL)
		log.Infof("anthropic", "created usage client for %q (via setup_token)", endpointName)
		return client, nil
	}

	// Priority 2: API key
	if apiKey, ok := store.Get(apiKeyName); ok && apiKey != "" {
		holder := NewTokenHolder(apiKey)
		client := NewUsageClientWithFunc(holder.Get)
		client.SetCacheTTL(r.usageCacheTTL)
		log.Infof("anthropic", "created usage client for %q (via %s)", endpointName, apiKeyName)
		return client, nil
	}

	// Priority 3: Claude Code credentials
	if r.ccSrc != nil {
		client := NewUsageClientWithFunc(r.ccSrc.Token)
		client.SetCacheTTL(r.usageCacheTTL)
		log.Infof("anthropic", "created usage client for %q (via CC credentials)", endpointName)
		return client, nil
	}

	return nil, fmt.Errorf("no Anthropic credentials found for usage client (endpoint %q)", endpointName)
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

		// Try setup-token first, then API key
		token, _ := st.Get("anthropic.setup_token")
		if token == "" {
			token, _ = st.Get("anthropic.api_key")
		}
		if token == "" {
			return fmt.Errorf("no setup_token or api_key found in secrets.toml after reload")
		}

		// Update all cached tokenHolders
		r.mu.Lock()
		for name, holder := range r.credHolders {
			holder.Set(token)
			log.Infof("anthropic", "hot-reloaded credentials for endpoint %q", name)
		}
		r.mu.Unlock()

		return nil
	}
}

// startClaudeForRefresh sends a trivial query via Claude Code to force a token refresh.
// claude auth status doesn't refresh tokens — only a real API call does.
// Fire-and-forget — logs errors but never blocks.
func startClaudeForRefresh() {
	cmd := exec.Command("claude",
		"--model", "haiku",
		"--system-prompt", "",
		"--print",
		"--effort", "low",
		"1+1",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		log.Warnf("anthropic", "claude token refresh failed (CC may not be installed): %v", err)
	} else {
		log.Infof("anthropic", "claude token refresh completed")
	}
}

