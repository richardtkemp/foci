# Multi-Platform Architecture Migration Plan

## Current State Analysis

**Dependency flow:**
```
main → telegram.NewBot()
main → agent.New()
telegram.Bot holds *agent.Agent (concrete type)
```

**Why main creates platforms:** To avoid circular dependency. Telegram imports agent (needs concrete type), so agent can't import telegram.

**The insight:** `platform.MessageHandler` interface already exists. If telegram.Bot used it instead of `*agent.Agent`, the circular dependency is broken.

---

## Stage 1: MessageHandler Interface Completion

**Goal:** Ensure `MessageHandler` has everything platforms need.

**Analysis needed:**
- What does telegram.Bot call on `agent.Agent`?
- Are all those methods in `MessageHandler`?
- What about callbacks like `OnTurnComplete`, `OnUserMessage`?

**Changes:**
- Add missing methods to `MessageHandler` interface in `internal/platform/types.go`
- Add callback registration methods if needed
- Agent already implements most of it - verify completeness

**Current MessageHandler interface:**
```go
type MessageHandler interface {
    HandleMessage(ctx context.Context, sessionKey, userID, text string, attachments []Attachment, callbacks TurnCallbacks) (string, error)
    IsProcessing() bool
    TransformMessage(text string) string
}
```

**Methods telegram.Bot calls on agent:**
- `TransformMessage(text)` ✓ already in interface
- `HandleMessage(ctx, sessionKey, text)` - slightly different signature (no userID, attachments separate)
- `HandleMessageWithAttachments(ctx, sessionKey, text, images)` - not in interface
- `agent.Warnings` - not in interface (for warning queue)

**Changes to interface:**
```go
type MessageHandler interface {
    HandleMessage(ctx context.Context, sessionKey, text string, attachments []Attachment, callbacks TurnCallbacks) (string, error)
    IsProcessing() bool
    TransformMessage(text string) string
    Warnings() *warnings.Queue  // add this
}
```

**Relationship:**
```
platform.MessageHandler (interface)
    ↑
agent.Agent implements
```

**Effort:** 0.5 day

---

## Stage 2: Telegram.Bot Uses MessageHandler

**Goal:** Telegram.Bot holds `MessageHandler`, not `*agent.Agent`.

**Changes:**

**File: `internal/telegram/bot.go`**
```go
// Before
type Bot struct {
    agent *agent.Agent
    // ...
}

// After
type Bot struct {
    handler platform.MessageHandler
    // ...
}
```

**File: `internal/telegram/bot.go` - Constructor**
```go
// Before
func NewBot(token string, allowedUsers []string, ag *agent.Agent, ...) (*Bot, error)

// After
func NewBot(token string, allowedUsers []string, handler platform.MessageHandler, ...) (*Bot, error)
```

**Update all Bot methods:**
- `b.agent.HandleMessage(...)` → `b.handler.HandleMessage(...)`
- `b.agent.TransformMessage(...)` → `b.handler.TransformMessage(...)`
- `b.agent.Warnings` → `b.handler.Warnings()`

**Dependency graph after:**
```
agent → telegram (can now import!)
telegram → platform (uses interface)
```

**Circular dependency broken.**

**Effort:** 0.5 day

---

## Stage 3: Agent Creates Platforms

**Goal:** Agent constructor creates its platforms based on config.

**Changes:**

**File: `internal/agent/agent.go`**
```go
type Agent struct {
    // Existing fields
    Sessions     *session.Store
    Tools        *tools.Registry
    // ...
    
    // NEW: Platform management
    platforms    map[string]platform.Sender  // "telegram" → bot instance
    platformMu   sync.RWMutex
}
```

