package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/backend"
	"foci/internal/backend/ccstream"
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

	// Build auto-approve rules from resolved config.
	autoApproveRules := buildAutoApproveRules(p)

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
			WorkDir:          p.acfg.Workspace,
			SystemPrompt:     systemPrompt,
			Model:            model,
			AgentID:          agentID,
			ExecRegistry:     registry,
			TmuxCols:         p.cfg.Tools.TmuxCols,
			TmuxRows:         p.cfg.Tools.TmuxRows,
			AutoApproveRules: autoApproveRules,
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
		PermissionPromptFunc: func(sessionKey, requestID, text, summary string, choices []backend.PromptChoice) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				log.Warnf("agent/"+agentID, "permission prompt: ForSessionOrPrimary returned nil for session=%s, prompt dropped", sessionKey)
				return
			}
			log.Debugf("agent/"+agentID, "permission prompt: sending via %s for session=%s summary=%q reqID=%s", conn.PlatformName(), sessionKey, summary, requestID)
			var buttons []platform.ButtonChoice
			for _, c := range choices {
				buttons = append(buttons, platform.ButtonChoice{Label: c.Label, Data: c.Data})
			}
			reqID := requestID // capture for closure
			_ = platform.SendInteractiveMessage(conn, text, buttons, func(choice platform.ButtonChoice) string {
				_ = ag.SendPermissionResponse(context.Background(), sessionKey, reqID, choice.Data)
				if strings.EqualFold(choice.Data, "deny") {
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

// buildAutoApproveRules assembles the foci-level auto-approve rules for a
// delegated backend from resolved config + workspace-scoped defaults.
func buildAutoApproveRules(p setupParams) []string {
	perms := p.resolved.Permissions

	// Start with common readonly rules if enabled.
	var rules []string
	if perms.AutoApproveCommonReadonly {
		rules = append(rules, ccstream.CommonReadonlyRules...)
	}

	// Add workspace Edit/Write rules — delegated backends always need
	// workspace file access without prompting.
	absWorkspace, err := filepath.Abs(p.acfg.Workspace)
	if err != nil {
		absWorkspace = p.acfg.Workspace
	}
	rules = append(rules,
		fmt.Sprintf("Edit:%s/*", absWorkspace),
		fmt.Sprintf("Write:%s/*", absWorkspace),
	)

	// Append user-configured rules (already merged: agent ∪ global).
	rules = append(rules, perms.AutoApproveRules...)

	return rules
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


