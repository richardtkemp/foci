package agent

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/display"
	"foci/internal/provider"
)

// sessionMeta tracks per-session state for metadata injection.
type sessionMeta struct {
	lastMessageTime time.Time
	prevCost        float64
	prevInput       int
	prevOutput      int
	prevCacheRead   int
	prevCacheWrite  int
	effort          string                 // per-session effort override (empty = use agent default)
	thinking        string                 // per-session thinking override (empty = use agent default)
	speed           string                 // per-session speed override (empty = use agent default)
	model           string                 // per-session model override (empty = use agent default)
	modelEndpoint   string                 // per-session endpoint override (empty = use agent default)
	modelFormat     string                 // per-session format override (empty = use agent default)
	client          provider.Client        // per-session client override (nil = use a.Client)
	usageClient     provider.UsageClient   // per-session usage client (nil = use agent default)
	noCompact       bool                   // per-session no_compact flag (sticky across async operations)
	systemBlocks    []provider.SystemBlock // per-session system prompt snapshot (nil = rebuild from bootstrap)
	apiSeqNum       int                    // per-session incrementing counter for payload log entries

	// Display overrides (empty = use agent/config default)
	showToolCalls      string // "off"/"preview"/"full"
	displayShowThink   string // "off"/"compact"/"true" — distinct from API thinking mode
	streamOutput       string // "true"/"false"
	displayWidth       string // numeric string (e.g. "80")
}

// sessionStringSetting describes a string field in sessionMeta for table-driven access.
type sessionStringSetting struct {
	prefix       string                     // state store key prefix
	getter       func(*sessionMeta) string  // read field value
	setter       func(*sessionMeta, string) // write field value
	agentDefault func(*Agent) string        // agent-level default (nil = returns "")
}

var (
	settingEffort = sessionStringSetting{
		prefix: "effort",
		getter: func(sm *sessionMeta) string { return sm.effort },
		setter: func(sm *sessionMeta, v string) { sm.effort = v },
		agentDefault: func(a *Agent) string { return a.Effort },
	}
	settingThinking = sessionStringSetting{
		prefix: "thinking",
		getter: func(sm *sessionMeta) string { return sm.thinking },
		setter: func(sm *sessionMeta, v string) { sm.thinking = v },
		agentDefault: func(a *Agent) string { return a.Thinking },
	}
	settingSpeed = sessionStringSetting{
		prefix: "speed",
		getter: func(sm *sessionMeta) string { return sm.speed },
		setter: func(sm *sessionMeta, v string) { sm.speed = v },
		agentDefault: func(a *Agent) string { return a.Speed },
	}
	settingModel = sessionStringSetting{
		prefix: "model",
		getter: func(sm *sessionMeta) string { return sm.model },
		setter: func(sm *sessionMeta, v string) { sm.model = v },
		agentDefault: func(a *Agent) string { return a.Model },
	}
	settingModelEndpoint = sessionStringSetting{
		prefix: "model_endpoint",
		getter: func(sm *sessionMeta) string { return sm.modelEndpoint },
		setter: func(sm *sessionMeta, v string) { sm.modelEndpoint = v },
	}
	settingModelFormat = sessionStringSetting{
		prefix: "model_format",
		getter: func(sm *sessionMeta) string { return sm.modelFormat },
		setter: func(sm *sessionMeta, v string) { sm.modelFormat = v },
		agentDefault: func(a *Agent) string { return a.Format },
	}
	settingShowToolCalls = sessionStringSetting{
		prefix:       "show_tool_calls",
		getter:       func(sm *sessionMeta) string { return sm.showToolCalls },
		setter:       func(sm *sessionMeta, v string) { sm.showToolCalls = v },
		agentDefault: func(a *Agent) string { return a.ShowToolCalls },
	}
	settingDisplayShowThinking = sessionStringSetting{
		prefix: "display_show_thinking",
		getter: func(sm *sessionMeta) string { return sm.displayShowThink },
		setter: func(sm *sessionMeta, v string) { sm.displayShowThink = v },
	}
	settingStreamOutput = sessionStringSetting{
		prefix: "stream_output",
		getter: func(sm *sessionMeta) string { return sm.streamOutput },
		setter: func(sm *sessionMeta, v string) { sm.streamOutput = v },
	}
	settingDisplayWidth = sessionStringSetting{
		prefix: "display_width",
		getter: func(sm *sessionMeta) string { return sm.displayWidth },
		setter: func(sm *sessionMeta, v string) { sm.displayWidth = v },
	}
)

