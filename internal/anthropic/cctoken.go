// CCTokenSource reads Claude Code credentials from disk on demand.
// It never refreshes tokens itself — it reads what CC writes and triggers
// a refresh via refreshFunc when the token is near expiry or expired.
package anthropic

import (
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/log"
)

// defaultExpiryThreshold is the default time before expiry at which a
// background refresh is triggered.
const defaultExpiryThreshold = 5 * time.Minute

// CCTokenSource reads a credentials file (typically ~/.claude/.credentials.json)
// and provides the current access token via Token(). It reads from disk on each
// Token() call — no background polling. When tokens are near expiry, it triggers
// a refresh callback.
type CCTokenSource struct {
	mu              sync.Mutex
	path            string
	token           string    // last successfully read token
	expiresAt       time.Time // expiry of last read token
	refreshing      bool      // prevents concurrent refresh triggers
	expiryThreshold time.Duration
	refreshFunc     func() // called to trigger token refresh (e.g. run claude)
}

// NewCCTokenSource creates a token source that reads CC credentials from path.
// Returns an error if the file cannot be read or parsed on first attempt.
func NewCCTokenSource(path string) (*CCTokenSource, error) {
	path = expandHome(path)

	creds, err := readCCCredentials(path)
	if err != nil {
		return nil, fmt.Errorf("CC credentials: %w", err)
	}

	return &CCTokenSource{
		path:            path,
		token:           creds.AccessToken,
		expiresAt:       time.UnixMilli(creds.ExpiresAt),
		expiryThreshold: defaultExpiryThreshold,
	}, nil
}

// SetRefreshFunc sets the function called to trigger a token refresh (e.g.
// running claude to force an OAuth refresh). The function is called in a
// new goroutine.
func (s *CCTokenSource) SetRefreshFunc(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshFunc = fn
}

// SetExpiryThreshold sets how far before expiry to trigger a proactive refresh.
func (s *CCTokenSource) SetExpiryThreshold(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiryThreshold = d
}

// Token reads credentials from disk and returns the access token.
// If the file cannot be read, returns the last known token (resilient to
// temporary file rewrites). If the token is expired, triggers a background
// refresh and returns an error.
func (s *CCTokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := readCCCredentials(s.path)
	if err != nil {
		log.Warnf("cctoken", "read %s: %v (using last known token)", s.path, err)
		if s.token == "" {
			return "", fmt.Errorf("no CC token available: %w", err)
		}
		// Check if the cached token is expired.
		if time.Now().After(s.expiresAt) {
			s.triggerRefresh()
			return "", fmt.Errorf("CC token expired at %s, refresh triggered",
				s.expiresAt.Format(time.RFC3339))
		}
		return s.token, nil
	}

	// Update cached state.
	if creds.AccessToken != s.token {
		log.Infof("cctoken", "token updated from %s (expires %s)",
			s.path, time.UnixMilli(creds.ExpiresAt).Format(time.RFC3339))
		// Fresh token arrived — allow future refresh triggers.
		s.refreshing = false
	}
	s.token = creds.AccessToken
	s.expiresAt = time.UnixMilli(creds.ExpiresAt)

	if time.Now().After(s.expiresAt) {
		s.triggerRefresh()
		return "", fmt.Errorf("CC token expired at %s, refresh triggered",
			s.expiresAt.Format(time.RFC3339))
	}

	return s.token, nil
}

// CheckRefresh checks whether the token is near expiry and triggers a
// background refresh if so. Should be called after a successful API call
// to proactively refresh before the next cache miss.
func (s *CCTokenSource) CheckRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.expiresAt.IsZero() {
		return
	}
	if time.Until(s.expiresAt) < s.expiryThreshold {
		log.Infof("cctoken", "token expires in %s (threshold %s), triggering proactive refresh",
			time.Until(s.expiresAt).Round(time.Second), s.expiryThreshold)
		s.triggerRefresh()
	}
}

// triggerRefresh starts a background refresh if one isn't already in flight.
// Caller must hold s.mu.
func (s *CCTokenSource) triggerRefresh() {
	if s.refreshing || s.refreshFunc == nil {
		return
	}
	s.refreshing = true
	fn := s.refreshFunc
	go fn()
}

// readCCCredentials reads and parses a CC credentials file.
func readCCCredentials(path string) (OAuthCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OAuthCredentials{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parseCredentials(data)
}
