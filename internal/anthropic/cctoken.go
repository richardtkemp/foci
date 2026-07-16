// CCTokenSource reads Claude Code credentials from disk on demand.
// It reads what CC writes — if the token is expired, callers get an error.
package anthropic

import (
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/log"
)

var (
	anthropicLog = log.NewComponentLogger("anthropic")
	cctokenLog   = log.NewComponentLogger("cctoken")
)

// CCTokenSource reads a credentials file (typically ~/.claude/.credentials.json)
// and provides the current access token via Token(). It reads from disk on each
// Token() call. If the token is expired, it returns an error.
type CCTokenSource struct {
	mu        sync.Mutex
	path      string
	token     string    // last successfully read token
	expiresAt time.Time // expiry of last read token
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
		path:      path,
		token:     creds.AccessToken,
		expiresAt: time.UnixMilli(creds.ExpiresAt),
	}, nil
}

// Token reads credentials from disk and returns the access token.
// If the file cannot be read, returns the last known token (resilient to
// temporary file rewrites). If the token is expired, returns an error.
func (s *CCTokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := readCCCredentials(s.path)
	if err != nil {
		cctokenLog.Warnf("read %s: %v (using last known token)", s.path, err)
		if s.token == "" {
			return "", fmt.Errorf("no CC token available: %w", err)
		}
		if time.Now().After(s.expiresAt) {
			return "", fmt.Errorf("CC token expired at %s", s.expiresAt.Format(time.RFC3339))
		}
		return s.token, nil
	}

	if creds.AccessToken != s.token {
		cctokenLog.Infof("token updated from %s (expires %s)",
			s.path, time.UnixMilli(creds.ExpiresAt).Format(time.RFC3339))
	}
	s.token = creds.AccessToken
	s.expiresAt = time.UnixMilli(creds.ExpiresAt)

	if time.Now().After(s.expiresAt) {
		return "", fmt.Errorf("CC token expired at %s", s.expiresAt.Format(time.RFC3339))
	}

	return s.token, nil
}

// readCCCredentials reads and parses a CC credentials file.
func readCCCredentials(path string) (OAuthCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OAuthCredentials{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parseCredentials(data)
}