**File: `internal/agent/platforms.go` (new file)**
```go
package agent

import (
    "context"
    "foci/internal/platform"
    "foci/internal/telegram"
    // future: "foci/internal/discord"
)

type PlatformConfig struct {
    Type   string
    Config interface{} // platform-specific config
}

// AddPlatform adds a platform to the agent
func (a *Agent) AddPlatform(platformType string, p platform.Sender) {
    a.platformMu.Lock()
    defer a.platformMu.Unlock()
    a.platforms[platformType] = p
}

// GetPlatform returns the platform for a session key
func (a *Agent) GetPlatform(sessionKey string) platform.Sender {
    platformType := extractPlatformType(sessionKey)
    a.platformMu.RLock()
    defer a.platformMu.RUnlock()
    return a.platforms[platformType]
}

// StartPlatforms starts all platforms
func (a *Agent) StartPlatforms(ctx context.Context) error {
    a.platformMu.RLock()
    defer a.platformMu.RUnlock()
    
    for _, p := range a.platforms {
        if starter, ok := p.(interface{ Start(context.Context) error }); ok {
            if err := starter.Start(ctx); err != nil {
                return err
            }
        }
    }
    return nil
}

// StopPlatforms stops all platforms
func (a *Agent) StopPlatforms() error {
    a.platformMu.RLock()
    defer a.platformMu.RUnlock()
    
    var errs []error
    for _, p := range a.platforms {
        if stopper, ok := p.(interface{ Stop() error }); ok {
            if err := stopper.Stop(); err != nil {
                errs = append(errs, err)
            }
        }
    }
    if len(errs) > 0 {
        return fmt.Errorf("platform stop errors: %v", errs)
    }
    return nil
}

func extractPlatformType(sessionKey string) string {
    // session key format: agentID:platform:identifier
    // e.g., "mybot:telegram:123456"
    parts := strings.Split(sessionKey, ":")
    if len(parts) >= 2 {
        return parts[1]
    }
    return "telegram" // default for backward compat
}
```

**File: `internal/agent/agent.go` - Constructor changes**
```go
func New(deps Deps) *Agent {
    a := &Agent{
        Sessions: deps.Sessions,
        Tools:    deps.Tools,
        // ...
        platforms: make(map[string]platform.Sender),
    }
    
    // Create platforms from config
    for _, platCfg := range deps.PlatformConfigs {
        switch platCfg.Type {
        case "telegram":
            tgCfg := platCfg.Config.(*TelegramPlatformConfig)
            bot, err := telegram.NewBot(
                tgCfg.BotToken,
                tgCfg.AllowedUsers,
                a,  // agent IS platform.MessageHandler
                deps.CommandRegistry,
                deps.LastMessageStore,
                deps.AgentID,
            )
            if err != nil {
                // handle error
            }
            a.AddPlatform("telegram", bot)
        // future: case "discord": ...
        }
    }
    
    return a
}
```

**Relationship:**
```
agent.New(platformConfigs, sessions, tools, ...)
    ↓ creates
telegram.NewBot(token, agent, ...)  // agent IS MessageHandler
discord.NewBot(token, agent, ...)
    ↓ stores
agent.platforms = {"telegram": bot1, "discord": bot2}
```

**Effort:** 1 day

---

## Stage 4: Platform-Aware Session Keys

**Goal:** Session keys encode platform type.

**Current format:** `agentID:chat:CHATID`
**Proposed format:** `agentID:PLATFORM:IDENTIFIER`

**Examples:**
- `mybot:telegram:123456` (was: `mybot:chat:123456`)
- `mybot:discord:789012`
- `mybot:matrix:@alice:matrix.org`

**Changes:**

**File: `internal/session/keys.go` (new file)**
```go
package session

import "fmt"

// NewPlatformSessionKey creates a session key for a platform user
func NewPlatformSessionKey(agentID, platformType, platformUserID string) string {
    return fmt.Sprintf("%s:%s:%s", agentID, platformType, platformUserID)
}

// ParseSessionKey extracts components from a session key
func ParseSessionKey(key string) (agentID, platformType, platformUserID string) {
    parts := strings.SplitN(key, ":", 3)
    if len(parts) != 3 {
        // Backward compat: old format "agent:chat:CHATID"
        if len(parts) == 3 && parts[1] == "chat" {
            return parts[0], "telegram", parts[2]
        }
        return "", "", ""
    }
    return parts[0], parts[1], parts[2]
}

// ChatIDFromKey extracts the chat ID (backward compat)
func ChatIDFromKey(key string) int64 {
    _, _, userID := ParseSessionKey(key)
    id, _ := strconv.ParseInt(userID, 10, 64)
    return id
}
```

