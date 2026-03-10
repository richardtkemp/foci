package agent

import (
	"fmt"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
)

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

// buildSystemBlocks assembles the system prompt blocks from bootstrap,
// environment, and extra blocks, applying the appropriate cache strategy.
// Results are cached per-session so that a compaction on one session
// (which calls Bootstrap.Reload) does not bust the cache for other sessions.
func (a *Agent) buildSystemBlocks(sessionKey string) []provider.SystemBlock {
	sm := a.getSessionMeta(sessionKey)
	if sm.systemBlocks != nil {
		return sm.systemBlocks
	}

	system := a.Bootstrap.SystemBlocks()
	if a.EnvironmentBlock != "" {
		envBlock := provider.SystemBlock{Type: "text", Text: a.EnvironmentBlock}
		system = append([]provider.SystemBlock{envBlock}, system...)
	}

	var result []provider.SystemBlock

	if a.CacheStrategy == "auto" {
		// Auto caching: strip intermediate cache_control, keep an explicit
		// breakpoint on the last block so tools+system are cached as a stable
		// prefix that survives message changes (e.g. compaction).
		if len(a.ExtraSystemBlocks) > 0 {
			system = append(system, a.ExtraSystemBlocks...)
		}
		clean := make([]provider.SystemBlock, len(system))
		copy(clean, system)
		for i := range clean {
			clean[i].CacheControl = nil
		}
		if len(clean) > 0 {
			clean[len(clean)-1].CacheControl = provider.Ephemeral()
		}
		result = clean
	} else if len(a.ExtraSystemBlocks) > 0 && len(system) > 0 {
		// Explicit caching: insert extra blocks before the last block
		// (which has cache_control).
		combined := make([]provider.SystemBlock, 0, len(system)+len(a.ExtraSystemBlocks))
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

// withCacheBreakpoint returns a deep copy of messages with cache_control set
// on exactly one place: the last content block of the second-to-last message.
// All other cache_control markers are stripped. This ensures exactly 1 message
// breakpoint per API call (plus the system prompt breakpoint = 2 total).
//
// Deep copy is critical: the originals are saved to session history and must
// never have cache_control persisted, or it accumulates across turns and
// mutates the prefix (causing cache misses).
func withCacheBreakpoint(messages []provider.Message) []provider.Message {
	// Deep copy all messages, stripping any existing cache_control
	result := make([]provider.Message, len(messages))
	for i, msg := range messages {
		content := make([]provider.ContentBlock, len(msg.Content))
		copy(content, msg.Content)
		for j := range content {
			content[j].CacheControl = nil
		}
		result[i] = provider.Message{Role: msg.Role, Content: content}
	}

	// Add the one breakpoint to second-to-last message
	if len(result) >= 2 {
		idx := len(result) - 2
		if len(result[idx].Content) > 0 {
			result[idx].Content[len(result[idx].Content)-1].CacheControl = provider.Ephemeral()
		}
	}

	return result
}

// logCacheDebug logs cache_control placement and warns about minimum token thresholds.
func logCacheDebug(sessionKey string, system []provider.SystemBlock, messages []provider.Message, model string) {
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

	log.Debugf("agent", "cache: session=%s system=%d blocks, ~%d tokens, breakpoint=%d; messages=%d, breakpoint=%d",
		sessionKey, len(system), systemTokensEst, systemCacheIdx, len(messages), msgCacheIdx)

	// Warn about minimum token thresholds
	minTokens := 2048 // Haiku default
	bareModel := config.StripDeveloperPrefix(model)
	if bareModel == "claude-sonnet-4-5" || bareModel == "claude-opus-4-6" {
		minTokens = 1024
	}

	if len(system) > 0 && systemTokensEst < minTokens {
		log.Warnf("agent", "session=%s system prompt ~%d tokens is below %s minimum of %d for caching — cache will not activate",
			sessionKey, systemTokensEst, model, minTokens)
	}
}
