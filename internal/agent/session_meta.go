package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/modelinfo"
	"foci/internal/provider"
	"foci/internal/timeutil"
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
	permissionMode  string                 // per-session CC permission mode (empty = ccstream default "default")
	client          provider.Client        // per-session client override (nil = use a.Client)
	usageClient     mana.UsageClient       // per-session usage client (nil may be intentional for non-Anthropic endpoints)
	usageClientSet  bool                   // true if usageClient was explicitly set (distinguishes nil-from-set vs nil-from-default)
	modelUserSet    bool                   // true if model was explicitly set by user (prevents backend clobber)
	contextLimit    int                    // override from backend get_context_usage; 0 = use model default
	noCompact       bool                   // per-session no_compact flag (sticky across async operations)
	systemBlocks    []provider.SystemBlock // per-session system prompt snapshot (nil = rebuild from bootstrap)
	apiSeqNum       int                    // per-session incrementing counter for payload log entries

	// Display overrides (empty = use agent/config default)
	showToolCalls    string // "off"/"preview"/"full"
	displayShowThink string // "off"/"compact"/"true" — distinct from API thinking mode
	streamOutput     string // "true"/"false"
	displayWidth     string // numeric string (e.g. "80")
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
		prefix:       "effort",
		getter:       func(sm *sessionMeta) string { return sm.effort },
		setter:       func(sm *sessionMeta, v string) { sm.effort = v },
		agentDefault: nil,
	}
	settingThinking = sessionStringSetting{
		prefix:       "thinking",
		getter:       func(sm *sessionMeta) string { return sm.thinking },
		setter:       func(sm *sessionMeta, v string) { sm.thinking = v },
		agentDefault: nil,
	}
	settingSpeed = sessionStringSetting{
		prefix:       "speed",
		getter:       func(sm *sessionMeta) string { return sm.speed },
		setter:       func(sm *sessionMeta, v string) { sm.speed = v },
		agentDefault: nil,
	}
	settingModel = sessionStringSetting{
		prefix:       "model",
		getter:       func(sm *sessionMeta) string { return sm.model },
		setter:       func(sm *sessionMeta, v string) { sm.model = v },
		agentDefault: func(a *Agent) string { return a.Model },
	}
	settingModelEndpoint = sessionStringSetting{
		prefix: "model_endpoint",
		getter: func(sm *sessionMeta) string { return sm.modelEndpoint },
		setter: func(sm *sessionMeta, v string) { sm.modelEndpoint = v },
	}
	settingModelFormat = sessionStringSetting{
		prefix:       "model_format",
		getter:       func(sm *sessionMeta) string { return sm.modelFormat },
		setter:       func(sm *sessionMeta, v string) { sm.modelFormat = v },
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
	settingPermissionMode = sessionStringSetting{
		prefix: "permission_mode",
		getter: func(sm *sessionMeta) string { return sm.permissionMode },
		setter: func(sm *sessionMeta, v string) { sm.permissionMode = v },
		// No agentDefault — CC's intrinsic default is "default"; we
		// surface "" as that in the UI rather than persisting it.
	}
)

// allSessionStringSettings lists every string-based session setting.
// Used by RestoreSessionOverrides and RotateSession to iterate without hardcoded prefix lists.
var allSessionStringSettings = []sessionStringSetting{
	settingEffort, settingThinking, settingSpeed,
	settingModel, settingModelEndpoint, settingModelFormat,
	settingShowToolCalls, settingDisplayShowThinking,
	settingStreamOutput, settingDisplayWidth,
	settingPermissionMode,
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
func (a *Agent) SessionEffort(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingEffort)
}

// SetSessionEffort sets the per-session effort override and persists it.
func (a *Agent) SetSessionEffort(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingEffort)
}

// SessionThinking returns the effective thinking mode for the session.
func (a *Agent) SessionThinking(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingThinking)
}

// SetSessionThinking sets the per-session thinking override and persists it.
func (a *Agent) SetSessionThinking(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingThinking)
}

// SessionSpeed returns the effective speed mode for the session.
func (a *Agent) SessionSpeed(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingSpeed)
}

// SetSessionSpeed sets the per-session speed override and persists it.
func (a *Agent) SetSessionSpeed(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingSpeed)
}