**File: `internal/telegram/bot.go`**
```go
// Update session key creation
func (b *Bot) sessionKeyForChat(chatID int64) string {
    return session.NewPlatformSessionKey(b.agentID, "telegram", strconv.FormatInt(chatID, 10))
}
```

**Backward compatibility:**
- Old session keys like `mybot:chat:123456` are parsed as `mybot:telegram:123456`
- New session keys use explicit platform type
- Migration happens transparently in `ParseSessionKey`

**Effort:** 0.5 day

---

## Stage 5: Config Structure

**Goal:** Config lists platforms per agent.

**Current config:**
```toml
[[agents]]
id = "mybot"
telegram_bot = "123:ABC"
```

**Proposed config:**
```toml
[[agents]]
id = "mybot"

[agents.platforms.telegram]
bot_token = "123:ABC"
allowed_users = ["123456"]

[agents.platforms.discord]
bot_token = "xyz"
allowed_guilds = ["789"]
```

**Changes:**

**File: `internal/config/config.go`**
```go
type AgentConfig struct {
    ID       string `toml:"id"`
    Model    string `toml:"model"`
    // ... existing fields
    
    // NEW: Platform configurations
    Platforms map[string]PlatformConfig `toml:"platforms"`
    
    // DEPRECATED but kept for backward compat
    TelegramBot string `toml:"telegram_bot"`
}

type PlatformConfig struct {
    // Telegram
    BotToken     string   `toml:"bot_token"`
    AllowedUsers []string `toml:"allowed_users"`
    
    // Discord (future)
    // BotToken     string   `toml:"bot_token"`
    // AllowedGuilds []string `toml:"allowed_guilds"`
    
    // Matrix (future)
    // HomeServer string `toml:"home_server"`
    // Username   string `toml:"username"`
    // Password   string `toml:"password"`
}
```

**File: `internal/config/migrate.go` (new file)**
```go
package config

// MigrateAgentConfig converts old config format to new format
func MigrateAgentConfig(acfg *AgentConfig) {
    // If telegram_bot is set but platforms.telegram isn't, migrate
    if acfg.TelegramBot != "" && acfg.Platforms["telegram"] == (PlatformConfig{}) {
        if acfg.Platforms == nil {
            acfg.Platforms = make(map[string]PlatformConfig)
        }
        acfg.Platforms["telegram"] = PlatformConfig{
            BotToken: acfg.TelegramBot,
        }
    }
}
```

**Call migration in config loading:**
```go
func Load(path string) (*Config, error) {
    // ... existing load logic
    
    // Migrate old config format
    for i := range cfg.Agents {
        MigrateAgentConfig(&cfg.Agents[i])
    }
    
    return cfg, nil
}
```

**Effort:** 0.5 day

---

## Stage 6: Main.go Simplification

**Goal:** Main doesn't create platforms.

**Current flow:**
```
main creates shared resources (sessions, memory, tools)
main creates telegram.Bot
main creates agent with bot reference
main calls bot.Start()
```

**Proposed flow:**
```
main creates shared resources (sessions, memory, tools)
main creates agent with config (agent creates its own platforms)
main calls agent.StartPlatforms()
```

**Changes:**

**File: `cmd/foci-gw/main.go`**

**Remove:**
- `import "foci/internal/telegram"`
- All `telegram.NewBot()` calls
- `botMgr := telegram.NewBotManager()`
- `botMgr.AddPrimary()`, `botMgr.StartAll()`

