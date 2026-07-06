package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"foci/internal/config"
	"foci/internal/delegator/ccstream"
	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// checkSystemPromptSizes logs warnings if system prompt files exceed thresholds.
// The thresholds are the per-agent effective values (override → global).
func checkSystemPromptSizes(bootstrap *workspace.Bootstrap, maxFileChars, maxTotalChars int, agentID string) {
	for _, w := range bootstrap.CheckSizes(maxFileChars, maxTotalChars) {
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
	cmd := procx.Spawn(context.Background(), "sh", "-c", "crontab -l 2>/dev/null | grep -v '^#' | grep -v '^$' | wc -l")
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
func writeEnvironmentCore(b *strings.Builder, acfg config.AgentConfig, configPath string, cfg *config.Config, rc *config.ResolvedAgentConfig, activePlatforms []string, crontabCount int) {
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
	b.WriteString("- **via** — which transport/source delivered this message: `telegram`/`app`/`discord` (a messaging platform), `voice` (speech-to-text), `external` (HTTP /send endpoint — foci send CLI or raw API; replies are already delivered), `agent` (another agent via send_to_session), `ask-grader` (the ask tool's answer/grader result), `webhook` (inbound webhook), `wake` (a scheduled /wake poke — cron job or self-scheduled wakeup; reply auto-delivered), `background` (a self-maintenance tick — keepalive/reflection/consolidation), `memory` (a memory-maintenance write), `system` (a system notification — restart changelog, proactive warning), `async` (an async tool result)\n")
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

	fmt.Fprintf(b, "- You may schedule recurring tasks using crontab. You have %d jobs scheduled.\n", crontabCount)
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

// writeCommandApproval documents the CC agent's effective auto-approve allowlist
// so it knows which tool/Bash calls run without prompting the user, instead of
// guessing (#950). Rendered from the ccstream rule sets (the source of truth) so
// it can't drift from what the backend actually approves.
func writeCommandApproval(b *strings.Builder, rc *config.ResolvedAgentConfig, ccAllowedTools string) {
	b.WriteString("\n## Command Approval\n")
	b.WriteString("Tool and Bash calls matching your auto-approve allowlist run WITHOUT prompting the user; everything else prompts. Your effective allowlist:\n")
	if ccAllowedTools != "" {
		// CC's --allowedTools layer: these are PRE-APPROVED (auto-run, no
		// prompt) — NOT an exclusive whitelist; tools outside it still work,
		// they just prompt. Distinct from the foci rules below, which
		// auto-answer prompts CC does generate.
		fmt.Fprintf(b, "- **CC pre-approved** (auto-run, no prompt — not a restriction): %s\n", ccAllowedTools)
	}
	b.WriteString("- **foci tools**: every `foci_*` shell function is always auto-approved.\n")
	if rc.Permissions.AutoApproveCommonReadonly {
		fmt.Fprintf(b, "- **read-only** (on): %s\n", strings.Join(stripBashPrefix(ccstream.CommonReadonlyRules), ", "))
	}
	swState := "off — these would prompt"
	if rc.Permissions.AutoApproveCommonSafeWrite {
		swState = "on"
	}
	fmt.Fprintf(b, "- **safe-write** (%s): %s\n", swState, strings.Join(stripBashPrefix(ccstream.CommonSafeWriteRules), ", "))
	if len(rc.Permissions.AutoApproveRules) > 0 {
		fmt.Fprintf(b, "- **configured for this agent**: %s\n", strings.Join(stripBashPrefix(rc.Permissions.AutoApproveRules), ", "))
	}
	b.WriteString("Everything else — e.g. bare `git`, writable `sqlite3`, `gh create`/`merge`, paths outside the above — prompts for your approval.\n")
}

// stripBashPrefix drops the "Bash:" prefix from auto-approve rules for readable
// prose (non-Bash tool rules like "Read" pass through unchanged).
func stripBashPrefix(rules []string) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = strings.TrimPrefix(r, "Bash:")
	}
	return out
}

// writeShellTools lists the foci shell functions available to delegated agents
// via the exec bridge (they call foci tools through Bash, so they need the
// names). API agents call the registry directly and get no such list.
func writeShellTools(b *strings.Builder, shellTools []tools.ExportedTool) {
	if len(shellTools) == 0 {
		return
	}
	b.WriteString("\n## Foci Shell Tools\n")
	b.WriteString("The following foci tools are available as shell functions in your Bash environment. ")
	b.WriteString("Call them via the Bash tool (e.g., `foci_todo list --status open`).\n")
	b.WriteString("Run any command with `--help` or `-h` for usage details.\n\n")
	for _, t := range shellTools {
		fmt.Fprintf(b, "- `%s` — %s\n", t.Name, t.Description)
	}
}

// writeAPIConfig documents API-agent-specific tool/loop limits (API agents are
// far more configurable than delegated ones) so the agent works within them
// rather than discovering them by hitting a guard (#1060). API agents only.
func writeAPIConfig(b *strings.Builder, acfg config.AgentConfig, cfg *config.Config, rc *config.ResolvedAgentConfig) {
	var body strings.Builder
	if rc.Loop.MaxToolLoops > 0 {
		fmt.Fprintf(&body, "- **Tool budget**: up to %d tool iterations per turn — pace multi-step work against this.\n", rc.Loop.MaxToolLoops)
	}
	if rc.Summary.MaxResultChars > 0 {
		disposition := "auto-summarised via the cheap model"
		if !rc.Summary.AutoSummarise {
			disposition = "saved to a temp file with hints (auto-summary off)"
		}
		fmt.Fprintf(&body, "- **Tool results** over %d chars aren't returned inline — they're %s. Prefer targeted queries over dumping.\n", rc.Summary.MaxResultChars, disposition)
	}
	if rc.Tools.MaxFileReadBytes > 0 {
		fmt.Fprintf(&body, "- **File reads**: read/edit refuse files over %d MB — use offset/limit for big files.\n", rc.Tools.MaxFileReadBytes/(1<<20))
	}
	if rc.Tools.MaxConcurrentSpawns > 0 {
		fmt.Fprintf(&body, "- **Spawn**: up to %d concurrent spawn sessions — batch parallel subagent work within this.\n", rc.Tools.MaxConcurrentSpawns)
	}
	if cfg.Tools.ExecDefaultTimeout > 0 {
		line := fmt.Sprintf("- **Exec**: commands default to a %ds timeout", cfg.Tools.ExecDefaultTimeout)
		if rc.Tools.ExecAutoBackground > 0 {
			line += fmt.Sprintf("; those still running after %ds auto-background", rc.Tools.ExecAutoBackground)
		}
		body.WriteString(line + ". Set an explicit timeout for long commands.\n")
	}
	blocked := acfg.BlockedPaths // per-agent overrides global
	if len(blocked) == 0 {
		blocked = cfg.BlockedPaths
	}
	if len(blocked) > 0 {
		body.WriteString("- **Blocked paths** (write/edit refused):")
		for i, bp := range blocked {
			if i > 0 {
				body.WriteString(",")
			}
			fmt.Fprintf(&body, " `%s`", bp.Path)
		}
		body.WriteString("\n")
	}

	if body.Len() == 0 {
		return
	}
	b.WriteString("\n## Tool & Loop Limits\n")
	b.WriteString(body.String())
}

// writeMemorySearch documents how foci_memory_search behaves for this agent —
// the backend, its query semantics, and what's indexed — so the agent queries
// effectively and reads a miss correctly (#1060).
func writeMemorySearch(b *strings.Builder, acfg config.AgentConfig, rc *config.ResolvedAgentConfig) {
	backend := rc.MemorySearch.SearchBackend
	if backend == "" {
		return
	}
	b.WriteString("\n## Memory & Search\n")
	fmt.Fprintf(b, "`foci_memory_search` runs on the **%s** backend. Query terms are stemmed (English/Porter — `program` matches `programmer`), and special characters are literal, not operators (no `+`/`-`/`field:` syntax — queries are sanitised).\n", backend)
	switch backend {
	case "bleve":
		b.WriteString("- Indexes your markdown memory files **only — NOT conversation history**. A query that finds nothing may just mean the match is in chat history (unindexed here), not that it's absent.\n")
	case "fts5":
		fmt.Fprintf(b, "- Indexes markdown files **and conversation history** (conversation hits weighted ×%.2g, so they rank lower).\n", rc.MemorySearch.ConversationWeight)
	}
	if len(acfg.Memory.Sources) > 0 {
		b.WriteString("Indexed sources:\n")
		for _, s := range acfg.Memory.Sources {
			fmt.Fprintf(b, "- `%s` — %s (weight %.2g)\n", s.Name, s.Dir, s.Weight)
		}
	}
	if rc.MemorySearch.SearchLimit > 0 {
		fmt.Fprintf(b, "Returns up to %d results.\n", rc.MemorySearch.SearchLimit)
	}
}

// writeBackend appends the "## Backend" section from the backend-<name>.md
// prompt file (user-editable via searchDirs, embedded default otherwise). An
// empty backend name (API agents) resolves to backend-api.md. No section is
// emitted if no file resolves.
func writeBackend(b *strings.Builder, backend string, searchDirs []string) {
	if backend == "" {
		backend = "api"
	}
	filename := "backend-" + backend + ".md"
	notes := prompts.ResolvePrompt("", filename, prompts.Backend(backend), searchDirs...)
	if notes == "" {
		return
	}
	b.WriteString("\n## Backend\n")
	b.WriteString(notes)
	b.WriteString("\n")
}

// buildEnvironmentAPI generates the environment block for API agents, which
// have direct access to foci's tool registry.
func buildEnvironmentAPI(acfg config.AgentConfig, configPath string, cfg *config.Config, rc *config.ResolvedAgentConfig, crontabCount int, activePlatforms []string, searchDirs []string) string {
	var b strings.Builder
	writeEnvironmentCore(&b, acfg, configPath, cfg, rc, activePlatforms, crontabCount)

	writeBackend(&b, acfg.Backend, searchDirs)
	writeMemorySearch(&b, acfg, rc)
	writeAPIConfig(&b, acfg, cfg, rc)

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
func buildEnvironmentDelegated(acfg config.AgentConfig, configPath string, cfg *config.Config, rc *config.ResolvedAgentConfig, crontabCount int, activePlatforms []string, shellTools []tools.ExportedTool, searchDirs []string) string {
	var b strings.Builder
	writeEnvironmentCore(&b, acfg, configPath, cfg, rc, activePlatforms, crontabCount)

	writeBackend(&b, acfg.Backend, searchDirs)
	writeMemorySearch(&b, acfg, rc)
	writeShellTools(&b, shellTools)

	// Auto-approve visibility is CC-specific (the ccstream Bash allowlist);
	// opencode has its own permission model.
	// skip_permissions bypasses all prompts, so a Command Approval section would
	// just be confusing noise — omit it entirely (everything is permitted).
	if skip, _ := acfg.BackendConfig["skip_permissions"].(bool); acfg.Backend == "claude-code" && !skip {
		writeCommandApproval(&b, rc, cfg.CCBackend.MergedAllowedTools(acfg.BackendConfig["allowed_tools"]))
	}

	writeVisibility(&b, rc)
	return b.String()
}
