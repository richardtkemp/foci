package anthropic

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/secrets"
)

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

// AnthropicResolver implements provider.CredentialResolver for anthropic-format endpoints.
// It handles the 3-tier priority: setup-token (OAuth) → API key → Claude Code credentials.
type AnthropicResolver struct {
	store         SecretsStore
	ccSrc         *CCTokenSource
	ccStartOnce   sync.Once
	ctx           context.Context
	credHolders   map[string]*tokenHolder
	mu            sync.Mutex
	httpTimeout   time.Duration
	usageCacheTTL time.Duration
	useSDK        bool
}

// NewResolver creates and initializes an AnthropicResolver.
// Initializes the shared CCTokenSource and sets up expiry callback to trigger token refresh.
// The store is captured and used for all subsequent credential resolution calls.
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
		ccSrc = src
		log.Infof("anthropic", "CC token source configured (%s, poll %s)", ccCredsFile, ccPollInterval)
	}

	return &AnthropicResolver{
		store:         store,
		ccSrc:         ccSrc,
		ctx:           ctx,
		credHolders:   make(map[string]*tokenHolder),
		httpTimeout:   httpTimeout,
		usageCacheTTL: usageCacheTTL,
		useSDK:        anthropicCfg.UseSDK,
	}, nil
}

// ensureCCStarted starts the CCTokenSource background poller on first use.
func (r *AnthropicResolver) ensureCCStarted() {
	if r.ccSrc == nil {
		return
	}
	r.ccStartOnce.Do(func() {
		r.ccSrc.Start(r.ctx)
		log.Infof("anthropic", "CC token source started (lazy)")
	})
}

// Close stops the CCTokenSource background poller.
func (r *AnthropicResolver) Close() {
	if r.ccSrc != nil {
		r.ccSrc.Stop()
	}
}

// ResolveClient implements provider.CredentialResolver.
// Priority: (1) setup-token, (2) api_key, (3) Claude Code credentials.
func (r *AnthropicResolver) ResolveClient(ctx context.Context, endpointName, apiKeyName, baseURL string) (provider.Client, error) {
	// Priority 1: setup-token (OAuth)
	setupToken, ok := r.store.Get("anthropic.setup_token")
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
		c.SetUseSDK(r.useSDK)
		return c, nil
	}

	// Priority 2: API key
	apiKey, ok := r.store.Get(apiKeyName)
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
		c.SetUseSDK(r.useSDK)
		return c, nil
	}

	// Priority 3: Claude Code credentials
	r.ensureCCStarted()
	if r.ccSrc != nil {
		log.Infof("anthropic", "using CC credentials from ~/.claude/.credentials.json (endpoint %q, passive)", endpointName)
		c := NewClientWithTokenFunc(r.ccSrc.Token, r.httpTimeout)
		if baseURL != "" {
			c.SetBaseURL(baseURL)
		}
		c.SetUseSDK(r.useSDK)
		return c, nil
	}

	return nil, fmt.Errorf("no Anthropic credentials found — run: foci auth")
}

// ResolveUsageClient implements provider.CredentialResolver.
// The usage API requires OAuth credentials with user:profile scope, so only
// Claude Code credentials are supported. Setup-tokens and API keys don't have
// OAuth scopes and will be rejected by the usage endpoint.
func (r *AnthropicResolver) ResolveUsageClient(endpointName, apiKeyName string) (provider.UsageClient, error) {
	// Usage API requires OAuth with user:profile scope — only CC credentials work.
	r.ensureCCStarted()
	if r.ccSrc != nil {
		client := NewUsageClientWithFunc(r.ccSrc.Token)
		client.SetCacheTTL(r.usageCacheTTL)
		log.Infof("anthropic", "created usage client for %q (via CC credentials)", endpointName)
		return client, nil
	}

	// No CC credentials available — usage API not supported.
	log.Debugf("anthropic", "no usage client for %q: requires Claude Code credentials with OAuth scopes", endpointName)
	return nil, fmt.Errorf("usage API requires Claude Code credentials (OAuth with user:profile scope)")
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

