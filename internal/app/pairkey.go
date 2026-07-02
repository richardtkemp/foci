package app

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"time"

	"foci/internal/secrets"
)

const (
	pairKeyBytes      = 32               // 256-bit single-use pairing keys (fallback)
	pairKeyWords      = 5                // human-readable passphrase length (~52 bits)
	defaultPairKeyTTL = 10 * time.Minute // short window: pair right after minting
)

// pairKeyStore holds single-use, short-TTL pairing keys entirely in memory
// (#862). A device exchanges a pairing key at POST /app/pair for its long-lived
// revocable device token; the key is consumed on first use and NEVER touches
// disk. Because pairing requires a live gateway anyway, an in-memory store is
// sufficient — a restart simply clears any unused key (re-mint to pair again).
// This replaces the old persisted app.api_key "master key": there is no longer
// any long-lived shared secret to leak.
//
// Keys are 256-bit random tokens looked up by map, so (as with device tokens)
// the token's entropy — not a constant-time compare — is the defense.
type pairKeyStore struct {
	mu   sync.Mutex
	keys map[string]time.Time // key -> expiry
}

func newPairKeyStore() *pairKeyStore {
	return &pairKeyStore{keys: make(map[string]time.Time)}
}

func newPairKey() string {
	if p, err := secrets.GeneratePassphrase(pairKeyWords); err == nil {
		return p
	}
	b := make([]byte, pairKeyBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// normalizePairKey makes manual entry forgiving: a user typing the passphrase
// with spaces or mixed case still matches the minted lowercase-hyphen form.
func normalizePairKey(k string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(k, "-", " ")), "-"))
}

// mint generates a pairing key valid for ttl (defaultPairKeyTTL when <= 0) and
// returns it with its expiry. Expired keys are swept on the way in.
func (s *pairKeyStore) mint(ttl time.Duration) (string, time.Time) {
	if ttl <= 0 {
		ttl = defaultPairKeyTTL
	}
	key := newPairKey()
	exp := time.Now().Add(ttl)
	s.mu.Lock()
	s.sweepLocked()
	s.keys[normalizePairKey(key)] = exp
	s.mu.Unlock()
	return key, exp
}

// consume validates a pairing key and removes it (single-use), returning false
// if the key is unknown or expired. An expired-but-present key is still deleted.
func (s *pairKeyStore) consume(key string) bool {
	key = normalizePairKey(key)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.keys[key]
	if !ok {
		return false
	}
	delete(s.keys, key)
	return !time.Now().After(exp)
}

func (s *pairKeyStore) sweepLocked() {
	now := time.Now()
	for k, exp := range s.keys {
		if now.After(exp) {
			delete(s.keys, k)
		}
	}
}