// SessionModel returns the effective model for the session.
func (a *Agent) SessionModel(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingModel)
}

// SessionContextLimit returns the context window size for the session's model.
// Checks backend-reported context limit first (from get_context_usage),
// then ModelMetaFn (config-defined), falls back to modelinfo registry.
func (a *Agent) SessionContextLimit(sessionKey string) int {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	override := sm.contextLimit
	a.metaMu.Unlock()
	if override > 0 {
		return override
	}
	model := a.SessionModel(sessionKey)
	if a.ModelMetaFn != nil {
		if meta := a.ModelMetaFn(model); meta.ContextWindow > 0 {
			return meta.ContextWindow
		}
	}
	return modelinfo.ContextWindow(model)
}

// SetSessionModel sets the per-session model, endpoint, format, and client override and persists it.
// client may be nil to fall back to the agent's default client.
func (a *Agent) SetSessionModel(sessionKey, value, endpoint, format string, client provider.Client) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	sm.model = value
	sm.modelEndpoint = endpoint
	sm.modelFormat = format
	sm.client = client
	// Update usage client for new endpoint (may be nil for non-Anthropic endpoints)
	if a.UsageClientProvider != nil {
		sm.usageClient = a.UsageClientProvider.GetUsageClient(endpoint)
		sm.usageClientSet = true
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

// SetModel is the high-level orchestrator for /model. It updates foci's
// session metadata AND tells the delegated backend (if any) to switch models.
// rawModel is the user's input (e.g. "opus", "sonnet") — passed verbatim to
// the backend since CC accepts bare model names.
func (a *Agent) SetModel(ctx context.Context, sessionKey string, model, endpoint, format string, client provider.Client, rawModel string) error {
	// Always update foci's own tracking.
	a.SetSessionModel(sessionKey, model, endpoint, format, client)

	// Only arm modelUserSet if a delegated turn is currently in flight.
	// The flag guards against that in-flight turn's stale FinalModel clobbering
	// the user's fresh /model choice. If no turn is in flight there is nothing
	// to protect against, so skip the flag entirely — the next turn's FinalModel
	// will resolve the alias immediately.
	sm := a.getSessionMeta(sessionKey)
	if a.DelegatedManager != nil {
		if mb, ok := a.DelegatedManager.getManaged(sessionKey); ok && mb.be.IsTurnInFlight() {
			a.metaMu.Lock()
			sm.modelUserSet = true
			a.metaMu.Unlock()
		}
	}

	// Tell the backend, if one exists and supports control requests.
	handled, err := a.SendBackendControl(ctx, sessionKey, &delegator.SetModelRequest{Model: rawModel})
	if err != nil {
		log.Warnf("agent", "session=%s backend set_model failed: %v", sessionKey, err)
		return fmt.Errorf("backend model switch failed: %w", err)
	}
	if handled {
		log.Infof("agent", "session=%s model switched via backend to %q", sessionKey, rawModel)
	}

	// Query context usage to get the real context window size and resolved
	// model name. This is zero-cost (no API call) and resolves aliases
	// immediately instead of waiting for the next turn's FinalModel.
	a.refreshContextFromBackend(ctx, sessionKey)

	return nil
}

// SessionPermissionMode returns the effective CC permission mode for the
// session. Returns "" if never set — callers should display "default" in
// that case (CC's intrinsic baseline).
func (a *Agent) SessionPermissionMode(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingPermissionMode)
}

// SetSessionPermissionMode sets the per-session permission mode override
// and persists it. Optimistic: caller is responsible for actually pushing
// the change to the backend via SetPermissionMode.
func (a *Agent) SetSessionPermissionMode(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingPermissionMode)
}

// SetPermissionMode is the high-level orchestrator for /mode. It tells the
// delegated backend to switch permission mode (fire-and-forget — CC's
// control_response is logged but not awaited) and optimistically updates
// foci's session metadata. No persistence across CC restarts: if CC
// restarts, the mode reverts to the --permission-mode flag (currently
// "default"). Returns ErrModeUnsupported if the backend doesn't implement
// ControlSender (e.g. cctmux).
func (a *Agent) SetPermissionMode(ctx context.Context, sessionKey, mode string) error {
	// Tell the backend first so we can refuse early for unsupported backends.
	handled, err := a.SendBackendControl(ctx, sessionKey, &delegator.SetPermissionModeRequest{Mode: mode})
	if err != nil {
		return fmt.Errorf("backend set_permission_mode failed: %w", err)
	}
	if !handled {
		return ErrModeUnsupported
	}

	// Optimistic update — fire-and-forget means we don't wait for CC's
	// control_response to confirm. Bad mode values are validated by the
	// command layer before we get here.
	a.SetSessionPermissionMode(sessionKey, mode)
	log.Infof("agent", "session=%s permission mode switched to %q", sessionKey, mode)
	return nil
}