// allSessionStringSettings lists every string-based session setting.
// Used by RestoreSessionOverrides and RotateSession to iterate without hardcoded prefix lists.
var allSessionStringSettings = []sessionStringSetting{
	settingEffort, settingThinking, settingSpeed,
	settingModel, settingModelEndpoint, settingModelFormat,
	settingShowToolCalls, settingDisplayShowThinking,
	settingStreamOutput, settingDisplayWidth,
}

// sessionStringWithDefault returns a session-specific override
// or the agent-wide default if the override is empty.
func (a *Agent) sessionStringWithDefault(sessionKey string, getter func(*sessionMeta) string, defaultVal string) string {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	val := getter(sm)
	if val != "" {
		return val
	}
	return defaultVal
}

// setSessionString sets a per-session string override and persists it.
func (a *Agent) setSessionString(sessionKey, prefix, value string, setter func(*sessionMeta, string)) {
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) { setter(sm, value) })
	a.persistSessionString(sessionKey, prefix, value)
}

// getStringSetting returns the session-specific value for a setting, falling back to the agent default.
func (a *Agent) getStringSetting(sessionKey string, s sessionStringSetting) string {
	def := ""
	if s.agentDefault != nil {
		def = s.agentDefault(a)
	}
	return a.sessionStringWithDefault(sessionKey, s.getter, def)
}

// setStringSetting sets a per-session string setting and persists it.
func (a *Agent) setStringSetting(sessionKey, value string, s sessionStringSetting) {
	a.setSessionString(sessionKey, s.prefix, value, s.setter)
}

// SessionEffort returns the effective effort for the session.
func (a *Agent) SessionEffort(sessionKey string) string { return a.getStringSetting(sessionKey, settingEffort) }

// SetSessionEffort sets the per-session effort override and persists it.
func (a *Agent) SetSessionEffort(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingEffort) }

// SessionThinking returns the effective thinking mode for the session.
func (a *Agent) SessionThinking(sessionKey string) string { return a.getStringSetting(sessionKey, settingThinking) }

// SetSessionThinking sets the per-session thinking override and persists it.
func (a *Agent) SetSessionThinking(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingThinking) }

// SessionSpeed returns the effective speed mode for the session.
func (a *Agent) SessionSpeed(sessionKey string) string { return a.getStringSetting(sessionKey, settingSpeed) }

// SetSessionSpeed sets the per-session speed override and persists it.
func (a *Agent) SetSessionSpeed(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingSpeed) }

// SessionModel returns the effective model for the session.
func (a *Agent) SessionModel(sessionKey string) string { return a.getStringSetting(sessionKey, settingModel) }

// SetSessionModel sets the per-session model, endpoint, format, and client override and persists it.
// client may be nil to fall back to the agent's default client.
func (a *Agent) SetSessionModel(sessionKey, value, endpoint, format string, client provider.Client) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.model = value
	sm.modelEndpoint = endpoint
	sm.modelFormat = format
	sm.client = client
	// Update usage client for new endpoint
	if a.UsageClientProvider != nil {
		sm.usageClient = a.UsageClientProvider.GetUsageClient(endpoint)
	}
	a.metaMu.Unlock()

	if a.SessionIndex != nil {
		if value == "" {
			_ = a.SessionIndex.DeleteSessionMetadata(sessionKey, "model")
			_ = a.SessionIndex.DeleteSessionMetadata(sessionKey, "model_endpoint")
			_ = a.SessionIndex.DeleteSessionMetadata(sessionKey, "model_format")
		} else {
			if err := a.SessionIndex.SetSessionMetadata(sessionKey, "model", value); err != nil {
				a.logger().Errorf("session=%s persist model: %v", sessionKey, err)
			}
			if endpoint != "" {
				if err := a.SessionIndex.SetSessionMetadata(sessionKey, "model_endpoint", endpoint); err != nil {
					a.logger().Errorf("session=%s persist model_endpoint: %v", sessionKey, err)
				}
			}
			if format != "" {
				if err := a.SessionIndex.SetSessionMetadata(sessionKey, "model_format", format); err != nil {
					a.logger().Errorf("session=%s persist model_format: %v", sessionKey, err)
				}
			}
		}
	}
}

