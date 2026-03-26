package main

import (
	"foci/internal/backend"
	"foci/internal/config"
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

	// Resolve model for backend startup.
	model := ""
	if primary := shared.groupResolver.ResolveCall(config.CallChat); primary != nil {
		model = primary.ModelID
	}

	// Build the agent with backend and shared fields.
	ag := shared.newAgent()
	ag.Backend = be
	ag.Model = p.acfg.Backend // display the backend name as the "model"

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
