package discord

import (
	"testing"
	"time"
)

// testActivityChecker maps session keys to LastActivity strings.
type testActivityChecker map[string]string

func (c testActivityChecker) LastActivity(key string) string {
	if v, ok := c[key]; ok {
		return v
	}
	return "n/a"
}

// testSecondaryBot builds a minimal secondary bot for pool tests.
func testSecondaryBot() *Bot {
	return &Bot{isSecondary: true}
}

// TestPoolAcquireRelease verifies the basic lifecycle: an added bot is
// available, acquiring marks it busy once a session key is set, and releasing
// clears the key and restores availability.
func TestPoolAcquireRelease(t *testing.T) {
	pool := NewPool()
	bot := testSecondaryBot()
	bot.pool = pool
	pool.Add(bot)

	if pool.Size() != 1 || pool.Available() != 1 {
		t.Fatalf("size=%d available=%d, want 1/1", pool.Size(), pool.Available())
	}

	got, ok := pool.Acquire()
	if !ok || got != bot {
		t.Fatal("expected to acquire the added bot")
	}
	got.SetSessionKey("a/c1/100")
	if pool.Available() != 0 {
		t.Fatalf("available=%d, want 0", pool.Available())
	}
	if _, ok := pool.Acquire(); ok {
		t.Fatal("should not acquire when all busy")
	}

	pool.Release(bot)
	if bot.SessionKey() != "" {
		t.Error("expected session key cleared on release")
	}
	if pool.Available() != 1 {
		t.Errorf("available=%d after release, want 1", pool.Available())
	}
}

// TestPoolAcquireLRU verifies the least-recently-used policy: after releasing
// the first-acquired bot, the next acquire returns it again.
func TestPoolAcquireLRU(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot()
	bot2 := testSecondaryBot()
	pool.Add(bot1)
	pool.Add(bot2)

	b1, _ := pool.Acquire()
	b1.SetSessionKey("s1")
	b2, ok := pool.Acquire()
	if !ok || b2 == b1 {
		t.Fatal("expected second distinct bot")
	}
	b2.SetSessionKey("s2")

	pool.Release(b1)
	b3, _ := pool.Acquire()
	if b3 != b1 {
		t.Error("LRU should return the bot released longest ago")
	}
}

// TestPoolAcquireEmpty verifies acquiring from an empty pool fails cleanly.
func TestPoolAcquireEmpty(t *testing.T) {
	pool := NewPool()
	if _, ok := pool.Acquire(); ok {
		t.Fatal("expected acquire to fail on empty pool")
	}
}

// TestPoolTTLReclaim verifies stale sessions (idle beyond the TTL or missing
// entirely) are auto-released on Acquire, firing the ReclaimHook for each, while
// fresh sessions are left alone.
func TestPoolTTLReclaim(t *testing.T) {
	pool := NewPool()
	staleBot := testSecondaryBot()
	missingBot := testSecondaryBot()
	freshBot := testSecondaryBot()
	pool.Add(staleBot)
	pool.Add(missingBot)
	pool.Add(freshBot)

	staleBot.SetSessionKeyDirect("stale-session")
	missingBot.SetSessionKeyDirect("missing-session")
	freshBot.SetSessionKeyDirect("fresh-session")

	checker := testActivityChecker{
		"stale-session": time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		"fresh-session": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		// missing-session: not in map -> "n/a"
	}
	pool.SetSessionTTL(time.Hour, checker)

	var reclaimed []string
	pool.ReclaimHook = func(key string) { reclaimed = append(reclaimed, key) }

	bot, ok := pool.Acquire()
	if !ok {
		t.Fatal("expected an acquire after reclaim")
	}
	if bot == freshBot {
		t.Error("fresh bot should not have been reclaimed/acquired")
	}
	if len(reclaimed) != 2 {
		t.Fatalf("expected 2 reclaim hooks, got %v", reclaimed)
	}
	if staleBot.SessionKey() != "" && staleBot != bot {
		t.Error("stale bot session should be cleared")
	}
	if missingBot.SessionKey() != "" && missingBot != bot {
		t.Error("missing-session bot should be cleared")
	}
	if freshBot.SessionKey() != "fresh-session" {
		t.Error("fresh bot should keep its session")
	}
}

// TestPoolTTLUnparseableTimestampIgnored verifies sessions with unparseable
// activity timestamps are not reclaimed.
func TestPoolTTLUnparseableTimestampIgnored(t *testing.T) {
	pool := NewPool()
	bot := testSecondaryBot()
	pool.Add(bot)
	bot.SetSessionKeyDirect("weird-session")

	pool.SetSessionTTL(time.Hour, testActivityChecker{"weird-session": "not-a-timestamp"})

	if _, ok := pool.Acquire(); ok {
		t.Fatal("expected no acquire: only bot is busy and unparseable timestamps are left alone")
	}
	if bot.SessionKey() != "weird-session" {
		t.Error("bot with unparseable activity should keep its session")
	}
}

// TestPoolForEach verifies ForEach visits every bot exactly once.
func TestPoolForEach(t *testing.T) {
	pool := NewPool()
	bot1 := testSecondaryBot()
	bot2 := testSecondaryBot()
	pool.Add(bot1)
	pool.Add(bot2)

	seen := map[*Bot]int{}
	pool.ForEach(func(b *Bot) { seen[b]++ })
	if len(seen) != 2 || seen[bot1] != 1 || seen[bot2] != 1 {
		t.Errorf("unexpected visit counts %v", seen)
	}
}
