# Compact-on-Idle Implementation Plan

## Context

This feature adds **idle-aware compaction** to prevent context bloat during idle periods. Currently, compaction only triggers when context usage exceeds a fixed threshold (default 80%). This works well for active conversations but can leave sessions bloated during idle periods when we could compact more aggressively.

**The problem:** When a user is idle, we continue to carry around a large context even though:
1. We have spare mana (no user activity = low API usage)
2. Cache warmth is less critical (no immediate follow-up messages)
3. Mana may reset soon, making compaction "free"

**The solution:** Add configurable idle-aware pressure that:
- Lowers the compaction threshold when the user has been idle
- Increases pressure as mana reset approaches (spend freely when quota will reset)
- Triggers special high-fidelity compaction just before mana resets (preserve all detail)

## Implementation Design

### 1. Configuration Schema

Add five new config fields to `SessionsConfig` (global) with per-agent overrides:

**File:** `/home/rich/git/foci-wt/20260306-091052/internal/config/config.go`

```go
// In SessionsConfig struct (after line 233)
CompactionIdleThreshold        string  `toml:"compaction_idle_threshold"`         // idle duration before pressure starts (default "45m", "0" disables)
CompactionIdlePressureStart    string  `toml:"compaction_idle_pressure_start"`    // context % to start ramping pressure (default "70%")
CompactionIdlePressureMax      float64 `toml:"compaction_idle_pressure_max"`      // max threshold reduction (default 0.15 → min 65%)
CompactionManaRefreshThreshold string  `toml:"compaction_mana_refresh_threshold"` // trigger special compact when reset this soon (default "15m")
CompactionManaRefreshPreserve  *int    `toml:"compaction_mana_refresh_preserve"`  // messages to preserve in refresh mode (nil = ALL)

// In AgentConfig struct (after compaction fields ~line 150)
CompactionIdleThreshold        string   `toml:"compaction_idle_threshold"`
CompactionIdlePressureStart    string   `toml:"compaction_idle_pressure_start"`
CompactionIdlePressureMax      *float64 `toml:"compaction_idle_pressure_max"`       // pointer for optional override
CompactionManaRefreshThreshold string   `toml:"compaction_mana_refresh_threshold"`
CompactionManaRefreshPreserve  *int     `toml:"compaction_mana_refresh_preserve"`
```

**Defaults** (in `setDefaults()` function):
```go
setStringDefault(&cfg.Sessions.CompactionIdleThreshold, "45m")
setStringDefault(&cfg.Sessions.CompactionIdlePressureStart, "70%")
setFloatDefault(&cfg.Sessions.CompactionIdlePressureMax, 0.15)
setStringDefault(&cfg.Sessions.CompactionManaRefreshThreshold, "15m")
// CompactionManaRefreshPreserve: nil = special "preserve ALL" mode
```

**Validation** (in `validateConfig()`):
- Parse idle threshold as duration, reject invalid
- Parse pressure start as percentage or decimal (0.0-1.0)
- Validate pressure_max in range 0.0-1.0
- Parse mana refresh threshold as duration

### 2. Core Algorithm

**New function in:** `/home/rich/git/foci-wt/20260306-091052/internal/compaction/compact.go`

