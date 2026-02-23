package telegram

import (
	"testing"
	"time"

	"clod/command"
)

func testSecondaryBot(name string) *Bot {
	allowed := map[string]bool{"111": true}
	return &Bot{
		client:       &mockClient{},
		commands:     command.NewRegistry(),
		allowedUsers: allowed,
		queue:        make(chan queuedMessage, 64),
		isSecondary:  true,
	}
}

func TestPool_AcquireRelease(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot1.pool = pool
	pool.Add(bot1)

	if pool.Size() != 1 {
		t.Fatalf("size = %d, want 1", pool.Size())
	}
	if pool.Available() != 1 {
		t.Fatalf("available = %d, want 1", pool.Available())
	}

	// Acquire
	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	if b != bot1 {
		t.Fatal("acquired wrong bot")
	}

	// Bot should not be idle anymore — it has no session key set by Acquire,
	// but the caller sets it. Let's simulate that.
	b.SetSessionKey("agent:main:multiball:mb-1")

	if pool.Available() != 0 {
		t.Fatalf("available = %d, want 0", pool.Available())
	}

	// Acquire again should fail
	_, ok = pool.Acquire()
	if ok {
		t.Fatal("should not acquire when all busy")
	}

	// Release
	pool.Release(bot1)
	if bot1.SessionKey() != "" {
		t.Fatal("session key should be cleared after release")
	}
	if pool.Available() != 1 {
		t.Fatalf("available = %d, want 1", pool.Available())
	}
}

func TestPool_AcquireLRU(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	pool.Add(bot1)
	pool.Add(bot2)

	// First acquire should get bot1 (both have zero time, bot1 is first)
	b1, _ := pool.Acquire()
	b1.SetSessionKey("session-1")

	// Second acquire should get bot2 (bot1 is busy)
	b2, ok := pool.Acquire()
	if !ok {
		t.Fatal("second acquire failed")
	}
	if b2 == b1 {
		t.Fatal("should acquire different bot")
	}
	b2.SetSessionKey("session-2")

	// Both busy
	if pool.Available() != 0 {
		t.Fatal("both should be busy")
	}

	// Release bot1
	pool.Release(b1)

	// Acquire should get bot1 (LRU — bot1 was used first)
	b3, _ := pool.Acquire()
	if b3 != b1 {
		t.Fatal("LRU should return bot1")
	}
}

func TestPool_Empty(t *testing.T) {
	pool := NewPool()
	if pool.Size() != 0 {
		t.Fatalf("size = %d, want 0", pool.Size())
	}
	_, ok := pool.Acquire()
	if ok {
		t.Fatal("should not acquire from empty pool")
	}
}

// mockSessionChecker implements SessionActivityChecker for testing.
type mockSessionChecker struct {
	activities map[string]string // session key → RFC3339 timestamp or "n/a"
}

func (m *mockSessionChecker) LastActivity(key string) string {
	if v, ok := m.activities[key]; ok {
		return v
	}
	return "n/a"
}

func TestPool_TTLReclaimsStaleBot(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	// Acquire and assign a session
	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	b.SetSessionKey("agent:main:multiball:mb-1")

	// All bots busy, no TTL configured — should fail
	_, ok = pool.Acquire()
	if ok {
		t.Fatal("should not acquire when all busy (no TTL)")
	}

	// Configure TTL with a stale session (last activity 2 hours ago)
	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:multiball:mb-1": staleTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// Now acquire should auto-reclaim the stale bot
	b2, ok := pool.Acquire()
	if !ok {
		t.Fatal("should reclaim stale bot")
	}
	if b2 != bot1 {
		t.Fatal("should reclaim the same bot")
	}
}

func TestPool_TTLDoesNotReclaimActiveBot(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	// Acquire and assign a session
	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	b.SetSessionKey("agent:main:multiball:mb-1")

	// Configure TTL with an active session (last activity 5 minutes ago)
	recentTime := time.Now().Add(-5 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:multiball:mb-1": recentTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// Should NOT reclaim — session is still active
	_, ok = pool.Acquire()
	if ok {
		t.Fatal("should not reclaim active bot")
	}

	// Bot should still have its session
	if bot1.SessionKey() != "agent:main:multiball:mb-1" {
		t.Fatalf("session key should be unchanged, got %q", bot1.SessionKey())
	}
}

func TestPool_TTLReclaimsPhantomSession(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	b.SetSessionKey("agent:main:multiball:mb-gone")

	// Session doesn't exist in the store (returns "n/a")
	checker := &mockSessionChecker{
		activities: map[string]string{}, // empty — session not found
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// Should reclaim — session file doesn't exist
	b2, ok := pool.Acquire()
	if !ok {
		t.Fatal("should reclaim phantom session bot")
	}
	if b2 != bot1 {
		t.Fatal("should reclaim the same bot")
	}
}

func TestPool_AllBotsBusyWithTTL(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	pool.Add(bot1)
	pool.Add(bot2)

	// Acquire both
	b1, _ := pool.Acquire()
	b1.SetSessionKey("agent:main:multiball:mb-1")
	b2, _ := pool.Acquire()
	b2.SetSessionKey("agent:main:multiball:mb-2")

	// Both sessions are active (recent activity)
	recentTime := time.Now().Add(-10 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:multiball:mb-1": recentTime,
			"agent:main:multiball:mb-2": recentTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// Should fail — both actively in use
	_, ok := pool.Acquire()
	if ok {
		t.Fatal("should not acquire when all bots actively in use")
	}
}

func TestPool_ZeroTTLDisablesReclaim(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	b, _ := pool.Acquire()
	b.SetSessionKey("agent:main:multiball:mb-1")

	// TTL=0 means no auto-reclaim even with a checker
	staleTime := time.Now().Add(-24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:multiball:mb-1": staleTime,
		},
	}
	pool.SetSessionTTL(0, checker) // disabled

	_, ok := pool.Acquire()
	if ok {
		t.Fatal("TTL=0 should not reclaim")
	}
}

func TestPool_MixedStaleAndActive(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	pool.Add(bot1)
	pool.Add(bot2)

	// Acquire both
	b1, _ := pool.Acquire()
	b1.SetSessionKey("agent:main:multiball:mb-1")
	b2, _ := pool.Acquire()
	b2.SetSessionKey("agent:main:multiball:mb-2")

	// bot1's session is stale, bot2's is active
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:multiball:mb-1": time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
			"agent:main:multiball:mb-2": time.Now().Add(-5 * time.Minute).UTC().Format("2006-01-02T15:04:05Z"),
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// Should reclaim bot1 (stale), not bot2 (active)
	acquired, ok := pool.Acquire()
	if !ok {
		t.Fatal("should reclaim stale bot1")
	}
	if acquired != bot1 {
		t.Fatal("should reclaim bot1 (stale), not bot2 (active)")
	}
}
