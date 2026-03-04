package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/anthropic"
	"foci/compaction"
	"foci/config"
	"foci/log"
	"foci/mana"
	"foci/memory"
	"foci/prompts"
	"foci/provider"
	"foci/session"
	"foci/state"
	"foci/tools"
	"foci/warnings"
	"foci/workspace"
)

const defaultBraindeadWarningPrompt = "You've made many consecutive tool calls. Stop and verify: is what you're doing right now what the user actually asked for?"

// Attachment holds a raw image or document for inclusion in a message.
type Attachment struct {
	MediaType string // "image/jpeg", "image/png", "application/pdf", etc.
	Data      []byte // raw bytes (base64-encoded when building content blocks)
	SavedPath string // non-empty if attachment was persisted to disk
}

// sessionMeta tracks per-session state for metadata injection.
type sessionMeta struct {
	lastMessageTime time.Time
	prevCost        float64
	prevInput       int
	prevOutput      int
	prevCacheRead   int
	prevCacheWrite  int
	voiceMode       bool
	effort          string          // per-session effort override (empty = use agent default)
	thinking        string          // per-session thinking override (empty = use agent default)
	model           string          // per-session model override (empty = use agent default)
	modelEndpoint   string          // per-session endpoint override (empty = use agent default)
	client          provider.Client // per-session client override (nil = use a.Client)
	noCompact       bool            // per-session no_compact flag (sticky across async operations)
	systemBlocks    []anthropic.SystemBlock // per-session system prompt snapshot (nil = rebuild from bootstrap)
}

// ReplyFunc is called to deliver intermediate messages during a turn.
// Used by the Telegram bot to send early/deferred replies while
// the agent continues working (e.g., "Looking into this...").
type ReplyFunc func(text string)

// ToolCallObserver is called before each tool execution.
// Used by the Telegram bot to show which tools the agent is calling.
type ToolCallObserver func(toolName string, params json.RawMessage)

// ToolResultObserver is called after each tool execution with the result.
// Used by the Telegram bot to store tool results for inline keyboard expansion.
type ToolResultObserver func(toolName string, result string, isError bool)

// CacheBustFunc is called when a cache bust is detected (cache_read drops
// significantly compared to the previous request).
// session is the session key, prevRead is what we had, curRead is what we got.
type CacheBustFunc func(session string, prevRead, curRead int)

// Agent is the core agent loop.
type Agent struct {
	Client        provider.Client
	GetClient     func(endpoint, format string) provider.Client  // lazy-init client for endpoint:format
	PeekClient    func(endpoint, format string) provider.Client  // no-init check for client existence
	Sessions      *session.Store
	Tools         *tools.Registry
	Bootstrap     *workspace.Bootstrap
	Compactor     *compaction.Compactor // nil disables auto-compaction
	AsyncNotifier *tools.AsyncNotifier  // nil disables async-pending compaction guard
	Reminders     *memory.ReminderStore // nil disables reminder injection
	AgentID       string                // unique agent identifier (for per-agent DB queries)
	Model         string
	Log           *log.ComponentLogger  // structured logger for this agent

	EnvironmentBlock            string                          // pre-built environment context block (prepended first in system prompt)
	ExtraSystemBlocks           []provider.SystemBlock          // additional system blocks (e.g. skills list), injected before cache marker
	CacheStrategy               string                          // "auto" (top-level) or "explicit" (manual breakpoints)
	CacheBustDetect             bool                            // detect cache busts (cache_read drop >50%)
	CacheBustIdleThreshold      time.Duration                   // suppress cache bust alert if session idle > this (default 10m)
	CacheBustAlert              CacheBustFunc                   // callback for cache bust alerts
	DuplicateMessages           bool                            // send user text twice per API call (improves instruction following)
	BatchPartialAssistantMessages bool                          // accumulate mid-turn text; send concatenated on turn end (default false = send immediately)
	BatchPartialJoiner           string                         // separator between batched partial messages (default "")
	MaxResultChars              int                             // max chars for tool result before writing to file (0 disables)
	ToolResultTempDir           string                          // where to write large tool results
	ModelAliases                map[string]string               // for resolving "haiku" → full model ID
	SummaryContextTurns         int                             // recent conversation turns for summary context
	SummaryContextChars         int                             // max chars of context to send to Haiku
	MaxSummaryChars             int                             // max chars to auto-summarise (skip Haiku above this)
	AutoSummarise               bool                            // enable auto-summarise of oversized tool results (default true)
	Warnings                    *warnings.Queue                 // nil disables warning injection into session
	ManaWatcher                 *ManaWatcher                    // nil disables mana threshold warnings
	ManaWarnFunc                func(string)                    // callback for mana threshold warnings (e.g. Telegram notification)
	MaxTokensWarnFunc           func(string)                    // callback when stop_reason=max_tokens (response truncated)
	RateLimitFunc               func(retryAfter int)            // callback when API returns 429 (rate limit exhausted)
	CompactionNotifyFunc        func(string, string)            // callback for compaction notifications (session key, message)
	CompactionDebugFunc         func(string, string)            // callback for compaction debug (session key, summary text)
	OnActivity                  func(string)                    // callback when a session has activity (session key); nil disables
	Redact                      func(string) string             // redact secrets from tool output; nil disables
	StateStore                  *state.Store                    // nil disables state persistence
	UsageClient                 *anthropic.UsageClient          // nil disables mana metadata
	MessageTransforms           []CompiledTransform             // compiled regex rules for inbound message transformation
	CompactionSummaryPromptPath string   // file path; read at compaction time via prompts.ResolvePrompt
	CompactionHandoffMsg        string   // inline handoff message; empty resolves from search dirs or embedded default
	PromptSearchDirs            []string // directories to search for prompt files (agent workspace, shared)
	MaxToolLoops                int                             // max tool iterations per turn (default 25)
	MaxOutputTokens             int                             // max tokens in model response (default 8192)
	BraindeadWarningEnable      bool                            // enable braindead warning (default true)
	BraindeadWarningThreshold   int                             // consecutive tool loops before warning (0 = disabled)
	BraindeadWarningPrompt      string                          // warning text (empty = hardcoded default)
	TurnLockWarnThreshold       time.Duration                   // warn if turn lock wait exceeds this (default 3m)
	Effort                      string                          // effort level for API requests (empty = omit from request)
	Thinking                    string                          // thinking mode: "off" or "adaptive" (empty/"off" = disabled)
	Streaming                   bool                            // use streaming API when provider supports it
	ManaInvestInterval          time.Duration                   // invest interval for mana good/bad indicator; 0 = no indicator
	ServerTools                 []provider.ToolDef              // server-side tools (web_search, web_fetch) — executed by Anthropic, not client
	DefaultSessionKey           func() string                   // returns the main/default session key; reminders only inject into this session

	processing      int32 // atomic: number of in-flight HandleMessage calls
	turnDetailsMu   sync.Mutex
	turnDetails     map[uint64]*TurnDetail // keyed by unique turn ID
	turnIDCounter   uint64                 // atomic: monotonic turn ID
	turnLocksMu     sync.Mutex
	turnLocks       map[string]*sync.Mutex // per-session turn serialization
	metaMu          sync.Mutex
	meta            map[string]*sessionMeta // per-session metadata
	manaCacheMu     sync.Mutex
	manaCached      string
	manaResetCached string
	manaGoodCached  bool
	manaCacheTime   time.Time
}

