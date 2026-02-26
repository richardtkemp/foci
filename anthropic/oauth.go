package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// OAuthRefreshURL is the Anthropic OAuth token refresh endpoint.
	OAuthRefreshURL = "https://console.anthropic.com/api/oauth/token"

	// OAuthClientID is the Claude Code OAuth client ID.
	OAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// RefreshWindow is how far before expiry to proactively refresh.
	RefreshWindow = 30 * time.Minute

	// RefreshInterval is how often the background ticker checks expiry.
	RefreshInterval = 5 * time.Minute
)

// OAuthCredentials represents the claudeAiOauth section of the credentials file.
type OAuthCredentials struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        string `json:"expiresAt"`
	Scopes           string `json:"scopes"`
	SubscriptionType string `json:"subscriptionType"`
	RateLimitTier    string `json:"rateLimitTier"`
}

// CredentialsFile wraps the top-level credentials JSON.
type CredentialsFile struct {
	ClaudeAiOauth OAuthCredentials `json:"claudeAiOauth"`
}

// OAuthManager manages OAuth token lifecycle: proactive background refresh
// and reactive refresh on 401 errors. Thread-safe.
type OAuthManager struct {
	credPath   string
	httpClient *http.Client
	refreshURL string
	logFunc    func(format string, args ...any)

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	refreshing   bool // true while a refresh is in progress (dedup guard)

	stop chan struct{}
	done chan struct{}
}

// OAuthOption configures an OAuthManager.
type OAuthOption func(*OAuthManager)

// WithLogger sets a log function for the OAuthManager.
func WithLogger(fn func(format string, args ...any)) OAuthOption {
	return func(m *OAuthManager) {
		m.logFunc = fn
	}
}

// WithRefreshURL overrides the refresh endpoint (for testing).
func WithRefreshURL(url string) OAuthOption {
	return func(m *OAuthManager) {
		m.refreshURL = url
	}
}

// WithHTTPClient sets a custom HTTP client (for testing).
func WithHTTPClient(c *http.Client) OAuthOption {
	return func(m *OAuthManager) {
		m.httpClient = c
	}
}

// NewOAuthManager creates an OAuthManager that reads credentials from credPath.
// It reads the file immediately to populate the initial token state.
func NewOAuthManager(credPath string, opts ...OAuthOption) (*OAuthManager, error) {
	m := &OAuthManager{
		credPath:   credPath,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		refreshURL: OAuthRefreshURL,
		logFunc:    func(format string, args ...any) {},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}

	if err := m.readCredentials(); err != nil {
		return nil, fmt.Errorf("oauth: read credentials: %w", err)
	}

	return m, nil
}

// Token returns the current access token. Thread-safe.
func (m *OAuthManager) Token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accessToken
}

// RefreshIfNeeded performs a reactive refresh when a 401 is received.
// If the current token differs from staleToken, another goroutine already
// refreshed it, so this returns nil immediately (dedup). Also deduplicates
// concurrent refresh attempts using a refreshing flag.
func (m *OAuthManager) RefreshIfNeeded(staleToken string) error {
	m.mu.Lock()
	if m.accessToken != staleToken || m.refreshing {
		// Already refreshed or refresh in progress.
		m.mu.Unlock()
		return nil
	}
	m.refreshing = true
	m.mu.Unlock()

	err := m.refresh()

	m.mu.Lock()
	m.refreshing = false
	m.mu.Unlock()

	return err
}

// Start begins the background proactive refresh goroutine.
func (m *OAuthManager) Start() {
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(RefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				remaining := time.Until(m.expiresAt)
				m.mu.Unlock()

				if remaining < RefreshWindow && remaining > 0 {
					m.logFunc("oauth: token expires in %s, refreshing proactively", remaining.Round(time.Second))
					if err := m.refresh(); err != nil {
						m.logFunc("oauth: proactive refresh failed: %v", err)
					}
				}
			}
		}
	}()
}

