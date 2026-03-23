package telegram

import (
	"testing"

	"foci/internal/command"
)

func TestBotManagerPrimary(t *testing.T) {
	// Verifies that AddPrimary registers bots by agent ID and that PrimaryBot
	// returns the correct bot or nil for unknown agents. Also confirms AgentIDs
	// returns all registered agent names.
	mgr := NewBotManager()

	bot1, _ := testBot(nil, command.NewRegistry())
	bot2, _ := testBot(nil, command.NewRegistry())

	mgr.AddPrimary("clutch", bot1)
	mgr.AddPrimary("scout", bot2)

	if got := mgr.PrimaryBot("clutch"); got != bot1 {
		t.Errorf("PrimaryBot(clutch) = %v, want bot1", got)
	}
	if got := mgr.PrimaryBot("scout"); got != bot2 {
		t.Errorf("PrimaryBot(scout) = %v, want bot2", got)
	}
	if got := mgr.PrimaryBot("unknown"); got != nil {
		t.Errorf("PrimaryBot(unknown) = %v, want nil", got)
	}

	ids := mgr.AgentIDs()
	if len(ids) != 2 {
		t.Fatalf("AgentIDs len = %d, want 2", len(ids))
	}
}

func TestBotManagerFacet(t *testing.T) {
	// Verifies that AddFacet adds bots to the per-agent pool, marks them as
	// secondary, and that Pool() returns the correct pool with the right size.
	mgr := NewBotManager()

	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Add two facet bots for clutch
	mb1, _ := testBot(nil, command.NewRegistry())
	mb2, _ := testBot(nil, command.NewRegistry())
	mgr.AddFacet("clutch", mb1)
	mgr.AddFacet("clutch", mb2)

	pool := mgr.Pool("clutch")
	if pool == nil {
		t.Fatal("Pool(clutch) = nil, want pool")
	}
	if pool.Size() != 2 {
		t.Errorf("pool size = %d, want 2", pool.Size())
	}

	// mb1 and mb2 should be marked as secondary
	if !mb1.isSecondary {
		t.Error("mb1 not marked secondary")
	}
	if !mb2.isSecondary {
		t.Error("mb2 not marked secondary")
	}

	// Scout has no facet
	if got := mgr.Pool("scout"); got != nil {
		t.Errorf("Pool(scout) = %v, want nil", got)
	}
}

func TestBotManagerIsolation(t *testing.T) {
	// Verifies that facet pools are per-agent and completely isolated:
	// acquiring from one agent's pool does not affect another agent's pool.
	mgr := NewBotManager()

	// Two agents, each with their own facet bot
	clutchPrimary, _ := testBot(nil, command.NewRegistry())
	scoutPrimary, _ := testBot(nil, command.NewRegistry())
	clutchMB, _ := testBot(nil, command.NewRegistry())
	clutchMB.SetSessionKey("") // facet bots start idle
	scoutMB, _ := testBot(nil, command.NewRegistry())
	scoutMB.SetSessionKey("") // facet bots start idle

	mgr.AddPrimary("clutch", clutchPrimary)
	mgr.AddPrimary("scout", scoutPrimary)
	mgr.AddFacet("clutch", clutchMB)
	mgr.AddFacet("scout", scoutMB)

	// Each agent has its own pool
	clutchPool := mgr.Pool("clutch")
	scoutPool := mgr.Pool("scout")

	if clutchPool == scoutPool {
		t.Error("clutch and scout share the same pool")
	}
	if clutchPool.Size() != 1 {
		t.Errorf("clutch pool size = %d, want 1", clutchPool.Size())
	}
	if scoutPool.Size() != 1 {
		t.Errorf("scout pool size = %d, want 1", scoutPool.Size())
	}

	// Acquiring from clutch's pool should not affect scout's
	acquired, ok := clutchPool.Acquire()
	if !ok {
		t.Fatal("failed to acquire from clutch pool")
	}
	acquired.SetSessionKey("clutch/c1/1/b1")

	if scoutPool.Available() != 1 {
		t.Errorf("scout pool available = %d after clutch acquire, want 1", scoutPool.Available())
	}
}

func TestBotManagerSharedPool(t *testing.T) {
	// Verifies that AddSharedFacet populates the shared pool, marks bots
	// as secondary, and that SharedPool() returns the pool with the correct size.
	mgr := NewBotManager()

	// No shared pool initially
	if mgr.SharedPool() != nil {
		t.Error("SharedPool() should be nil initially")
	}

	// Add shared bots
	shared1, _ := testBot(nil, command.NewRegistry())
	shared2, _ := testBot(nil, command.NewRegistry())
	mgr.AddSharedFacet(shared1)
	mgr.AddSharedFacet(shared2)

	pool := mgr.SharedPool()
	if pool == nil {
		t.Fatal("SharedPool() = nil, want pool")
	}
	if pool.Size() != 2 {
		t.Errorf("shared pool size = %d, want 2", pool.Size())
	}
	if !shared1.isSecondary {
		t.Error("shared1 not marked secondary")
	}
	if !shared2.isSecondary {
		t.Error("shared2 not marked secondary")
	}
}

