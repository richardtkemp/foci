package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/delegator/ccstream"
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
	// Make prompt search dirs available for orientation template resolution
	// (webhooks, keepalive, memory formation). Delegated agents don't need
	// groupResolver since their model comes from backendConfig.
	ag.PromptSearchDirs = shared.promptSearchDirs

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

	// For Claude Code-family backends, fold global [cc_backend]
	// default_allowed_tools into the per-agent backend_config.allowed_tools
	// so both cctmux and ccstream pick up the merged rule list from the
	// same cfg key. Non-CC backends (codex, opencode, ...) are skipped so
	// the key doesn't leak into their config surface.
	if backendName == "claude-code" || backendName == "claude-code-tmux" {
		merged := p.cfg.CCBackend.MergedAllowedTools(backendConfig["allowed_tools"])
		if merged != "" {
			if backendConfig == nil {
				backendConfig = map[string]any{}
			} else {
				// Copy so we don't mutate the shared AgentConfig.BackendConfig map.
				copied := make(map[string]any, len(backendConfig)+1)
				for k, v := range backendConfig {
					copied[k] = v
				}
				backendConfig = copied
			}
			backendConfig["allowed_tools"] = merged
		}
	}

	// Build a tool registry with exec-exported tools so foci shell commands
	// (foci_todo, foci_send_to_chat, etc.) are available in the backend's
	// shell environment via the persistent exec bridge.
	//
	// Built before buildAutoApproveRules so its ExportedNames can drive the
	// always-on FociShellRules — keeps the auto-approve list in sync with
	// what's actually wired in (no hand-list to drift).
	agLazy := func() *agent.Agent { return ag }
	registry := buildExecRegistry(p, shared.wakeScheduleFn, agLazy)

	// Build auto-approve rules from resolved config.
	autoApproveRules := buildAutoApproveRules(p, registry.ExportedNames())

	// Per-agent environment block for delegated backends.
	if p.resolved.Environment.Enabled {
		envBlock := buildEnvironmentDelegated(p.acfg, p.configPath, p.cfg, p.resolved, p.plat.ActivePlatformNames(), registry.ExportedTools())
		if systemPrompt != "" {
			systemPrompt = envBlock + "\n\n" + systemPrompt
		} else {
			systemPrompt = envBlock
		}
	}

	// Override model display name to show the backend name.
	ag.Model = backendName

	// Shared rate limit state — account-wide, not per-session. All backends
	// under this agent write to the same state via OnRateLimit.
	rateLimitState := &ccstream.RateLimitState{}
	ag.UsageClient = rateLimitState // implements mana.UsageClient

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
		NewBackend: func() (delegator.Delegator, error) {
			be, err := delegator.New(backendName, backendConfig)
			if err != nil {
				return nil, err
			}
			// Inject shared rate limit state into ccstream backends.
			if sb, ok := be.(*ccstream.Backend); ok {
				sb.SetRateLimitState(rateLimitState)
			}
			return be, nil
		},
		StartOpts: delegator.StartOptions{
			WorkDir:          p.acfg.Workspace,
			SystemPrompt:     systemPrompt,
			Model:            model,
			AgentID:          agentID,
			ExecRegistry:     registry,
			TmuxCols:         p.cfg.Tools.TmuxCols,
			TmuxRows:         p.cfg.Tools.TmuxRows,
			AutoApproveRules: autoApproveRules,
		},
		PermissionPromptFunc: func(sessionKey, requestID, text, summary string, choices []delegator.PromptChoice) {
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
				log.Debugf("agent/"+agentID, "permission button pressed: sk=%s reqID=%s choice=%q", sessionKey, reqID, choice.Data)
				if err := ag.SendPermissionResponse(context.Background(), sessionKey, reqID, choice.Data); err != nil {
					log.Errorf("agent/"+agentID, "SendPermissionResponse failed: sk=%s reqID=%s choice=%q err=%v", sessionKey, reqID, choice.Data, err)
				}
				switch {
				case choice.Data == "deny" || choice.Data == "qa:cancel":
					if summary != "" {
						return "❌ " + summary
					}
					return "❌ Cancelled"
				case strings.HasPrefix(choice.Data, "qa:"):
					return "✅ " + choice.Label
				default:
					if summary != "" {
						return "✅ " + summary
					}
					return "✅ Approved"
				}
			})
		},
		TypingFunc: func(sessionKey string, typing bool) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				return
			}
			conn.SetTyping(typing)
		},
		IdleTimeout: idleTimeout,
	}

	return finalizeParams{
		bootstrap: bs,
	}, true
}

// buildAutoApproveRules assembles the foci-level auto-approve rules for a
// delegated backend from resolved config + workspace-scoped defaults.
//
// fociExecNames is the list of foci shell-function names exported by the
// agent's tool registry (e.g. "foci_todo", "foci_remind"). These get an
// always-on auto-approve rule via FociShellRulesFor — no toggle, since
// they're foci's own constrained wrappers.
func buildAutoApproveRules(p setupParams, fociExecNames []string) []string {
	perms := p.resolved.Permissions

	// Foci shell functions are always auto-approved — derived from the
	// registry so adding/removing an ExecExport tool updates the rules
	// automatically.
	rules := ccstream.FociShellRulesFor(fociExecNames)

	// Common readonly rules if enabled.
	if perms.AutoApproveCommonReadonly {
		rules = append(rules, ccstream.CommonReadonlyRules...)
	}
	if perms.AutoApproveCommonSafeWrite {
		rules = append(rules, ccstream.CommonSafeWriteRules...)
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
//
// wakeScheduleFn is the agent's scheduled-wake callback (built once
// transport-independently in setupAgent). Pass nil to skip remind-tool
// registration — useful when reminderStore is unconfigured.
//
// agLazy is a closure that returns the agent — used by the async notifier so
// send_to_session can deliver replies back to the calling session. May be nil
// in tests where the notifier path isn't exercised; when nil, send_to_session
// is still registered but reply_to=caller deliveries are a no-op.
func buildExecRegistry(p setupParams, wakeScheduleFn tools.ScheduleWakeFn, agLazy func() *agent.Agent) *tools.Registry {
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

	// send_to_session: cross-session messaging. Wires the same async notifier
	// and session-notify routes as the API path (registerSessionTools). The
	// notifier handles reply_to=caller (response routes back); sessionNotifyFn
	// handles reply_to=session (response goes to target's own chat).
	var notifier *tools.AsyncNotifier
	if agLazy != nil {
		notifier = newAsyncNotifier(agLazy, acfg.ID, p.ctx, connMgr)
	}
	sessionNotifyFn := newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr)
	var resolveKeyFn tools.SessionKeyResolverFn
	if p.sessionIndex != nil {
		resolveKeyFn = p.sessionIndex.ResolvePartialKey
	}
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier, sessionNotifyFn, resolveKeyFn))

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

	if p.reminderStore != nil && wakeScheduleFn != nil {
		registry.Register(tools.NewRemindTool(p.reminderStore, acfg.ID, wakeScheduleFn))
	}

	log.Infof("agent/"+acfg.ID, "exec bridge registry: %d tools (%v)", len(registry.All()), registry.ExportedNames())
	return registry
}


