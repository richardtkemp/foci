package main

import (
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// setupBackendAgent wires up an agent that delegates turns to a coding agent
// backend (Claude Code, Codex, OpenCode). Each session gets its own Backend
// instance (own tmux pane, own CC session), created lazily on first message.
func setupBackendAgent(p setupParams, backendName string, backendConfig map[string]any) *agentInstance {
	shared := resolveSharedSetup(p)
	p = shared.p // p.resolved is now set

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

	// Build a tool registry with exec-exported tools so foci shell commands
	// (foci_todo, foci_send_to_chat, etc.) are available in the backend's
	// shell environment via the persistent exec bridge.
	registry := buildExecRegistry(p)

	// Build the agent with shared fields.
	ag := shared.newAgent()
	ag.Model = backendName // display the backend name as the "model"

	// Wire BackendManager: lazy per-session Backend creation.
	connMgr := p.connMgr
	agentID := p.acfg.ID
	// Parse idle timeout from config (default 24h).
	var idleTimeout time.Duration
	if v, ok := backendConfig["idle_timeout"].(string); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			idleTimeout = d
		}
	}

	ag.BackendManager = &agent.BackendManager{
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
		},
		SendFunc: func(sessionKey, text string) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				log.Warnf("agent/"+agentID, "backend: no connection for session %s", sessionKey)
				return
			}
			_ = platform.SendText(conn, text)
		},
		PermissionPromptFunc: func(sessionKey, text string, choices []backend.PromptChoice) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				return
			}
			// Extract a short reason from the description for the post-approval edit.
			// The text is "⚠️ Permission required:\n\n<description>".
			reason := extractPermissionReason(text)

			// Use inline keyboard if the platform supports it.
			if bs, ok := conn.(platform.ButtonSender); ok {
				var buttons []platform.ButtonChoice
				for _, c := range choices {
					// Encode reason in callback data: "1:go vet on backend"
					// Truncated to fit Telegram's 64-byte callback data limit.
					data := c.Data + ":" + truncate(reason, 50)
					buttons = append(buttons, platform.ButtonChoice{Label: c.Label, Data: data})
				}
				_ = bs.SendTextWithButtons(text, buttons, "perm:")
				return
			}
			_ = platform.SendText(conn, text+"\n\nReply with your choice (1, 2, 3, etc.)")
		},
		IdleTimeout: idleTimeout,
	}

	return shared.finalize(ag, finalizeParams{
		bootstrap: bs,
	})
}

// buildExecRegistry creates a tools.Registry containing only exec-exported
// tools. These are exposed as shell functions (foci_todo, foci_send_to_chat,
// etc.) via the persistent exec bridge in the backend's tmux session.
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

// extractPermissionReason extracts a short reason from the permission prompt text.
// Input: "⚠️ Permission required:\n\n Bash command\n\n   cd ... && go vet ...\n   Run go vet on backend\n"
// Returns the last non-empty indented line (the description), e.g. "Run go vet on backend".
func extractPermissionReason(text string) string {
	// Find the description block after the header.
	idx := strings.Index(text, "\n\n")
	if idx < 0 {
		return ""
	}
	desc := text[idx+2:]
	// The description has the command first, then the reason on the next indented line.
	// Look for the last non-empty trimmed line.
	var reason string
	for _, line := range strings.Split(desc, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			reason = t
		}
	}
	return reason
}

// truncate returns s truncated to maxLen bytes.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