func TestAcquireFacet_PerAgentOnly(t *testing.T) {
	// Verifies that AcquireFacet returns a bot from the per-agent pool when
	// one is available, without touching the shared pool.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	// Should acquire from per-agent pool
	bot, ok := mgr.AcquireFacet("clutch")
	if !ok {
		t.Fatal("AcquireFacet failed")
	}
	if bot != fb {
		t.Error("expected per-agent bot")
	}
}

func TestAcquireFacet_SharedFallback(t *testing.T) {
	// Verifies that AcquireFacet falls back to the shared pool when no
	// per-agent bots are configured for the requested agent.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// No per-agent bots, only shared
	shared := testSecondaryBot("shared1")
	mgr.AddSharedFacet(shared)

	bot, ok := mgr.AcquireFacet("clutch")
	if !ok {
		t.Fatal("AcquireFacet should fall back to shared pool")
	}
	if bot != shared {
		t.Error("expected shared pool bot")
	}
}

func TestAcquireFacet_PerAgentBusyFallsToShared(t *testing.T) {
	// Verifies that when all per-agent bots are busy, AcquireFacet falls back
	// to the shared pool rather than failing immediately.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Per-agent bot — acquire and make busy
	perAgent := testSecondaryBot("pa1")
	mgr.AddFacet("clutch", perAgent)
	bot1, ok := mgr.AcquireFacet("clutch")
	if !ok {
		t.Fatal("initial acquire failed")
	}
	bot1.SetSessionKey("clutch/c1/1/b1")

	// Add shared bot
	shared := testSecondaryBot("shared1")
	mgr.AddSharedFacet(shared)

	// Should fall back to shared since per-agent is busy
	bot2, ok := mgr.AcquireFacet("clutch")
	if !ok {
		t.Fatal("AcquireFacet should fall back to shared when per-agent is busy")
	}
	if bot2 != shared {
		t.Error("expected shared pool bot as fallback")
	}
}

func TestAcquireFacet_BothExhausted(t *testing.T) {
	// Verifies that AcquireFacet returns false when both the per-agent pool
	// and shared pool are fully occupied.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Per-agent bot — make busy
	perAgent := testSecondaryBot("pa1")
	mgr.AddFacet("clutch", perAgent)
	bot1, _ := mgr.AcquireFacet("clutch")
	bot1.SetSessionKey("clutch/c1/1/b1")

	// Shared bot — make busy
	shared := testSecondaryBot("shared1")
	mgr.AddSharedFacet(shared)
	bot2, _ := mgr.AcquireFacet("clutch")
	bot2.SetSessionKey("clutch/c1/1/b2")

	// Both exhausted
	_, ok := mgr.AcquireFacet("clutch")
	if ok {
		t.Fatal("AcquireFacet should fail when both pools are exhausted")
	}
}

func TestAcquireFacet_NoPools(t *testing.T) {
	// Verifies that AcquireFacet returns false when no pools are configured
	// at all for the agent.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// No pools at all
	_, ok := mgr.AcquireFacet("clutch")
	if ok {
		t.Fatal("AcquireFacet should fail with no pools")
	}
}

func TestHasFacet(t *testing.T) {
	// Verifies that HasFacet correctly reports whether an agent has any
	// available facet bots, including when only the shared pool is present.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// No bots configured
	if mgr.HasFacet("clutch") {
		t.Error("HasFacet should be false with no pools")
	}

	// Add per-agent bot
	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)
	if !mgr.HasFacet("clutch") {
		t.Error("HasFacet should be true with per-agent bot")
	}

	// Scout has no per-agent, but shared pool exists
	shared := testSecondaryBot("shared1")
	mgr.AddSharedFacet(shared)
	if !mgr.HasFacet("scout") {
		t.Error("HasFacet should be true for any agent when shared pool exists")
	}
}

