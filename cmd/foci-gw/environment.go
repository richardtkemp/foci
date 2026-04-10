package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// checkSystemPromptSizes logs warnings if system prompt files exceed thresholds.
func checkSystemPromptSizes(bootstrap *workspace.Bootstrap, sess config.SessionsConfig, agentID string) {
	for _, w := range bootstrap.CheckSizes(sess.MaxSystemPromptFile, sess.MaxSystemPromptTotal) {
		log.Warnf(agentID, "%s", w)
	}
}

// checkSkillSizes logs warnings if any skill's SKILL.md exceeds maxResultChars.
func checkSkillSizes(registry *skills.Registry, maxResultChars int, agentID string) {
	for _, w := range registry.CheckSizes(maxResultChars) {
		log.Warnf(agentID, "%s", w)
	}
}

// countCrontabJobs counts the number of active cron jobs for the current user
func countCrontabJobs() int {
	cmd := exec.Command("sh", "-c", "crontab -l 2>/dev/null | grep -v '^#' | grep -v '^$' | wc -l")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0
	}
	return count
}

// writeEnvironmentCore writes the shared environment sections common to both
// API and delegated agents: identity, workspace, paths, message metadata,
// session structure, and visibility.
func writeEnvironmentCore(b *strings.Builder, acfg config.AgentConfig, configPath string, cfg *config.Config, rc *config.ResolvedAgentConfig, activePlatforms []string) {
	logDir := filepath.Dir(cfg.Logging.EventFile)

	b.WriteString("# Environment\n\n")
	b.WriteString("You are running on **foci**, an AI agent platform.\n\n")

	// Workspace
	b.WriteString("## Workspace\n")
	fmt.Fprintf(b, "- Workspace: %s\n", acfg.Workspace)
	fmt.Fprintf(b, "- Agent ID: %s\n", acfg.ID)
	b.WriteString("- Platform: foci (https://github.com/richardtkemp/foci)\n")
	if rc.Environment.DocsPath != "" {
		fmt.Fprintf(b, "- Platform docs: %s\n", rc.Environment.DocsPath)
	}
	if len(activePlatforms) > 0 {
		fmt.Fprintf(b, "- Messaging: %s\n", strings.Join(activePlatforms, ", "))
	}

	// Paths
	b.WriteString("\n## Paths\n")
	fmt.Fprintf(b, "- Config: %s\n", configPath)
	fmt.Fprintf(b, "- Logs: %s\n", logDir)

	// Message Metadata
	b.WriteString("\n## Message Metadata\n")
	b.WriteString("Every inbound message includes a `[meta]` header with:\n")
	b.WriteString("- **time** — timestamp with timezone offset\n")
	b.WriteString("- **gap** — time since last message\n")
	b.WriteString("- **model** — current model\n")
	b.WriteString("- **via** — which transport delivered this message: `telegram` (Telegram chat), `android` (Android app), `api` (HTTP /send endpoint — replies are already delivered), `cron` (system-initiated — keepalive, scheduled wake, etc.; replies are auto-delivered to the user's platform)\n")
	b.WriteString("- **prev_cost** — USD equivalent cost of previous turn\n")
	b.WriteString("- **prev_tokens** — token breakdown: in (new input), out (output), cR (cache read), cW (cache write)\n")
	b.WriteString("- **mana** — remaining API quota percentage, followed by 🟢 (above invest threshold — safe for heavy work) or 🔴 (low — conserve, avoid expensive operations)\n")

	// Session Structure
	b.WriteString("\n## Session Structure\n")
	b.WriteString("Your context is assembled from: this environment block, character files, a secrets list, and the conversation history.\n")
	sysFiles := acfg.System.SystemFiles
	if len(sysFiles) == 0 {
		sysFiles = workspace.DefaultFileOrder
	}
	b.WriteString("Character files (in order, relative to workspace): ")
	b.WriteString(strings.Join(sysFiles, ", "))
	b.WriteString("\n")
	b.WriteString("The human only sees the conversation — they cannot see your system prompt, character files, or this environment block. ")
	b.WriteString("Do not assume shared context when referencing system prompt content. If you need the human to understand something from your instructions, explain it in your own words.\n")
}

