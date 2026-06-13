package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/ccstream"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/tools"
	"foci/internal/voice"
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

	// Bootstrap and skill registry — shared loader with API agents.
	//
	// Fixes two latent gaps in the previous local NewBootstrap call:
	//   1. p.acfg.System.SystemFiles is now honoured. The old call passed
	//      nil for fileOrder, which fell through to DefaultFileOrder and
	//      silently ignored per-agent system_files config.
	//   2. The skill registry is populated, so the Available Skills system
	//      block and the default skill nudge reach delegated agents (was
	//      previously inert — 0 default rules across all delegated agents).
	agentStore := p.store.ForAgent(p.acfg.ID)
	br := setupBootstrapAndSkills(p, agentStore)
	bs := br.bootstrap

	systemPrompt := buildDelegatedSystemPrompt(bs.SystemBlocks(), br.extraSystemBlocks)

	// Model for the backend — from backend_config, not from the group resolver.
	model := ""
	if v, ok := backendConfig["model"].(string); ok {
		model = v
	}

	// For Claude Code-family backends, fold global [cc_backend] settings
	// into the per-agent backend_config map so both cctmux and ccstream
	// pick them up from the same cfg key. Non-CC backends (codex,
	// opencode, ...) are skipped so the keys don't leak into their
	// config surface.
	//
	// Folded keys (per-agent values always win):
	//   allowed_tools — merged (per-agent rules appended to global)
	//   claude_binary — global default; per-agent override wins
	if backendName == "claude-code" || backendName == "claude-code-tmux" {
		merged := p.cfg.CCBackend.MergedAllowedTools(backendConfig["allowed_tools"])
		_, claudeBinSet := backendConfig["claude_binary"]
		injectClaudeBin := !claudeBinSet && p.cfg.CCBackend.ClaudeBinary != ""
		if merged != "" || injectClaudeBin {
			// Copy so we don't mutate the shared AgentConfig.BackendConfig.
			copied := make(map[string]any, len(backendConfig)+2)
			for k, v := range backendConfig {
				copied[k] = v
			}
			backendConfig = copied
			if merged != "" {
				backendConfig["allowed_tools"] = merged
			}
			if injectClaudeBin {
				backendConfig["claude_binary"] = p.cfg.CCBackend.ClaudeBinary
			}
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
			// claude_binary is read from the same merged map ccstream
			// consumes, but RunOnce doesn't see backendConfig — fold it
			// onto StartOpts so DelegatedManager.RunOnce honours it.
			ClaudeBinary: func() string {
				v, _ := backendConfig["claude_binary"].(string)
				return v
			}(),
			// Per-agent backend_config.env propagates to the backend
			// subprocess. Used by integration tests to inject CCSTUB_*
			// env vars per agent (e.g. one agent gets CCSTUB_HANG, others
			// don't). DelegatedManager.Get merges these with the exec
			// bridge's BASH_ENV/FOCI_SOCK so both layers survive.
			Env: backendConfigEnv(backendConfig),
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
			// Use the requestID as the platform prompt ID so the cancel
			// listener (registered below) can find and edit this exact
			// message later if CC cancels the request before the user
			// responds.
			_ = platform.SendInteractiveMessageWithID(conn, requestID, text, buttons, func(choice platform.ButtonChoice) string {
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
			}, func() {
				// Expiry: deny the prompt so a turn blocked in WaitForPermission
				// unblocks instead of orphaning. The message edit to an "expired"
				// notice is handled by CleanupExpiredInteractive.
				if err := ag.SendPermissionResponse(context.Background(), sessionKey, reqID, "deny"); err != nil {
					log.Warnf("agent/"+agentID, "expire permission deny failed: sk=%s reqID=%s err=%v", sessionKey, reqID, err)
				}
			})
			// Register a cancel listener so the orphaned inline keyboard is
			// disabled if CC aborts this prompt before the user responds
			// (typically because a follow-up message interrupted the
			// in-flight tool). This replaces the global PermissionCancelFunc
			// chain with a per-prompt registration owned by the same closure
			// that created the UI.
			ag.DelegatedManager.RegisterPromptCancelListener(sessionKey, reqID, func(reason string) {
				finalText := "❌ tool request cancelled by follow-up message"
				if summary != "" {
					finalText = fmt.Sprintf("❌ %s cancelled by follow-up message", summary)
				}
				if err := platform.CancelInteractiveMessage(reqID, finalText); err != nil {
					log.Warnf("agent/"+agentID, "cancel interactive message: sk=%s reqID=%s err=%v", sessionKey, reqID, err)
				} else {
					log.Debugf("agent/"+agentID, "permission cancelled: sk=%s reqID=%s reason=%q", sessionKey, reqID, reason)
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
		bootstrap:     bs,
		skillRegistry: br.skillRegistry,
		skillsDirs:    br.skillsDirs,
	}, true
}

// buildDelegatedSystemPrompt concatenates workspace bootstrap blocks and the
// extra system blocks (Available Skills) into the single SystemPrompt string
// that delegator.StartOptions takes. Skills come after workspace files so the
// agent's identity/character is established first and skills land as reference
// material below it. API agents inject extraSystemBlocks via
// ag.ExtraSystemBlocks (separate provider block); delegated agents have to
// concatenate because the CC subprocess takes a single string at start.
//
// Empty inputs are handled cleanly — no leading or trailing separator if
// either side is empty.
func buildDelegatedSystemPrompt(workspaceBlocks, extraBlocks []provider.SystemBlock) string {
	var b strings.Builder
	write := func(blocks []provider.SystemBlock) {
		for _, blk := range blocks {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	write(workspaceBlocks)
	write(extraBlocks)
	return b.String()
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
	// workspace file access without prompting. Use the canonical (symlink-
	// resolved) workspace so the rule boundary lives in the same path space as
	// the canonicalized candidate path the auto-approver compares against
	// (P1-6) — otherwise a symlinked workspace parent would diverge and reject
	// legitimate nested writes.
	canonWorkspace := secrets.CanonicalPath(p.acfg.Workspace)
	rules = append(rules,
		fmt.Sprintf("Edit:%s/*", canonWorkspace),
		fmt.Sprintf("Write:%s/*", canonWorkspace),
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

	// Shared infrastructure that mirrors the API path's tool wiring
	// (cmd/foci-gw/agents_setup.go). Build once at registry construction so
	// every tool gets the same agent-scoped store, async notifier, file
	// permissions, and resolved TTS as their API-path counterparts.
	var agentStore *secrets.Store
	if p.store != nil {
		agentStore = p.store.ForAgent(acfg.ID)
	}
	fileMode, _ := config.ParseFileMode(p.cfg.FileMode)
	var notifier *tools.AsyncNotifier
	if agLazy != nil {
		notifier = newAsyncNotifier(agLazy, acfg.ID, p.agentResolverFn, p.ctx, connMgr)
	}
	vc := p.resolved.Voice
	ttsRepls := voice.MergeReplacements(p.cfg.Voice.TTSReplacements, acfg.Voice.TTSReplacements)
	agentTTS := resolveTTS(p.ttsMap, p.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls)

	registry.Register(tools.NewSendToChatTool(func(sessionKey string) platform.Sender {
		conn := connMgr.ForSessionOrPrimary(sessionKey, acfg.ID)
		if conn == nil {
			return nil
		}
		return conn
	}, agentTTS))

	// send_to_session: cross-session messaging. Wires the same async notifier
	// and session-notify routes as the API path (registerSessionTools). The
	// notifier handles reply_to=caller (response routes back); sessionNotifyFn
	// handles reply_to=session (response goes to target's own chat).
	sessionNotifyFn := newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr)
	var resolveKeyFn tools.SessionKeyResolverFn
	if p.sessionIndex != nil {
		resolveKeyFn = p.sessionIndex.ResolveLooseKey
	}
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier, sessionNotifyFn, resolveKeyFn))

	// ask / foci_ask: async, backend-agnostic AskUserQuestion. Posts questions
	// as interactive buttons and delivers the answer batch back into this
	// session via the same session-notify route as send_to_session.
	askRouter := registerAskTool(registry, acfg.ID, connMgr, sessionNotifyFn)
	if agLazy != nil {
		if a := agLazy(); a != nil {
			a.AskRouter = askRouter
		}
	}

	if p.todoStore != nil {
		registry.Register(tools.NewTodoTool(p.todoStore, acfg.ID))
	}

	if p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	registry.Register(tools.NewWebFetchTool())
	registry.Register(tools.NewHTTPRequestTool(agentStore, p.bwStore, p.cfg.Tools.TempDir, p.resolved.Tools.ExecAutoBackground, p.resolved.Tools.MaxUploadFileSize, notifier, fileMode))

	// Summary tool: piped/file content summarisation. Delegated agents shell
	// out to `claude --print` (CLISummariser), routing through the parent CC
	// subprocess's subscription auth so the call charges mana, not API spend.
	// API agents register this in registerCoreTools with APISummariser.
	cliSummariser := tools.NewCLISummariser("", "haiku", p.resolved.Summary.MaxSummaryInputChars)
	registry.Register(tools.NewSummaryTool(agentStore, cliSummariser, acfg.Workspace))

	if len(p.memBackends) > 0 {
		registry.Register(tools.NewMemorySearchTool(p.memBackends, p.resolved.MemorySearch.SearchBackend, p.convReader))
	}

	if p.reminderStore != nil && wakeScheduleFn != nil {
		registry.Register(tools.NewRemindTool(p.reminderStore, acfg.ID, wakeScheduleFn))
	}

	log.Infof("agent/"+acfg.ID, "exec bridge registry: %d tools (%v)", len(registry.All()), registry.ExportedNames())
	return registry
}

// backendConfigEnv reads the optional backend_config.env sub-table and
// returns it as the env-var map shape StartOptions expects. Each value
// is coerced to a string — TOML's tolerance of unquoted ints/bools
// means a value like `CCSTUB_EXIT_CODE = 1` round-trips through any/int
// rather than any/string, and we'd silently drop the entry otherwise.
//
// Returns nil when no env block is present (callers can pass nil into
// StartOptions.Env safely — the bridge merge in DelegatedManager.Get
// allocates as needed).
func backendConfigEnv(cfg map[string]any) map[string]string {
	raw, ok := cfg["env"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case bool:
			if x {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case int:
			out[k] = strconv.Itoa(x)
		case int64:
			out[k] = strconv.FormatInt(x, 10)
		case float64:
			out[k] = strconv.FormatFloat(x, 'f', -1, 64)
		default:
			out[k] = fmt.Sprintf("%v", x)
		}
	}
	return out
}