```go
// calculateIdlePressure returns the adjusted compaction threshold based on
// idle time and mana state. Returns (adjustedThreshold, isManaRefreshMode).
//
// Algorithm:
// 1. If mana reset is imminent (<15m), return aggressive threshold (0.4) + mana refresh flag
// 2. If not idle yet, return base threshold unchanged
// 3. If context below pressure start (70%), return base threshold unchanged
// 4. Otherwise, linearly reduce threshold based on idle duration:
//    - At idle threshold (45m): 0% pressure → base threshold (0.8)
//    - At 2x idle threshold (90m): 100% pressure → base - max (0.65)
//
// Example with defaults (base=0.8, idle=45m, max=0.15):
// - idle 0-44m:    threshold = 0.80 (no change)
// - idle 45m:      threshold = 0.80 (pressure starts)
// - idle 67.5m:    threshold = 0.725 (50% pressure)
// - idle 90m+:     threshold = 0.65 (100% pressure)
// - <15m to reset: threshold = 0.4 (mana refresh mode)
func calculateIdlePressure(
	baseThreshold float64,              // e.g. 0.8
	idleDuration time.Duration,         // time since last user message
	idleThreshold time.Duration,        // when idle pressure starts (e.g. 45m)
	pressureStart string,               // e.g. "70%" - context % to start ramping
	pressureMax float64,                // max reduction (e.g. 0.15)
	manaResetsAt time.Time,             // from usage API (zero if unavailable)
	manaRefreshThreshold time.Duration, // e.g. 15m before reset
	currentTokens int,                  // actual input tokens
	contextLimit int,                   // model's context window
) (adjustedThreshold float64, isManaRefresh bool) {

	// Priority 1: Mana refresh special mode (overrides everything)
	if !manaResetsAt.IsZero() {
		untilReset := time.Until(manaResetsAt)
		if untilReset > 0 && untilReset < manaRefreshThreshold {
			// Aggressive compaction while we have free mana incoming
			return baseThreshold * 0.5, true
		}
	}

	// Priority 2: Not idle yet - no pressure
	if idleDuration < idleThreshold {
		return baseThreshold, false
	}

	// Priority 3: Parse pressure start threshold
	startPct := 0.70 // default
	if strings.HasSuffix(pressureStart, "%") {
		trimmed := strings.TrimSuffix(pressureStart, "%")
		if val, err := strconv.ParseFloat(trimmed, 64); err == nil {
			startPct = val / 100.0
		}
	} else if val, err := strconv.ParseFloat(pressureStart, 64); err == nil {
		startPct = val
	}

	// Priority 4: Context below pressure start - no pressure yet
	currentPct := float64(currentTokens) / float64(contextLimit)
	if currentPct < startPct {
		return baseThreshold, false
	}

	// Priority 5: Apply linear idle pressure ramp
	// idleThreshold (45m) = 0% pressure
	// 2 * idleThreshold (90m) = 100% pressure
	idleProgress := float64(idleDuration-idleThreshold) / float64(idleThreshold)
	if idleProgress > 1.0 {
		idleProgress = 1.0
	}

	reduction := pressureMax * idleProgress
	adjustedThreshold = baseThreshold - reduction

	return adjustedThreshold, false
}
```

### 3. Integration Point

**Modify:** `/home/rich/git/foci-wt/20260306-091052/internal/agent/agent.go`

**Current `maybeCompact()` at line 1330:**
```go
if a.Compactor == nil || a.AsyncNotifier.HasPending(sessionKey) || !a.Compactor.ShouldCompact(messages, usage) {
    return
}
```

**Replace with idle-aware logic:**