// TransformMessage applies compiled message transforms to the text.
// Returns the original text unchanged if no transforms are configured.
func (a *Agent) TransformMessage(text string) string {
	if len(a.MessageTransforms) == 0 {
		return text
	}
	return ApplyTransforms(a.MessageTransforms, text)
}

// logger returns the agent's ComponentLogger, lazily creating a default if nil.
func (a *Agent) logger() *log.ComponentLogger {
	if a.Log != nil {
		return a.Log
	}
	a.Log = log.NewComponentLogger("agent")
	return a.Log
}

// InvalidateSystemCaches clears per-session system prompt caches so the
// next turn on every session rebuilds from the bootstrap. Call after
// explicit user actions that change the system prompt (e.g. /reload,
// session reset) where a global cache bust is expected.
func (a *Agent) InvalidateSystemCaches() {
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	for _, sm := range a.meta {
		sm.systemBlocks = nil
	}
}

// TurnDetail describes one in-flight turn for shutdown diagnostics.
type TurnDetail struct {
	SessionKey string
	Trigger    string // "user", "keepalive", "wake", "scheduled_wake", "telegram", "async_notify"
	ToolName   string // currently executing tool, or empty
	StartTime  time.Time
}

// IsProcessing returns true if the agent is currently handling a message.
func (a *Agent) IsProcessing() bool {
	return atomic.LoadInt32(&a.processing) > 0
}

// ProcessingDetails returns detail for every in-flight turn.
func (a *Agent) ProcessingDetails() []TurnDetail {
	a.turnDetailsMu.Lock()
	defer a.turnDetailsMu.Unlock()
	out := make([]TurnDetail, 0, len(a.turnDetails))
	for _, d := range a.turnDetails {
		out = append(out, *d)
	}
	return out
}

// registerTurn adds a TurnDetail and returns its ID and pointer (for tool tracking).
func (a *Agent) registerTurn(d *TurnDetail) uint64 {
	id := atomic.AddUint64(&a.turnIDCounter, 1)
	a.turnDetailsMu.Lock()
	if a.turnDetails == nil {
		a.turnDetails = make(map[uint64]*TurnDetail)
	}
	a.turnDetails[id] = d
	a.turnDetailsMu.Unlock()
	return id
}

// unregisterTurn removes a TurnDetail by ID.
func (a *Agent) unregisterTurn(id uint64) {
	a.turnDetailsMu.Lock()
	delete(a.turnDetails, id)
	a.turnDetailsMu.Unlock()
}

// SetProcessingForTest sets the processing counter directly. Test-only.
func (a *Agent) SetProcessingForTest(n int32) {
	atomic.StoreInt32(&a.processing, n)
}

// VoiceReplyFunc is called to deliver voice audio during a turn.
type VoiceReplyFunc func(oggData []byte)

// VoiceMode returns whether voice mode is active for the session.
func (a *Agent) VoiceMode(sessionKey string) bool {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.voiceMode
}

// SetVoiceMode toggles voice mode for the session.
func (a *Agent) SetVoiceMode(sessionKey string, on bool) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	sm.voiceMode = on

	if a.StateStore != nil {
		if err := a.StateStore.Set("voice:"+sessionKey, on); err != nil {
			a.logger().Errorf("session=%s persist voice mode: %v", sessionKey, err)
		}
	}
}

// RestoreVoiceMode loads voice mode from state store if available.
func (a *Agent) RestoreVoiceMode(sessionKey string) {
	if a.StateStore == nil {
		return
	}
	var on bool
	if a.StateStore.Get("voice:"+sessionKey, &on) && on {
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.voiceMode = on
		a.metaMu.Unlock()
		a.logger().Infof("restored voice mode for %s", sessionKey)
	}
}

// SessionEffort returns the effective effort for the session.
// Returns the per-session override if set, otherwise the agent-wide default.
func (a *Agent) SessionEffort(sessionKey string) string {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.effort != "" {
		return sm.effort
	}
	return a.Effort
}

// SetSessionEffort sets the per-session effort override and persists it.
func (a *Agent) SetSessionEffort(sessionKey, value string) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.effort = value
	a.metaMu.Unlock()

	if a.StateStore != nil {
		if value == "" {
			_ = a.StateStore.Delete("effort:" + sessionKey)
		} else if err := a.StateStore.Set("effort:"+sessionKey, value); err != nil {
			a.logger().Errorf("session=%s persist effort: %v", sessionKey, err)
		}
	}
}

// SessionThinking returns the effective thinking mode for the session.
// Returns the per-session override if set, otherwise the agent-wide default.
func (a *Agent) SessionThinking(sessionKey string) string {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.thinking != "" {
		return sm.thinking
	}
	return a.Thinking
}

// SetSessionThinking sets the per-session thinking override and persists it.
func (a *Agent) SetSessionThinking(sessionKey, value string) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.thinking = value
	a.metaMu.Unlock()

	if a.StateStore != nil {
		if value == "" {
			_ = a.StateStore.Delete("thinking:" + sessionKey)
		} else if err := a.StateStore.Set("thinking:"+sessionKey, value); err != nil {
			a.logger().Errorf("session=%s persist thinking: %v", sessionKey, err)
		}
	}
}

// SessionModel returns the effective model for the session.
// Returns the per-session override if set, otherwise the agent-wide default.
func (a *Agent) SessionModel(sessionKey string) string {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.model != "" {
		return sm.model
	}
	return a.Model
}

// SetSessionModel sets the per-session model, endpoint, and client override and persists it.
// client may be nil to fall back to the agent's default client.
func (a *Agent) SetSessionModel(sessionKey, value, endpoint string, client provider.Client) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.model = value
	sm.modelEndpoint = endpoint
	sm.client = client
	a.metaMu.Unlock()

	if a.StateStore != nil {
		if value == "" {
			_ = a.StateStore.Delete("model:" + sessionKey)
			_ = a.StateStore.Delete("model_endpoint:" + sessionKey)
		} else {
			if err := a.StateStore.Set("model:"+sessionKey, value); err != nil {
				a.logger().Errorf("session=%s persist model: %v", sessionKey, err)
			}
			if endpoint != "" {
				if err := a.StateStore.Set("model_endpoint:"+sessionKey, endpoint); err != nil {
					a.logger().Errorf("session=%s persist model_endpoint: %v", sessionKey, err)
				}
			}
		}
	}
}

