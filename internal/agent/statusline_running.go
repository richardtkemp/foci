package agent

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/display"
	"foci/internal/tools"
)

// The [running] statusline line (#2127): what the session has running in the
// background, so the agent need not check before a deploy or restart that would
// kill it. It reads the same tracking the app's running-subagents list shows:
// the backend's SubagentTracker (Agent-tool subagents and background shell
// commands) and the spawn tool's SpawnTracker (#2139).

const (
	runningMaxItems   = 5  // items listed per field; the rest are counted
	runningMaxDescLen = 40 // display columns of each item's description
)

// runningItem is one listed background job: its label and start time.
type runningItem struct {
	desc    string
	started time.Time
}

// spawnTracker returns the tracker the spawn tool registers with.
func (a *Agent) spawnTracker() *tools.SpawnTracker {
	if a.Spawns != nil {
		return a.Spawns
	}
	return tools.Spawns
}

// backendRunning returns the session's live backend's running list, or nil.
func (a *Agent) backendRunning(sessionKey string) []delegator.RunningSubagent {
	if a == nil || a.DelegatedManager == nil {
		return nil
	}
	return a.DelegatedManager.RunningSubagents(sessionKey)
}

// statusSubagents renders "subagents: N (desc 3m0s; …)" for the session's
// running Agent-tool subagents, or "". Backs the {subagents} field.
func (a *Agent) statusSubagents(sessionKey string) string {
	var items []runningItem
	for _, r := range a.backendRunning(sessionKey) {
		if r.Kind != delegator.SubagentKindCommand {
			items = append(items, runningItem{r.Description, r.Started})
		}
	}
	return formatRunning("subagents", items)
}

// statusBgCommands renders "bg_commands: N (…)" for the session's running
// background shell commands, or "". Backs the {bg_commands} field.
func (a *Agent) statusBgCommands(sessionKey string) string {
	var items []runningItem
	for _, r := range a.backendRunning(sessionKey) {
		if r.Kind == delegator.SubagentKindCommand {
			desc := r.Description
			if desc == "" {
				desc = r.Command
			}
			items = append(items, runningItem{desc, r.Started})
		}
	}
	return formatRunning("bg_commands", items)
}

// statusSpawns renders "spawns: N (…)" for the background spawns this session
// started, or "". Backs the {spawns} field.
func (a *Agent) statusSpawns(sessionKey string) string {
	if a == nil {
		return ""
	}
	var items []runningItem
	for _, s := range a.spawnTracker().Running(sessionKey) {
		items = append(items, runningItem{s.Description, s.Started})
	}
	return formatRunning("spawns", items)
}

// statusRunning joins the non-empty running fields. Backs {running}.
func (a *Agent) statusRunning(sessionKey string) string {
	var parts []string
	for _, s := range []string{a.statusSubagents(sessionKey), a.statusBgCommands(sessionKey), a.statusSpawns(sessionKey)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " | ")
}

// formatRunning renders "<label>: N (desc age; …)", or "" for no items.
func formatRunning(label string, items []runningItem) string {
	if len(items) == 0 {
		return ""
	}
	var shown []string
	for i, it := range items {
		if i == runningMaxItems {
			shown = append(shown, fmt.Sprintf("+%d more", len(items)-runningMaxItems))
			break
		}
		desc := display.Truncate(strings.Join(strings.Fields(it.desc), " "), runningMaxDescLen)
		if !it.started.IsZero() {
			desc = strings.TrimSpace(desc + " " + display.FormatDuration(time.Since(it.started).Truncate(time.Second)))
		}
		if desc != "" {
			shown = append(shown, desc)
		}
	}
	if len(shown) == 0 {
		return fmt.Sprintf("%s: %d", label, len(items))
	}
	return fmt.Sprintf("%s: %d (%s)", label, len(items), strings.Join(shown, "; "))
}
