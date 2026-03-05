// Package bitwarden provides a dynamic secret store backed by the Bitwarden CLI.
// It caches vault metadata (names, URIs, folders) locally and fetches individual
// passwords on demand via aisudo, which routes through Telegram approval.
//
// Two-tier security model:
//   - "bw list items" runs as the bitwarden system user via aisudo (allowlisted, auto-approved)
//   - "bw get password <id>" runs as the bitwarden system user via aisudo (requires Telegram approval)
//
// Secrets are cached with a configurable TTL and automatically cleaned up.
package bitwarden

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
)

// Item holds vault item metadata (never the password value).
type Item struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Folder   string   `json:"folderId"`
	Username string   `json:"-"` // extracted from login.username
	URIs     []string `json:"-"` // extracted from login.uris[].uri
}

// rawItem is the JSON shape returned by "bw list items".
type rawItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	FolderID string `json:"folderId"`
	Login    *struct {
		Username string `json:"username"`
		URIs     []struct {
			URI string `json:"uri"`
		} `json:"uris"`
	} `json:"login"`
}

// Executor runs bw CLI commands. The default implementation uses aisudo
// for privilege escalation; tests use a mock.
type Executor interface {
	// Run executes a bw CLI command and returns stdout.
	// args are the bw subcommand and arguments (e.g. "list", "items").
	// The implementation decides whether to use sudo/aisudo.
	Run(args ...string) (string, error)
}

// DefaultExecutor runs bw commands via aisudo as the bitwarden system user.
//   - "list" subcommand → `sudo -u bitwarden bw list items ...` (allowlisted, auto-approved)
//   - "get" subcommand → `sudo -u bitwarden bw get password ...` (requires Telegram approval)
//
// The bitwarden user reads its own session file — foci never sees the session token.
type DefaultExecutor struct {
	SessionFile string // path to session file (default /home/bitwarden/.bw_session)
}

// shellCommand builds the shell command string for the bitwarden user.
// Exported for testing only.
func (e *DefaultExecutor) ShellCommand(args ...string) string {
	var bwParts []string
	bwParts = append(bwParts, "bw")
	bwParts = append(bwParts, args...)
	bwParts = append(bwParts, "--nointeraction")
	bwCmd := strings.Join(bwParts, " ")
	return fmt.Sprintf("export BW_SESSION=$(cat %s) && %s", e.SessionFile, bwCmd)
}

// Run executes a bw command via aisudo as the bitwarden user.
// The session token is read from SessionFile by the bitwarden user at execution time.
func (e *DefaultExecutor) Run(args ...string) (string, error) {
	// Wrap: sudo -u bitwarden sh -c 'export BW_SESSION=$(cat FILE) && bw ...'
	shellCmd := e.ShellCommand(args...)
	cmd := exec.Command("sudo", "-u", "bitwarden", "sh", "-c", shellCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr != "" {
			return "", fmt.Errorf("bw %s: %s", args[0], stderrStr)
		}
		return "", fmt.Errorf("bw %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// cachedValue holds a fetched password with its expiry time.
type cachedValue struct {
	value   string
	expires time.Time
}

// Store holds Bitwarden vault metadata and cached secret values.
type Store struct {
	exec      Executor
	secretTTL time.Duration

	mu     sync.RWMutex
	items  []Item
	values map[string]cachedValue // id → cached password

	refreshedAt time.Time // when items were last refreshed

	stopCh chan struct{}
	stopWg sync.WaitGroup
}

// New creates a new Bitwarden store with the given executor and secret TTL.
func New(exec Executor, secretTTL time.Duration) *Store {
	return &Store{
		exec:      exec,
		secretTTL: secretTTL,
		values:    make(map[string]cachedValue),
		stopCh:    make(chan struct{}),
	}
}

// Refresh loads vault item metadata via the executor.
// Only metadata is cached — no passwords are fetched.
func (s *Store) Refresh() error {
	out, err := s.exec.Run("list", "items")
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}

	var raw []rawItem
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return fmt.Errorf("refresh: parse items: %w", err)
	}

	items := make([]Item, 0, len(raw))
	for _, r := range raw {
		item := Item{
			ID:     r.ID,
			Name:   r.Name,
			Folder: r.FolderID,
		}
		if r.Login != nil {
			item.Username = r.Login.Username
			for _, u := range r.Login.URIs {
				if u.URI != "" {
					item.URIs = append(item.URIs, u.URI)
				}
			}
		}
		items = append(items, item)
	}

	s.mu.Lock()
	s.items = items
	s.refreshedAt = time.Now()
	s.mu.Unlock()

	log.Infof("bitwarden", "refreshed %d items", len(items))
	return nil
}

// RefreshedAt returns when the item cache was last refreshed.
func (s *Store) RefreshedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.refreshedAt
}

// ItemCount returns the number of cached items.
func (s *Store) ItemCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// Search returns items matching the query (substring, case-insensitive).
// Matches against name, URI, folder, and username. Returns at most 5 results.
func (s *Store) Search(query string) []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(query)
	var results []Item

	for _, item := range s.items {
		if matchesItem(item, q) {
			results = append(results, item)
			if len(results) >= 5 {
				break
			}
		}
	}
	return results
}

