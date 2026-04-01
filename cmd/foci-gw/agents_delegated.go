package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// configureDelegated sets up delegated transport agent state: DelegatedManager
// with all callbacks, model override, permissions, and exec registry. The
// agent's shared fields (compaction, warnings, etc.) are already set by
// setupAgent before this is called.
func configureDelegated(ag *agent.Agent, p setupParams, shared *sharedAgentSetup, backendName string, backendConfig map[string]any) (finalizeParams, bool) {
	// Bootstrap for building the system prompt (workspace *.md files).
	bs := workspace.NewBootstrap(p.acfg.Workspace, nil)
	systemBlocks := bs.SystemBlocks()
	var systemPrompt string
	for _, block := range systemBlocks {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		systemPrompt += block.Text
	}

	// Model for the backend — from backend_config, not from the group resolver.
	model := ""
	if v, ok := backendConfig["model"].(string); ok {
		model = v
	}

	// Ensure CC has permissions to edit the workspace without prompting.
	seedDelegatedPermissions(p.acfg.Workspace)

	// Build a tool registry with exec-exported tools so foci shell commands
	// (foci_todo, foci_send_to_chat, etc.) are available in the backend's
	// shell environment via the persistent exec bridge.
	registry := buildExecRegistry(p)

	// Override model display name to show the backend name.
	ag.Model = backendName

	// Wire DelegatedManager: lazy per-session Backend creation.
	connMgr := p.connMgr
	agentID := p.acfg.ID
	// Parse idle timeout from config (default 24h).
	var idleTimeout time.Duration
	if v, ok := backendConfig["idle_timeout"].(string); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			idleTimeout = d
		}
	}

	ag.DelegatedManager = &agent.DelegatedManager{
		SessionIndex: p.sessionIndex,
		AgentID:      agentID,
		NewBackend: func() (backend.Backend, error) {
			return backend.New(backendName, backendConfig)
		},
		StartOpts: backend.StartOptions{
			WorkDir:      p.acfg.Workspace,
			SystemPrompt: systemPrompt,
			Model:        model,
			AgentID:      agentID,
			ExecRegistry: registry,
			TmuxCols:     p.cfg.Tools.TmuxCols,
			TmuxRows:     p.cfg.Tools.TmuxRows,
		},
		SendFunc: func(sessionKey, text string) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn != nil {
				_ = platform.SendText(conn, text)
				return
			}
			// Platform not ready yet (e.g. Discord still connecting on startup).
			// Queue and retry until the connection is available.
			go func() {
				for i := 0; i < 60; i++ {
					time.Sleep(500 * time.Millisecond)
					conn = connMgr.ForSessionOrPrimary(sessionKey, agentID)
					if conn != nil {
						_ = platform.SendText(conn, text)
						return
					}
				}
				log.Warnf("agent/"+agentID, "delegated: no connection for session %s after 30s, message dropped", sessionKey)
			}()
		},
		PermissionPromptFunc: func(sessionKey, text, summary string, choices []backend.PromptChoice) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				return
			}
			var buttons []platform.ButtonChoice
			for _, c := range choices {
				buttons = append(buttons, platform.ButtonChoice{Label: c.Label, Data: c.Data})
			}
			_ = platform.SendInteractiveMessage(conn, text, buttons, func(choice platform.ButtonChoice) string {
				// Send keystroke to CC's TUI.
				_ = ag.SendPermissionResponse(context.Background(), sessionKey, choice.Data)
				// Permission resolved via button press. The onPermCleared
				// callback (wired in DelegatedManager.Get) handles clearing
				// permPending when the prompt disappears from the TUI,
				// which covers both button responses and CC timeouts.
				if strings.EqualFold(choice.Label, "No") {
					if summary != "" {
						return "❌ " + summary
					}
					return "❌ Denied"
				}
				if summary != "" {
					return "✅ " + summary
				}
				return "✅ Approved"
			})
		},
		TypingFunc: func(sessionKey string, typing bool) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				return
			}
			if typing {
				conn.SetTyping(true)
			}
			// Don't propagate false here. Typing lifecycle is handled by
			// OnTurnDone callback (set by processAgentMessage, called by
			// runPostTurn when the turn completes). TypingFunc just starts
			// typing when CC begins working; stopping is OnTurnDone's job.
		},
		IdleTimeout: idleTimeout,
	}

	return finalizeParams{
		bootstrap: bs,
	}, true
}