// ErrModeUnsupported is returned by SetPermissionMode when the session's
// backend doesn't implement runtime control requests (e.g. the legacy
// cctmux backend). The command layer surfaces this as a user-facing
// "mode switch requires ccstream backend" message.
var ErrModeUnsupported = fmt.Errorf("permission mode switching requires the ccstream backend")

// refreshContextFromBackend queries the backend's context usage and updates
// the session's context limit and model name. No-op if the backend doesn't
// implement ContextUsageQuerier.
func (a *Agent) refreshContextFromBackend(ctx context.Context, sessionKey string) {
	if a.DelegatedManager == nil {
		return
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return
	}
	cuq, ok := be.(delegator.ContextUsageQuerier)
	if !ok {
		return
	}

	// Use a short timeout — this is a convenience query, not critical path.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	usage, err := cuq.GetContextUsage(queryCtx)
	if err != nil {
		log.Warnf("agent", "session=%s get_context_usage failed: %v", sessionKey, err)
		return
	}

	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	if usage.MaxTokens > 0 {
		sm.contextLimit = usage.MaxTokens
	}
	// Resolve the model alias immediately (e.g. "sonnet" → "claude-sonnet-4-6").
	if usage.Model != "" {
		sm.model = usage.Model
	}
	a.metaMu.Unlock()

	log.Infof("agent", "session=%s context_usage: %d/%d tokens (%d%%), model=%s",
		sessionKey, usage.TotalTokens, usage.MaxTokens, usage.Percentage, usage.Model)
}

// SessionFormat returns the effective wire format for the session.
func (a *Agent) SessionFormat(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingModelFormat)
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

// SessionUsageClient returns the usage client for a session's active endpoint.
// When the session has been explicitly configured (via SetSessionModel or
// RestoreSessionOverrides), usageClientSet is true and we return the resolved
// client (nil for non-Anthropic endpoints — no mana tracking).
//
// When usageClientSet is false (session using agent defaults, never explicitly
// configured), we resolve from the session's effective endpoint via
// UsageClientProvider rather than blindly returning a.UsageClient. This
// prevents returning the agent's Anthropic usage client for sessions that are
// actually using a non-Anthropic endpoint. The resolved value is cached so
// subsequent calls don't re-resolve.
func (a *Agent) SessionUsageClient(sessionKey string) mana.UsageClient {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.usageClientSet {
		return sm.usageClient // may be nil for non-Anthropic endpoints
	}
	// Not explicitly set — resolve from the session's effective endpoint.
	if a.UsageClientProvider != nil {
		endpoint := sm.modelEndpoint
		if endpoint == "" {
			endpoint = a.Endpoint
		}
		sm.usageClient = a.UsageClientProvider.GetUsageClient(endpoint)
		sm.usageClientSet = true
		return sm.usageClient
	}
	return nil
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
func (a *Agent) SessionShowToolCalls(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingShowToolCalls)
}

// SetSessionShowToolCalls sets the per-session show_tool_calls override and persists it.
func (a *Agent) SetSessionShowToolCalls(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingShowToolCalls)
}

// SessionDisplayShowThinking returns the per-session display show_thinking override (empty = not overridden).
func (a *Agent) SessionDisplayShowThinking(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingDisplayShowThinking)
}

// SetSessionDisplayShowThinking sets the per-session display show_thinking override and persists it.
func (a *Agent) SetSessionDisplayShowThinking(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingDisplayShowThinking)
}

// SessionStreamOutput returns the per-session stream_output override (empty = not overridden).
func (a *Agent) SessionStreamOutput(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingStreamOutput)
}

