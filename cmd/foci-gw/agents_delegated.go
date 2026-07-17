package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/ccstream"
	"foci/internal/delegator/codex"
	"foci/internal/delegator/opencode"
	"foci/internal/log"
	"foci/internal/modelcaps"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/ratelimit"
	"foci/internal/relogin"
	"foci/internal/route"
	"foci/internal/secrets"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/internal/voice"
	"foci/shared/prompts"
)

// backendDefaultModel returns the launch model a delegated backend uses when
// neither a session override nor a config value picks one. It is the bottom rung
// of the launch-model ladder (override → config → backend default).
func backendDefaultModel(backendName string) string {
	switch backendName {
	case "claude-code", "claude-code-tmux":
		return "opus"
	default:
		// TODO(#1163): define opencode's default launch model.
		return ""
	}
}

// configureDelegated sets up delegated transport agent state: DelegatedManager
// with all callbacks, model override, permissions, and exec registry. The
// agent's shared fields (compaction, warnings, etc.) are already set by
// setupAgent before this is called.
func configureDelegated(ag *agent.Agent, p setupParams, shared *sharedAgentSetup, backendName string, backendConfig config.BackendConfig) (finalizeParams, bool) {
	ag.Backend = backendName
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

	// Model for the backend — from backend_config, not from the group resolver.
	// Ladder: config value, then the backend's own default model.
	model := config.DerefStr(backendConfig.Model)
	if model == "" {
		model = backendDefaultModel(backendName)
	}

	// Work on a copy so global-default folding below doesn't mutate the
	// shared AgentConfig.BackendConfig (struct is a value type — assignment copies).
	bc := backendConfig

	// For Claude Code-family backends, fold global [cc_backend] settings
	// into the per-agent backend_config so both cctmux and ccstream
	// pick them up from the same keys. Non-CC backends (codex,
	// opencode, ...) are skipped so the keys don't leak into their
	// config surface.
	//
	// Folded keys (per-agent values always win):
	//   allowed_tools — merged (per-agent rules appended to global)
	//   binary — global default; per-agent override wins
	if backendName == "claude-code" || backendName == "claude-code-tmux" {
		merged := p.cfg.CCBackend.MergedAllowedTools(bc.AllowedTools)
		if merged != "" {
			bc.AllowedTools = strings.Split(merged, ",")
		}
		if bc.Binary == nil && p.cfg.CCBackend.Binary != "" {
			bc.Binary = &p.cfg.CCBackend.Binary
		}
	}

	// For opencode-backed agents, fold global [opencode_backend] settings
	// into the per-agent backend_config so the Backend reads them via
	// the same keys as ccstream does. Per-agent values always win.
	if backendName == "opencode" {
		if bc.Binary == nil {
			bc.Binary = &p.cfg.OpencodeBackend.Binary
		}
		if bc.Hostname == nil {
			bc.Hostname = &p.cfg.OpencodeBackend.Hostname
		}
		if bc.ServerAuth == nil {
			bc.ServerAuth = &p.cfg.OpencodeBackend.ServerAuth
		}
		if bc.LogLevel == nil {
			bc.LogLevel = &p.cfg.OpencodeBackend.LogLevel
		}
		if bc.Port == nil {
			bc.Port = &p.cfg.OpencodeBackend.Port
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

	// Build auto-approve rules from resolved config. bakedPerms is the same
	// frozen snapshot, reused below so the environment block's Command
	// Approval description matches what's actually baked into the rules.
	bakedPerms := p.resolved.Permissions // static-cfg:ignore: must match the frozen rules buildAutoApproveRules bakes into the CC session (no live-reload path of its own), see writeCommandApproval
	autoApproveRules := buildAutoApproveRules(p, registry.ExportedNames())

	// Per-agent environment block for delegated backends ("" when disabled).
	// Captured below by the SystemPromptFunc closure so it is re-prepended on
	// every per-session rebuild — NOT just baked into the static SystemPrompt.
	// The rebuild wins over the static prompt at each session start (#828/#706),
	// and that rebuild path previously dropped the env block (and with it the
	// "Foci Shell Tools" list), so CC agents never saw foci_todo etc.
	crontabCount := 0
	if p.resolved.Environment.Enabled { // static-cfg:ignore: startup-only gate for an expensive subprocess spawn
		crontabCount = countCrontabJobs()
	}
	buildEnv := func(sessionPlatform string) string {
		rc := p.resolvedLive.Load()
		if !rc.Environment.Enabled {
			return ""
		}
		return buildEnvironmentDelegated(p.acfg, p.configPath, p.cfg, rc, bakedPerms, crontabCount, p.plat.ActivePlatformNames(), registry.ExportedTools(), shared.promptSearchDirs, sessionPlatform)
	}
	systemPrompt := delegatedSystemPrompt(buildEnv(""), bs.SystemBlocks(), br.extraSystemBlocks)

	ag.Model = model

	// Wire DelegatedManager: lazy per-session Backend creation.
	connMgr := p.connMgr
	agentID := p.acfg.ID
	// Late-delivery fallback for the session router (#1068 Phase 1): resolve the
	// current delivering connection at Emit time, falling back to the agent's
	// primary when the session has no live connection of its own.
	ag.ResolveLateConn = func(sessionKey string) platform.Connection {
		conn, _ := route.ConnFor(connMgr, agentID, sessionKey, route.PolicyFallback)
		return conn
	}
	// Durable last-resort fallback for an adopted autonomous turn when
	// ResolveLateConn's normal (ownership-respecting) cascade comes up with
	// nothing at all: reach past chat-ownership routing straight to the
	// session's app registration, if it has one. Every send through it persists
	// durably before checking who's live (clutch #1350 follow-up) — see
	// app.DurableConnFor and Agent.DurableTurnSink's doc comments.
	ag.DurableTurnSink = func(sessionKey string) turnevent.Sink {
		conn := app.DurableConnFor(sessionKey)
		if conn == nil {
			return nil
		}
		return turn.NewSessionSink(conn, sessionKey, "autonomous")
	}
	// Resolve a session's messaging platform (telegram/app/discord) for the
	// per-session ## Platform block from the durable chat claim, so the prompt
	// (and its compaction-time fingerprint) never depends on connection
	// liveness. See platformForSession.
	sessionIdx := p.sessionIndex
	platformFor := func(sessionKey string) string {
		return platformForSession(sessionIdx, agentID, sessionKey)
	}
	// Parse idle timeout from config (default 3h; see agent.DefaultIdleTimeout).
	var idleTimeout time.Duration
	if bc.IdleTimeout != nil && *bc.IdleTimeout != "" {
		if d, err := time.ParseDuration(*bc.IdleTimeout); err == nil {
			idleTimeout = d
		}
	}

	// Automated re-login trigger (#843). Built once here so both the 401
	// auth-failure callback and the manual /login command invoke the same
	// path. The gate single-flights across all agents (shared OAuth
	// credential), so only the first caller launches the driver; a second
	// caller (concurrent 401, or /login while one is running) gets false.
	// ccstream-only: the driver drives a `claude /login` TUI in tmux, which
	// has no meaning for the API transport. The /login command is gated by
	// RequiresBackend; this field stays nil for cctmux so that command
	// reports "unavailable" rather than mis-driving the wrong backend.
	claudeBin := config.DerefStr(bc.Binary)
	workDir := p.acfg.Workspace
	triggerRelogin := func(reason, sessionKey string) bool {
		if !relogin.G.Start() {
			return false
		}
		log.NewComponentLogger("agent:"+agentID).Warnf("CC re-login starting: %s", reason)
		go relogin.Run(context.Background(), relogin.Config{
			AgentID:   agentID,
			WorkDir:   workDir,
			ClaudeBin: claudeBin,
			Gate:      relogin.G,
			SendMessage: func(text string) error {
				// Resolve via the triggering session key so the login URL reaches
				// the chat that asked for it. An empty key falls back to the
				// agent's primary bot (which owns the persisted default chat) —
				// NOT an arbitrary idle facet, now that BotForSession("") returns
				// nil. SendToSession reads the chat ID straight from the key, so
				// delivery works whichever bot resolves, even agent-less facets
				// (the #843 "no chat ID — no default chat configured" bug).
				conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
				if conn == nil {
					return fmt.Errorf("no connection for agent %s", agentID)
				}
				return conn.SendToSession(sessionKey, text)
			},
		})
		return true
	}
	if backendName == "claude-code" {
		ag.ReloginTrigger = triggerRelogin
	}

	// Shared across all Backends for this agent so main + facet sessions
	// don't each fire their own first-seen warning for the same account-wide
	// rate limit.
	rlThrottle := ccstream.NewRateLimitThrottle()

	ag.DelegatedManager = &agent.DelegatedManager{
		SessionIndex: p.sessionIndex,
		AgentID:      agentID,
		NewBackend: func() (delegator.Delegator, error) {
			cfgMap := bc.ToMap()
			cfgMap["foci_version"] = version
			be, err := delegator.New(backendName, cfgMap)
			if err != nil {
				return nil, err
			}
			if sb, ok := be.(*ccstream.Backend); ok {
				sb.SetRateLimitThrottle(rlThrottle)
				// On a 401, run the automated re-login (#843) via the shared
				// trigger built above (same path as the manual /login command).
				sb.SetOnAuthFailure(func(detail string) {
					// Auto-401 has no triggering chat; "" → the agent's default chat.
					triggerRelogin("401 auth failure: "+firstLine(detail), "")
				})
				// Surface CC's structured rate_limit_event directly to the
				// agent's default chat (i.e. to the human), formatted for a
				// person — NOT as a log warning. Deliberately off the generic
				// log.Warnf→WarningQueue→PROACTIVE-WARNINGS path so it reaches
				// the user instead of being injected into the agent's own
				// context. It reflects API utilization past a threshold, not a
				// block, so it does NOT gate periodic work (#1211/#1238).
				sb.SetOnRateLimited(func(notice string) {
					if conn := connMgr.Primary(agentID); conn != nil {
						conn.SendNotification(notice)
						log.NewComponentLogger("agent:" + agentID).Debugf("rate limit notice delivered to default chat")
					} else {
						log.NewComponentLogger("agent:"+agentID).Debugf("rate limit notice undelivered (no primary connection): %s", notice)
					}
				})
				// A CC session-limit message engages the rate-limit gate so
				// background/periodic work pauses until the window resets; the
				// gate's own hooks notify the user.
				sb.SetOnSessionLimit(func(signal ratelimit.Signal) {
					ag.EngageRateLimit(signal)
				})
			}
			// Inject account-state callbacks into opencode backends. Auth has
			// no automated relogin for v1 (it is per-provider), while usage
			// limits engage the same agent gate and notification hooks as the
			// API and Claude Code paths.
			if ob, ok := be.(*opencode.Backend); ok {
				ob.SetOnAuthFailure(func(detail string) {
					log.NewComponentLogger("agent:"+agentID).Warnf("opencode auth failure: %s", detail)
				})
				ob.SetOnRateLimited(func(signal ratelimit.Signal) {
					ag.EngageRateLimit(signal)
				})
			}
			// Deliver Codex config/runtime warnings to the user's chat as
			// system notifications — same pattern as ccstream's rate-limit
			// notices. Deliberately off the generic log→WarningQueue path
			// so they reach the human instead of being injected into the
			// agent's own context.
			if cb, ok := be.(*codex.Backend); ok {
				cb.SetOnModelCaps(func(entries map[string]modelcaps.Caps) {
					modelcaps.Publish(modelcaps.BackendCodex, entries)
				})
				cb.SetOnWarning(func(notice string) {
					if conn := connMgr.Primary(agentID); conn != nil {
						conn.SendNotification(notice)
						log.NewComponentLogger("agent:" + agentID).Debugf("codex warning delivered to default chat")
					} else {
						log.NewComponentLogger("agent:"+agentID).Debugf("codex warning undelivered (no primary connection): %s", notice)
					}
				})
			}
			return be, nil
		},
		StartOpts: delegator.StartOptions{
			WorkDir:      p.acfg.Workspace,
			SystemPrompt: systemPrompt,
			// Rebuild the prompt from disk at every session-start so a fresh
			// session (reset, idle-respawn, emulated compaction #828) picks up
			// character-file AND skill edits — instead of the prompt frozen at
			// setup (the #706 / #828 bug). bs.SystemBlocks() reflects in-place
			// Bootstrap.Reload(); ReloadSystemFn re-loads skills from disk
			// (side-effect-free — it does NOT mutate ag.ExtraSystemBlocks).
			// Falls back to the setup-time snapshot (systemPrompt) if it yields
			// empty. ag is a pointer; ReloadSystemFn is wired later in finalize,
			// but this closure only runs at runtime session-start, by when it's set.
			// The env block is re-prepended via newDelegatedSystemPromptFunc so it
			// survives this rebuild (the #828/#706 rebuild used to drop it).
			SystemPromptFunc: newDelegatedSystemPromptFunc(buildEnv, platformFor, func() (ws, extra []provider.SystemBlock) {
				// Re-read character files from disk so this fresh session reflects
				// edits since setup, independent of whether the caller reloaded
				// (the compaction-bounce path #828 does not). bs is shared
				// per-agent; Reload mutates it in place under its own lock.
				// ReloadSystemFn re-loads skills from disk too.
				bs.Reload()
				extra = br.extraSystemBlocks
				if ag.ReloadSystemFn != nil {
					if fresh, _ := ag.ReloadSystemFn(); fresh != nil {
						extra = fresh
					}
				}
				return bs.SystemBlocks(), extra
			}),
			Model: model,
			// Re-inject the session's effort at every launch so a bounce
			// (post-compaction reload, idle respawn) keeps the level the user
			// set via /effort — apply_flag_settings is runtime-only. Returns
			// "" when no override is set (CC uses the model default). (#840)
			EffortFunc: func(sk string) string { return ag.SessionEffort(sk) },
			ModelFunc:  func(sk string) string { return ag.SessionModel(sk) },
			// Drive opencode's internal compaction (/summarize) with foci's
			// compaction-summary.md — the SAME resolution the CC backend uses
			// (internal/agent/compaction.go) — instead of opencode's built-in
			// template, via the blank-system plugin's session.compacting hook.
			// Resolved fresh per Start (mirrors SystemPromptFunc); "" leaves
			// opencode's default compaction prompt untouched.
			CompactionPromptFunc: func(string) string {
				return prompts.ResolvePrompt(ag.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), ag.PromptSearchDirs...)
			},
			AgentID:          agentID,
			ExecRegistry:     registry,
			TmuxCols:         p.cfg.Tools.TmuxCols,
			TmuxRows:         p.cfg.Tools.TmuxRows,
			AutoApproveRules: autoApproveRules,
			// Prune threshold for tracked background tasks (subagents,
			// run_in_background Bash). Empty/invalid → 0, and the tracker falls
			// back to its 30m default. Unwedge backstop for the pending-work gate.
			SubagentMaxAge: func() time.Duration {
				d, _ := time.ParseDuration(p.cfg.CCBackend.BackgroundTaskMaxAge)
				return d
			}(),
			// claude_binary is read from the same merged map ccstream
			// consumes, but RunOnce doesn't see backendConfig — fold it
			// onto StartOpts so DelegatedManager.RunOnce honours it.
			ClaudeBinary: func() string {
				v := config.DerefStr(bc.Binary)
				return v
			}(),
			// Per-agent backend_config.env propagates to the backend
			// subprocess. Used by integration tests to inject CCSTUB_*
			// env vars per agent (e.g. one agent gets CCSTUB_HANG, others
			// don't). DelegatedManager.Get merges these with the exec
			// bridge's BASH_ENV/FOCI_SOCK so both layers survive.
			Env: bc.Env,
		},
		PermissionPromptFunc: func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
			resolve := connResolver(connMgr, sessionKey, agentID)
			conn := resolve()
			if conn == nil {
				log.NewComponentLogger("agent:"+agentID).Warnf("permission prompt: ForSessionOrPrimary returned nil for session=%s, prompt dropped", sessionKey)
				return
			}
			log.NewComponentLogger("agent:"+agentID).Debugf("permission prompt: sending via %s for session=%s summary=%q reqID=%s", conn.PlatformName(), sessionKey, summary, requestID)
			// Attachment (e.g. the full ExitPlanMode plan markdown) is sent as a
			// document before the keyboard so the user sees the content, then the
			// Allow/Deny buttons. A send failure is non-fatal — fall through to
			// the prompt so the permission gate still resolves.
			if attachmentPath != "" {
				if err := conn.SendDocument(attachmentPath, ""); err != nil {
					log.NewComponentLogger("agent:"+agentID).Warnf("permission prompt: SendDocument(%q) failed for session=%s: %v", attachmentPath, sessionKey, err)
				}
			}
			var buttons []platform.ButtonChoice
			for _, c := range choices {
				btn := platform.ButtonChoice{Label: c.Label, Data: c.Data}
				if c.Toggle != nil {
					btn.Toggle = &platform.ButtonToggle{
						ExtraBody: c.Toggle.ExtraBody,
						ShowLabel: c.Toggle.ShowLabel,
						HideLabel: c.Toggle.HideLabel,
					}
				}
				buttons = append(buttons, btn)
			}
			reqID := requestID // capture for closure
			// Use the requestID as the platform prompt ID so the cancel
			// listener (registered below) can find and edit this exact
			// message later if CC cancels the request before the user
			// responds. The resolver (not the conn grabbed above) is stored for
			// those later edits so they survive a reconnect.
			_, _ = platform.SendInteractiveMessageWithID(resolve, requestID, text, buttons, func(choice platform.ButtonChoice) string {
				log.NewComponentLogger("agent:"+agentID).Debugf("permission button pressed: sk=%s reqID=%s choice=%q", sessionKey, reqID, choice.Data)
				if err := ag.SendPermissionResponse(context.Background(), sessionKey, reqID, choice.Data); err != nil {
					log.NewComponentLogger("agent:"+agentID).Errorf("SendPermissionResponse failed: sk=%s reqID=%s choice=%q err=%v", sessionKey, reqID, choice.Data, err)
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
					log.NewComponentLogger("agent:"+agentID).Warnf("expire permission deny failed: sk=%s reqID=%s err=%v", sessionKey, reqID, err)
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
					log.NewComponentLogger("agent:"+agentID).Warnf("cancel interactive message: sk=%s reqID=%s err=%v", sessionKey, reqID, err)
				} else {
					log.NewComponentLogger("agent:"+agentID).Debugf("permission cancelled: sk=%s reqID=%s reason=%q", sessionKey, reqID, reason)
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
		SubagentStatusFunc: func(sessionKey, detail string) {
			// Subagent (CC Agent-tool) status has an app-native surface: the
			// conversation's unified Activity indicator (subagents kind). Route
			// straight to the app hub's binding; a no-op for non-app sessions.
			app.SetSubagentDetail(sessionKey, detail)
		},
		SystemNoticeFunc: func(sessionKey, text string) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				log.NewComponentLogger("agent:"+agentID).Warnf("system notice: no connection for session=%s, dropped: %s", sessionKey, text)
				return
			}
			if err := conn.SendText(text); err != nil {
				log.NewComponentLogger("agent:"+agentID).Warnf("system notice send failed for session=%s: %v", sessionKey, err)
			}
		},
		// Adopt CC-initiated runs as first-class foci turns — streaming, in-flight
		// tracking, accounting, and meta all flow through the normal turn path
		// (#1261).
		OpenAutonomousTurn: ag.OpenAutonomousTurn,
		AttachDelivery:     ag.AttachDelivery,
		IdleTimeout:        idleTimeout,
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
// firstLine returns the first line of s, for compact single-line logging of a
// multi-line error detail.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

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

// delegatedSystemPrompt assembles the single concatenated system-prompt string
// a CC-backend agent launches with: the per-agent environment block (envBlock,
// "" when disabled) prepended ahead of the workspace identity + skills blocks.
// Both the static setup-time prompt and the per-session SystemPromptFunc
// rebuild route through here so the environment block (and its "Foci Shell
// Tools" list) is present on every session — the rebuild wins over the static
// prompt at each start (#828/#706) and used to drop the env block.
func delegatedSystemPrompt(envBlock string, workspaceBlocks, extraBlocks []provider.SystemBlock) string {
	base := buildDelegatedSystemPrompt(workspaceBlocks, extraBlocks)
	switch {
	case envBlock == "":
		return base
	case base == "":
		return envBlock
	default:
		return envBlock + "\n\n" + base
	}
}

// newDelegatedSystemPromptFunc builds the StartOptions.SystemPromptFunc closure
// that rebuilds a delegated agent's prompt from disk at every session start
// (#828/#706 — so character-file and skill edits take effect on a fresh
// session). reload re-reads the workspace and skill blocks; the captured
// envBlock is re-prepended each time so the environment context survives the
// rebuild. This closure's result wins over the static StartOptions.SystemPrompt
// whenever non-empty (see delegated_manager.go), which is why the env block
// MUST be re-applied here and not only baked into the static prompt.
// newDelegatedSystemPromptFunc returns the per-session prompt generator. buildEnv
// rebuilds the environment block for a given session platform (so the ## Platform
// block matches the session's messaging platform); platformFor resolves that
// platform from the session key.
func newDelegatedSystemPromptFunc(buildEnv func(sessionPlatform string) string, platformFor func(sessionKey string) string, reload func() (workspaceBlocks, extraBlocks []provider.SystemBlock)) func(sessionKey string) string {
	return func(sessionKey string) string {
		envBlock := buildEnv(platformFor(sessionKey))
		ws, extra := reload()
		return delegatedSystemPrompt(envBlock, ws, extra)
	}
}

// buildAutoApproveRules assembles the foci-level auto-approve rules for a
// delegated backend from resolved config + workspace-scoped defaults.
//
// fociExecNames is the list of foci shell-function names exported by the
// agent's tool registry (e.g. "foci_todo", "foci_remind"). These get an
// always-on auto-approve rule via FociShellRulesFor — no toggle, since
// they're foci's own constrained wrappers.
func buildAutoApproveRules(p setupParams, fociExecNames []string) []string {
	perms := p.resolved.Permissions // static-cfg:ignore: baked into the CC backend's session config once, no live-reload path exists for it

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

	// Shared infrastructure that mirrors the API path's tool wiring. Build once
	// at registry construction so every tool gets the same agent-scoped store,
	// async notifier, and resolved TTS as their API-path counterparts.
	var agentStore *secrets.Store
	if p.store != nil {
		agentStore = p.store.ForAgent(acfg.ID)
	}
	var notifier *tools.AsyncNotifier
	if agLazy != nil {
		notifier = newAsyncNotifier(agLazy, acfg.ID, p.agentResolverFn, p.ctx, connMgr)
	}
	ttsRepls := voice.MergeReplacements(p.cfg.Voice.TTSReplacements, acfg.Voice.TTSReplacements)
	resolvedLive := p.resolvedLive
	agentTTS := func() voice.TTS {
		vc := resolvedLive.Load().Voice
		return resolveTTS(p.ttsMap, p.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls)
	}

	// Delegated agents shell out to `claude --print` (CLISummariser) for the
	// summary tool, routing through the parent CC subprocess's subscription auth
	// so the call charges mana, not API spend. (API agents use APISummariser.)
	cliSummariser := tools.NewCLISummariser("", "haiku", func() int { return p.resolvedLive.Load().Summary.MaxSummaryInputChars })

	// Register the exec-exported subset from the single data-driven table (see
	// tool_table.go) — the same source of truth that drives the API path. The
	// table's pathExec filter selects exactly the exec-bridge tools.
	out := &toolOutputs{}
	registerTools(&toolDeps{
		p:             p,
		path:          pathExec,
		registry:      registry,
		agentStore:    agentStore,
		notifier:      notifier,
		connMgr:       connMgr,
		agLazy:        agLazy,
		summariser:    cliSummariser,
		wakeFn:        wakeScheduleFn,
		sessionNotify: newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr, "session_notify"),
		askDeliver:    newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr, "ask_grader"),
		agentTTS:      agentTTS,
		out:           out,
	})
	if agLazy != nil {
		if a := agLazy(); a != nil {
			a.AskRouter = out.askRouter
			// Mirror the API path (agents.go), which sets AsyncNotifier on the
			// agent. The delegated path built the notifier and wired it into
			// tools but never stored it here, leaving a.AsyncNotifier nil. That
			// disabled two things for delegated agents: the /plan EnterPlanMode
			// injection (command/plan.go) and the #845 compaction-resume nudge,
			// which is invoked from runDelegatedCompact (compaction.go:199) but
			// suppressed by the nil guard. notifier is non-nil here (agLazy !=
			// nil, so it was constructed above).
			a.AsyncNotifier = notifier
		}
	}

	log.NewComponentLogger("agent:"+acfg.ID).Infof("exec bridge registry: %d tools (%v)", len(registry.All()), registry.ExportedNames())
	return registry
}