**Replace with:**
```go
func main() {
    // ... load config ...
    
    // Create shared resources
    sessions := initSessions(cfg)
    mem := initMemorySystem(cfg)
    // ...
    
    // Create agents (agents create their own platforms)
    agents := make(map[string]*agent.Agent)
    for _, acfg := range cfg.Agents {
        ag := agent.New(agent.Deps{
            AgentConfig:     acfg,
            Sessions:        sessions,
            Tools:           toolsRegistry,
            // ... other deps
            
            // NEW: Platform configs
            PlatformConfigs: buildPlatformConfigs(acfg),
        })
        agents[acfg.ID] = ag
    }
    
    // Start all platforms
    for _, ag := range agents {
        if err := ag.StartPlatforms(ctx); err != nil {
            log.Fatalf("start platforms: %v", err)
        }
    }
    
    // ... wait for shutdown ...
    
    // Stop all platforms
    for _, ag := range agents {
        _ = ag.StopPlatforms()
    }
}

func buildPlatformConfigs(acfg config.AgentConfig) []agent.PlatformConfig {
    var configs []agent.PlatformConfig
    for platformType, platCfg := range acfg.Platforms {
        configs = append(configs, agent.PlatformConfig{
            Type:   platformType,
            Config: platCfg,
        })
    }
    return configs
}
```

**Main's responsibilities after:**
- Load config
- Create shared resources (sessions, memory, voice, tools)
- Create agents with config
- Start agents (which start their platforms)
- Wait for shutdown
- Stop agents (which stop their platforms)

**Effort:** 1 day

---

## Stage 7: Tool Platform Integration

**Goal:** Tools like `send_telegram` work with any platform.

**Current:** Tool receives `TelegramSender` via `getSender(sessionKey)` callback.

**Changes:**

**File: `internal/tools/telegram.go` → rename to `internal/tools/messaging.go`**
```go
// Tool name: send_telegram → send_message (or keep both for compat)

func NewSendMessageTool(getSender func(sessionKey string) platform.Sender, tts voice.TTS) *Tool {
    return &Tool{
        Name: "send_message",  // or keep "send_telegram" for compat
        Description: "Send a proactive message to the user via their messaging platform.",
        Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
            // ... parse params ...
            
            sessionKey := SessionKeyFromContext(ctx)
            sender := getSender(sessionKey)
            if sender == nil {
                return ToolResult{}, fmt.Errorf("no platform configured for session")
            }
            
            // Extract platform user ID from session key
            _, _, platformUserID := session.ParseSessionKey(sessionKey)
            
            // Send via platform interface
            if err := sender.SendText(platformUserID, text); err != nil {
                return ToolResult{}, err
            }
            
            return ToolResult{Text: "Message sent"}, nil
        },
    }
}
```

**File: `cmd/foci-gw/agents.go`**
```go
// Wire tool with agent's GetPlatform method
toolsRegistry.Register(tools.NewSendMessageTool(
    func(sessionKey string) platform.Sender {
        return ag.GetPlatform(sessionKey)
    },
    ttsProvider,
))
```

**Flow:**
```
Tool receives session key: "mybot:telegram:123456"
    ↓
agent.GetPlatform(sessionKey)
    ↓ extracts "telegram"
    ↓ looks up platforms["telegram"]
    ↓
*telegram.Bot (implements platform.Sender)
    ↓
bot.SendText(chatID, message)
```

**Effort:** 0.5 day

---

## Stage 8: Multiball Platform Support

**Goal:** Multiball works across platforms.

**Current:** Multiball is Telegram-specific (creates secondary `telegram.Bot` instances for parallel conversations).

**Analysis:**
- Multiball is conceptually platform-specific (Telegram group chats, Discord threads)
- Not all platforms need this feature
- Keep as Telegram-only for now, generalize later if needed

**Changes:**

**File: `internal/telegram/multiball.go`**
- No changes needed
- Multiball stays Telegram-specific
- Agent checks platform type before invoking multiball

**File: `internal/command/builtins.go`**
```go
func NewMultiballCommand(forkFn func(ctx context.Context) (string, error)) *Command {
    // forkFn implementation checks if current platform supports multiball
    // If not, returns error: "multiball not supported on this platform"
}
```