```go
func (a *Agent) maybeCompact(ctx context.Context, client provider.Client, sessionKey string,
                             messages []anthropic.Message, system []anthropic.SystemBlock,
                             usage *anthropic.Usage, sm *sessionMeta) {

	// Early exits: no compactor or async pending
	if a.Compactor == nil || a.AsyncNotifier.HasPending(sessionKey) {
		return
	}

	// Calculate idle duration from session metadata
	idleDuration := time.Duration(0)
	if !sm.lastMessageTime.IsZero() {
		idleDuration = time.Since(sm.lastMessageTime)
	}

	// Get mana reset time (if available)
	var manaResetsAt time.Time
	if a.UsageClient != nil {
		if usageResp, err := a.UsageClient.GetUsage(ctx); err == nil && usageResp.FiveHour != nil {
			if usageResp.FiveHour.ResetsAt != nil {
				manaResetsAt, _ = time.Parse(time.RFC3339Nano, *usageResp.FiveHour.ResetsAt)
			}
		}
	}

	// Calculate current token usage
	totalTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	contextLimit := compaction.ContextLimit(a.Model)

	// Parse idle threshold (with "0" special case for disable)
	idleThreshold, err := time.ParseDuration(a.CompactionIdleThreshold)
	if err != nil || a.CompactionIdleThreshold == "0" {
		idleThreshold = 0 // disabled
	}

	// Parse mana refresh threshold
	manaRefreshThreshold, err := time.ParseDuration(a.CompactionManaRefreshThreshold)
	if err != nil {
		manaRefreshThreshold = 15 * time.Minute // fallback default
	}

	// Get pressure-adjusted threshold
	adjustedThreshold := a.Compactor.Threshold() // base threshold
	isManaRefresh := false

	if idleThreshold > 0 {
		// Calculate idle pressure adjustment
		adjustedThreshold, isManaRefresh = compaction.CalculateIdlePressure(
			a.Compactor.Threshold(),
			idleDuration,
			idleThreshold,
			a.CompactionIdlePressureStart,
			a.CompactionIdlePressureMax,
			manaResetsAt,
			manaRefreshThreshold,
			totalTokens,
			contextLimit,
		)
	}

	// Check if we should compact with adjusted threshold
	triggerPoint := int(float64(contextLimit) * adjustedThreshold)
	shouldCompact := totalTokens > triggerPoint

	if !shouldCompact {
		return
	}

	// Check no_compact flag (same as before)
	if a.SessionNoCompact(sessionKey) {
		percent := int(float64(totalTokens) / float64(contextLimit) * 100)
		a.logger().Infof("context at %d%% capacity for no_compact session", percent)
		return
	}

	// Log compaction reason
	if isManaRefresh {
		untilReset := time.Until(manaResetsAt).Round(time.Minute)
		a.logger().Infof("session=%s mana-refresh compaction (reset in %s, %d/%d tokens)",
			sessionKey, untilReset, totalTokens, contextLimit)
	} else if idleDuration > idleThreshold && idleThreshold > 0 {
		a.logger().Infof("session=%s idle compaction (idle %s, threshold %.1f%%, %d/%d tokens)",
			sessionKey, idleDuration.Round(time.Minute), adjustedThreshold*100, totalTokens, contextLimit)
	} else {
		a.logger().Infof("session=%s threshold compaction (%d/%d tokens)",
			sessionKey, totalTokens, contextLimit)
	}

	// Special handling for mana-refresh mode: preserve more messages
	if isManaRefresh {
		oldPreserve := a.Compactor.PreserveMessages()
		defer func() {
			a.Compactor.SetPreserveMessages(oldPreserve) // restore after
		}()

		if a.CompactionManaRefreshPreserve != nil {
			// Explicit preserve count (e.g. 100)
			a.Compactor.SetPreserveMessages(*a.CompactionManaRefreshPreserve)
		} else {
			// nil = preserve ALL messages (special mode)
			a.Compactor.SetPreserveMessages(len(messages))
		}
	}

	// Rest of compaction logic unchanged (lines 1341-1366)
	oldCount := len(messages)
	if a.CompactionNotifyFunc != nil {
		a.CompactionNotifyFunc(sessionKey, "⏳ Compacting context...")
	}
	// ... (existing code)
}
```

### 4. Compactor Interface Changes

**File:** `/home/rich/git/foci-wt/20260306-091052/internal/compaction/compact.go`

Add getter/setter methods for `preserveMessages` and `threshold` (currently private):

```go
// Threshold returns the base compaction threshold.
func (c *Compactor) Threshold() float64 {
	return c.threshold
}

// PreserveMessages returns the current preserve messages count.
func (c *Compactor) PreserveMessages() int {
	return c.preserveMessages
}

// SetPreserveMessages sets the preserve messages count.
func (c *Compactor) SetPreserveMessages(n int) {
	c.preserveMessages = n
}
```

### 5. Agent Struct Fields

**File:** `/home/rich/git/foci-wt/20260306-091052/internal/agent/agent.go`

Add new fields to Agent struct for resolved config values (match other compaction fields):

```go
// In Agent struct (around existing compaction fields)
CompactionIdleThreshold        string
CompactionIdlePressureStart    string
CompactionIdlePressureMax      float64
CompactionManaRefreshThreshold string
CompactionManaRefreshPreserve  *int
```

Wire these from config in the agent constructor (follow existing pattern for other compaction fields).

### 6. Testing Strategy

**Unit tests:** `/home/rich/git/foci-wt/20260306-091052/internal/compaction/compact_test.go`