// SessionClient returns the effective client for the session.
// Returns the per-session client override if set, otherwise the agent-wide default.
func (a *Agent) SessionClient(sessionKey string) provider.Client {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.client != nil {
		return sm.client
	}
	return a.Client
}

// SessionNoCompact returns the effective no_compact setting for the session.
func (a *Agent) SessionNoCompact(sessionKey string) bool {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.noCompact
}

// SetSessionNoCompact sets the per-session no_compact override and persists it.
func (a *Agent) SetSessionNoCompact(sessionKey string, value bool) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.noCompact = value
	a.metaMu.Unlock()

	if a.StateStore != nil {
		key := "no_compact:" + sessionKey
		val := ""
		if value {
			val = "true"
		}
		if err := a.StateStore.Set(key, val); err != nil {
			a.logger().Errorf("session=%s persist no_compact: %v", sessionKey, err)
		}
	}
}

// RestoreSessionOverrides loads per-session effort/thinking/model/no_compact from state store.
func (a *Agent) RestoreSessionOverrides(sessionKey string) {
	if a.StateStore == nil {
		return
	}
	var restored []string
	var val string
	if a.StateStore.Get("effort:"+sessionKey, &val) && val != "" {
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.effort = val
		a.metaMu.Unlock()
		restored = append(restored, "effort="+val)
	}
	if a.StateStore.Get("thinking:"+sessionKey, &val) && val != "" {
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.thinking = val
		a.metaMu.Unlock()
		restored = append(restored, "thinking="+val)
	}
	if a.StateStore.Get("model:"+sessionKey, &val) && val != "" {
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.model = val
		a.metaMu.Unlock()
		restored = append(restored, "model="+val)

		// Restore endpoint and resolve the matching client
		var ep string
		if a.StateStore.Get("model_endpoint:"+sessionKey, &ep) && ep != "" {
			sm2 := a.getSessionMeta(sessionKey)
			a.metaMu.Lock()
			sm2.modelEndpoint = ep
			a.metaMu.Unlock()
			restored = append(restored, "endpoint="+ep)

			if a.GetClient != nil {
				format := config.InferFormat(val)
				if c := a.GetClient(ep, format); c != nil {
					a.metaMu.Lock()
					sm2.client = c
					a.metaMu.Unlock()
				}
			}
		}
	}
	if a.StateStore.Get("no_compact:"+sessionKey, &val) && val != "" {
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.noCompact = (val == "true")
		a.metaMu.Unlock()
		restored = append(restored, "no_compact")
	}
	if len(restored) > 0 {
		a.logger().Infof("restored session overrides for %s: %s", sessionKey, strings.Join(restored, ", "))
	}
}

// manaString returns a cached mana percentage string (e.g. "75%").
// Returns empty string if UsageClient is nil or on error.
func (a *Agent) manaString() string {
	mana, _, _ := a.manaAndReset()
	return mana
}

// manaAndReset returns cached mana percentage, reset time strings, and whether
// mana is "good" (above invest threshold). Returns empty strings and false if
// UsageClient is nil or on error.
func (a *Agent) manaAndReset() (pct, reset string, good bool) {
	if a.UsageClient == nil {
		return "", "", false
	}

	a.manaCacheMu.Lock()
	defer a.manaCacheMu.Unlock()

	// Cache for 5 minutes
	if time.Since(a.manaCacheTime) < 5*time.Minute && a.manaCached != "" {
		return a.manaCached, a.manaResetCached, a.manaGoodCached
	}

	usage, err := a.UsageClient.GetUsage(context.Background())
	if err != nil {
		a.logger().Debugf("mana fetch: %v", err)
		// Return stale values only if cache is recent; otherwise return empty
		// to avoid displaying dangerously outdated mana readings.
		if time.Since(a.manaCacheTime) > 10*time.Minute {
			return "", "", false
		}
		return a.manaCached, a.manaResetCached, a.manaGoodCached
	}

	a.manaCached = mana.FormatPercent(usage)
	a.manaResetCached = mana.FormatReset(usage)
	a.manaGoodCached = a.computeManaGood(usage)
	a.manaCacheTime = time.Now()
	return a.manaCached, a.manaResetCached, a.manaGoodCached
}

// computeManaGood evaluates whether current mana is above the invest threshold.
func (a *Agent) computeManaGood(usage *anthropic.UsageResponse) bool {
	if a.ManaInvestInterval == 0 {
		return false
	}
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil {
		return false
	}
	manaVal := mana.FromUtilization(*usage.FiveHour.Utilization)
	var resetsAt time.Time
	if usage.FiveHour.ResetsAt != nil {
		resetsAt, _ = time.Parse(time.RFC3339Nano, *usage.FiveHour.ResetsAt)
	}
	return mana.IsGood(manaVal, resetsAt, a.ManaInvestInterval, time.Now())
}

func (a *Agent) getSessionMeta(key string) *sessionMeta {
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if a.meta == nil {
		a.meta = make(map[string]*sessionMeta)
	}
	m, ok := a.meta[key]
	if !ok {
		m = &sessionMeta{}
		a.meta[key] = m
	}
	return m
}

// ResetCacheBaseline clears the cache-read baseline for a session so that the
// next API call won't trigger a false cache-bust warning. Call this after any
// operation that changes the message prefix (e.g. manual compaction).
func (a *Agent) ResetCacheBaseline(sessionKey string) {
	a.getSessionMeta(sessionKey).prevCacheRead = 0
}

// SeedSessionMeta loads the session history and extracts the last user message's
// [meta] time= timestamp to seed lastMessageTime. This ensures the first turn
// after a restart shows a correct gap instead of gap=none.
func (a *Agent) SeedSessionMeta(key string) {
	msgs, err := a.Sessions.Load(key)
	if err != nil || len(msgs) == 0 {
		return
	}
	// Walk backwards to find last user message with a meta timestamp
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		text := anthropic.TextOf(msgs[i].Content)
		if t, ok := parseMetaTime(text); ok {
			sm := a.getSessionMeta(key)
			sm.lastMessageTime = t
			return
		}
	}
}