// SetSessionStreamOutput sets the per-session stream_output override and persists it.
func (a *Agent) SetSessionStreamOutput(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingStreamOutput)
}

// SessionDisplayWidth returns the per-session display_width override (empty = not overridden).
func (a *Agent) SessionDisplayWidth(sessionKey string) string {
	return a.getStringSetting(sessionKey, settingDisplayWidth)
}

// SetSessionDisplayWidth sets the per-session display_width override and persists it.
func (a *Agent) SetSessionDisplayWidth(sessionKey, value string) {
	a.setStringSetting(sessionKey, value, settingDisplayWidth)
}

// ClearSessionDisplayOverrides removes all per-session display overrides.
func (a *Agent) ClearSessionDisplayOverrides(sessionKey string) {
	a.SetSessionShowToolCalls(sessionKey, "")
	a.SetSessionDisplayShowThinking(sessionKey, "")
	a.SetSessionStreamOutput(sessionKey, "")
	a.SetSessionDisplayWidth(sessionKey, "")
}

// SessionOverrides returns a map of prefix→value for all non-empty session overrides.
func (a *Agent) SessionOverrides(sessionKey string) map[string]string {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()

	overrides := make(map[string]string)
	for _, s := range allSessionStringSettings {
		if v := s.getter(sm); v != "" {
			overrides[s.prefix] = v
		}
	}
	if sm.noCompact {
		overrides["no_compact"] = "true"
	}
	return overrides
}

// ClearAllSessionOverrides removes all per-session overrides (string settings + no_compact).
func (a *Agent) ClearAllSessionOverrides(sessionKey string) {
	for _, s := range allSessionStringSettings {
		a.setStringSetting(sessionKey, "", s)
	}
	a.SetSessionNoCompact(sessionKey, false)

	// Clear model client overrides
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) {
		sm.client = nil
		sm.usageClient = nil
		sm.usageClientSet = false
	})
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

		// Restore usage client for the endpoint (may be nil for non-Anthropic)
		if ep != "" && a.UsageClientProvider != nil {
			a.setMetaLocked(sessionKey, func(sm *sessionMeta) {
				sm.usageClient = a.UsageClientProvider.GetUsageClient(ep)
				sm.usageClientSet = true
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
		// Hydrate lastMessageTime from the session index so that
		// mostRecentSessionKey works correctly after a restart
		// (in-memory meta is empty, but the DB has last_activity_at).
		if a.SessionIndex != nil {
			if entry, err := a.SessionIndex.Get(key); err == nil && !entry.LastActivityAt.IsZero() {
				m.lastMessageTime = entry.LastActivityAt
			}
		}
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

	// Note: delegated backends are intentionally NOT migrated. The only flows
	// that rotate a key (reset, API compaction) either have no live backend or
	// want a fresh one, so carrying the running CC forward is never desired.

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
		return fmt.Sprintf("[meta] time=%s gap=%s model=%s via=%s%s", timeutil.Format(now), gap, model, platform, manaFlag)
	}

	return fmt.Sprintf("[meta] time=%s gap=%s model=%s via=%s prev_cost=%s prev_tokens=in:%d/out:%d/cR:%d/cW:%d%s",
		timeutil.Format(now), gap, model,
		platform,
		formatCost(sm.prevCost),
		sm.prevInput, sm.prevOutput, sm.prevCacheRead, sm.prevCacheWrite,
		manaFlag)
}

// formatCost formats a dollar cost, trimming unnecessary trailing zeros.
// $0.0000 → "$0", $1.2300 → "$1.23", $0.0016 → "$0.0016".
func formatCost(cost float64) string {
	if cost == 0 {
		return "$0"
	}
	s := fmt.Sprintf("$%.4f", cost)
	// Trim trailing zeros after the decimal point, but keep at least
	// two decimal places for non-zero values (e.g. $1.20 not $1.2).
	if i := len(s) - 1; s[i] == '0' {
		for i > 0 && s[i] == '0' {
			i--
		}
		if s[i] == '.' {
			i-- // drop the dot too if all decimals were zero
		}
		s = s[:i+1]
	}
	return s
}

// LastUserMessageTime returns the last user message time for the session.
// Used by keepalive proactive warning dispatch to determine user activity.
func (a *Agent) LastUserMessageTime(sessionKey string) time.Time {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.lastMessageTime
}