**File: `internal/agent/agent.go`**
```go
// Multiball support - Telegram only for now
func (a *Agent) SupportsMultiball(sessionKey string) bool {
    platformType := extractPlatformType(sessionKey)
    return platformType == "telegram"
}
```

**Future generalization (if needed):**
```go
type MultiballPlatform interface {
    platform.Sender
    Fork(sessionKey string) (platform.Sender, error)
}

// Agent checks if platform implements MultiballPlatform
```

**Effort:** 0 days (no implementation needed now)

---

## Stage 9: Callbacks and Observers

**Goal:** Platform → Agent callbacks work through interface.

**Current callbacks telegram.Bot needs:**
- `OnTurnComplete()` - fired after agent turn completes (for cache warming tracking)
- `OnUserMessage()` - fired on inbound user message (for keepalive interaction tracking)
- `SessionKey()` - get current session key (for context)

**Changes:**

**File: `internal/platform/types.go`**
```go
type AgentCallbacks interface {
    OnTurnComplete()
    OnUserMessage(sessionKey string)
}

// Updated MessageHandler interface
type MessageHandler interface {
    HandleMessage(ctx context.Context, sessionKey, text string, attachments []Attachment, callbacks TurnCallbacks) (string, error)
    IsProcessing() bool
    TransformMessage(text string) string
    Warnings() *warnings.Queue
    
    // NEW: Callbacks for platform notifications
    AgentCallbacks
}
```

**File: `internal/telegram/bot.go`**
```go
// Bot calls callbacks through interface
func (b *Bot) afterTurnComplete() {
    if b.handler != nil {
        b.handler.OnTurnComplete()
    }
}

func (b *Bot) onUserMessage(sessionKey string) {
    if b.handler != nil {
        b.handler.OnUserMessage(sessionKey)
    }
}
```

**File: `internal/agent/agent.go`**
```go
// Agent implements AgentCallbacks
func (a *Agent) OnTurnComplete() {
    // Track for cache warming
    a.lastTurnTime = time.Now()
}

func (a *Agent) OnUserMessage(sessionKey string) {
    // Track for keepalive
    a.lastUserMessage = time.Now()
}
```

**Effort:** 0.5 day

---

## Stage 10: Testing and Validation

**Goal:** Verify architecture works end-to-end.

**Test scenarios:**

**File: `internal/agent/platform_test.go` (new)**
```go
func TestAgentCreatesSinglePlatform(t *testing.T) {
    // Test agent creates telegram platform from config
    // Verify platform is accessible via GetPlatform()
}

func TestAgentCreatesMultiplePlatforms(t *testing.T) {
    // Test agent creates telegram + discord platforms
    // Verify both platforms accessible
    // Verify correct platform returned for session keys
}

func TestSessionKeyParsing(t *testing.T) {
    // Test new format: "agent:telegram:123456"
    // Test backward compat: "agent:chat:123456"
    // Verify platform extraction works
}

func TestPlatformStartStop(t *testing.T) {
    // Test StartPlatforms() starts all platforms
    // Test StopPlatforms() stops all platforms
}

func TestGetPlatform(t *testing.T) {
    // Test GetPlatform("mybot:telegram:123") returns telegram bot
    // Test GetPlatform("mybot:discord:456") returns discord bot
    // Test GetPlatform("mybot:unknown:789") returns nil
}
```

**File: `internal/telegram/bot_handler_test.go` (new)**
```go
func TestBotUsesMessageHandler(t *testing.T) {
    // Test Bot holds MessageHandler interface
    // Test Bot calls HandleMessage through interface
    // Mock MessageHandler, verify calls
}
```

**Integration test:**
```bash
# Manual test with real config
make build
./bin/foci-gw -config test-multiplatform.toml

# Verify:
# 1. Agent starts with telegram + discord
# 2. Messages from both platforms work
# 3. Sessions are isolated by platform
# 4. Tools work with both platforms
```

**Effort:** 1 day

---

## Summary of Changes by Package