// parseMetaTime extracts the timestamp from a [meta] time=... header line.
func parseMetaTime(text string) (time.Time, bool) {
	if !strings.HasPrefix(text, "[meta] ") {
		return time.Time{}, false
	}
	idx := strings.Index(text, "time=")
	if idx < 0 {
		return time.Time{}, false
	}
	s := text[idx+5:]
	if sp := strings.IndexByte(s, ' '); sp > 0 {
		s = s[:sp]
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// buildMetaPrefix creates the metadata line prepended to user messages.
func buildMetaPrefix(now time.Time, model string, mana string, manaGood bool, sm *sessionMeta) string {
	gap := "none"
	if !sm.lastMessageTime.IsZero() {
		gap = formatGap(now.Sub(sm.lastMessageTime))
	}

	voiceFlag := ""
	if sm.voiceMode {
		voiceFlag = " voice=on"
	}

	manaFlag := ""
	if mana != "" {
		indicator := "🔴"
		if manaGood {
			indicator = "🟢"
		}
		manaFlag = " mana=" + mana + " " + indicator
	}

	if sm.prevCost == 0 && sm.prevInput == 0 {
		// First message in session — no previous turn data
		return fmt.Sprintf("[meta] time=%s gap=%s%s model=%s%s", now.UTC().Format(time.RFC3339), gap, voiceFlag, model, manaFlag)
	}

	return fmt.Sprintf("[meta] time=%s gap=%s%s model=%s prev_cost=$%.4f prev_tokens=in:%d/out:%d/cR:%d/cW:%d%s",
		now.UTC().Format(time.RFC3339), gap, voiceFlag, model,
		sm.prevCost,
		sm.prevInput, sm.prevOutput, sm.prevCacheRead, sm.prevCacheWrite,
		manaFlag)
}

// formatGap formats a duration as human-readable (e.g., "3h12m", "2d4h", "38s").
func formatGap(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

// LastUserMessageTime returns the last user message time for the session.
// Used by keepalive proactive warning dispatch to determine user activity.
func (a *Agent) LastUserMessageTime(sessionKey string) time.Time {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.lastMessageTime
}

// isSystemMessage returns true if the message is from a system source
// (keepalive, scheduled wake, proactive warnings) rather than a human user.
func isSystemMessage(msg string) bool {
	return strings.HasPrefix(msg, "[KEEPALIVE]") ||
		strings.HasPrefix(msg, "[SCHEDULED WAKE]") ||
		strings.HasPrefix(msg, "[proactive system warnings]")
}

func detectContentExtension(content string) string {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) > 0 {
		switch trimmed[0] {
		case '{', '[':
			return ".json"
		case '#':
			return ".md"
		case '<':
			if strings.HasPrefix(trimmed, "<?xml") || strings.HasPrefix(trimmed, "<rss") {
				return ".xml"
			}
			return ".html"
		}
	}
	return ".txt"
}

func (a *Agent) guardToolResult(ctx context.Context, client provider.Client, sessionKey, toolName string, result string, messages []anthropic.Message) string {
	if a.MaxResultChars <= 0 || len(result) <= a.MaxResultChars {
		return result
	}

	if err := os.MkdirAll(a.ToolResultTempDir, 0o700); err != nil {
		a.logger().Warnf("session=%s create tool result temp dir: %v", sessionKey, err)
		return result
	}

	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		a.logger().Warnf("session=%s generate random filename: %v", sessionKey, err)
		return result
	}
	ext := detectContentExtension(result)
	filename := fmt.Sprintf("tool-result-%s-%s%s", toolName, hex.EncodeToString(randBytes[:]), ext)
	fpath := filepath.Join(a.ToolResultTempDir, filename)

	if err := os.WriteFile(fpath, []byte(result), 0o600); err != nil {
		a.logger().Warnf("session=%s write tool result to file: %v", sessionKey, err)
		return result
	}

	a.logger().Debugf("tool result guard: %s produced %d chars (limit %d), saved to %s", toolName, len(result), a.MaxResultChars, fpath)

	// Try to auto-summarise via Haiku (skip if disabled or result exceeds MaxSummaryChars)
	if a.AutoSummarise && client != nil && len(a.ModelAliases) > 0 && (a.MaxSummaryChars <= 0 || len(result) <= a.MaxSummaryChars) {
		if summary := a.summariseToolResult(ctx, client, sessionKey, toolName, result, messages, fpath); summary != "" {
			return summary
		}
	}

	hint := guardHint(result, fpath)
	return fmt.Sprintf("Result too large (%d chars, limit %d). Full output saved to %s.\n%s", len(result), a.MaxResultChars, fpath, hint)
}

// summariseToolResult calls a cheap model to produce a summary of an oversized tool result.
// Returns the formatted summary string, or empty string on failure (caller falls back).
func (a *Agent) summariseToolResult(ctx context.Context, client provider.Client, sessionKey, toolName, result string, messages []anthropic.Message, savedPath string) string {
	// Pick cheap model based on which provider the client is.
	summaryAlias := "haiku"
	if a.isGeminiClient(client) {
		summaryAlias = "flash"
	} else if a.isOpenAIClient(client) {
		summaryAlias = "gpt4o"
	}
	model := summaryAlias
	if full, ok := a.ModelAliases[summaryAlias]; ok {
		model = full
	}
	// Strip endpoint prefix — aliases now include endpoint (e.g. "anthropic:claude-haiku-4-5")
	if i := strings.IndexByte(model, ':'); i > 0 {
		model = model[i+1:]
	}

	convContext := recentContext(messages, a.SummaryContextTurns, a.SummaryContextChars)

	var userText string
	if convContext != "" {
		userText = fmt.Sprintf("<context>\n%s\n</context>\n\n<tool_output tool=%q>\n%s\n</tool_output>\n\nSummarise this tool output. First give a general overview, then list the parts most relevant to the conversation context with exact quotes and their addresses (line numbers, section headers, JSON paths, or key names) so the reader knows exactly where to look for details.",
			convContext, toolName, result)
	} else {
		userText = fmt.Sprintf("<tool_output tool=%q>\n%s\n</tool_output>\n\nSummarise this tool output. First give a general overview, then list the key sections or data points with exact quotes and their addresses (line numbers, section headers, JSON paths, or key names) so the reader knows exactly where to look for details.",
			toolName, result)
	}

	req := &anthropic.MessageRequest{
		Model:     model,
		MaxTokens: 4096,
		System: []anthropic.SystemBlock{
			{Type: "text", Text: "You are a tool output summarisation assistant. Your job is to summarise oversized tool output so the reader gets useful visibility without the full content in context.\n\nYour summary must have two parts:\n1. **Overview**: A concise general summary of the content (what it is, how large, key structure).\n2. **Relevant details**: Exact quotes from the parts most relevant to the conversation context, each annotated with its address — line number, section header, JSON path, key name, or other locator. These addresses let the reader jump directly to the source if they need more detail.\n\nBe concise. Preserve exact values (numbers, names, paths, error messages) rather than paraphrasing them."},
		},
		Messages: []anthropic.Message{
			{Role: "user", Content: anthropic.TextContent(userText)},
		},
	}

	start := time.Now()
	resp, err := provider.Send(ctx, client, req, nil)
	if err != nil {
		a.logger().Warnf("session=%s auto-summary failed for %s: %v", sessionKey, toolName, err)
		return ""
	}

	duration := time.Since(start)
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	a.logger().Infof("auto-summary model=%s input=%d output=%d cost=$%.4f duration=%s",
		model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	summary := anthropic.TextOf(resp.Content)
	if summary == "" {
		return ""
	}

	return fmt.Sprintf("[Auto-summary by %s — full output (%d chars) saved to %s]\n\n%s", model, len(result), savedPath, summary)
}

// recentContext extracts text from the last N conversation turns,
// capped at maxChars. Skips tool_use and tool_result blocks.
func recentContext(messages []anthropic.Message, maxTurns, maxChars int) string {
	if maxTurns <= 0 || maxChars <= 0 || len(messages) == 0 {
		return ""
	}

	var parts []string
	total := 0
	turns := 0
	for i := len(messages) - 1; i >= 0 && turns < maxTurns; i-- {
		msg := messages[i]
		var text string
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				text = block.Text
				break
			}
		}
		if text == "" {
			continue
		}
		turns++
		remaining := maxChars - total
		if len(text) > remaining {
			text = text[:remaining]
		}
		parts = append(parts, fmt.Sprintf("[%s] %s", msg.Role, text))
		total += len(text)
		if total >= maxChars {
			break
		}
	}
	// Reverse to chronological order
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "\n")
}