func TestAcquireFacet_ReleaseToCorrectPool(t *testing.T) {
	// Verifies that bots released via their respective pool objects (per-agent and
	// shared) return to the correct pool and become available there, not in the other.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Per-agent and shared bots
	perAgent := testSecondaryBot("pa1")
	mgr.AddFacet("clutch", perAgent)

	shared := testSecondaryBot("shared1")
	mgr.AddSharedFacet(shared)

	// Acquire per-agent
	bot1, _ := mgr.AcquireFacet("clutch")
	bot1.SetSessionKey("clutch/c1/1/b1")

	// Acquire shared (per-agent still has one available, but let's exhaust per-agent first)
	bot1b, _ := mgr.AcquireFacet("clutch") // gets shared (per-agent pool was just acquired from)
	_ = bot1b

	// Actually, let's redo this test more carefully
	// Reset: release bot1 first, then acquire both sequentially
	mgr.Pool("clutch").Release(bot1)

	// Acquire per-agent (it's idle again)
	b1, _ := mgr.AcquireFacet("clutch")
	b1.SetSessionKey("clutch/c1/1/b1")

	// Per-agent is now busy, next acquire gets shared
	b2, _ := mgr.AcquireFacet("clutch")
	b2.SetSessionKey("clutch/c1/1/b2")

	// Release per-agent bot — should return to per-agent pool
	mgr.Pool("clutch").Release(b1)
	if mgr.Pool("clutch").Available() != 1 {
		t.Errorf("per-agent pool available = %d, want 1", mgr.Pool("clutch").Available())
	}
	if mgr.SharedPool().Available() != 0 {
		t.Errorf("shared pool available = %d, want 0", mgr.SharedPool().Available())
	}

	// Release shared bot — should return to shared pool
	mgr.SharedPool().Release(b2)
	if mgr.SharedPool().Available() != 1 {
		t.Errorf("shared pool available = %d, want 1", mgr.SharedPool().Available())
	}
}

// --- BotForSession tests ---

func TestBotForSession_PerAgentPool(t *testing.T) {
	// Verifies that BotForSession finds a secondary bot in the per-agent pool
	// that currently holds the matching session key.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	// Acquire and assign a session key
	bot, _ := mgr.Pool("clutch").Acquire()
	bot.SetSessionKey("clutch/c1/1/b100")

	found := mgr.BotForSession("clutch/c1/1/b100")
	if found != bot {
		t.Errorf("BotForSession should find per-agent facet bot")
	}
}

func TestBotForSession_SharedPool(t *testing.T) {
	// Verifies that BotForSession finds a secondary bot in the shared pool
	// when it holds the matching session key.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	shared := testSecondaryBot("shared1")
	mgr.AddSharedFacet(shared)

	// Acquire and assign a session key
	bot, _ := mgr.SharedPool().Acquire()
	bot.SetSessionKey("clutch/c1/1/b200")

	found := mgr.BotForSession("clutch/c1/1/b200")
	if found != bot {
		t.Errorf("BotForSession should find shared facet bot")
	}
}

func TestBotForSession_NotFound(t *testing.T) {
	// Verifies that BotForSession returns nil when no secondary bot holds
	// the requested session key.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	found := mgr.BotForSession("clutch/c1/1/b998")
	if found != nil {
		t.Errorf("BotForSession should return nil for unknown session key, got %v", found)
	}
}

func TestBotForSession_EmptyKey(t *testing.T) {
	// Verifies that BotForSession returns nil safely when called with an empty
	// session key, without panicking.
	mgr := NewBotManager()
	found := mgr.BotForSession("")
	if found != nil {
		t.Errorf("BotForSession('') should return nil, got %v", found)
	}
}

func TestBotForSession_NoFacetMatchReturnsNil(t *testing.T) {
	// Verifies that BotForSession returns nil for a regular (non-facet) session key
	// even when a primary bot is registered. BotForSession must only match facet pool
	// entries, never fall back to the primary — that's BotForSessionOrPrimary's job.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	// New-format key that parses successfully — before the fix this would
	// have returned the primary bot via the agentID fallback.
	found := mgr.BotForSession("clutch/c12345/1709590000")
	if found != nil {
		t.Errorf("BotForSession should return nil when no facet matches, got %v", found)
	}
}

// --- OnSessionKeyChange persistence integration ---

func TestFacet_SessionKeyCallbackIntegration(t *testing.T) {
	// Verifies that SetSessionKey fires the OnSessionKeyChange callback and
	// that the callback can persist and clean up session assignments, mirroring
	// the real wiring in main.go where state is persisted to a store.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	// Simulate the callback wiring from main.go
	persisted := map[string]string{} // key → value
	fb.OnSessionKeyChange = func(username, sessionKey string) {
		key := "facet:" + username
		if sessionKey == "" {
			delete(persisted, key)
		} else {
			persisted[key] = sessionKey
		}
	}

	// Fork: set session key
	fb.SetSessionKey("clutch/c1/1/b123")
	if v, ok := persisted["facet:"]; !ok || v != "clutch/c1/1/b123" {
		t.Errorf("persisted = %v, want facet: → agent:clutch:facet:f-123", persisted)
	}

	// Done: clear session key
	fb.SetSessionKey("")
	if _, ok := persisted["facet:"]; ok {
		t.Error("persisted should be cleaned up after clear")
	}
}