```go
func TestCalculateIdlePressure_NotIdle(t *testing.T) {
	// idle 30m, threshold 45m → no adjustment
	adj, refresh := calculateIdlePressure(0.8, 30*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 140000, 200000)
	assert.Equal(t, 0.8, adj)
	assert.False(t, refresh)
}

func TestCalculateIdlePressure_IdleRamp(t *testing.T) {
	// idle 45m → 0% pressure (threshold 0.8)
	// idle 67.5m → 50% pressure (threshold 0.725)
	// idle 90m → 100% pressure (threshold 0.65)

	// At pressure start (45m)
	adj, _ := calculateIdlePressure(0.8, 45*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 140000, 200000)
	assert.Equal(t, 0.8, adj)

	// At 50% pressure (67.5m)
	adj, _ = calculateIdlePressure(0.8, 67*time.Minute+30*time.Second, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 140000, 200000)
	assert.InDelta(t, 0.725, adj, 0.01)

	// At 100% pressure (90m)
	adj, _ = calculateIdlePressure(0.8, 90*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 140000, 200000)
	assert.Equal(t, 0.65, adj)
}

func TestCalculateIdlePressure_ManaRefreshMode(t *testing.T) {
	// <15m to mana reset → aggressive threshold (0.4), refresh flag
	resetsAt := time.Now().Add(10 * time.Minute)
	adj, refresh := calculateIdlePressure(0.8, 30*time.Minute, 45*time.Minute,
		"70%", 0.15, resetsAt, 15*time.Minute, 100000, 200000)
	assert.Equal(t, 0.4, adj)
	assert.True(t, refresh)
}

func TestCalculateIdlePressure_BelowPressureStart(t *testing.T) {
	// context 60%, pressure starts at 70% → no adjustment
	adj, _ := calculateIdlePressure(0.8, 60*time.Minute, 45*time.Minute,
		"70%", 0.15, time.Time{}, 15*time.Minute, 120000, 200000)
	assert.Equal(t, 0.8, adj)
}
```

**Integration tests:** Manual testing with low thresholds

```bash
# In foci.toml:
[sessions]
compaction_idle_threshold = "5m"  # fast testing
compaction_idle_pressure_start = "60%"
compaction_mana_refresh_threshold = "30m"  # easier to test

# Test procedure:
# 1. Start conversation, reach ~65% context (130k tokens)
# 2. Wait 5 minutes (no user messages)
# 3. Send any message → should trigger idle compaction
# 4. Check logs for "idle compaction" message
# 5. Test mana-refresh by waiting until 20m before quota reset
# 6. Send message → should trigger mana-refresh compaction with more preserved messages
```

### 7. Documentation Updates

**File:** `/home/rich/git/foci-wt/20260306-091052/docs/CONFIG.md`

Add new section under compaction configuration:

```markdown
#### Idle-Aware Compaction

Foci can trigger compaction proactively when the user has been idle, with mana-aware pressure adjustment:

- **`compaction_idle_threshold`** (string, default `"45m"`): Idle duration before pressure starts. Set to `"0"` to disable idle-aware compaction. Format: Go duration string (e.g., `"30m"`, `"1h"`).

- **`compaction_idle_pressure_start`** (string, default `"70%"`): Context usage percentage where idle pressure starts ramping. Below this threshold, idle time has no effect. Format: percentage string (e.g., `"70%"`) or decimal (e.g., `"0.7"`).

- **`compaction_idle_pressure_max`** (float64, default `0.15`): Maximum threshold reduction from idle pressure. With default base threshold of 0.8, this allows reduction to 0.65 (80% → 65%). Range: 0.0-1.0.

- **`compaction_mana_refresh_threshold`** (string, default `"15m"`): Trigger special high-fidelity compaction when mana reset is this soon. Format: Go duration string.

- **`compaction_mana_refresh_preserve`** (int, optional): Messages to preserve during mana-refresh compaction. If unset (nil), preserves ALL messages (special mode). If set to 0, uses normal preservation count.

**How it works:**

1. **Normal threshold compaction** (existing): Triggers at 80% context usage
2. **Idle pressure** (new): After `compaction_idle_threshold` idle time, gradually reduces threshold from 80% → 65% over the next idle period (linear ramp)
3. **Mana refresh mode** (new): When mana reset is <15m away, triggers aggressive compaction (40% threshold) and preserves all/most messages (cheap summary since mana will reset)

**Example scenarios:**

```toml
# Scenario A: Conservative (minimize compactions)
[sessions]
compaction_idle_threshold = "0"  # disable idle compaction

