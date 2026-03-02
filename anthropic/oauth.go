// OAuth token management for Anthropic API authentication.
package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	OAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	OAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	refreshMargin = 5 * time.Minute

	// Setup token validation constants.
	SetupTokenPrefix    = "sk-ant-oat01-"
	SetupTokenMinLength = 80
)

// SecretsStore is the subset of secrets.Store used by OAuthManager.
type SecretsStore interface {
	Get(name string) (string, bool)
	Set(name, value string)
	Save() error
}

// OAuthCredentials holds the tokens from an OAuth flow.
type OAuthCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix milliseconds
}

// OAuthManager manages OAuth credentials with automatic background refresh.
type OAuthManager struct {
	mu         sync.RWMutex
	creds      OAuthCredentials
	store      SecretsStore // non-nil for secrets.toml-backed credentials
	readOnly   bool         // true for CC fallback — don't write back on refresh
	httpClient *http.Client
	tokenURL   string // overridable for testing
	stop       context.CancelFunc
}

// NewOAuthManagerFromStore loads OAuth credentials from a secrets.Store.
// Reads anthropic.oauth_access_token, anthropic.oauth_refresh_token,
// and anthropic.oauth_expires_at. Returns an error if the access token
// or refresh token is missing.
func NewOAuthManagerFromStore(store SecretsStore) (*OAuthManager, error) {
	accessToken, ok := store.Get("anthropic.oauth_access_token")
	if !ok || accessToken == "" {
		return nil, fmt.Errorf("no anthropic.oauth_access_token in secrets")
	}

	refreshToken, _ := store.Get("anthropic.oauth_refresh_token")
	if refreshToken == "" {
		return nil, fmt.Errorf("no anthropic.oauth_refresh_token in secrets; run: foci auth")
	}

	var expiresAt int64
	if v, ok := store.Get("anthropic.oauth_expires_at"); ok {
		expiresAt, _ = strconv.ParseInt(v, 10, 64)
	}

	return &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
		},
		store:      store,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		tokenURL:   OAuthTokenURL,
	}, nil
}

// NewOAuthManager loads credentials from a JSON file on disk.
// Used for the Claude Code credentials fallback (~/.claude/.credentials.json).
// The manager is read-only: refreshes update in-memory state but don't write back.
func NewOAuthManager(credsPath string) (*OAuthManager, error) {
	credsPath = expandHome(credsPath)

	data, err := os.ReadFile(credsPath)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	creds, err := parseCredentials(data)
	if err != nil {
		return nil, err
	}

	if creds.RefreshToken == "" {
		return nil, fmt.Errorf("credentials file has no refresh_token (static setup-token?); run: foci auth")
	}

	return &OAuthManager{
		creds:      creds,
		readOnly:   true,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		tokenURL:   OAuthTokenURL,
	}, nil
}

// parseCredentials tries foci-native format first, then Claude Code format.
func parseCredentials(data []byte) (OAuthCredentials, error) {
	// Try foci-native format: {"access_token":"...","refresh_token":"...","expires_at":...}
	var native OAuthCredentials
	if err := json.Unmarshal(data, &native); err == nil && native.AccessToken != "" {
		return native, nil
	}

	// Try Claude Code format: {"claudeAiOauth":{"accessToken":"...","refreshToken":"...","expiresAt":...}}
	var claude struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &claude); err == nil && claude.ClaudeAiOauth.AccessToken != "" {
		return OAuthCredentials{
			AccessToken:  claude.ClaudeAiOauth.AccessToken,
			RefreshToken: claude.ClaudeAiOauth.RefreshToken,
			ExpiresAt:    claude.ClaudeAiOauth.ExpiresAt,
		}, nil
	}

	return OAuthCredentials{}, fmt.Errorf("unrecognized credentials format")
}

// Token returns the current access token (thread-safe).
func (m *OAuthManager) Token() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.creds.AccessToken == "" {
		return "", fmt.Errorf("no access token available")
	}
	return m.creds.AccessToken, nil
}

