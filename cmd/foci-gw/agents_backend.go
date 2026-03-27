package main

import (
	"context"
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
				if summary != "" {
					return "✅ " + summary
				}
				return "✅ Approved"
			})
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


