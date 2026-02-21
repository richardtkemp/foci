package telegram

import (
	"testing"

	"clod/command"
)

func testSecondaryBot(name string) *Bot {
	allowed := map[string]bool{"111": true}
	return &Bot{
		sender:       &mockSender{},
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
