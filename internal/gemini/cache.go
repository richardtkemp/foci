package gemini

import (
	"context"
	"crypto/md5" // #nosec G501 - used for cache key generation, not security
	"encoding/json"
	"strings"
	"sync"
	"time"

	"foci/internal/log"

	"google.golang.org/genai"
)

// CacheManager manages explicit Gemini cached content for a client.
// It creates a cache from system instruction + tools (the stable prefix),
// reuses it when the content hasn't changed, and extends the TTL before expiry.
type CacheManager struct {
	client *genai.Client
	ttl    time.Duration

	mu        sync.Mutex
	cacheName string    // server-assigned cache resource name
	cacheHash [16]byte  // MD5 of system + tools content
	expiresAt time.Time // when the cache expires
	model     string    // model the cache was created for

	cachingNotSupported bool // true if we've detected free tier (no caching available)
}

// NewCacheManager creates a new cache manager with the given TTL.
func NewCacheManager(client *genai.Client, ttl time.Duration) *CacheManager {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &CacheManager{
		client: client,
		ttl:    ttl,
	}
}

// EnsureCache creates or reuses a cache for the given system instruction and tools.
// Returns the cache name if successful, or empty string if caching should be skipped.
// When a cache name is returned, the caller should NOT pass SystemInstruction or
// Tools in the GenerateContentConfig — they're already in the cache.
func (m *CacheManager) EnsureCache(ctx context.Context, model string, system *genai.Content, tools []*genai.Tool) string {
	if system == nil && len(tools) == 0 {
		return "" // nothing to cache
	}

	hash := contentHash(system, tools)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Skip cache creation if we've detected free tier
	if m.cachingNotSupported {
		return "" // caching not available, already warned
	}

	// Reuse existing cache if content matches and not expired
	if m.cacheName != "" && m.cacheHash == hash && m.model == model {
		if time.Now().Before(m.expiresAt) {
			// Extend TTL if past halfway to prevent expiry during active use
			if time.Now().After(m.expiresAt.Add(-m.ttl / 2)) {
				m.extendTTL(ctx)
			}
			return m.cacheName
		}
		// Cache expired — delete and recreate
		m.deleteLocked(ctx)
	}

	// Content changed — delete old cache
	if m.cacheName != "" && (m.cacheHash != hash || m.model != model) {
		m.deleteLocked(ctx)
	}

	// Create new cache
	cfg := &genai.CreateCachedContentConfig{
		TTL:         m.ttl,
		DisplayName: "foci-system-cache",
	}
	if system != nil {
		cfg.SystemInstruction = system
	}
	if len(tools) > 0 {
		cfg.Tools = tools
	}

	cached, err := m.client.Caches.Create(ctx, model, cfg)
	if err != nil {
		isFreeTier := logCacheCreationError(err)
		if isFreeTier {
			m.cachingNotSupported = true // remember to skip future attempts
		}
		return ""
	}

	m.cacheName = cached.Name
	m.cacheHash = hash
	m.expiresAt = time.Now().Add(m.ttl)
	m.model = model
	log.Infof("gemini_cache", "created cache %s (ttl=%s, model=%s)", m.cacheName, m.ttl, model)
	return m.cacheName
}

// extendTTL extends the cache TTL. Must be called with m.mu held.
func (m *CacheManager) extendTTL(ctx context.Context) {
	if m.cacheName == "" {
		return
	}
	_, err := m.client.Caches.Update(ctx, m.cacheName, &genai.UpdateCachedContentConfig{
		TTL: m.ttl,
	})
	if err != nil {
		log.Warnf("gemini_cache", "extend TTL: %v", err)
		return
	}
	m.expiresAt = time.Now().Add(m.ttl)
	log.Debugf("gemini_cache", "extended cache TTL to %s", m.expiresAt.Format(time.RFC3339))
}

// deleteLocked deletes the current cache. Must be called with m.mu held.
func (m *CacheManager) deleteLocked(ctx context.Context) {
	if m.cacheName == "" {
		return
	}
	_, err := m.client.Caches.Delete(ctx, m.cacheName, nil)
	if err != nil {
		log.Warnf("gemini_cache", "delete cache %s: %v", m.cacheName, err)
	} else {
		log.Infof("gemini_cache", "deleted cache %s", m.cacheName)
	}
	m.cacheName = ""
	m.cacheHash = [16]byte{}
	m.expiresAt = time.Time{}
	m.model = ""
}

// Close deletes any active cache. Safe to call multiple times.
func (m *CacheManager) Close(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteLocked(ctx)
}

// IsCachingNotSupported returns true if caching was detected as unavailable (e.g., free tier).
// Thread-safe: uses mutex to protect access to cachingNotSupported field.
func (m *CacheManager) IsCachingNotSupported() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cachingNotSupported
}

// contentHash computes a hash of the system instruction and tools for change detection.
func contentHash(system *genai.Content, tools []*genai.Tool) [16]byte {
	h := md5.New() // #nosec G401 - used for cache key generation, not security
	enc := json.NewEncoder(h)
	if system != nil {
		_ = enc.Encode(system) // encoding to hash, errors are impossible
	}
	for _, t := range tools {
		_ = enc.Encode(t) // encoding to hash, errors are impossible
	}
	var result [16]byte
	copy(result[:], h.Sum(nil))
	return result
}

// logCacheCreationError logs cache creation errors with context-appropriate messages.
// Returns true if the error indicates a free-tier account with no caching available.
func logCacheCreationError(err error) bool {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "RESOURCE_EXHAUSTED"):
		if strings.Contains(msg, "TotalCachedContentStorageTokensPerModelFreeTier") && strings.Contains(msg, "limit=0") {
			log.Warnf("gemini_cache", "caching not available on free tier (limit=0), continuing without cache")
			return true // free tier detected
		} else {
			log.Warnf("gemini_cache", "cache rate limited (429), continuing without cache")
		}
	default:
		log.Warnf("gemini_cache", "create cache failed: %v", err)
	}
	return false
}