// guardHint returns a contextual suggestion for how to extract data from a
// saved tool result file, based on content sniffing. Includes the file path
// in example commands so the agent can copy-paste.
func guardHint(content, path string) string {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) == 0 {
		return fmt.Sprintf("Use the `summary` tool to extract specific information from %s.", path)
	}
	// Check TOML before JSON — both can start with '[' but TOML sections start with [letter
	if looksLikeTOML(trimmed) {
		return fmt.Sprintf("Use `yq` to query, e.g. `yq '.section.key' %s`.", path)
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return fmt.Sprintf("Use `jq` to query, e.g. `jq 'keys' %s` or `jq '.items[:3]' %s`.", path, path)
	}
	if trimmed[0] == '#' {
		return fmt.Sprintf("Use `mdq` to query sections, e.g. `mdq '# Section' %s`.", path)
	}
	if detectContentExtension(content) == ".xml" {
		return fmt.Sprintf("Use `yq` to query, e.g. `yq -p xml '.' %s`.", path)
	}
	if looksLikeYAML(trimmed) {
		return fmt.Sprintf("Use `yq` to query, e.g. `yq '.key' %s`.", path)
	}
	return fmt.Sprintf("Use the `summary` tool to extract specific information from %s.", path)
}

// looksLikeTOML checks if content starts with TOML-like patterns (e.g. [section] or key = value).
func looksLikeTOML(trimmed string) bool {
	if len(trimmed) == 0 {
		return false
	}
	// [section] at start of line — must start with a letter (not digit, quote, brace)
	if trimmed[0] == '[' && len(trimmed) > 1 && isLetter(trimmed[1]) {
		if idx := strings.IndexByte(trimmed, ']'); idx > 1 && idx < 80 {
			return true
		}
	}
	// key = value pattern on first line
	firstLine := trimmed
	if nl := strings.IndexByte(trimmed, '\n'); nl > 0 {
		firstLine = trimmed[:nl]
	}
	if strings.Contains(firstLine, " = ") {
		return true
	}
	return false
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// looksLikeYAML checks if content starts with YAML-like patterns (e.g. key: value or ---).
func looksLikeYAML(trimmed string) bool {
	if len(trimmed) == 0 {
		return false
	}
	if strings.HasPrefix(trimmed, "---") {
		return true
	}
	firstLine := trimmed
	if nl := strings.IndexByte(trimmed, '\n'); nl > 0 {
		firstLine = trimmed[:nl]
	}
	// key: value (but not URLs like http:)
	if idx := strings.Index(firstLine, ": "); idx > 0 && !strings.Contains(firstLine[:idx], "//") {
		return true
	}
	return false
}

// collectReminders returns due reminders formatted for injection into the user message.
// Reminders only surface on the default/main session to avoid leaking into branches.
// Returns empty string if no reminders are due or the store is nil.
func (a *Agent) collectReminders(sessionKey string) string {
	if a.DefaultSessionKey != nil {
		if dsk := a.DefaultSessionKey(); dsk != "" && dsk != sessionKey {
			return ""
		}
	}
	if a.Reminders == nil {
		return ""
	}

	reminders, err := a.Reminders.Due(a.AgentID)
	if err != nil {
		a.logger().Errorf("session=%s fetch reminders: %v", sessionKey, err)
		return ""
	}
	if len(reminders) == 0 {
		return ""
	}

	var block string
	block = "\n[reminders]"
	for _, r := range reminders {
		block += fmt.Sprintf("\n- %s (set %s, due: %s)", r.Text, r.DueTag, r.Created.Format("2006-01-02 15:04"))
	}

	// Auto-dismiss surfaced reminders
	if err := a.Reminders.DismissAll(a.AgentID); err != nil {
		a.logger().Errorf("session=%s dismiss reminders: %v", sessionKey, err)
	}

	return block
}

// turnLock returns a per-session mutex that serializes HandleMessage calls.
// This prevents concurrent turns on the same session from interleaving messages
// in the session file, which would invalidate Anthropic's prefix-matched prompt cache.
func (a *Agent) turnLock(sessionKey string) *sync.Mutex {
	a.turnLocksMu.Lock()
	defer a.turnLocksMu.Unlock()
	if a.turnLocks == nil {
		a.turnLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := a.turnLocks[sessionKey]
	if !ok {
		mu = &sync.Mutex{}
		a.turnLocks[sessionKey] = mu
	}
	return mu
}

// HandleMessage processes a text-only user message. Delegates to HandleMessageWithAttachments.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	return a.HandleMessageWithAttachments(ctx, sessionKey, userMessage, nil)
}

// HandleMessageWithAttachments processes a user message with optional image/document attachments.
func (a *Agent) HandleMessageWithAttachments(ctx context.Context, sessionKey string, userMessage string, images []Attachment) (string, error) {
	// Serialize turns on the same session. Without this, concurrent callers
	// (keepalive, tmux watch, scheduled wakes, exec auto-background) could
	// load the same session state, run concurrent turns, and interleave their
	// messages in the session file. This would break Anthropic's prefix-matched
	// prompt cache — any insertion in the middle of conversation history
	// invalidates all cached tokens after the insertion point.
	sessionLock := a.turnLock(sessionKey)
	waiterTrigger := TriggerFromContext(ctx)
	a.logger().Debugf("turn_lock_wait session=%s trigger=%s", sessionKey, waiterTrigger)
	lockStart := time.Now()
	sessionLock.Lock()
	lockDur := time.Since(lockStart)
	a.logTurnLockWait(sessionKey, lockDur, waiterTrigger)
	defer sessionLock.Unlock()

	atomic.AddInt32(&a.processing, 1)
	defer atomic.AddInt32(&a.processing, -1)

	td := &TurnDetail{
		SessionKey: sessionKey,
		Trigger:    TriggerFromContext(ctx),
		StartTime:  time.Now(),
	}
	turnID := a.registerTurn(td)
	defer a.unregisterTurn(turnID)

	// Check if context was cancelled while waiting for the turn lock
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	// Touch session activity for index tracking.
	if a.OnActivity != nil {
		a.OnActivity(sessionKey)
	}

	// Load existing messages
	messages, err := a.Sessions.LoadFull(sessionKey)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	// Repair interrupted tool calls (e.g. SIGTERM during tool execution).
	// If the last message is assistant with tool_use but no tool_result follows,
	// inject synthetic error results so the API accepts the message history.
	if repair := repairInterruptedToolCalls(messages); repair != nil {
		messages = append(messages, *repair)
		if err := a.Sessions.Append(sessionKey, *repair); err != nil {
			a.logger().Errorf("session=%s persist tool call repair: %v", sessionKey, err)
		} else {
			a.logger().Infof("repaired %d interrupted tool calls in %s", len(repair.Content), sessionKey)
		}
	}

	turnModel := a.SessionModel(sessionKey)
	turnClient := a.SessionClient(sessionKey)
	turnEffort := a.SessionEffort(sessionKey)
	turnThinking := a.SessionThinking(sessionKey)

	now := time.Now()
	sm := a.getSessionMeta(sessionKey)

	userMsg := a.prepareUserMessage(ctx, sessionKey, userMessage, turnModel, images)
	messages = append(messages, userMsg)

	// Track new messages to save. The defer flushes unsaved messages on
	// shutdown (e.g. SIGTERM during a tool call like "systemctl restart foci").
	// Normal exits set newMessages=nil after saving, so the defer is a no-op.
	var newMessages []anthropic.Message
	newMessages = append(newMessages, userMsg)
	defer func() {
		if len(newMessages) > 0 {
			if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
				a.logger().Errorf("session=%s flush in-flight messages: %v", sessionKey, err)
			} else {
				a.logger().Infof("flushed %d in-flight messages for %s", len(newMessages), sessionKey)
			}
		}
	}()

	system := a.buildSystemBlocks(sessionKey)
	useAutoCache := a.CacheStrategy == "auto"
	toolDefs := a.Tools.ToolDefs()
	if len(a.ServerTools) > 0 {
		toolDefs = append(toolDefs, a.ServerTools...)
	}

	maxLoops := a.MaxToolLoops
	if maxLoops <= 0 {
		maxLoops = 25 // default
	}
	maxOutput := a.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = 8192 // default
	}
	braindeadWarningThreshold := a.BraindeadWarningThreshold
	braindeadWarned := false
	var batchedText strings.Builder // accumulates intermediate text when BatchPartialAssistantMessages=true
	for i := 0; i < maxLoops; i++ {
		var reqMessages []anthropic.Message
		if useAutoCache {
			reqMessages = messages
		} else {
			reqMessages = withCacheBreakpoint(messages)
		}
		req := &anthropic.MessageRequest{
			Model:     turnModel,
			MaxTokens: maxOutput,
			System:    system,
			Messages:  reqMessages,
			Tools:     toolDefs,
		}
		if useAutoCache {
			req.CacheControl = anthropic.Ephemeral()
		}
		if turnEffort != "" {
			req.Output = &anthropic.OutputConfig{Effort: turnEffort}
		}
		if turnThinking == "adaptive" {
			req.Thinking = &anthropic.ThinkingConfig{Type: "adaptive"}
		}

		// Debug: log cache_control placement
		logCacheDebug(sessionKey, system, reqMessages, turnModel)

		a.logger().Debugf("api_request session=%s model=%s messages=%d tools=%d system_blocks=%d",
			sessionKey, turnModel, len(reqMessages), len(toolDefs), len(system))

		start := time.Now()
		a.logger().Debugf("api_call_start session=%s model=%s streaming=%v", sessionKey, turnModel, a.Streaming)

		var resp *anthropic.MessageResponse
		var err error

		// Use streaming if enabled; provider.Send auto-selects streaming
		// when the client supports it, falling back to SendMessage otherwise.
		var handler *provider.StreamHandler
		if a.Streaming {
			handler = &provider.StreamHandler{
				OnTextDelta: func(delta string) {
					notifyTextDeltaCtx(ctx, delta)
					signalActivityCtx(ctx) // keep typing indicator alive
				},
				OnThinkingDelta: func(delta string) {
					notifyThinkingDeltaCtx(ctx, delta)
				},
			}
		}
		resp, err = provider.Send(ctx, turnClient, req, handler)

		duration := time.Since(start)
		a.logger().Debugf("api_call_done session=%s duration=%s err=%v", sessionKey, duration, err)

		if err != nil {
			return "", a.classifyAPIError(ctx, err, sessionKey, duration)
		}

		// Check for cancellation after API call
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		cost := a.logAPIResponse(sessionKey, turnModel, start, duration, req, resp, len(reqMessages))
		a.processAPIResponse(sessionKey, sm, resp, cost, now, maxOutput)

		// Build assistant message from response
		assistantMsg := anthropic.Message{
			Role:    resp.Role,
			Content: resp.Content,
		}
		messages = append(messages, assistantMsg)
		newMessages = append(newMessages, assistantMsg)

		notifyResponseBlocks(ctx, resp.Content)

		// pause_turn: server paused a long-running turn — continue without client-side tool execution
		if resp.StopReason == "pause_turn" {
			continue
		}

		if resp.StopReason != "tool_use" {
			// Done — save all new messages and return text
			if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
				return "", fmt.Errorf("save session: %w", err)
			}
			newMessages = nil // saved — defer won't double-save

			// Update session metadata for next turn
			sm.lastMessageTime = now
			sm.prevCost = cost
			sm.prevInput = resp.Usage.InputTokens
			sm.prevOutput = resp.Usage.OutputTokens
			sm.prevCacheWrite = resp.Usage.CacheCreationInputTokens

			a.maybeCompact(ctx, turnClient, sessionKey, messages, system, &resp.Usage, sm)

			finalText := anthropic.TextOf(resp.Content)
			if a.BatchPartialAssistantMessages && batchedText.Len() > 0 {
				if finalText != "" {
					batchedText.WriteString(a.BatchPartialJoiner)
					batchedText.WriteString(finalText)
				}
				return batchedText.String(), nil
			}
			return finalText, nil
		}

		// Handle text in tool_use responses: either send immediately or accumulate for batch delivery.
		if intermediateText := anthropic.TextOf(resp.Content); intermediateText != "" {
			if a.BatchPartialAssistantMessages {
				if batchedText.Len() > 0 {
					batchedText.WriteString(a.BatchPartialJoiner)
				}
				batchedText.WriteString(intermediateText)
			} else {
				sendIntermediateCtx(ctx, intermediateText)
			}
		}

		// If this is the last allowed iteration, don't execute tools —
		// instead inject descriptive error results so the session JSONL
		// ends with a proper tool_use / tool_result pair.
		if i+1 >= maxLoops {
			var toolResults []anthropic.ContentBlock
			errMsg := fmt.Sprintf("Tool call not executed: max tool loop depth reached (limit: %d)", maxLoops)
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}
				toolResults = append(toolResults, anthropic.ToolResultBlock(block.ID, errMsg, true))
			}
			toolMsg := anthropic.Message{Role: "user", Content: toolResults}
			messages = append(messages, toolMsg)
			newMessages = append(newMessages, toolMsg)
			break
		}

		// Execute tool calls. server_tool_use blocks are skipped — already
		// executed by Anthropic's servers.
		toolResults, err := a.executeToolCalls(ctx, td, turnClient, sessionKey, resp.Content, messages)
		if err != nil {
			return "", err
		}

		// Braindead warning detection: fold warning into tool results to avoid
		// a separate user message that breaks tool_use/tool_result adjacency.
		if a.BraindeadWarningEnable && !braindeadWarned && braindeadWarningThreshold > 0 && i+1 >= braindeadWarningThreshold {
			prompt := a.BraindeadWarningPrompt
			if prompt == "" {
				prompt = defaultBraindeadWarningPrompt
			}
			toolResults = append(toolResults, anthropic.ContentBlock{Type: "text", Text: "[system] " + prompt})
			braindeadWarned = true
			a.logger().Infof("braindead warning injected at loop %d for session %s", i+1, sessionKey)
		}

		// Append tool results as user message
		toolMsg := anthropic.Message{
			Role:    "user",
			Content: toolResults,
		}
		messages = append(messages, toolMsg)
		newMessages = append(newMessages, toolMsg)
	}

	// Max loops reached — save what we have and return last text
	sessionFile := sessionKey
	if p, err := a.Sessions.SessionPath(sessionKey); err == nil {
		sessionFile = p
	}
	a.logger().Warnf("max tool call depth reached for session %s", sessionFile)
	if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	newMessages = nil // saved — defer won't double-save
	return "Max tool call depth reached.", nil
}

