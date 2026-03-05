package telegram

import (
	"strings"
	"testing"

	"foci/internal/command"
)

func TestBotManagerPrimary(t *testing.T) {
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

func TestBotManagerMultiball(t *testing.T) {
	mgr := NewBotManager()

	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Add two multiball bots for clutch
	mb1, _ := testBot(nil, command.NewRegistry())
	mb2, _ := testBot(nil, command.NewRegistry())
	mgr.AddMultiball("clutch", mb1)
	mgr.AddMultiball("clutch", mb2)

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

	// Scout has no multiball
	if got := mgr.Pool("scout"); got != nil {
		t.Errorf("Pool(scout) = %v, want nil", got)
	}
}

func TestBotManagerIsolation(t *testing.T) {
	// Verify that multiball pools are per-agent, not shared
	mgr := NewBotManager()

	// Two agents, each with their own multiball bot
	clutchPrimary, _ := testBot(nil, command.NewRegistry())
	scoutPrimary, _ := testBot(nil, command.NewRegistry())
	clutchMB, _ := testBot(nil, command.NewRegistry())
	clutchMB.SetSessionKey("") // multiball bots start idle
	scoutMB, _ := testBot(nil, command.NewRegistry())
	scoutMB.SetSessionKey("") // multiball bots start idle

	mgr.AddPrimary("clutch", clutchPrimary)
	mgr.AddPrimary("scout", scoutPrimary)
	mgr.AddMultiball("clutch", clutchMB)
	mgr.AddMultiball("scout", scoutMB)

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
	acquired.SetSessionKey("agent:clutch:multiball:mb-1")

	if scoutPool.Available() != 1 {
		t.Errorf("scout pool available = %d after clutch acquire, want 1", scoutPool.Available())
	}
}

func TestBotManagerSharedPool(t *testing.T) {
	mgr := NewBotManager()

	// No shared pool initially
	if mgr.SharedPool() != nil {
		t.Error("SharedPool() should be nil initially")
	}

	// Add shared bots
	shared1, _ := testBot(nil, command.NewRegistry())
	shared2, _ := testBot(nil, command.NewRegistry())
	mgr.AddSharedMultiball(shared1)
	mgr.AddSharedMultiball(shared2)

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

func TestAcquireMultiball_PerAgentOnly(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	// Should acquire from per-agent pool
	bot, ok := mgr.AcquireMultiball("clutch")
	if !ok {
		t.Fatal("AcquireMultiball failed")
	}
	if bot != mb {
		t.Error("expected per-agent bot")
	}
}

func TestAcquireMultiball_SharedFallback(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// No per-agent bots, only shared
	shared := testSecondaryBot("shared1")
	mgr.AddSharedMultiball(shared)

	bot, ok := mgr.AcquireMultiball("clutch")
	if !ok {
		t.Fatal("AcquireMultiball should fall back to shared pool")
	}
	if bot != shared {
		t.Error("expected shared pool bot")
	}
}

func TestAcquireMultiball_PerAgentBusyFallsToShared(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Per-agent bot — acquire and make busy
	perAgent := testSecondaryBot("pa1")
	mgr.AddMultiball("clutch", perAgent)
	bot1, ok := mgr.AcquireMultiball("clutch")
	if !ok {
		t.Fatal("initial acquire failed")
	}
	bot1.SetSessionKey("agent:clutch:multiball:mb-1")

	// Add shared bot
	shared := testSecondaryBot("shared1")
	mgr.AddSharedMultiball(shared)

	// Should fall back to shared since per-agent is busy
	bot2, ok := mgr.AcquireMultiball("clutch")
	if !ok {
		t.Fatal("AcquireMultiball should fall back to shared when per-agent is busy")
	}
	if bot2 != shared {
		t.Error("expected shared pool bot as fallback")
	}
}

func TestAcquireMultiball_BothExhausted(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Per-agent bot — make busy
	perAgent := testSecondaryBot("pa1")
	mgr.AddMultiball("clutch", perAgent)
	bot1, _ := mgr.AcquireMultiball("clutch")
	bot1.SetSessionKey("agent:clutch:multiball:mb-1")

	// Shared bot — make busy
	shared := testSecondaryBot("shared1")
	mgr.AddSharedMultiball(shared)
	bot2, _ := mgr.AcquireMultiball("clutch")
	bot2.SetSessionKey("agent:clutch:multiball:mb-2")

	// Both exhausted
	_, ok := mgr.AcquireMultiball("clutch")
	if ok {
		t.Fatal("AcquireMultiball should fail when both pools are exhausted")
	}
}

func TestAcquireMultiball_NoPools(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// No pools at all
	_, ok := mgr.AcquireMultiball("clutch")
	if ok {
		t.Fatal("AcquireMultiball should fail with no pools")
	}
}

func TestHasMultiball(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// No bots configured
	if mgr.HasMultiball("clutch") {
		t.Error("HasMultiball should be false with no pools")
	}

	// Add per-agent bot
	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)
	if !mgr.HasMultiball("clutch") {
		t.Error("HasMultiball should be true with per-agent bot")
	}

	// Scout has no per-agent, but shared pool exists
	shared := testSecondaryBot("shared1")
	mgr.AddSharedMultiball(shared)
	if !mgr.HasMultiball("scout") {
		t.Error("HasMultiball should be true for any agent when shared pool exists")
	}
}

func TestAcquireMultiball_ReleaseToCorrectPool(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Per-agent and shared bots
	perAgent := testSecondaryBot("pa1")
	mgr.AddMultiball("clutch", perAgent)

	shared := testSecondaryBot("shared1")
	mgr.AddSharedMultiball(shared)

	// Acquire per-agent
	bot1, _ := mgr.AcquireMultiball("clutch")
	bot1.SetSessionKey("agent:clutch:multiball:mb-1")

	// Acquire shared (per-agent still has one available, but let's exhaust per-agent first)
	bot1b, _ := mgr.AcquireMultiball("clutch") // gets shared (per-agent pool was just acquired from)
	_ = bot1b

	// Actually, let's redo this test more carefully
	// Reset: release bot1 first, then acquire both sequentially
	mgr.Pool("clutch").Release(bot1)

	// Acquire per-agent (it's idle again)
	b1, _ := mgr.AcquireMultiball("clutch")
	b1.SetSessionKey("agent:clutch:multiball:mb-1")

	// Per-agent is now busy, next acquire gets shared
	b2, _ := mgr.AcquireMultiball("clutch")
	b2.SetSessionKey("agent:clutch:multiball:mb-2")

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
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	// Acquire and assign a session key
	bot, _ := mgr.Pool("clutch").Acquire()
	bot.SetSessionKey("agent:clutch:multiball:mb-100")

	found := mgr.BotForSession("agent:clutch:multiball:mb-100")
	if found != bot {
		t.Errorf("BotForSession should find per-agent multiball bot")
	}
}

func TestBotForSession_SharedPool(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	shared := testSecondaryBot("shared1")
	mgr.AddSharedMultiball(shared)

	// Acquire and assign a session key
	bot, _ := mgr.SharedPool().Acquire()
	bot.SetSessionKey("agent:clutch:multiball:mb-200")

	found := mgr.BotForSession("agent:clutch:multiball:mb-200")
	if found != bot {
		t.Errorf("BotForSession should find shared multiball bot")
	}
}

func TestBotForSession_NotFound(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	found := mgr.BotForSession("agent:clutch:multiball:mb-nonexistent")
	if found != nil {
		t.Errorf("BotForSession should return nil for unknown session key, got %v", found)
	}
}

func TestBotForSession_EmptyKey(t *testing.T) {
	mgr := NewBotManager()
	found := mgr.BotForSession("")
	if found != nil {
		t.Errorf("BotForSession('') should return nil, got %v", found)
	}
}

// --- OnSessionKeyChange persistence integration ---

func TestMultiball_SessionKeyCallbackIntegration(t *testing.T) {
	// Verify that SetSessionKey fires the callback and the callback
	// can persist/delete from a state store, matching the main.go wiring.
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	// Simulate the callback wiring from main.go
	persisted := map[string]string{} // key → value
	mb.OnSessionKeyChange = func(username, sessionKey string) {
		key := "multiball:" + username
		if sessionKey == "" {
			delete(persisted, key)
		} else {
			persisted[key] = sessionKey
		}
	}

	// Fork: set session key
	mb.SetSessionKey("agent:clutch:multiball:mb-123")
	if v, ok := persisted["multiball:"]; !ok || v != "agent:clutch:multiball:mb-123" {
		t.Errorf("persisted = %v, want multiball: → agent:clutch:multiball:mb-123", persisted)
	}

	// Done: clear session key
	mb.SetSessionKey("")
	if _, ok := persisted["multiball:"]; ok {
		t.Error("persisted should be cleaned up after clear")
	}
}

func TestMultiball_SetSessionKeyDirectSkipsCallback(t *testing.T) {
	mb := testSecondaryBot("mb1")
	called := false
	mb.OnSessionKeyChange = func(username, sessionKey string) {
		called = true
	}

	// Restoration path — should NOT fire callback
	mb.SetSessionKeyDirect("agent:clutch:multiball:mb-456")
	if called {
		t.Error("SetSessionKeyDirect should not fire OnSessionKeyChange")
	}
	if sk := mb.SessionKey(); sk != "agent:clutch:multiball:mb-456" {
		t.Errorf("session key = %q, want agent:clutch:multiball:mb-456", sk)
	}
}

// --- Multiball routing pattern tests (BotForSession + PrimaryBot fallback) ---

func TestMultiballRouting_MultiballSessionUsesMultiballBot(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	sessionKey := "agent:clutch:multiball:mb-100"
	acquired, _ := mgr.Pool("clutch").Acquire()
	acquired.SetSessionKey(sessionKey)

	// Pattern from main.go: check multiball first, fallback to primary
	var bot *Bot
	if strings.Contains(sessionKey, ":multiball:") {
		if mb := mgr.BotForSession(sessionKey); mb != nil {
			bot = mb
		}
	}
	if bot == nil {
		bot = mgr.PrimaryBot("clutch")
	}

	if bot != acquired {
		t.Errorf("routing should find multiball bot for multiball session key")
	}
}

func TestMultiballRouting_MultiballSessionFallsBackToPrimary(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	// No bot assigned to this session key
	sessionKey := "agent:clutch:multiball:mb-unassigned"

	var bot *Bot
	if strings.Contains(sessionKey, ":multiball:") {
		if mb := mgr.BotForSession(sessionKey); mb != nil {
			bot = mb
		}
	}
	if bot == nil {
		bot = mgr.PrimaryBot("clutch")
	}

	if bot != primary {
		t.Errorf("routing should fall back to primary when multiball bot not found")
	}
}

func TestMultiballRouting_NonMultiballSessionUsesPrimary(t *testing.T) {
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)
	acquired, _ := mgr.Pool("clutch").Acquire()
	acquired.SetSessionKey("agent:clutch:multiball:mb-100")

	// Non-multiball session key
	sessionKey := "agent:clutch:main"

	var bot *Bot
	if strings.Contains(sessionKey, ":multiball:") {
		if mb := mgr.BotForSession(sessionKey); mb != nil {
			bot = mb
		}
	}
	if bot == nil {
		bot = mgr.PrimaryBot("clutch")
	}

	if bot != primary {
		t.Errorf("routing should use primary for non-multiball session key")
	}
}

func TestMultiballRouting_NoPrimaryReturnsNil(t *testing.T) {
	mgr := NewBotManager()
	// No primary bot registered

	sessionKey := "agent:clutch:main"

	var bot *Bot
	if strings.Contains(sessionKey, ":multiball:") {
		if mb := mgr.BotForSession(sessionKey); mb != nil {
			bot = mb
		}
	}
	if bot == nil {
		bot = mgr.PrimaryBot("clutch")
	}

	if bot != nil {
		t.Errorf("routing should return nil when no primary bot exists")
	}
}

func TestMultiballRouting_MultiballNoPrimaryReturnsNil(t *testing.T) {
	mgr := NewBotManager()
	// No primary, but multiball exists (edge case)
	mb := testSecondaryBot("mb1")
	mgr.AddMultiball("clutch", mb)

	sessionKey := "agent:clutch:multiball:mb-unassigned"

	var bot *Bot
	if strings.Contains(sessionKey, ":multiball:") {
		if found := mgr.BotForSession(sessionKey); found != nil {
			bot = found
		}
	}
	if bot == nil {
		bot = mgr.PrimaryBot("clutch")
	}

	if bot != nil {
		t.Errorf("routing should return nil when multiball not found and no primary exists")
	}
}
