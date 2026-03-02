// CCTokenSource reads Claude Code credentials from disk on a polling interval.
// It never refreshes tokens itself — it only reads what CC writes.
package anthropic

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"foci/log"
)

// CCTokenSource polls a credentials file (typically ~/.claude/.credentials.json)
// and provides the current access token via Token(). It never performs OAuth
// refreshes — it only reads tokens that Claude Code has written to disk.
type CCTokenSource struct {
	mu           sync.RWMutex
	path         string
	token        string
	expiresAt    time.Time
	lastRead     time.Time
	pollInterval time.Duration
	onExpired    func()
	expiredFired bool
	stop         context.CancelFunc
}

// NewCCTokenSource creates a token source that reads CC credentials from path.
// Returns an error if the file cannot be read or parsed on first attempt.
func NewCCTokenSource(path string, pollInterval time.Duration) (*CCTokenSource, error) {
	path = expandHome(path)

	creds, err := readCCCredentials(path)
	if err != nil {
		return nil, fmt.Errorf("CC credentials: %w", err)
	}

	src := &CCTokenSource{
		path:         path,
		token:        creds.AccessToken,
		expiresAt:    time.UnixMilli(creds.ExpiresAt),
		lastRead:     time.Now(),
		pollInterval: pollInterval,
	}
	return src, nil
}

// OnExpired sets a callback that fires once when token expiry is detected.
// The callback resets when fresh tokens are subsequently read from disk.
func (s *CCTokenSource) OnExpired(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onExpired = fn
}

// Token returns the current access token. It re-reads the file if the poll
// interval has elapsed. Never returns an error for expired tokens — returns
// the last known (possibly stale) token instead.
func (s *CCTokenSource) Token() (string, error) {
	s.mu.RLock()
	token := s.token
	elapsed := time.Since(s.lastRead)
	interval := s.pollInterval
	s.mu.RUnlock()

	if elapsed < interval {
		if token == "" {
			return "", fmt.Errorf("no CC token available")
		}
		return token, nil
	}

	// Poll interval elapsed — re-read file.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check under write lock (another goroutine may have refreshed).
	if time.Since(s.lastRead) < s.pollInterval {
		if s.token == "" {
			return "", fmt.Errorf("no CC token available")
		}
		return s.token, nil
	}

	s.reload()

	if s.token == "" {
		return "", fmt.Errorf("no CC token available")
	}
	return s.token, nil
}

// Start begins background polling. The token source also polls lazily on
// Token() calls, but Start ensures timely expiry detection even when Token()
// isn't called frequently.
func (s *CCTokenSource) Start(ctx context.Context) {
	ctx, s.stop = context.WithCancel(ctx)
	go s.pollLoop(ctx)
}

// Stop cancels background polling.
func (s *CCTokenSource) Stop() {
	if s.stop != nil {
		s.stop()
	}
}

// pollLoop runs in the background, re-reading credentials on each tick.
func (s *CCTokenSource) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.reload()
			s.mu.Unlock()
		}
	}
}

// reload re-reads credentials from disk. Caller must hold s.mu write lock.
func (s *CCTokenSource) reload() {
	s.lastRead = time.Now()

	creds, err := readCCCredentials(s.path)
	if err != nil {
		log.Warnf("cctoken", "re-read %s: %v (using last known token)", s.path, err)
		s.checkExpiry()
		return
	}

	// Token changed — update cache.
	if creds.AccessToken != s.token {
		s.token = creds.AccessToken
		s.expiresAt = time.UnixMilli(creds.ExpiresAt)
		// Fresh token detected — reset expiredFired so callback can fire again.
		if time.Now().Before(s.expiresAt) {
			s.expiredFired = false
		}
		log.Infof("cctoken", "token updated from %s (expires %s)",
			s.path, s.expiresAt.Format(time.RFC3339))
	} else {
		// Same token, but expiry may have changed (unlikely but possible).
		s.expiresAt = time.UnixMilli(creds.ExpiresAt)
	}

	s.checkExpiry()
}

// checkExpiry fires the onExpired callback once if the token has expired.
// Caller must hold s.mu write lock.
func (s *CCTokenSource) checkExpiry() {
	if s.expiredFired || s.onExpired == nil {
		return
	}
	if time.Now().After(s.expiresAt) {
		s.expiredFired = true
		fn := s.onExpired
		// Fire callback outside the lock to avoid deadlocks.
		go fn()
	}
}

// readCCCredentials reads and parses a CC credentials file.
func readCCCredentials(path string) (OAuthCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OAuthCredentials{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parseCredentials(data)
}
