package telegram

import (
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
)

func testSecondaryBot(name string) *Bot {
	allowed := map[string]bool{"111": true}
	b := &Bot{
		client:       &mockClient{},
		commands:     command.NewRegistry(),
		allowedUsers: allowed,
		isSecondary:  true,
	}
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{
		Size:       64,
		TurnActive: b.isTurnActive,
	})
	return b
}

func TestPool_AcquireRelease(t *testing.T) {
	// Verifies the basic acquire/release lifecycle: adding a bot makes it available,
	// acquiring it reduces availability, and releasing it restores availability and
	// clears the session key.
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
	b.SetSessionKey("agent:main:facet:f-1")

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
	// Verifies the LRU (least recently used) acquisition policy: after acquiring
	// and releasing bot1, a subsequent acquire returns bot1 again because it was
	// used longest ago.
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
	// Verifies that acquiring from an empty pool returns false without panicking.
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
	// Verifies that when all bots are busy but a bot's session has exceeded
	// the TTL, it is automatically reclaimed and returned by the next Acquire.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	// Acquire and assign a session
	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	b.SetSessionKey("agent:main:facet:f-1")

	// All bots busy, no TTL configured — should fail
	_, ok = pool.Acquire()
	if ok {
		t.Fatal("should not acquire when all busy (no TTL)")
	}

	// Configure TTL with a stale session (last activity 2 hours ago)
	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": staleTime,
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
	// Verifies that a bot whose session has recent activity is not reclaimed
	// by the TTL mechanism, even when all bots are busy.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	// Acquire and assign a session
	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	b.SetSessionKey("agent:main:facet:f-1")

	// Configure TTL with an active session (last activity 5 minutes ago)
	recentTime := time.Now().Add(-5 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": recentTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// Should NOT reclaim — session is still active
	_, ok = pool.Acquire()
	if ok {
		t.Fatal("should not reclaim active bot")
	}

	// Bot should still have its session
	if bot1.SessionKey() != "agent:main:facet:f-1" {
		t.Fatalf("session key should be unchanged, got %q", bot1.SessionKey())
	}
}

func TestPool_TTLReclaimsPhantomSession(t *testing.T) {
	// Verifies that a bot holding a session key that no longer exists in the
	// store (returns "n/a") is treated as stale and reclaimed by TTL.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	b, ok := pool.Acquire()
	if !ok {
		t.Fatal("acquire failed")
	}
	b.SetSessionKey("agent:main:facet:f-gone")

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
	// Verifies that when all bots have active (non-stale) sessions, TTL does not
	// reclaim them and Acquire correctly returns false.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	pool.Add(bot1)
	pool.Add(bot2)

	// Acquire both
	b1, _ := pool.Acquire()
	b1.SetSessionKey("agent:main:facet:f-1")
	b2, _ := pool.Acquire()
	b2.SetSessionKey("agent:main:facet:f-2")

	// Both sessions are active (recent activity)
	recentTime := time.Now().Add(-10 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": recentTime,
			"agent:main:facet:f-2": recentTime,
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
	// Verifies that setting TTL=0 disables automatic reclaim entirely, even
	// when a checker is configured and sessions are demonstrably stale.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	b, _ := pool.Acquire()
	b.SetSessionKey("agent:main:facet:f-1")

	// TTL=0 means no auto-reclaim even with a checker
	staleTime := time.Now().Add(-24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": staleTime,
		},
	}
	pool.SetSessionTTL(0, checker) // disabled

	_, ok := pool.Acquire()
	if ok {
		t.Fatal("TTL=0 should not reclaim")
	}
}

func TestPool_MixedStaleAndActive(t *testing.T) {
	// Verifies that when one bot's session is stale and another is active,
	// only the stale bot is reclaimed and the active bot is left untouched.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	pool.Add(bot1)
	pool.Add(bot2)

	// Acquire both
	b1, _ := pool.Acquire()
	b1.SetSessionKey("agent:main:facet:f-1")
	b2, _ := pool.Acquire()
	b2.SetSessionKey("agent:main:facet:f-2")

	// bot1's session is stale, bot2's is active
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
			"agent:main:facet:f-2": time.Now().Add(-5 * time.Minute).UTC().Format("2006-01-02T15:04:05Z"),
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

func TestPool_ReclaimHookFires(t *testing.T) {
	// Verifies that when a stale bot is reclaimed, the ReclaimHook is called
	// with the session key before the bot is returned to the caller.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	b, _ := pool.Acquire()
	b.SetSessionKey("agent:main:facet:f-1")

	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": staleTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	var hookedKeys []string
	pool.ReclaimHook = func(sessionKey string) {
		hookedKeys = append(hookedKeys, sessionKey)
	}

	// Acquire triggers reclaim, which should fire the hook first
	b2, ok := pool.Acquire()
	if !ok {
		t.Fatal("should reclaim stale bot")
	}
	if b2 != bot1 {
		t.Fatal("should return the reclaimed bot")
	}
	if len(hookedKeys) != 1 || hookedKeys[0] != "agent:main:facet:f-1" {
		t.Errorf("hook called with %v, want [agent:main:facet:f-1]", hookedKeys)
	}
}

func TestPool_ReclaimHookNil(t *testing.T) {
	// Verifies that TTL reclaim works correctly when ReclaimHook is nil,
	// i.e., the absence of a hook does not cause a panic.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	pool.Add(bot1)

	b, _ := pool.Acquire()
	b.SetSessionKey("agent:main:facet:f-1")

	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": staleTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	// No hook set — should not panic, just reclaim normally
	b2, ok := pool.Acquire()
	if !ok {
		t.Fatal("should reclaim stale bot without hook")
	}
	if b2 != bot1 {
		t.Fatal("should return reclaimed bot")
	}
}

func TestPool_ForEach(t *testing.T) {
	// Verifies that ForEach visits all bots in the pool in insertion order.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	bot3 := testSecondaryBot("bot3")
	pool.Add(bot1)
	pool.Add(bot2)
	pool.Add(bot3)

	var visited []*Bot
	pool.ForEach(func(b *Bot) {
		visited = append(visited, b)
	})

	if len(visited) != 3 {
		t.Fatalf("ForEach visited %d bots, want 3", len(visited))
	}
	if visited[0] != bot1 || visited[1] != bot2 || visited[2] != bot3 {
		t.Error("ForEach did not visit bots in order")
	}
}

func TestPool_ForEachEmpty(t *testing.T) {
	// Verifies that ForEach on an empty pool calls the callback zero times
	// without panicking.
	pool := NewPool()
	count := 0
	pool.ForEach(func(b *Bot) {
		count++
	})
	if count != 0 {
		t.Errorf("ForEach on empty pool visited %d bots", count)
	}
}

func TestPool_ReclaimHookMultipleBots(t *testing.T) {
	// Verifies that when multiple stale bots exist, the ReclaimHook is called
	// once per reclaimed bot during a single Acquire call.
	pool := NewPool()
	bot1 := testSecondaryBot("bot1")
	bot2 := testSecondaryBot("bot2")
	pool.Add(bot1)
	pool.Add(bot2)

	b1, _ := pool.Acquire()
	b1.SetSessionKey("agent:main:facet:f-1")
	b2, _ := pool.Acquire()
	b2.SetSessionKey("agent:main:facet:f-2")

	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	checker := &mockSessionChecker{
		activities: map[string]string{
			"agent:main:facet:f-1": staleTime,
			"agent:main:facet:f-2": staleTime,
		},
	}
	pool.SetSessionTTL(1*time.Hour, checker)

	var hookedKeys []string
	pool.ReclaimHook = func(sessionKey string) {
		hookedKeys = append(hookedKeys, sessionKey)
	}

	// Should reclaim both, hook fires for each
	pool.Acquire()
	if len(hookedKeys) != 2 {
		t.Errorf("hook called %d times, want 2", len(hookedKeys))
	}
}