// ExpiresIn returns the time until the current token expires.
func (m *OAuthManager) ExpiresIn() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return time.Until(time.UnixMilli(m.creds.ExpiresAt))
}

// Refresh performs an immediate token refresh using the refresh token.
// For store-backed managers, updated credentials are written to secrets.toml.
// For read-only managers (CC fallback), credentials are updated in-memory only.
func (m *OAuthManager) Refresh() error {
	m.mu.RLock()
	refreshToken := m.creds.RefreshToken
	tokenURL := m.tokenURL
	m.mu.RUnlock()

	if refreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	newCreds, err := refreshAccessToken(m.httpClient, tokenURL, refreshToken)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.creds = *newCreds
	m.mu.Unlock()

	if m.readOnly || m.store == nil {
		return nil
	}
	return saveCredsToStore(m.store, newCreds)
}

// saveCredsToStore writes OAuth credentials to the secrets store.
func saveCredsToStore(store SecretsStore, creds *OAuthCredentials) error {
	store.Set("anthropic.oauth_access_token", creds.AccessToken)
	store.Set("anthropic.oauth_refresh_token", creds.RefreshToken)
	store.Set("anthropic.oauth_expires_at", strconv.FormatInt(creds.ExpiresAt, 10))
	return store.Save()
}

// StartRefresh starts a background goroutine that refreshes the token
// before it expires. Call Stop() to cancel.
func (m *OAuthManager) StartRefresh(ctx context.Context) {
	ctx, m.stop = context.WithCancel(ctx)
	go m.refreshLoop(ctx)
}

// Stop cancels the background refresh goroutine.
func (m *OAuthManager) Stop() {
	if m.stop != nil {
		m.stop()
	}
}

func (m *OAuthManager) refreshLoop(ctx context.Context) {
	for {
		ttl := m.ExpiresIn() - refreshMargin
		if ttl < 0 {
			ttl = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(ttl):
			slog.Info("oauth: refreshing token")
			if err := m.Refresh(); err != nil {
				slog.Error("oauth: refresh failed", "error", err)
				// Retry in 30 seconds on failure
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
			} else {
				slog.Info("oauth: token refreshed", "expires_in", m.ExpiresIn().String())
			}
		}
	}
}

// refreshAccessToken POSTs to the token endpoint with a refresh token.
func refreshAccessToken(client *http.Client, tokenURL, refreshToken string) (*OAuthCredentials, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {OAuthClientID},
		"refresh_token": {refreshToken},
	}

	resp, err := client.PostForm(tokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}

	return &OAuthCredentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).UnixMilli(),
	}, nil
}

// ValidateSetupToken checks that a token has the expected prefix and minimum length.
func ValidateSetupToken(token string) error {
	if token == "" {
		return fmt.Errorf("token is empty")
	}
	if !strings.HasPrefix(token, SetupTokenPrefix) {
		return fmt.Errorf("expected token starting with %s", SetupTokenPrefix)
	}
	if len(token) < SetupTokenMinLength {
		return fmt.Errorf("token looks too short; paste the full setup-token")
	}
	return nil
}

// RunSetupTokenFlow runs the interactive setup-token flow: instructs the user
// to run `claude setup-token`, reads the token from stdin, validates it,
// and saves it to the secrets store as anthropic.setup_token.
func RunSetupTokenFlow(store SecretsStore) error {
	fmt.Println("Run this command in another terminal:")
	fmt.Println()
	fmt.Println("  claude setup-token")
	fmt.Println()
	fmt.Print("Paste the token: ")

	reader := bufio.NewReader(os.Stdin)
	raw, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(raw)

	if err := ValidateSetupToken(token); err != nil {
		return err
	}

	store.Set("anthropic.setup_token", token)
	if err := store.Save(); err != nil {
		return fmt.Errorf("save token: %w", err)
	}

	return nil
}