### `internal/platform`
**Files changed:**
- `types.go` - Add `Warnings()`, `OnTurnComplete()`, `OnUserMessage()` to `MessageHandler`

**New files:**
- None

**Breaking changes:**
- `MessageHandler` interface additions (agent already implements most)

### `internal/telegram`
**Files changed:**
- `bot.go` - Change `agent *agent.Agent` → `handler platform.MessageHandler`
- Constructor signature change
- All agent method calls go through interface
- Session key format updated

**New files:**
- None

**Breaking changes:**
- Constructor signature (internal package, acceptable)

### `internal/agent`
**Files changed:**
- `agent.go` - Add `platforms map[string]platform.Sender`, create platforms in constructor
- Implement full `MessageHandler` interface including callbacks

**New files:**
- `platforms.go` - Platform management methods (`AddPlatform`, `GetPlatform`, `StartPlatforms`, `StopPlatforms`)

**Breaking changes:**
- Constructor signature (internal package, acceptable)
- Agent now creates its own platforms

### `internal/config`
**Files changed:**
- `config.go` - Add `Platforms map[string]PlatformConfig` to `AgentConfig`

**New files:**
- `migrate.go` - Backward compatibility migration for old config format

**Breaking changes:**
- None (backward compat maintained)

### `internal/session`
**Files changed:**
- None

**New files:**
- `keys.go` - Session key helpers (`NewPlatformSessionKey`, `ParseSessionKey`)

**Breaking changes:**
- None (session keys are just strings, format is internal)

### `internal/tools`
**Files changed:**
- `telegram.go` → rename to `messaging.go` (or keep both)
- Tool uses `platform.Sender` interface
- Tool name: `send_telegram` → `send_message` (or keep both)

**Breaking changes:**
- None (tool name can be aliased for compat)

### `cmd/foci-gw`
**Files changed:**
- `main.go` - Remove all platform creation, simplify to agent creation
- `agents.go` - Update agent constructor call, wire tools with `agent.GetPlatform`

**Breaking changes:**
- None (internal command)

---

## Migration Path

### Phase 1: Interface Changes (Stages 1-2)
**Deploy independently:** Yes
**Risk:** Low - internal interfaces only
**Rollback:** Easy - revert interface changes

**What changes:**
- `MessageHandler` interface additions
- `telegram.Bot` holds interface instead of concrete type

**Validation:**
```bash
make test  # All tests pass
make build  # Compiles successfully
```

### Phase 2: Agent Creates Platforms (Stages 3-4)
**Deploy independently:** Yes (after Phase 1)
**Risk:** Medium - core agent logic changes
**Rollback:** Moderate - revert agent constructor changes

**What changes:**
- Agent creates platforms in constructor
- Session key format updated
- Backward compat for old session keys

**Validation:**
```bash
make test  # All tests pass
./bin/foci-gw -config foci.toml  # Existing config still works
```

### Phase 3: Config and Main (Stages 5-6)
**Deploy independently:** Yes (after Phase 2)
**Risk:** Medium - config format changes
**Rollback:** Easy - revert config changes, main.go is simple

**What changes:**
- Config structure updated
- Migration for old config
- Main.go simplified

**Validation:**
```bash
# Old config works
./bin/foci-gw -config old-format.toml

# New config works
./bin/foci-gw -config new-format.toml
```

### Phase 4: Tools and Polish (Stages 7-10)
**Deploy independently:** Yes (after Phase 3, stages can be done separately)
**Risk:** Low - incremental improvements
**Rollback:** Easy - individual tool changes

**What changes:**
- Tools use platform interface
- Testing coverage
- Multiball generalization (optional)
- Callbacks finalization

**Validation:**
```bash
make test  # New tests pass
# Integration tests with multiple platforms
```

---

## Open Questions

### 1. Should multiball be generalized or stay Telegram-specific?

**Current recommendation:** Keep Telegram-specific for now.

**Reasoning:**
- Multiball is tightly coupled to Telegram's group chat + secondary bot token pattern
- Discord has different threading model
- Matrix has rooms, not clear if multiball concept applies
- YAGNI - don't generalize until we have a second use case