// classifyAPIError maps API errors to user-friendly messages, notifying
// rate limit and server error callbacks as appropriate.
func (a *Agent) classifyAPIError(ctx context.Context, err error, sessionKey string, duration time.Duration) error {
	if ctx.Err() != nil {
		a.logger().Debugf("api_call_ctx_cancelled session=%s ctx_err=%v duration=%s", sessionKey, ctx.Err(), duration)
		return ctx.Err()
	}
	var apiErr *anthropic.APIError
	if errors.As(err, &apiErr) && apiErr.IsRateLimit() {
		if a.RateLimitFunc != nil {
			a.RateLimitFunc(apiErr.RetryAfterSeconds())
		}
		return fmt.Errorf("rate limited — mana exhausted")
	}
	if errors.As(err, &apiErr) && apiErr.IsOverloaded() {
		return fmt.Errorf("Anthropic API is overloaded — try again shortly")
	}
	if errors.As(err, &apiErr) && apiErr.IsRetryable() {
		a.logger().Debugf("server error detail: %s", err)
		if a.RateLimitFunc != nil {
			a.RateLimitFunc(0)
		}
		return fmt.Errorf("anthropic API is temporarily unavailable, try again in a few minutes")
	}
	return fmt.Errorf("send message: %w", err)
}

// logAPIResponse logs usage, cost, and optionally the full request/response payload.
func (a *Agent) logAPIResponse(sessionKey, model string, start time.Time, duration time.Duration, req *anthropic.MessageRequest, resp *anthropic.MessageResponse, msgCount int) float64 {
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	a.logger().Infof("stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f",
		resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens, cost)

	sessionFile := ""
	if a.Sessions != nil {
		if p, err := a.Sessions.SessionPath(sessionKey); err == nil {
			sessionFile = p
		}
	}
	log.API(log.APIEntry{
		Timestamp:   start.UTC(),
		Session:     sessionKey,
		Model:       model,
		Input:       resp.Usage.InputTokens,
		Output:      resp.Usage.OutputTokens,
		CacheRead:   resp.Usage.CacheReadInputTokens,
		CacheWrite:  resp.Usage.CacheCreationInputTokens,
		CostUSD:     cost,
		DurationMS:  duration.Milliseconds(),
		StopReason:  resp.StopReason,
		CallType:    "conversation",
		SessionFile: sessionFile,
		SessionLine: msgCount + 2, // +2 for the user message and assistant response being appended
	})

	if log.PayloadEnabled() {
		reqJSON, _ := json.Marshal(req)
		respJSON, _ := json.Marshal(resp)
		log.Payload(log.PayloadEntry{
			Timestamp:  start.UTC(),
			Session:    sessionKey,
			Model:      model,
			Request:    reqJSON,
			Response:   respJSON,
			DurationMS: duration.Milliseconds(),
		})
	}

	return cost
}

