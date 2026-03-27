package main

import (
	"time"

	"foci/internal/agent"
	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/workspace"
)

// setupBackendAgent wires up an agent that delegates turns to a coding agent
// backend (Claude Code, Codex, OpenCode). Each session gets its own Backend
// instance (own tmux pane, own CC session), created lazily on first message.
func setupBackendAgent(p setupParams, backendName string, backendConfig map[string]any) *agentInstance {
	shared := resolveSharedSetup(p)

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
		NewBackend: func() (backend.Backend, error) {
			return backend.New(backendName, backendConfig)
		},
		StartOpts: backend.StartOptions{
			WorkDir:      p.acfg.Workspace,
			SystemPrompt: systemPrompt,
			Model:        model,
			AgentID:      agentID,
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
			// Use inline keyboard if the platform supports it.
			if bs, ok := conn.(platform.ButtonSender); ok {
				var buttons []platform.ButtonChoice
				for _, c := range choices {
					buttons = append(buttons, platform.ButtonChoice{Label: c.Label, Data: c.Data})
				}
				_ = bs.SendTextWithButtons(text, buttons, "perm:")
				return
			}
			// Fallback: plain text with numbered choices.
			_ = platform.SendText(conn, text+"\n\nReply with your choice (1, 2, 3, etc.)")
		},
		IdleTimeout: idleTimeout,
	}

	return shared.finalize(ag, finalizeParams{
		bootstrap: bs,
	})
}
