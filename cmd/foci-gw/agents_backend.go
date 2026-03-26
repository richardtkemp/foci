package main

import (
	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/workspace"
)

// setupBackendAgent wires up an agent that delegates turns to a coding agent
// backend (Claude Code, Codex, OpenCode). Skips tool registry, compactor,
// client resolution, and other traditional agent loop machinery. Keeps
// reminders, scratchpad, nudges, platform connections, and command dispatch.
func setupBackendAgent(p setupParams, be backend.Backend) *agentInstance {
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

	// Model for the backend — from backend_config, not from the group resolver
	// (which holds API-routed models like OpenRouter IDs that CC doesn't understand).
	model := ""
	if v, ok := p.acfg.BackendConfig["model"].(string); ok {
		model = v
	}

	// Build the agent with backend and shared fields.
	ag := shared.newAgent()
	ag.Backend = be
	ag.Model = p.acfg.Backend // display the backend name as the "model"

	// Wire BackendSendFunc to deliver text to the correct chat via connMgr.
	connMgr := p.connMgr
	agentID := p.acfg.ID
	ag.BackendSendFunc = func(sessionKey, text string) {
		conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
		if conn == nil {
			return
		}
		_ = conn.SendText(text)
	}

	// Start the backend subprocess.
	if err := be.Start(p.ctx, backend.StartOptions{
		WorkDir:      p.acfg.Workspace,
		SystemPrompt: systemPrompt,
		Model:        model,
		AgentID:      p.acfg.ID,
	}); err != nil {
		log.Errorf("agent/"+p.acfg.ID, "backend start failed: %v", err)
		return nil
	}

	return shared.finalize(ag, finalizeParams{
		bootstrap: bs,
	})
}