// buildSystemBlocks assembles the system prompt blocks from bootstrap,
// environment, and extra blocks, applying the appropriate cache strategy.
// Results are cached per-session so that a compaction on one session
// (which calls Bootstrap.Reload) does not bust the cache for other sessions.
func (a *Agent) buildSystemBlocks(sessionKey string) []anthropic.SystemBlock {
	sm := a.getSessionMeta(sessionKey)
	if sm.systemBlocks != nil {
		return sm.systemBlocks
	}

	system := a.Bootstrap.SystemBlocks()
	if a.EnvironmentBlock != "" {
		envBlock := anthropic.SystemBlock{Type: "text", Text: a.EnvironmentBlock}
		system = append([]anthropic.SystemBlock{envBlock}, system...)
	}

	var result []anthropic.SystemBlock

	if a.CacheStrategy == "auto" {
		// Auto caching: strip intermediate cache_control, keep an explicit
		// breakpoint on the last block so tools+system are cached as a stable
		// prefix that survives message changes (e.g. compaction).
		if len(a.ExtraSystemBlocks) > 0 {
			system = append(system, a.ExtraSystemBlocks...)
		}
		clean := make([]anthropic.SystemBlock, len(system))
		copy(clean, system)
		for i := range clean {
			clean[i].CacheControl = nil
		}
		if len(clean) > 0 {
			clean[len(clean)-1].CacheControl = anthropic.Ephemeral()
		}
		result = clean
	} else if len(a.ExtraSystemBlocks) > 0 && len(system) > 0 {
		// Explicit caching: insert extra blocks before the last block
		// (which has cache_control).
		combined := make([]anthropic.SystemBlock, 0, len(system)+len(a.ExtraSystemBlocks))
		combined = append(combined, system[:len(system)-1]...)
		combined = append(combined, a.ExtraSystemBlocks...)
		combined = append(combined, system[len(system)-1])
		result = combined
	} else {
		result = system
	}

	sm.systemBlocks = result
	return result
}

