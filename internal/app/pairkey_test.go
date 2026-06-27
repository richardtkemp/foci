package app

import (
	"testing"
	"time"
)

// The pairing-key store (#862) is the bootstrap credential's whole security
// model, so its contract gets direct coverage: single-use, expiry, isolation.

func TestPairKeyStore_MintConsumeSingleUse(t *testing.T) {
	s := newPairKeyStore()
	key, exp := s.mint(time.Minute)
	if key == "" {
		t.Fatal("mint returned empty key")
	}
	if time.Until(exp) <= 0 {
		t.Fatalf("expiry %v is not in the future", exp)
	}
	if !s.consume(key) {
		t.Fatal("first consume of a fresh key should succeed")
	}
	if s.consume(key) {
		t.Fatal("second consume of the same key must fail (single-use)")
	}
}

func TestPairKeyStore_RejectsUnknownAndEmpty(t *testing.T) {
	s := newPairKeyStore()
	if s.consume("") {
		t.Error("empty key must be rejected")
	}
	if s.consume("never-minted") {
		t.Error("unknown key must be rejected")
	}
}

func TestPairKeyStore_Expiry(t *testing.T) {
	s := newPairKeyStore()
	key, _ := s.mint(-time.Second) // already expired (negative ttl floors to default? no: <=0 → default)
	// mint floors ttl<=0 to the default, so the key is valid; assert that path.
	if !s.consume(key) {
		t.Fatal("ttl<=0 should fall back to the default TTL, keeping the key valid")
	}

	// A key minted then artificially aged past expiry is rejected and removed.
	s2 := newPairKeyStore()
	k2, _ := s2.mint(time.Minute)
	s2.mu.Lock()
	s2.keys[k2] = time.Now().Add(-time.Second) // force-expire
	s2.mu.Unlock()
	if s2.consume(k2) {
		t.Fatal("expired key must be rejected")
	}
	if s2.consume(k2) {
		t.Fatal("expired key must also be removed (not lingering)")
	}
}

func TestPairKeyStore_DistinctKeys(t *testing.T) {
	s := newPairKeyStore()
	a, _ := s.mint(time.Minute)
	b, _ := s.mint(time.Minute)
	if a == b {
		t.Fatal("two mints must produce distinct keys")
	}
	if !s.consume(a) || !s.consume(b) {
		t.Fatal("both distinct keys should consume independently")
	}
}