// writeVisibility appends the display visibility section.
func writeVisibility(b *strings.Builder, rc *config.ResolvedAgentConfig) {
	dc := rc.Display
	toolCalls := config.ToolCallDisplay(dc.ShowToolCalls)
	if toolCalls == "" {
		toolCalls = config.ToolCallOff
	}
	thinking := config.ShowThinking(dc.ShowThinking)
	if thinking == "" {
		thinking = config.ShowThinkingOff
	}
	var toolDesc, thinkDesc string
	switch toolCalls {
	case config.ToolCallOff:
		toolDesc = "Tool calls are hidden from the user — narrate important actions in your replies."
	case config.ToolCallPreview:
		toolDesc = "Tool calls are shown as brief previews (tool name only) — the user sees what tools you use but not the details."
	case config.ToolCallFull:
		toolDesc = "Tool calls are fully visible — the user can see your tool inputs and outputs."
	}
	switch thinking {
	case config.ShowThinkingOff:
		thinkDesc = "Your thinking is hidden from the user."
	case config.ShowThinkingCompact:
		thinkDesc = "Your thinking is available behind a toggle button."
	case config.ShowThinkingTrue:
		thinkDesc = "Your thinking is shown inline before each response."
	}
	if toolDesc != "" || thinkDesc != "" {
		b.WriteString("\n## Visibility\n")
		if toolDesc != "" {
			b.WriteString(toolDesc + "\n")
		}
		if thinkDesc != "" {
			b.WriteString(thinkDesc + "\n")
		}
	}
}

// buildEnvironmentAPI generates the environment block for API agents, which
// have direct access to foci's tool registry.
func buildEnvironmentAPI(acfg config.AgentConfig, configPath string, cfg *config.Config, rc *config.ResolvedAgentConfig, crontabCount int, activePlatforms []string) string {
	var b strings.Builder
	writeEnvironmentCore(&b, acfg, configPath, cfg, rc, activePlatforms)

	fmt.Fprintf(&b, "- You may schedule recurring tasks using crontab. You have %d jobs scheduled.\n", crontabCount)

	// Task List
	b.WriteString("\n## Task List\n")
	b.WriteString("The `task_list` tool tracks progress when working through a list of steps with the user (e.g., reviewing items, multi-step processes).\n")
	b.WriteString("Create tasks using the `tasks` JSON array (each item has `subject` and optional `description`), mark them `in_progress` as you work on each, and `completed` when done.\n")
	b.WriteString("The state dashboard auto-injects progress into every message (e.g., \"tasks: 2/12 → current task\"), and tasks survive compaction.\n")
	b.WriteString("Use it for collaborative step-tracking, not solo background work.\n")

	writeVisibility(&b, rc)
	return b.String()
}

// buildEnvironmentDelegated generates the environment block for delegated
// (CC backend) agents. These agents use Claude Code's built-in tools plus
// foci shell functions exposed via the exec bridge.
func buildEnvironmentDelegated(acfg config.AgentConfig, configPath string, cfg *config.Config, rc *config.ResolvedAgentConfig, activePlatforms []string, shellTools []tools.ExportedTool) string {
	var b strings.Builder
	writeEnvironmentCore(&b, acfg, configPath, cfg, rc, activePlatforms)

	// Backend description
	b.WriteString("\n## Backend\n")
	b.WriteString("You run inside **Claude Code** (CC) as a delegated backend. ")
	b.WriteString("Your primary tools are CC's built-in tools (Read, Write, Edit, Bash, Grep, Glob, Agent, WebFetch, WebSearch, etc.). ")
	b.WriteString("Foci bridges messaging platforms to CC and provides additional tools as shell functions.\n")

	// Shell tools
	if len(shellTools) > 0 {
		b.WriteString("\n## Foci Shell Tools\n")
		b.WriteString("The following foci tools are available as shell functions in your Bash environment. ")
		b.WriteString("Call them via the Bash tool (e.g., `foci_todo list --status open`).\n")
		b.WriteString("Run any command with `--help` or `-h` for usage details.\n\n")
		for _, t := range shellTools {
			fmt.Fprintf(&b, "- `%s` — %s\n", t.Name, t.Description)
		}
	}

	writeVisibility(&b, rc)
	return b.String()
}