// seedDelegatedPermissions ensures .claude/settings.local.json in the workspace
// has permissions allowing the delegated backend to edit workspace files without
// prompting. Merges with any existing settings; never overwrites user entries.
func seedDelegatedPermissions(workspace string) {
	settingsDir := filepath.Join(workspace, ".claude")
	settingsPath := filepath.Join(settingsDir, "settings.local.json")

	// Read existing settings (if any).
	var settings map[string]any
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			log.Warnf("delegated", "parse %s: %v — not modifying", settingsPath, err)
			return
		}
	} else {
		settings = make(map[string]any)
	}

	// Build the rules we want present.
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		absWorkspace = workspace
	}
	wantRules := []string{
		// Workspace file access. The // prefix is required for absolute
		// paths in CC permission rules (single / is not matched).
		fmt.Sprintf("Edit(//%s/**)", absWorkspace),
		fmt.Sprintf("Write(//%s/**)", absWorkspace),
		// Read-only tools — blanket access everywhere.
		"Search",
		"Glob",
		"Grep",
		"Read",
		"WebSearch",
		"WebFetch",
		// Basic shell commands — blanket access.
		"Bash(ls:*)",
		"Bash(echo:*)",
		"Bash(cat:*)",
		"Bash(head:*)",
		"Bash(tail:*)",
		"Bash(wc:*)",
		"Bash(sort:*)",
		"Bash(cut:*)",
		"Bash(tr:*)",
		"Bash(diff:*)",
		"Bash(stat:*)",
		"Bash(file:*)",
		"Bash(which:*)",
		"Bash(date:*)",
		"Bash(pwd:*)",
		"Bash(id:*)",
		"Bash(uname:*)",
		"Bash(ps:*)",
		"Bash(ss:*)",
		"Bash(du:*)",
		"Bash(df:*)",
		// Search/filter tools.
		"Bash(grep:*)",
		"Bash(rg:*)",
		"Bash(ack:*)",
		"Bash(sed -n:*)",
		// Compressed file inspection.
		"Bash(zcat:*)",
		"Bash(zgrep:*)",
		// Environment and system inspection.
		"Bash(env:*)",
		"Bash(crontab -l:*)",
		"Bash(npm list:*)",
		// System logs.
		"Bash(journalctl:*)",
		// Data tools.
		"Bash(jq:*)",
		"Bash(yq:*)",
		"Bash(mds:*)",
		"Bash(mdq:*)",
		"Bash(sqlite3:*)",
		// Foci shell functions.
		"Bash(foci_todo:*)",
		"Bash(foci_send_to_chat:*)",
		"Bash(foci_memory_search:*)",
		"Bash(foci_http_request:*)",
		"Bash(foci_web_search:*)",
		"Bash(foci_web_fetch:*)",
	}

	// Get or create the permissions.allow array.
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = make(map[string]any)
	}
	allowRaw, _ := perms["allow"].([]any)
	existing := make(map[string]bool, len(allowRaw))
	for _, v := range allowRaw {
		if s, ok := v.(string); ok {
			existing[s] = true
		}
	}

	// Add missing rules.
	changed := false
	for _, rule := range wantRules {
		if !existing[rule] {
			allowRaw = append(allowRaw, rule)
			changed = true
		}
	}
	if !changed {
		return
	}

	perms["allow"] = allowRaw
	settings["permissions"] = perms

	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		log.Warnf("delegated", "mkdir %s: %v", settingsDir, err)
		return
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		log.Warnf("delegated", "marshal settings: %v", err)
		return
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0644); err != nil {
		log.Warnf("delegated", "write %s: %v", settingsPath, err)
		return
	}
	log.Infof("delegated", "seeded workspace permissions in %s", settingsPath)
}

// buildExecRegistry creates a tools.Registry containing only exec-exported
// tools. These are exposed as shell functions (foci_todo, foci_send_to_chat,
// etc.) via the persistent exec bridge in the delegated backend's tmux session.
func buildExecRegistry(p setupParams) *tools.Registry {
	registry := tools.NewRegistry()
	acfg := p.acfg
	connMgr := p.connMgr

	registry.Register(tools.NewSendToChatTool(func(sessionKey string) platform.Sender {
		conn := connMgr.ForSessionOrPrimary(sessionKey, acfg.ID)
		if conn == nil {
			return nil
		}
		return conn
	}, nil))

	if p.todoStore != nil {
		registry.Register(tools.NewTodoTool(p.todoStore, acfg.ID))
	}

	if p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	registry.Register(tools.NewWebFetchTool())
	registry.Register(tools.NewHTTPRequestTool(p.store, p.bwStore, p.cfg.Tools.TempDir, 0, 0, nil, 0o644))

	if len(p.memBackends) > 0 {
		registry.Register(tools.NewMemorySearchTool(p.memBackends, p.resolved.MemorySearch.SearchBackend, p.convReader))
	}

	log.Infof("agent/"+acfg.ID, "exec bridge registry: %d tools (%v)", len(registry.All()), registry.ExportedNames())
	return registry
}