// SessionFormat returns the effective wire format for the session.
func (a *Agent) SessionFormat(sessionKey string) string { return a.getStringSetting(sessionKey, settingModelFormat) }

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

// SessionUsageClient returns the usage client for a session's active endpoint,
// falling back to the agent's default if not overridden.
func (a *Agent) SessionUsageClient(sessionKey string) provider.UsageClient {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.usageClient != nil {
		return sm.usageClient
	}
	return a.UsageClient
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
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) {
		sm.noCompact = value
	})
	val := ""
	if value {
		val = "true"
	}
	a.persistSessionString(sessionKey, "no_compact", val)
}

// SessionShowToolCalls returns the per-session show_tool_calls override (empty = not overridden).
func (a *Agent) SessionShowToolCalls(sessionKey string) string { return a.getStringSetting(sessionKey, settingShowToolCalls) }

// SetSessionShowToolCalls sets the per-session show_tool_calls override and persists it.
func (a *Agent) SetSessionShowToolCalls(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingShowToolCalls) }

// SessionDisplayShowThinking returns the per-session display show_thinking override (empty = not overridden).
func (a *Agent) SessionDisplayShowThinking(sessionKey string) string { return a.getStringSetting(sessionKey, settingDisplayShowThinking) }

// SetSessionDisplayShowThinking sets the per-session display show_thinking override and persists it.
func (a *Agent) SetSessionDisplayShowThinking(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingDisplayShowThinking) }

// SessionStreamOutput returns the per-session stream_output override (empty = not overridden).
func (a *Agent) SessionStreamOutput(sessionKey string) string { return a.getStringSetting(sessionKey, settingStreamOutput) }

// SetSessionStreamOutput sets the per-session stream_output override and persists it.
func (a *Agent) SetSessionStreamOutput(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingStreamOutput) }

// SessionDisplayWidth returns the per-session display_width override (empty = not overridden).
func (a *Agent) SessionDisplayWidth(sessionKey string) string { return a.getStringSetting(sessionKey, settingDisplayWidth) }

// SetSessionDisplayWidth sets the per-session display_width override and persists it.
func (a *Agent) SetSessionDisplayWidth(sessionKey, value string) { a.setStringSetting(sessionKey, value, settingDisplayWidth) }

// ClearSessionDisplayOverrides removes all per-session display overrides.
func (a *Agent) ClearSessionDisplayOverrides(sessionKey string) {
	a.SetSessionShowToolCalls(sessionKey, "")
	a.SetSessionDisplayShowThinking(sessionKey, "")
	a.SetSessionStreamOutput(sessionKey, "")
	a.SetSessionDisplayWidth(sessionKey, "")
}

// RestoreSessionOverrides loads per-session effort/thinking/model/no_compact from session metadata.
func (a *Agent) RestoreSessionOverrides(sessionKey string) {
	if a.SessionIndex == nil {
		return
	}
	var restored []string

	// Restore all string settings from session metadata.
	for _, s := range allSessionStringSettings {
		setter := s.setter
		val, err := a.SessionIndex.GetSessionMetadata(sessionKey, s.prefix)
		if err != nil {
			a.logger().Warnf("session=%s restore %s: %v", sessionKey, s.prefix, err)
			continue
		}
		if val != "" {
			a.setMetaLocked(sessionKey, func(sm *sessionMeta) { setter(sm, val) })
			restored = append(restored, s.prefix+"="+val)
		}
	}

	// Resolve client for restored model+endpoint+format.
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	model, ep, format := sm.model, sm.modelEndpoint, sm.modelFormat
	a.metaMu.Unlock()

	if model != "" {
		if ep != "" && format != "" && a.ClientProvider != nil {
			if c := a.ClientProvider.GetClient(ep, format); c != nil {
				a.setMetaLocked(sessionKey, func(sm *sessionMeta) { sm.client = c })
			}
		}
		if ep != "" && a.UsageClientProvider != nil {
			a.setMetaLocked(sessionKey, func(sm *sessionMeta) {
				sm.usageClient = a.UsageClientProvider.GetUsageClient(ep)
			})
		}
	}

	// Restore no_compact (bool, not string).
	val, err := a.SessionIndex.GetSessionMetadata(sessionKey, "no_compact")
	if err != nil {
		a.logger().Warnf("session=%s restore no_compact: %v", sessionKey, err)
	}
	if val != "" {
		a.setMetaLocked(sessionKey, func(sm *sessionMeta) { sm.noCompact = (val == "true") })
		restored = append(restored, "no_compact")
	}

	if len(restored) > 0 {
		a.logger().Infof("restored session overrides for %s: %s", sessionKey, strings.Join(restored, ", "))
	}
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

// setMetaLocked sets the session meta value while holding metaMu.
// Caller must NOT hold metaMu.
func (a *Agent) setMetaLocked(sessionKey string, setter func(*sessionMeta)) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	setter(sm)
	a.metaMu.Unlock()
}