// maybeCompact checks whether context compaction is needed and performs it.
func (a *Agent) maybeCompact(ctx context.Context, client provider.Client, sessionKey string, messages []anthropic.Message, system []anthropic.SystemBlock, usage *anthropic.Usage, sm *sessionMeta) {
	if a.Compactor == nil || a.AsyncNotifier.HasPending(sessionKey) || !a.Compactor.ShouldCompact(messages, usage) {
		return
	}
	if a.SessionNoCompact(sessionKey) {
		totalTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		limit := compaction.ContextLimit(a.Model)
		percent := int(float64(totalTokens) / float64(limit) * 100)
		a.logger().Infof("context at %d%% capacity for no_compact session", percent)
		return
	}
	oldCount := len(messages)
	if a.CompactionNotifyFunc != nil {
		a.CompactionNotifyFunc(sessionKey, "⏳ Compacting context...")
	}
	summaryPrompt := prompts.ResolvePrompt(a.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
	handoffMsg := a.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), a.PromptSearchDirs...)
	}
	if summary, err := a.Compactor.Compact(ctx, client, sessionKey, system, summaryPrompt, handoffMsg, false); err != nil {
		a.logger().Errorf("session=%s compaction failed: %v", sessionKey, err)
	} else {
		if a.CompactionNotifyFunc != nil {
			a.CompactionNotifyFunc(sessionKey, fmt.Sprintf("✅ Context compacted — %d messages summarised.", oldCount))
		}
		if a.CompactionDebugFunc != nil && summary != "" {
			a.CompactionDebugFunc(sessionKey, summary)
		}
	}
	// Reload system prompt — compaction may have changed memory files.
	// Only invalidate THIS session's cached system blocks so other sessions
	// keep their byte-identical prompts and don't suffer cache busts.
	a.Bootstrap.Reload()
	sm.systemBlocks = nil
	// Reset cache baseline — next request will have a different prefix
	sm.prevCacheRead = 0
}

// withCacheBreakpoint returns a deep copy of messages with cache_control set
// on exactly one place: the last content block of the second-to-last message.
// All other cache_control markers are stripped. This ensures exactly 1 message
// breakpoint per API call (plus the system prompt breakpoint = 2 total).
//
// Deep copy is critical: the originals are saved to session history and must
// never have cache_control persisted, or it accumulates across turns and
// mutates the prefix (causing cache misses).
func withCacheBreakpoint(messages []anthropic.Message) []anthropic.Message {
	// Deep copy all messages, stripping any existing cache_control
	result := make([]anthropic.Message, len(messages))
	for i, msg := range messages {
		content := make([]anthropic.ContentBlock, len(msg.Content))
		copy(content, msg.Content)
		for j := range content {
			content[j].CacheControl = nil
		}
		result[i] = anthropic.Message{Role: msg.Role, Content: content}
	}

	// Add the one breakpoint to second-to-last message
	if len(result) >= 2 {
		idx := len(result) - 2
		if len(result[idx].Content) > 0 {
			result[idx].Content[len(result[idx].Content)-1].CacheControl = anthropic.Ephemeral()
		}
	}

	return result
}

// logCacheDebug logs cache_control placement and warns about minimum token thresholds.
func logCacheDebug(sessionKey string, system []anthropic.SystemBlock, messages []anthropic.Message, model string) {
	// Estimate tokens: ~4 chars per token (rough heuristic)
	const charsPerToken = 4

	var systemChars int
	var systemCacheIdx = -1
	for i, block := range system {
		systemChars += len(block.Text)
		if block.CacheControl != nil {
			systemCacheIdx = i
		}
	}
	systemTokensEst := systemChars / charsPerToken

	var msgCacheIdx = -1
	for i, msg := range messages {
		for _, block := range msg.Content {
			if block.CacheControl != nil {
				msgCacheIdx = i
				break
			}
		}
	}

	log.Debugf("agent", "cache: system=%d blocks, ~%d tokens, breakpoint=%d; messages=%d, breakpoint=%d",
		len(system), systemTokensEst, systemCacheIdx, len(messages), msgCacheIdx)

	// Warn about minimum token thresholds
	minTokens := 2048 // Haiku default
	if model == "claude-sonnet-4-5" || model == "claude-opus-4-6" {
		minTokens = 1024
	}

	if len(system) > 0 && systemTokensEst < minTokens {
		log.Warnf("agent", "session=%s system prompt ~%d tokens is below %s minimum of %d for caching — cache will not activate",
			sessionKey, systemTokensEst, model, minTokens)
	}
}

// repairInterruptedToolCalls checks if the last message in the history is an
// assistant message with tool_use blocks that have no following tool_result.
// This happens when SIGTERM kills the process during tool execution — the defer
// flushes the assistant message but no tool_result was ever created.
// Returns a synthetic tool_result message to append, or nil if no repair needed.
func repairInterruptedToolCalls(messages []anthropic.Message) *anthropic.Message {
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" {
		return nil
	}

	var toolUseIDs []string
	for _, block := range last.Content {
		if block.Type == "tool_use" {
			toolUseIDs = append(toolUseIDs, block.ID)
		}
	}
	if len(toolUseIDs) == 0 {
		return nil
	}

	var results []anthropic.ContentBlock
	for _, id := range toolUseIDs {
		results = append(results, anthropic.ToolResultBlock(id, "Tool call interrupted by service restart", true))
	}
	return &anthropic.Message{Role: "user", Content: results}
}

// summarizeServerToolResult extracts a brief text summary from a server tool result block.
// Server tool result blocks (web_search_tool_result, web_fetch_tool_result) contain
// structured data in their Raw JSON. We extract a human-readable snippet for observers.
func summarizeServerToolResult(block anthropic.ContentBlock) string {
	// Try to extract content from the raw JSON
	if len(block.Raw) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(block.Raw, &raw); err == nil {
			// web_search_tool_result has a "content" array with search results
			if content, ok := raw["content"]; ok {
				var items []json.RawMessage
				if json.Unmarshal(content, &items) == nil && len(items) > 0 {
					return fmt.Sprintf("%d results", len(items))
				}
			}
		}
	}
	return block.Type
}

// isGeminiClient returns true if the given client is a gemini client from the registry.
func (a *Agent) isGeminiClient(c provider.Client) bool {
	if a.PeekClient == nil {
		return false
	}
	if gc := a.PeekClient("gemini", "gemini"); gc != nil && c == gc {
		return true
	}
	return false
}

// isOpenAIClient returns true if the given client is an openai client from the registry.
func (a *Agent) isOpenAIClient(c provider.Client) bool {
	if a.PeekClient == nil {
		return false
	}
	if oc := a.PeekClient("openai", "openai"); oc != nil && c == oc {
		return true
	}
	return false
}

// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage anthropic.Usage
}