// Stop stops the background refresh goroutine and waits for it to finish.
func (m *OAuthManager) Stop() {
	close(m.stop)
	<-m.done
}

// refresh performs the actual token refresh via the OAuth endpoint.
func (m *OAuthManager) refresh() error {
	m.mu.Lock()
	rt := m.refreshToken
	m.mu.Unlock()

	if rt == "" {
		return fmt.Errorf("oauth: no refresh token available")
	}

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": rt,
		"client_id":     OAuthClientID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("oauth: marshal refresh request: %w", err)
	}

	req, err := http.NewRequest("POST", m.refreshURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("oauth: create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("oauth: read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: refresh failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("oauth: unmarshal refresh response: %w", err)
	}

	if result.AccessToken == "" {
		return fmt.Errorf("oauth: refresh response missing access_token")
	}

	expiresAt := time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)

	m.mu.Lock()
	m.accessToken = result.AccessToken
	if result.RefreshToken != "" {
		m.refreshToken = result.RefreshToken
	}
	m.expiresAt = expiresAt
	m.mu.Unlock()

	// Write back to credentials file
	if err := m.writeCredentials(); err != nil {
		m.logFunc("oauth: write credentials: %v", err)
		// Non-fatal — in-memory token is still updated.
	}

	m.logFunc("oauth: token refreshed, expires at %s", expiresAt.Format(time.RFC3339))
	return nil
}

// readCredentials reads the credentials file with a shared flock.
func (m *OAuthManager) readCredentials() error {
	path := expandHome(m.credPath)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("flock shared %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var creds CredentialsFile
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	if creds.ClaudeAiOauth.AccessToken == "" {
		return fmt.Errorf("credentials file %s: missing accessToken", path)
	}
	if creds.ClaudeAiOauth.RefreshToken == "" {
		return fmt.Errorf("credentials file %s: missing refreshToken", path)
	}

	var expiresAt time.Time
	if creds.ClaudeAiOauth.ExpiresAt != "" {
		expiresAt, err = time.Parse(time.RFC3339, creds.ClaudeAiOauth.ExpiresAt)
		if err != nil {
			// Try other formats
			expiresAt, err = time.Parse(time.RFC3339Nano, creds.ClaudeAiOauth.ExpiresAt)
			if err != nil {
				m.logFunc("oauth: cannot parse expiresAt %q, treating as expired", creds.ClaudeAiOauth.ExpiresAt)
			}
		}
	}

	m.mu.Lock()
	m.accessToken = creds.ClaudeAiOauth.AccessToken
	m.refreshToken = creds.ClaudeAiOauth.RefreshToken
	m.expiresAt = expiresAt
	m.mu.Unlock()

	return nil
}

// writeCredentials does a read-modify-write of the credentials file
// using an exclusive flock. Uses map[string]interface{} to preserve
// unknown fields that Claude Code might add.
func (m *OAuthManager) writeCredentials() error {
	path := expandHome(m.credPath)

	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open %s for write: %w", path, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock exclusive %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Parse as generic map to preserve unknown fields.
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	// Get or create the claudeAiOauth section.
	oauthRaw, ok := doc["claudeAiOauth"]
	if !ok {
		oauthRaw = map[string]interface{}{}
	}
	oauth, ok := oauthRaw.(map[string]interface{})
	if !ok {
		oauth = map[string]interface{}{}
	}

	m.mu.Lock()
	oauth["accessToken"] = m.accessToken
	if m.refreshToken != "" {
		oauth["refreshToken"] = m.refreshToken
	}
	if !m.expiresAt.IsZero() {
		oauth["expiresAt"] = m.expiresAt.Format(time.RFC3339)
	}
	m.mu.Unlock()

	doc["claudeAiOauth"] = oauth

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	out = append(out, '\n')

	// Truncate and rewrite.
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate %s: %w", path, err)
	}
	if _, err := f.Write(out); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// expandHome resolves ~ to the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home + path[1:]
	}
	return path
}
