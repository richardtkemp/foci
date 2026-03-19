package agent

import (
	"fmt"
	"strings"

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
	block = "[reminders]"
	for _, r := range reminders {
		block += fmt.Sprintf("\n- %s (set %s, due: %s)", r.Text, r.DueTag, r.Created.Format("2006-01-02 15:04"))
	}

	// Auto-dismiss surfaced reminders
	if err := a.Reminders.DismissAll(a.AgentID); err != nil {
		a.logger().Errorf("session=%s dismiss reminders: %v", sessionKey, err)
	}

	return block
}

// collectStateDashboard builds a one-line state summary from all active stores.
// Returns empty string if all stores are empty/nil.
func (a *Agent) collectStateDashboard(sessionKey string) string {
	var parts []string

	// Tasks: "2/5" or "2/5 → first in_progress subject"
	if a.TaskListStore != nil {
		if tasks, err := a.TaskListStore.List(a.AgentID); err != nil {
			a.logger().Warnf("session=%s state dashboard: tasks: %v", sessionKey, err)
		} else if len(tasks) > 0 {
			completed, total := 0, len(tasks)
			var firstActive string
			for _, t := range tasks {
				if t.Status == "completed" {
					completed++
				} else if firstActive == "" && t.Status == "in_progress" {
					firstActive = t.Subject
				}
			}
			part := fmt.Sprintf("%d/%d", completed, total)
			if firstActive != "" {
				part += " → " + firstActive
			}
			parts = append(parts, "tasks: "+part)
		}
	}

	// Todos: "2 open (1 high)"
	if a.TodoStore != nil {
		if items, err := a.TodoStore.List(a.AgentID, "open", nil, "", "", false, 0); err != nil {
			a.logger().Warnf("session=%s state dashboard: todos: %v", sessionKey, err)
		} else if len(items) > 0 {
			highCount := 0
			for _, item := range items {
				if item.Priority == "high" {
					highCount++
				}
			}
			part := fmt.Sprintf("%d open", len(items))
			if highCount > 0 {
				part += fmt.Sprintf(" (%d high)", highCount)
			}
			parts = append(parts, "todos: "+part)
		}
	}

	// Scratchpad: "N entries"
	if a.ScratchpadStore != nil {
		if entries, err := a.ScratchpadStore.List(a.AgentID); err != nil {
			a.logger().Warnf("session=%s state dashboard: scratchpad: %v", sessionKey, err)
		} else if len(entries) > 0 {
			parts = append(parts, fmt.Sprintf("scratchpad: %d entries", len(entries)))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return "[state] " + strings.Join(parts, " | ")
}

// buildSystemBlocks assembles the system prompt blocks from bootstrap,
// environment, and extra blocks. Cache markers are applied later by the
// Anthropic translate layer — blocks returned here are clean.
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
		// Auto caching: extra blocks appended after bootstrap blocks.
		if len(a.ExtraSystemBlocks) > 0 {
			system = append(system, a.ExtraSystemBlocks...)
		}
		result = system
	} else if len(a.ExtraSystemBlocks) > 0 && len(system) > 0 {
		// Explicit caching: insert extra blocks before the last bootstrap
		// block so the translate layer's system breakpoint covers them.
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

// logCacheDebug logs system prompt size and warns about minimum token thresholds.
func logCacheDebug(sessionKey string, system []provider.SystemBlock, messages []provider.Message, model string) {
	// Estimate tokens: ~4 chars per token (rough heuristic)
	const charsPerToken = 4

	var systemChars int
	for _, block := range system {
		systemChars += len(block.Text)
	}
	systemTokensEst := systemChars / charsPerToken

	log.Debugf("agent", "cache: session=%s system=%d blocks, ~%d tokens; messages=%d",
		sessionKey, len(system), systemTokensEst, len(messages))

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
