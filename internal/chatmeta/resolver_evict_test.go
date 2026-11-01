package chatmeta

import (
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
)

// countingIndex counts SetChatMetadata calls per chat.
type countingIndex struct {
	platform.SessionIndex
	mu     sync.Mutex
	writes map[int64]int
}

func (c *countingIndex) SetChatMetadata(agentID, platformName string, chatID int64, key, value string) error {
	c.mu.Lock()
	c.writes[chatID]++
	c.mu.Unlock()
	return c.SessionIndex.SetChatMetadata(agentID, platformName, chatID, key, value)
}

func (c *countingIndex) count(chatID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes[chatID]
}

// TestRegisterChat_IdleChatDroppedAndReRegistered proves (h): a chat not seen
// for the idle threshold is dropped from the cache and its next contact makes
// exactly one more SetChatMetadata write, while a recently seen chat is not
// dropped (no write) and the map does not keep the idle chat.
func TestRegisterChat_IdleChatDroppedAndReRegistered(t *testing.T) {
	r := testResolver(t)
	idx := &countingIndex{SessionIndex: r.Index, writes: map[int64]int{}}
	r.Index = idx
	r.idleAfter = 30 * time.Millisecond
	r.sweepEvery = 10 * time.Millisecond
	clock := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return clock }

	r.RegisterChat(1) // idle chat
	r.RegisterChat(2) // busy chat
	for i := 0; i < 4; i++ {
		clock = clock.Add(10 * time.Millisecond)
		r.RegisterChat(2) // seen every 10ms: stays fresh
	}
	// Chat 1 has now been idle 40ms > 30ms; the sweep on the last call dropped it.
	r.regMu.Lock()
	_, kept := r.registered[1]
	r.regMu.Unlock()
	if kept {
		t.Fatal("idle chat 1 still cached")
	}
	if got := idx.count(2); got != 1 {
		t.Fatalf("busy chat 2: %d writes, want 1 (never dropped)", got)
	}

	r.RegisterChat(1)
	r.RegisterChat(1)
	if got := idx.count(1); got != 2 {
		t.Fatalf("idle chat 1: %d writes, want 2 (exactly one more after the drop)", got)
	}
}