**Future path:**
- If Discord needs similar feature, create `MultiballPlatform` interface
- Generalize pattern then

### 2. Do we need lazy platform creation or is eager sufficient?

**Current recommendation:** Eager (config-time) creation is sufficient.

**Reasoning:**
- Platforms require API keys/tokens (can't create without config)
- Simpler error handling (fail fast at startup)
- Clearer lifecycle management
- No obvious use case for runtime platform addition

**Future path:**
- If needed, add `AddPlatform()` method to agent
- Hot-reload of config could trigger lazy creation

### 3. Should platforms have their own config package or stay in main config?

**Current recommendation:** Keep in `internal/config`.

**Reasoning:**
- Agent config already exists, adding platforms section is natural
- Avoids circular dependencies
- Simpler TOML structure
- Platform-specific fields are just more fields in `PlatformConfig`

**Alternative:** Create `internal/telegram/config.go`, `internal/discord/config.go` if platform configs get complex.

### 4. How do we handle platform-specific features?

**Examples:**
- Telegram: inline keyboards, callback queries, voice notes
- Discord: embeds, reactions, slash commands
- Matrix: reactions, threads

**Current recommendation:** Keep platform-specific features in platform implementations.

**Pattern:**
```go
// Tools can check for optional interfaces
type InlineKeyboarder interface {
    SendTextWithKeyboard(chatID, text string, keyboard Keyboard) error
}

func (t *Tool) Execute(...) {
    sender := t.getSender(sessionKey)
    
    if kb, ok := sender.(InlineKeyboarder); ok {
        // Use inline keyboard
        kb.SendTextWithKeyboard(...)
    } else {
        // Fallback to plain text
        sender.SendText(...)
    }
}
```

**Alternative:** Create `TelegramSender` interface extending `Sender` with Telegram-specific methods.

---

## Effort Summary

| Stage | Effort | Risk | Dependencies |
|-------|--------|------|--------------|
| 1. MessageHandler Interface | 0.5 day | Low | None |
| 2. Telegram Uses Interface | 0.5 day | Low | Stage 1 |
| 3. Agent Creates Platforms | 1 day | Medium | Stage 2 |
| 4. Session Key Format | 0.5 day | Low | Stage 3 |
| 5. Config Structure | 0.5 day | Medium | None |
| 6. Main Simplification | 1 day | Medium | Stages 3, 5 |
| 7. Tool Integration | 0.5 day | Low | Stage 3 |
| 8. Multiball Support | 0 days | N/A | Stage 3 |
| 9. Callbacks | 0.5 day | Low | Stage 2 |
| 10. Testing | 1 day | Low | All |
| **Total** | **6 days** | | |

---

## Success Criteria

### Phase 1 Complete When:
- [ ] `MessageHandler` interface has all methods telegram needs
- [ ] `telegram.Bot` holds `MessageHandler` not `*agent.Agent`
- [ ] All existing tests pass
- [ ] Telegram messages still work

### Phase 2 Complete When:
- [ ] Agent creates its own platforms
- [ ] Session keys include platform type
- [ ] Backward compat for old session keys works
- [ ] Agent.GetPlatform() returns correct platform
- [ ] All existing tests pass

### Phase 3 Complete When:
- [ ] New config format works
- [ ] Old config format migrates correctly
- [ ] Main.go doesn't create platforms
- [ ] Multiple platforms work simultaneously
- [ ] All existing tests pass

### Phase 4 Complete When:
- [ ] Tools use platform interface
- [ ] Tests cover multi-platform scenarios
- [ ] Documentation updated (WIRING.md, arch-review.md)
- [ ] All existing tests pass
- [ ] Integration test with 2+ platforms works

---

## Next Steps

1. **Review this plan** - discuss any concerns or modifications
2. **Start Phase 1** - MessageHandler interface completion
3. **Incremental deployment** - each phase independently deployable
4. **Update docs** - WIRING.md and arch-review.md as we go

**Ready to proceed?**