func TestFacet_SetSessionKeyDirectSkipsCallback(t *testing.T) {
	// Verifies that SetSessionKeyDirect sets the session key without firing
	// OnSessionKeyChange, which is used during state restoration at startup.
	fb := testSecondaryBot("mb1")
	called := false
	fb.OnSessionKeyChange = func(username, sessionKey string) {
		called = true
	}

	// Restoration path — should NOT fire callback
	fb.SetSessionKeyDirect("clutch/c1/1/b456")
	if called {
		t.Error("SetSessionKeyDirect should not fire OnSessionKeyChange")
	}
	if sk := fb.SessionKey(); sk != "clutch/c1/1/b456" {
		t.Errorf("session key = %q, want agent:clutch:facet:f-456", sk)
	}
}

// --- BotForSessionOrPrimary routing tests ---

func TestBotForSessionOrPrimary_FacetSessionUsesFacetBot(t *testing.T) {
	// Verifies that
	// BotForSessionOrPrimary returns the facet bot when it holds the session key.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	sessionKey := "clutch/c1/1/b100"
	acquired, _ := mgr.Pool("clutch").Acquire()
	acquired.SetSessionKey(sessionKey)

	bot := mgr.BotForSessionOrPrimary(sessionKey, "clutch")
	if bot != acquired {
		t.Errorf("BotForSessionOrPrimary should find facet bot for its session key")
	}
}

func TestBotForSessionOrPrimary_UnassignedSessionFallsBackToPrimary(t *testing.T) {
	// Verifies
	// fallback to primary when no secondary bot holds the session key.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	bot := mgr.BotForSessionOrPrimary("clutch/c1/1/b997", "clutch")
	if bot != primary {
		t.Errorf("BotForSessionOrPrimary should fall back to primary when facet bot not found")
	}
}

func TestBotForSessionOrPrimary_NonFacetSessionUsesPrimary(t *testing.T) {
	// Verifies that
	// a regular (non-facet) session key routes to the primary bot.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)
	acquired, _ := mgr.Pool("clutch").Acquire()
	acquired.SetSessionKey("clutch/c1/1/b100")

	bot := mgr.BotForSessionOrPrimary("agent:clutch:main", "clutch")
	if bot != primary {
		t.Errorf("BotForSessionOrPrimary should use primary for non-facet session key")
	}
}

func TestBotForSessionOrPrimary_NoPrimaryReturnsNil(t *testing.T) {
	// Verifies nil when no
	// primary bot is registered and no secondary matches.
	mgr := NewBotManager()

	bot := mgr.BotForSessionOrPrimary("agent:clutch:main", "clutch")
	if bot != nil {
		t.Errorf("BotForSessionOrPrimary should return nil when no primary bot exists")
	}
}

func TestBotForSessionOrPrimary_FacetNoPrimaryReturnsNil(t *testing.T) {
	// Verifies nil when
	// facet exists but no bot holds the key and no primary is registered.
	mgr := NewBotManager()
	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	bot := mgr.BotForSessionOrPrimary("clutch/c1/1/b997", "clutch")
	if bot != nil {
		t.Errorf("BotForSessionOrPrimary should return nil when facet not found and no primary exists")
	}
}

func TestBotForSessionOrPrimary_NewFormatBranchKey(t *testing.T) {
	// Verifies that new slash-separated
	// branch keys (which don't contain ":facet:") still find the secondary bot
	// when it holds that session key. This was broken when BotForSessionOrPrimary
	// gated the lookup on strings.Contains(":facet:").
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	// New-format branch key assigned to facet bot
	sessionKey := "clutch/c12345/1709590000/b1709596800"
	acquired, _ := mgr.Pool("clutch").Acquire()
	acquired.SetSessionKey(sessionKey)

	bot := mgr.BotForSessionOrPrimary(sessionKey, "clutch")
	if bot != acquired {
		t.Errorf("BotForSessionOrPrimary should find facet bot for new-format branch key")
	}
}

func TestBotForSessionOrPrimary_NewFormatChatKey(t *testing.T) {
	// Verifies that new slash-separated
	// chat keys route to primary when no secondary holds the key.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	fb := testSecondaryBot("mb1")
	mgr.AddFacet("clutch", fb)

	bot := mgr.BotForSessionOrPrimary("clutch/c12345/1709590000", "clutch")
	if bot != primary {
		t.Errorf("BotForSessionOrPrimary should fall back to primary for unmatched new-format key")
	}
}