// persistSessionString persists a string key-value pair to SessionIndex.
// Deletes the key if value is empty.
func (a *Agent) persistSessionString(sessionKey, prefix, value string) {
	if a.SessionIndex == nil {
		return
	}
	if value == "" {
		_ = a.SessionIndex.DeleteSessionMetadata(sessionKey, prefix)
	} else if err := a.SessionIndex.SetSessionMetadata(sessionKey, prefix, value); err != nil {
		a.logger().Errorf("session=%s persist %s: %v", sessionKey, prefix, err)
	}
}

// RotateSession migrates all per-session state from oldKey to newKey.
// This includes the meta map, state store keys, turn locks, and fires
// SessionKeyRotatedFunc callbacks.
func (a *Agent) RotateSession(oldKey, newKey string) {
	if oldKey == newKey || newKey == "" {
		return
	}

	// Migrate meta map
	a.metaMu.Lock()
	if a.meta != nil {
		if m, ok := a.meta[oldKey]; ok {
			a.meta[newKey] = m
			delete(a.meta, oldKey)
		}
	}
	a.metaMu.Unlock()

	// Migrate session metadata keys (single SQL UPDATE)
	if a.SessionIndex != nil {
		if err := a.SessionIndex.RenameSessionMetadata(oldKey, newKey); err != nil {
			a.logger().Errorf("rename session metadata %s → %s: %v", oldKey, newKey, err)
		}
	}

	// Migrate turn lock
	a.turnLocksMu.Lock()
	if a.turnLocks != nil {
		if mu, ok := a.turnLocks[oldKey]; ok {
			a.turnLocks[newKey] = mu
			delete(a.turnLocks, oldKey)
		}
	}
	a.turnLocksMu.Unlock()

	// Migrate async pending tracking so in-flight goroutines that captured
	// the old key resolve to the new key when they deliver results.
	a.AsyncNotifier.MigrateSession(oldKey, newKey)

	// Fire callbacks
	for _, fn := range a.SessionKeyRotatedFunc {
		fn(oldKey, newKey)
	}

	a.logger().Infof("session rotated %s → %s", oldKey, newKey)
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
		text := provider.TextOf(msgs[i].Content)
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
func buildMetaPrefix(now time.Time, model, platform string, mana string, manaGood bool, sm *sessionMeta) string {
	gap := "none"
	if !sm.lastMessageTime.IsZero() {
		gap = display.FormatDuration(now.Sub(sm.lastMessageTime))
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
		return fmt.Sprintf("[meta] time=%s gap=%s model=%s via=%s%s", now.UTC().Format(time.RFC3339), gap, model, platform, manaFlag)
	}

	return fmt.Sprintf("[meta] time=%s gap=%s model=%s via=%s prev_cost=$%.4f prev_tokens=in:%d/out:%d/cR:%d/cW:%d%s",
		now.UTC().Format(time.RFC3339), gap, model,
		platform,
		sm.prevCost,
		sm.prevInput, sm.prevOutput, sm.prevCacheRead, sm.prevCacheWrite,
		manaFlag)
}

// LastUserMessageTime returns the last user message time for the session.
// Used by keepalive proactive warning dispatch to determine user activity.
func (a *Agent) LastUserMessageTime(sessionKey string) time.Time {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.lastMessageTime
}