# Scenario B: Aggressive (keep context small)
[sessions]
compaction_idle_threshold = "30m"
compaction_idle_pressure_max = 0.25  # reduce to 55% threshold
compaction_mana_refresh_preserve = 50  # preserve last 50 messages in refresh mode

# Scenario C: Per-agent override
[[agents]]
id = "research"
[agents.sessions]
compaction_idle_threshold = "15m"  # compact sooner for research bot
```
```

### 8. Critical Files Summary

1. **`/home/rich/git/foci-wt/20260306-091052/internal/config/config.go`**
   - Add 5 new fields to `SessionsConfig` (line ~233)
   - Add 5 new fields to `AgentConfig` (line ~150)
   - Add defaults in `setDefaults()`
   - Add validation in `validateConfig()`

2. **`/home/rich/git/foci-wt/20260306-091052/internal/compaction/compact.go`**
   - Add `calculateIdlePressure()` function
   - Add getter/setter methods for threshold and preserveMessages
   - Export `ContextLimit()` if not already public

3. **`/home/rich/git/foci-wt/20260306-091052/internal/agent/agent.go`**
   - Add 5 new fields to Agent struct
   - Completely rewrite `maybeCompact()` function (lines 1329-1366)
   - Wire config values in agent constructor

4. **`/home/rich/git/foci-wt/20260306-091052/internal/compaction/compact_test.go`**
   - Add unit tests for `calculateIdlePressure()`

5. **`/home/rich/git/foci-wt/20260306-091052/docs/CONFIG.md`**
   - Document new config fields with examples

### 9. Verification Plan

**After implementation, verify:**

1. **Config parsing:**
   ```bash
   make build
   ./bin/foci-gw -config foci.toml -validate
   # Should accept new config fields without errors
   ```

2. **Idle detection:**
   - Set `compaction_idle_threshold = "2m"`
   - Start conversation, wait 2 minutes
   - Send message → check logs for idle duration calculation

3. **Pressure ramp:**
   - Set context to 75% usage
   - Wait past idle threshold
   - Verify compaction triggers earlier than 80%

4. **Mana refresh mode:**
   - Wait until <15m before quota reset (check `/usage` command)
   - Send message with moderate context (50%)
   - Verify mana-refresh compaction triggers
   - Check that more messages were preserved

5. **Per-agent overrides:**
   - Set different idle thresholds for different agents
   - Verify each agent respects its own settings

6. **Disable functionality:**
   - Set `compaction_idle_threshold = "0"`
   - Verify idle pressure never applies (falls back to standard threshold)

## Implementation Sequence

1. **Config schema** (internal/config/config.go) - Add fields, defaults, validation
2. **Algorithm** (internal/compaction/compact.go) - Add calculateIdlePressure function
3. **Integration** (internal/agent/agent.go) - Rewrite maybeCompact
4. **Tests** (internal/compaction/compact_test.go) - Unit tests
5. **Documentation** (docs/CONFIG.md) - User documentation
6. **Manual testing** - Verify with real usage
7. **Update WIRING.md** if agent loop semantics changed (unlikely)

## Edge Cases Handled

- **No UsageClient** (non-Anthropic endpoints): Mana refresh mode disabled, idle pressure works
- **UsageAPI failures**: Falls back to normal threshold compaction
- **Async operations pending**: Existing guard prevents compaction (unchanged)
- **No-compact sessions**: Existing guard logs warning (unchanged)
- **Rapid idle→active cycles**: Each idle period independent, no cumulative state
- **Config hot-reload**: New values require restart (document this)
- **Timezone handling**: time.Parse() and time.Until() are timezone-aware
