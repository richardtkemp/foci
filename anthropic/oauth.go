package anthropic

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OAuth PKCE constants matching Claude Code's OAuth flow.
const (
	OAuthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	OAuthAuthURL     = "https://claude.ai/oauth/authorize"
	OAuthTokenURL    = "https://console.anthropic.com/v1/oauth/token"
	OAuthRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	OAuthScopes      = "org:create_api_key user:profile user:inference"
	refreshMargin    = 5 * time.Minute
)

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
	credsPath  string
	httpClient *http.Client
	tokenURL   string // overridable for testing
	stop       context.CancelFunc
}

// NewOAuthManager loads credentials from disk and returns a manager.
// It reads foci-native format first, falling back to Claude Code's
// claudeAiOauth format. Returns an error if the file is missing or
// contains no refresh token (e.g. a static setup-token).
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
		credsPath:  credsPath,
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

	return saveCredentials(m.credsPath, newCreds)
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
	defer resp.Body.Close()

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

// saveCredentials atomically writes credentials to disk with 0600 permissions.
func saveCredentials(path string, creds *OAuthCredentials) error {
	data, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".creds-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename credentials: %w", err)
	}
	return nil
}

// --- PKCE helpers ---

// GeneratePKCE generates a PKCE code verifier and its SHA256 challenge.
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate random: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)

	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// BuildAuthURL returns the full OAuth authorization URL with PKCE challenge.
func BuildAuthURL(challenge string) string {
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {OAuthClientID},
		"redirect_uri":          {OAuthRedirectURI},
		"scope":                 {OAuthScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return OAuthAuthURL + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for tokens.
func ExchangeCode(ctx context.Context, code, verifier string) (*OAuthCredentials, error) {
	return exchangeCodeWithURL(ctx, OAuthTokenURL, code, verifier)
}

func exchangeCodeWithURL(ctx context.Context, tokenURL, code, verifier string) (*OAuthCredentials, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {OAuthClientID},
		"code":          {code},
		"redirect_uri":  {OAuthRedirectURI},
		"code_verifier": {verifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.URL.RawQuery = ""
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Body = io.NopCloser(nil)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.PostForm(tokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse exchange response: %w", err)
	}

	return &OAuthCredentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).UnixMilli(),
	}, nil
}

// RunAuthFlow runs the interactive OAuth PKCE flow: prints an auth URL,
// reads the authorization code from stdin, exchanges it, and saves credentials.
func RunAuthFlow(credsPath string) error {
	credsPath = expandHome(credsPath)

	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return fmt.Errorf("generate PKCE: %w", err)
	}

	authURL := BuildAuthURL(challenge)
	fmt.Println("Open this URL in your browser to authenticate:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Print("Paste the authorization code: ")

	var code string
	if _, err := fmt.Scanln(&code); err != nil {
		return fmt.Errorf("read code: %w", err)
	}

	creds, err := ExchangeCode(context.Background(), code, verifier)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}

	if err := saveCredentials(credsPath, creds); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	fmt.Printf("Credentials saved to %s\n", credsPath)
	return nil
}