func matchesItem(item Item, query string) bool {
	if strings.Contains(strings.ToLower(item.Name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(item.Username), query) {
		return true
	}
	if strings.Contains(strings.ToLower(item.Folder), query) {
		return true
	}
	for _, uri := range item.URIs {
		if strings.Contains(strings.ToLower(uri), query) {
			return true
		}
	}
	return false
}

// GetPassword returns the cached password for an item, fetching it via the
// executor if not cached or expired. This call may block waiting for aisudo
// approval (Telegram). On denial, returns an error.
func (s *Store) GetPassword(id string) (string, error) {
	s.mu.RLock()
	if cv, ok := s.values[id]; ok && time.Now().Before(cv.expires) {
		s.mu.RUnlock()
		return cv.value, nil
	}
	s.mu.RUnlock()

	// Fetch via executor — may block for Telegram approval
	log.Infof("bitwarden", "fetching password for %s (requires approval)", id)
	val, err := s.exec.Run("get", "password", id)
	if err != nil {
		return "", fmt.Errorf("bitwarden unlock denied by administrator: %w", err)
	}

	s.mu.Lock()
	s.values[id] = cachedValue{
		value:   val,
		expires: time.Now().Add(s.secretTTL),
	}
	s.mu.Unlock()

	return val, nil
}

// AllowedHosts returns the hostnames parsed from a vault item's URIs.
// Used for host validation before sending secrets in HTTP requests.
func (s *Store) AllowedHosts(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, item := range s.items {
		if item.ID == id {
			return parseHosts(item.URIs)
		}
	}
	return nil
}

// CheckHostAllowed validates that the target URL's host is in the vault item's
// URI list. Returns an error if the host is not allowed.
func (s *Store) CheckHostAllowed(id, targetURL string) error {
	hosts := s.AllowedHosts(id)
	if len(hosts) == 0 {
		return fmt.Errorf("bitwarden item %q has no URIs configured — add URIs to the vault item", id)
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", targetURL, err)
	}

	hostname := parsed.Hostname() // strips userinfo and port
	for _, allowed := range hosts {
		if strings.EqualFold(hostname, allowed) {
			return nil
		}
	}

	return fmt.Errorf("host %q not in allowed hosts for bitwarden item %q (allowed: %v)", hostname, id, hosts)
}

// parseHosts extracts hostnames from URIs.
func parseHosts(uris []string) []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, uri := range uris {
		parsed, err := url.Parse(uri)
		if err != nil {
			continue
		}
		h := parsed.Hostname()
		if h == "" {
			continue
		}
		h = strings.ToLower(h)
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// Get returns a cached secret value by vault item ID.
// Used for template resolution. Returns (value, true) if cached and not expired.
func (s *Store) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cv, ok := s.values[name]
	if !ok || time.Now().After(cv.expires) {
		return "", false
	}
	return cv.value, true
}

var bwTemplateRe = regexp.MustCompile(`\{\{secret:bw\.([a-zA-Z0-9\-]+)\}\}`)

// Resolve expands all {{secret:bw.ID}} templates in text with cached values.
// Returns an error if any referenced secret is not cached (must be unlocked first).
func (s *Store) Resolve(text string) (string, error) {
	var resolveErr error

	result := bwTemplateRe.ReplaceAllStringFunc(text, func(match string) string {
		submatch := bwTemplateRe.FindStringSubmatch(match)
		id := submatch[1]

		s.mu.RLock()
		cv, ok := s.values[id]
		s.mu.RUnlock()

		if !ok || time.Now().After(cv.expires) {
			resolveErr = fmt.Errorf("bitwarden secret %q not unlocked — use bitwarden_unlock tool first", id)
			return match
		}
		return cv.value
	})

	if resolveErr != nil {
		return "", resolveErr
	}
	return result, nil
}

// Redact replaces any cached secret values in text with [REDACTED].
// Longer values are checked first to avoid partial matches.
func (s *Store) Redact(text string) string {
	s.mu.RLock()
	var vals []string
	for _, cv := range s.values {
		if len(cv.value) >= 4 && time.Now().Before(cv.expires) {
			vals = append(vals, cv.value)
		}
	}
	s.mu.RUnlock()

	if len(vals) == 0 {
		return text
	}

	// Sort by length descending so longer secrets are redacted first
	for i := 0; i < len(vals); i++ {
		for j := i + 1; j < len(vals); j++ {
			if len(vals[j]) > len(vals[i]) {
				vals[i], vals[j] = vals[j], vals[i]
			}
		}
	}

	for _, v := range vals {
		text = strings.ReplaceAll(text, v, "[REDACTED]")
	}
	return text
}

// CachedIDs returns the IDs of currently cached (unlocked) values.
func (s *Store) CachedIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	var ids []string
	for id, cv := range s.values {
		if now.Before(cv.expires) {
			ids = append(ids, id)
		}
	}
	return ids
}

// StartCleanup starts a background goroutine that removes expired values.
func (s *Store) StartCleanup(interval time.Duration) {
	s.stopWg.Add(1)
	go func() {
		defer s.stopWg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.cleanup()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *Store) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for id, cv := range s.values {
		if now.After(cv.expires) {
			delete(s.values, id)
			log.Debugf("bitwarden", "expired cached value for %s", id)
		}
	}
}

// Close stops the cleanup goroutine.
func (s *Store) Close() {
	close(s.stopCh)
	s.stopWg.Wait()
}

// IsBitwardenRef returns true if the secret name is a bitwarden reference (starts with "bw.").
func IsBitwardenRef(name string) bool {
	return strings.HasPrefix(name, "bw.")
}

// ExtractID returns the vault item ID from a "bw.UUID" secret name.
func ExtractID(name string) string {
	return strings.TrimPrefix(name, "bw.")
}

// ItemByID returns the item with the given ID, or nil if not found.
func (s *Store) ItemByID(id string) *Item {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.items {
		if s.items[i].ID == id {
			item := s.items[i]
			return &item
		}
	}
	return nil
}
