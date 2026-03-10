# Phase 3+: Agent Creates Platforms

## Overview

After Phase 2 breaks the circular dependency, the agent package can import the telegram package. This enables agents to create their own platforms.

## Current State

```
main.go creates telegram.Bot
main.go creates agent.Agent
main.go wires them together
```

## Target State

```
main.go creates shared resources (sessions, memory, tools)
agent.New() creates its own platforms from config
agent.StartPlatforms() starts all platforms
main.go calls agent.StartPlatforms()
```

---

## Phase 3: Agent Creates Platforms

**Goal:** Agent constructor creates its platforms based on config.

### Changes

1. **Add platform management to Agent struct** (`internal/agent/agent.go`):
```go
type Agent struct {
    // ... existing fields ...
    
    platforms  map[string]platform.Sender
    platformMu sync.RWMutex
}
```

2. **Create platform management methods** (`internal/agent/platforms.go` - new file):
```go
package agent

import (
    "context"
    "foci/internal/platform"
)

func (a *Agent) AddPlatform(name string, p platform.Sender) {
    a.platformMu.Lock()
    defer a.platformMu.Unlock()
    if a.platforms == nil {
        a.platforms = make(map[string]platform.Sender)
    }
    a.platforms[name] = p
}

func (a *Agent) GetPlatform(name string) platform.Sender {
    a.platformMu.RLock()
    defer a.platformMu.RUnlock()
    return a.platforms[name]
}

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
```

3. **Update agent constructor** to create platforms from config

4. **Simplify main.go** - remove platform creation, call agent.StartPlatforms()

---

## Phase 4: Platform-Aware Session Keys

**Goal:** Session keys encode platform type.

### Current Format
```
agentID:chat:CHATID
```

### Target Format
```
agentID:PLATFORM:IDENTIFIER
```

### Examples
- `mybot:telegram:123456`
- `mybot:discord:789012`

### Changes

1. **Create session key helpers** (`internal/session/keys.go` - new file)
2. **Update telegram.Bot to use new format**
3. **Add backward compatibility for old format**

---

## Phase 5: Config Structure

**Goal:** Config lists platforms per agent.

### Current Config
```toml
[[agents]]
id = "mybot"
telegram_bot = "123:ABC"
```

### Target Config
```toml
[[agents]]
id = "mybot"

[agents.platforms.telegram]
bot_token = "123:ABC"
allowed_users = ["123456"]
```

### Changes

1. **Add Platforms field to AgentConfig** (`internal/config/config.go`)
2. **Add config migration** for backward compatibility
3. **Update config loading**

---

## Phase 6: Main.go Simplification

**Goal:** Main doesn't create platforms.

### Current Flow
```
main creates shared resources
main creates telegram.Bot
main creates agent with bot reference
main calls bot.Start()
```

### Target Flow
```
main creates shared resources
main creates agent with config (agent creates platforms)
main calls agent.StartPlatforms()
```

### Changes

1. **Remove telegram import from main.go**
2. **Remove all telegram.NewBot() calls**
3. **Remove BotManager usage**
4. **Update agent creation to include platform configs**

---

## Phase 7: Tool Integration

**Goal:** Tools work with any platform.

### Changes

1. **Update send_telegram tool to use platform.Sender**
2. **Wire tools with agent.GetPlatform()**

---

## Phase 8: Testing

**Goal:** Verify multi-platform architecture works.

### Tests to Add

1. Test agent creates single platform
2. Test agent creates multiple platforms
3. Test session key parsing
4. Test platform start/stop
5. Test GetPlatform returns correct platform

---

## Summary

| Phase | Goal | Effort | Status |
|-------|------|--------|--------|
| 1 | Align MessageHandler interface | 0.5 day | ✓ DONE |
| 2 | Telegram uses interface | 0.5 day | ✓ DONE |
| 3 | Agent creates platforms | 1 day | ✓ DONE |
| 4 | Platform-aware session keys | 0.5 day | — SKIPPED (current format better) |
| 5 | Config structure | 0.5 day | ✓ DONE |
| 6 | Main simplification + BotManager singleton | 1 day | ✓ DONE |
| 7 | Tool integration | 0.5 day | pending |
| 8 | Testing | 1 day | pending |
| **Total** | | **5.5 days** |

### Phase 6 Complete
- BotManager is now a singleton (`telegram.DefaultManager()`)
- Removed botMgr parameter passing from ~15 function signatures
- Main.go simplified: uses singleton instead of passing botMgr around
- All multiball logic stays in telegram package (internal detail)
- Agent owns its platforms via `AddPlatform()`
