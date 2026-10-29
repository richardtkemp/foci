package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/delegator"
	mcpkg "foci/internal/mcp"
	"foci/internal/mermaid"
	"foci/internal/platform"
	"foci/internal/procx"
	"foci/internal/provider"
	"foci/internal/route"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/tools/browser"
	"foci/internal/tools/shell"
	"foci/internal/tools/tmux"
	"foci/internal/voice"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// toolPath selects which registration path(s) a tool participates in: the API
// agent loop, the delegated/CC exec bridge, or both.
type toolPath uint8

const (
	pathAPI toolPath = 1 << iota
	pathExec
	pathBoth = pathAPI | pathExec
)

// toolDeps bundles everything any tool constructor needs (a superset across all
// tools and both paths). Fields only relevant to one path are left zero on the
// other — the tools that need them don't run there. Per-path "prep" (agentStore,
// notifier, summariser, etc.) is computed at each call site before registerTools.
type toolDeps struct {
	p            setupParams
	path         toolPath
	registry     *tools.Registry
	agentStore   *secrets.Store
	notifier     *tools.AsyncNotifier
	connMgr      platform.ConnectionManager
	agLazy       func() *agent.Agent
	summariser   tools.Summariser // APISummariser (API) or BatchSummariser (delegated)
	wakeFn       tools.ScheduleWakeFn
	wakeCancelFn tools.CancelWakeFn

	sessionNotify  tools.SessionNotifyFn // send_to_session → via=agent
	askDeliver     tools.AskDeliverFn    // ask system notices (expiry/restore) → via=ask-grader
	askUserDeliver tools.AskDeliverFn    // ask user-input deliveries (answers, cancel) → user path, via=ask-grader (#2233)
	agentTTS       func() voice.TTS
	blockedPaths   []config.BlockedPath // API-only (write/edit)

	// API-only deps for spawn.
	client           provider.Client
	groupResolver    *config.GroupResolver
	fallbackFn       provider.FallbackFunc
	bootstrap        *workspace.Bootstrap
	promptSearchDirs []string
	resolvedModel    string
	defaultFormat    string

	out *toolOutputs

	// spawner is the state spawn, spawn_send and spawn_status share (one
	// semaphore, one agent getter), built on first use by d.getSpawner.
	spawner *tools.Spawner

	// depth is the depth-mode manager the depth_* tools share (#2588), built
	// on first use by d.getDepth.
	depth *tools.DepthManager
}

// toolOutputs captures the non-tool side-products of registration that the
// orchestrator consumes downstream.
type toolOutputs struct {
	tmuxTool       *tools.Tool
	tmuxClearAll   func()
	tmuxWatchCount func() int
	serverTools    []provider.ToolDef
	mcpMgr         *mcpkg.Manager
	askRouter      *tools.AskRouter
	spawner        *tools.Spawner      // nil when the agent has no spawn tool
	depth          *tools.DepthManager // nil when depth mode is off for the agent (#2588)
}

// toolEntry is one row of the registration table.
type toolEntry struct {
	name    string
	paths   toolPath
	enabled func(*toolDeps) bool        // nil = always enabled
	build   func(*toolDeps) *tools.Tool // returns the tool to register (nil = none); may write d.out
}

// registerTools is the single registration driver for both the API and exec
// (delegated) paths. It is the one source of truth for which tools exist, on
// which path, and under what condition — replacing the old hand-maintained
// registerCoreTools/registerWebTools/... functions AND buildExecRegistry's
// parallel list.
//
// The agent's tool access lists (allow_tools / deny_tools, #2278, resolved in
// config.ResolvedTool.ToolPermitted) are applied here, BEFORE enabled and
// build: a withheld row is never constructed, so it has no side effects
// (no server tool, browser pool or MCP manager) and is absent from everything
// derived from the registry — the API tools array, the shell tool's foci_*
// list, spawned sub-agents, the exec bridge's functions and dispatch,
// FociShellRules and the delegated environment block.
func registerTools(d *toolDeps) {
	for _, e := range toolTable {
		if e.paths&d.path == 0 {
			continue
		}
		if !d.p.resolved.Tools.ToolPermitted(e.name) { // static-cfg:ignore: tools are built once at startup; the access lists need a restart (#2278)
			continue
		}
		if e.enabled != nil && !e.enabled(d) {
			continue
		}
		if t := e.build(d); t != nil {
			d.registry.Register(t)
		}
	}
}

func tmuxAvailable(*toolDeps) bool {
	// Operator: matches the population internal/tools/tmux spawns under.
	_, err := procx.LookPath(procx.Operator, "tmux")
	return err == nil
}

// execExtraEnv builds the FOCI_ADDR / FOCI_GW_SOCK env injected into exec-tool
// subprocesses so agents can run foci CLI commands without sourcing vars.
func execExtraEnv(p setupParams) []string {
	var env []string
	if p.cfg.HTTP.Port > 0 {
		bind := p.cfg.HTTP.Bind
		if bind == "" || bind == "0.0.0.0" {
			bind = "127.0.0.1"
		}
		env = append(env, fmt.Sprintf("FOCI_ADDR=%s:%d", bind, p.cfg.HTTP.Port))
	}
	if p.gwSocketPath != "" {
		env = append(env, "FOCI_GW_SOCK="+p.gwSocketPath)
	}
	return env
}

// toolTable's row order decides registration order only; it is otherwise
// cosmetic. The model's tool-list order — and so the prompt-cache prefix —
// comes from Registry.ToolDefs(), which sorts by name, and the exec bridge's
// shell-functions file likewise iterates Registry.All(), also name-sorted —
// so this order reaches neither the API tools array nor the exec bridge.
var toolTable = []toolEntry{
	{name: "shell", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		live := d.p.resolvedLive
		return shell.NewExecTool(d.agentStore, d.p.bwStore, func() int { return live.Load().Tools.ExecAutoBackground }, d.notifier,
			d.p.acfg.Workspace, d.registry, func() int64 { return int64(live.Load().Summary.MaxResultChars) }, d.p.cfg.Tools.TempDir,
			execExtraEnv(d.p), d.p.cfg.Tools.ExecDefaultTimeout)
	}},

	// tmux settings stay baked (not Bucket-C-converted): autopilot/watchSec/ttl
	// drive an already-running watch loop and session timer, so a live swap
	// needs timer-reset logic, not just a value read — out of scope here.
	{name: "tmux", paths: pathAPI, enabled: tmuxAvailable, build: func(d *toolDeps) *tools.Tool {
		tc := d.p.resolved.Tools // static-cfg:ignore: see comment above
		watchSec := 30
		if dur, err := time.ParseDuration(tc.TmuxWatchThreshold); err == nil {
			watchSec = int(dur.Seconds())
		}
		var ttl time.Duration
		if tc.TmuxSessionTTL != "0" {
			if dur, err := time.ParseDuration(tc.TmuxSessionTTL); err == nil {
				ttl = dur
			}
		}
		wc, t, clear := tmux.NewTmuxTool(d.p.cfg.Tools.TmuxCols, d.p.cfg.Tools.TmuxRows,
			d.notifier, d.p.sessionIndex, d.p.acfg.ID, tc.TmuxAutopilot, watchSec, ttl, "")
		d.out.tmuxWatchCount, d.out.tmuxTool, d.out.tmuxClearAll = wc, t, clear
		return t
	}},

	// browser.enabled decides whether the tool gets registered at all, and
	// bc feeds a whole browser-automation manager (opens a real browser
	// process) — construction-time only, like tmux above.
	//
	// Both paths (#1600): delegated backends reach it as foci_browser. The
	// pool gives every session (main chat, forks, branches, other chats) its
	// own browser rather than one per agent.
	{name: "browser", paths: pathBoth, enabled: func(d *toolDeps) bool { return d.p.resolved.Browser.Enabled }, // static-cfg:ignore: see comment above
		build: func(d *toolDeps) *tools.Tool {
			bc := d.p.resolved.Browser // static-cfg:ignore: see comment above
			// Default the persistent profile to a per-agent dir under the
			// workspace's gitignored .data/ (alongside the agent's databases),
			// so non-incognito sessions retain state without colliding with
			// other agents. An explicitly-configured UserDataDir overrides this.
			if bc.UserDataDir == "" {
				bc.UserDataDir = filepath.Join(d.p.acfg.Workspace, ".data", "browser-profile")
			}
			fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
			return browser.NewSessionBrowserTool(browser.NewSessionPool(&bc, fileMode))
		}},

	{name: "read", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		live := d.p.resolvedLive
		return tools.NewReadTool(d.agentStore, d.p.acfg.Workspace, func() int64 { return live.Load().Tools.MaxFileReadBytes })
	}},
	{name: "write", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		return tools.NewWriteTool(d.agentStore, d.p.acfg.Workspace, d.blockedPaths, fileMode)
	}},
	{name: "edit", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		live := d.p.resolvedLive
		return tools.NewEditTool(d.agentStore, d.p.acfg.Workspace, d.blockedPaths, fileMode,
			func() int64 { return live.Load().Tools.MaxFileReadBytes })
	}},

	{name: "summary", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		return tools.NewSummaryTool(d.agentStore, d.summariser, d.p.acfg.Workspace)
	}},

	{name: "http_request", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		live := d.p.resolvedLive
		return tools.NewHTTPRequestTool(d.agentStore, d.p.bwStore, d.p.cfg.Tools.TempDir,
			func() int { return live.Load().Tools.ExecAutoBackground },
			func() int64 { return live.Load().Tools.MaxUploadFileSize },
			func() int64 { return live.Load().Tools.HTTPMaxSpillBytes },
			func() int64 { return int64(live.Load().Summary.MaxResultChars) },
			d.notifier, fileMode)
	}},

	// SearchProvider/FetchProvider stay baked: they pick WHICH tool object gets
	// registered (server-tool vs. brave/fetch), not a scalar within one — a
	// live swap means re-registering the tool, out of scope here.
	{name: "web_search", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		if d.path == pathAPI && d.p.resolved.Tools.SearchProvider == "anthropic" { // static-cfg:ignore: see comment above
			d.out.serverTools = append(d.out.serverTools, buildServerTool("web_search_20250305", "web_search",
				d.p.cfg.Tools.WebSearchMaxUses, d.p.cfg.Tools.WebSearchAllowedDomains, d.p.cfg.Tools.WebSearchBlockedDomains))
			return nil
		}
		// brave (API only when provider=="brave"; exec path always uses brave)
		if (d.path == pathExec || d.p.resolved.Tools.SearchProvider == "brave") && d.p.braveKey != "" { // static-cfg:ignore: see comment above
			return tools.NewWebSearchTool(d.p.braveKey)
		}
		return nil
	}},

	{name: "web_fetch", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		if d.path == pathAPI && d.p.resolved.Tools.FetchProvider == "anthropic" { // static-cfg:ignore: see comment above web_search entry
			d.out.serverTools = append(d.out.serverTools, buildServerTool("web_fetch_20250910", "web_fetch",
				d.p.cfg.Tools.WebFetchMaxUses, d.p.cfg.Tools.WebFetchAllowedDomains, d.p.cfg.Tools.WebFetchBlockedDomains))
			return nil
		}
		return tools.NewWebFetchTool()
	}},

	{name: "memory_search", paths: pathBoth, enabled: func(d *toolDeps) bool { return len(d.p.memBackends) > 0 },
		build: func(d *toolDeps) *tools.Tool {
			live := d.p.resolvedLive
			return tools.NewMemorySearchTool(d.p.memBackends, func() string { return live.Load().MemorySearch.SearchBackend }, d.p.convReader)
		}},

	{name: "scratchpad", paths: pathAPI, enabled: func(d *toolDeps) bool { return d.p.scratchpadStore != nil },
		build: func(d *toolDeps) *tools.Tool { return tools.NewScratchpadTool(d.p.scratchpadStore, d.p.acfg.ID) }},

	{name: "todo", paths: pathBoth, enabled: func(d *toolDeps) bool { return d.p.todoStore != nil },
		build: func(d *toolDeps) *tools.Tool { return tools.NewTodoTool(d.p.todoStore, d.p.acfg.ID) }},

	{name: "task_list", paths: pathAPI, enabled: func(d *toolDeps) bool { return d.p.taskListStore != nil },
		build: func(d *toolDeps) *tools.Tool {
			return tools.NewTaskListTool(d.p.taskListStore, d.p.acfg.ID, func(sk, msg string) {
				ag := d.agLazy()
				for _, fn := range ag.TaskListNotifyFunc {
					fn(sk, msg)
				}
			})
		}},

	{name: "bitwarden_search", paths: pathAPI, enabled: func(d *toolDeps) bool { return d.p.bwStore != nil },
		build: func(d *toolDeps) *tools.Tool { return tools.NewBitwardenSearchTool(d.p.bwStore) }},
	{name: "bitwarden_unlock", paths: pathAPI, enabled: func(d *toolDeps) bool { return d.p.bwStore != nil },
		build: func(d *toolDeps) *tools.Tool { return tools.NewBitwardenUnlockTool(d.p.bwStore) }},

	{name: "mcp", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		// The manager is always created (and returned via out.mcpMgr); only the
		// tool is conditional on mcp.toml existing.
		mgr := mcpkg.NewManagerForAgent(filepath.Dir(d.p.configPath), d.p.acfg.ID)
		d.out.mcpMgr = mgr
		return mgr.Tool() // nil → not registered
	}},

	{name: "send_to_chat", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		return tools.NewSendToChatTool(func(sessionKey string) platform.Sender {
			conn := d.connMgr.ForSessionOrPrimary(sessionKey, d.p.acfg.ID)
			if conn == nil {
				return nil
			}
			return conn
		}, d.agentTTS, func(sessionKey string) session.SessionType {
			entry, err := d.p.sessionIndex.Get(sessionKey)
			if err != nil {
				return session.SessionTypeUnknown
			}
			return entry.SessionType
		}, sendToChatMermaid(d.p.cfg))
	}},

	{name: "send_to_session", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		// Loose targets resolve through the shared route ladder. Create is
		// disabled for named sessions (the tool addresses existing ones), but
		// CreateDefault is wired: a bare agent name whose only sessions are
		// archived mints a fresh visible conversation rather than failing to
		// deliver into a hidden one.
		idx := d.p.sessionIndex
		resolveKeyFn := func(target string) (string, string, error) {
			resolver := &route.Resolver{
				Index:             idx,
				PreferredPlatform: d.p.cfg.DefaultPlatformFor,
				CreateDefault:     app.CreateDefaultConversation,
			}
			// Literal-first: an alias containing '?' is matched whole before
			// the '?' is read as options (#2158).
			t, err := resolver.ParseTarget(target)
			if err != nil {
				return "", "", err
			}
			t.Create = false
			res, err := resolver.Resolve(t)
			if err != nil {
				return "", "", err
			}
			return res.SessionKey, string(res.Rung), nil
		}
		return tools.NewSendToSessionTool(d.p.sessions, d.notifier, d.sessionNotify, resolveKeyFn, func(callerSessionKey, targetAgent string) {
			// reply_to=caller waits for the target's reply — surface it on the
			// caller's app conversation as a "waiting on <agent>" indicator
			// (cleared when the caller's reply turn begins). No-op off the app.
			app.SetWaiting(callerSessionKey, targetAgent)
		}, func(sessionKey string) session.SessionType {
			entry, err := d.p.sessionIndex.Get(sessionKey)
			if err != nil {
				return session.SessionTypeUnknown
			}
			return entry.SessionType
		})
	}},

	{name: "ask", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		chats := d.askChats()
		t, router := tools.NewAskTool(
			newAskPresentFn(d.p.acfg.ID, d.connMgr, chats),
			newAskRestoreFn(d.p.acfg.ID, d.connMgr, chats),
			d.askDeliver,
			func(msgID, finalText string) { _ = platform.CancelInteractiveMessage(msgID, finalText) },
			d.p.sessionIndex, d.p.acfg.ID,
			// The user-input delivery route (#2233): answer batches, late
			// answers and the cancel notice dispatch like typed messages.
			tools.WithUserInputDeliver(d.askUserDeliver),
			tools.WithBatchPresent(newAskPresentBatchFn(d.p.acfg.ID, d.connMgr, chats)),
			tools.WithBatchRestore(newAskRestoreBatchFn(d.p.acfg.ID, d.connMgr, chats)),
			tools.WithRemove(newAskRemoveFn(d.p.acfg.ID, d.connMgr, chats)),
			// Quiz reveals (#1316): after a chat user answers a quiz question,
			// post its withheld answer+explanation to the asking session's chat.
			tools.WithReveal(newAskRevealFn(d.p.acfg.ID, d.connMgr, chats)),
			// Per-ask (#1711 ruling 5): release only what THIS ask was holding,
			// so a sibling ask's backlog stays deferred (#1712).
			tools.WithOnResolve(func(sk, reqID string) {
				if ag := d.agLazy(); ag != nil {
					ag.DrainDeferredInjects(sk, reqID)
				}
			}),
			// Gate the trivial "ask cancelled" injection away from cold-cache
			// turns: only deliver it when the session's prompt cache is still
			// live (expiry in the future), so a stale/expired cancel doesn't
			// rebuild the whole cached prefix for a notice the agent's
			// already-ended asking turn doesn't need. (#1302)
			tools.WithCacheWarm(func(sk string) bool {
				ag := d.agLazy()
				if ag == nil {
					return true // can't resolve the agent → don't suppress
				}
				now := time.Now()
				return ag.CacheExpiry(sk, now).After(now)
			}))
		d.out.askRouter = router
		return t
	}},

	// spawn on BOTH paths (#2523). API agents get every mode, through foci's
	// own one-shot loop. A delegated agent's one-shot modes run as batches on
	// its own backend, never through model groups (tools.DelegatedSpawn); it
	// gets the modes its backend declares (tools.DelegatedSpawnModes): clone
	// needs a fork, raw an isolated batch, explore a read-only one.
	// spawn_send and spawn_status steer and check on clone spawns, so they
	// need clone's fork.
	{name: "spawn", paths: pathBoth, enabled: spawnEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getSpawner().SpawnTool()
	}},
	{name: "spawn_send", paths: pathBoth, enabled: spawnEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getSpawner().SendTool()
	}},
	{name: "spawn_status", paths: pathBoth, enabled: spawnEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getSpawner().StatusTool()
	}},

	// Depth mode (#2588), experimental: fan an app conversation out into leaf
	// branches answered one at a time. Off unless [depth] enabled (depthEnabled).
	{name: "depth_fanout", paths: pathBoth, enabled: depthEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getDepth().Tools()[0]
	}},
	{name: "depth_report", paths: pathBoth, enabled: depthEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getDepth().Tools()[1]
	}},
	{name: "depth_claims", paths: pathBoth, enabled: depthEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getDepth().Tools()[2]
	}},
	{name: "depth_status", paths: pathBoth, enabled: depthEnabled, build: func(d *toolDeps) *tools.Tool {
		return d.getDepth().Tools()[3]
	}},

	{name: "remind", paths: pathBoth,
		enabled: func(d *toolDeps) bool { return d.p.reminderStore != nil && d.wakeFn != nil },
		build: func(d *toolDeps) *tools.Tool {
			// The lazy agent getter keeps this nil-safe: the tool is built
			// before the agent exists on some paths, and the predicate is
			// consulted per call (#2233).
			return tools.NewRemindTool(d.p.reminderStore, d.p.acfg.ID, d.wakeFn, d.wakeCancelFn,
				tools.WithSessionHeld(func(sk string) bool {
					ag := d.agLazy()
					return ag != nil && ag.SystemInputHeld(sk)
				}))
		}},

	{name: "app_android", paths: pathBoth,
		enabled: func(d *toolDeps) bool { return d.p.cfg.Platform("app") != nil },
		build: func(d *toolDeps) *tools.Tool {
			return tools.NewAppAndroidTool(func() (tools.AppInvoker, bool) {
				conn := d.connMgr.Primary(d.p.acfg.ID)
				if conn == nil {
					return nil, false
				}
				inv, ok := conn.(tools.AppInvoker)
				return inv, ok
			})
		}},

	// set_session_alias: lets the agent name its own conversation. Only
	// registered for backends that DON'T name their own sessions (their Spec
	// lacks thread_naming: CC, opencode, API). Codex generates names
	// server-side via TurnResult.ThreadName.
	{name: "set_session_alias", paths: pathBoth,
		enabled: func(d *toolDeps) bool {
			s, _ := delegator.SpecFor(d.p.acfg.Backend)
			return !s.Supports(delegator.CapThreadNaming)
		},
		build: func(d *toolDeps) *tools.Tool {
			return tools.NewSetSessionAliasTool(d.p.sessionIndex, app.NotifyAliasChanged)
		}},

	// close_session: the agent closes the app chat it is running in, the same
	// open-set removal as the user's Close chat (#2573), optionally marking it
	// read at the turn's end (mark_read, #2592). Gated like
	// app_android: there is no open/closed state without the app platform.
	{name: "close_session", paths: pathBoth,
		enabled: func(d *toolDeps) bool { return d.p.cfg.Platform("app") != nil },
		build: func(d *toolDeps) *tools.Tool {
			return tools.NewCloseSessionTool(app.CloseSessionConversations, app.MarkSessionRead)
		}},

	// whoami: read-only identity of the calling session (agent, session key,
	// chat, platform, backend, transport, model) — the facts agents otherwise
	// guess, like the chat id send_to_session addressing needs (#1135).
	// The row's position here is cosmetic: ToolDefs() sorts by name, so whoami
	// reaches the API tools array between web_search and write wherever it
	// sits in this table. The nil guards cover minimalSetupParams-based tests
	// (nil sessionIndex, literal-nil agLazy); the lookups stay per-call so
	// a /model switch is reflected.
	{name: "whoami", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		return tools.NewWhoamiTool(tools.WhoamiDeps{
			AgentID:   d.p.acfg.ID,
			Backend:   d.p.acfg.Backend,
			Delegated: d.p.acfg.IsDelegated(),
			PlatformForChat: func(agentID string, chatID int64) string {
				if d.p.sessionIndex == nil {
					return ""
				}
				return d.p.sessionIndex.PlatformForChat(agentID, chatID)
			},
			SessionModel: func(sessionKey string) string {
				if d.agLazy == nil {
					return ""
				}
				if ag := d.agLazy(); ag != nil {
					return ag.ReportedSessionModel(sessionKey)
				}
				return ""
			},
		})
	}},
}

// sendToChatMermaid is the renderer send_to_chat uses for a mermaid file
// (#2195): the same process-wide instance the app's /app/render/mermaid uses,
// configured by [platforms.app] mermaid_cmd / mermaid_browser (mmdc on PATH
// when there is no app platform).
func sendToChatMermaid(cfg *config.Config) *mermaid.Renderer {
	if cfg == nil {
		return mermaid.Shared("", "", "")
	}
	var cmd, browser string
	if p := cfg.Platform("app"); p != nil && p.App != nil {
		cmd, browser = p.App.MermaidCmd, p.App.MermaidBrowser
	}
	return mermaid.Shared(cfg.DataDir, cmd, browser)
}

// spawnEnabled gates the spawn, spawn_send and spawn_status rows: always on
// the API path; on the exec path for any registered delegated backend, which
// can run at least character mode, directly or in the background (#2643: a
// background character spawn needs no fork, so spawn_send/spawn_status no
// longer need CapBranch). Decided from the backend's static Spec: the
// agent's DelegatedManager does not exist yet when the exec registry is built.
func spawnEnabled(d *toolDeps) bool {
	if d.path == pathAPI {
		return true
	}
	_, ok := delegator.SpecFor(d.p.acfg.Backend)
	return ok
}

// setAPICharacterModel puts an API agent's background character branch on
// the model a direct character spawn would run on: the named group, else the
// spawn_character call site's group, else (neither resolves) the agent's own
// model, which needs no override. A persisted session override, so a resumed
// run after a restart keeps it.
func (d *toolDeps) setAPICharacterModel(ag *agent.Agent, branchKey, group string) {
	gr := d.groupResolver
	if gr == nil {
		return
	}
	var r *config.ResolvedModel
	if group != "" {
		r = gr.ResolveGroup(group)
	}
	if r == nil {
		r = gr.ResolveCall(config.CallSpawnCharacter)
	}
	if r == nil {
		return
	}
	var client provider.Client
	if d.p.clientProvider != nil {
		client = d.p.clientProvider.GetClient(r.Endpoint, r.Format)
	}
	ag.SetSessionModel(branchKey, r.Developer+"/"+r.ModelID, r.Endpoint, r.Format, client)
}

// getSpawner builds the path's Spawner once. MaxConcurrentSpawns stays baked:
// it sizes the semaphore at construction, which can't be live-resized without
// a redesign. MaxToolLoops/ExploreMaxDepth are read fresh per spawn call.
func (d *toolDeps) getSpawner() *tools.Spawner {
	if d.spawner != nil {
		return d.spawner
	}
	acfg := d.p.acfg
	fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
	tc := d.p.resolved.Tools // static-cfg:ignore: MaxConcurrentSpawns sizes a semaphore channel at construction, can't be live-resized without a redesign
	live := d.p.resolvedLive
	orientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, d.p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	spawnDeps := tools.SpawnDeps{
		Registry:            d.registry,
		Sessions:            &sessionBranchAdapter{store: d.p.sessions, ag: d.agLazy},
		AgentID:             acfg.ID,
		MaxInherit:          tc.MaxConcurrentSpawns,
		Notifier:            d.notifier,
		OrientationTemplate: prompts.ResolveOrientationTemplate(orientPath, false, d.promptSearchDirs...),
		SetNoCompact:        func(sk string, v bool) { d.agLazy().SetSessionNoCompact(sk, v) },
		FileMode:            fileMode,
		Store:               d.p.store,
		Tracker:             tools.Spawns,
		Observer:            app.SpawnObserver{},
		ModelFor: func(sk string) string {
			if d.agLazy == nil {
				return ""
			}
			if ag := d.agLazy(); ag != nil {
				return ag.SessionModel(sk)
			}
			return ""
		},
		CanResume: func(bk string) bool {
			if d.agLazy == nil {
				return false
			}
			ag := d.agLazy()
			return ag != nil && ag.HasResumableSession(bk)
		},
		TakeUndelivered: func(bk string) []string {
			if d.agLazy == nil {
				return nil
			}
			if ag := d.agLazy(); ag != nil && ag.DelegatedManager != nil {
				return ag.DelegatedManager.TakeUndelivered(bk)
			}
			return nil
		},
		Notice: func(sk, text string) {
			if d.connMgr == nil {
				return
			}
			if conn, _ := route.ConnFor(d.connMgr, acfg.ID, sk, route.PolicyFallback); conn != nil {
				conn.SendNotification(text)
			}
		},
		// #2643: a background character spawn's fresh branch.
		CharacterBranch: func(_ context.Context, parent, model string) (string, error) {
			if d.agLazy == nil {
				return "", fmt.Errorf("agent not available")
			}
			ag := d.agLazy()
			if ag == nil {
				return "", fmt.Errorf("agent not available")
			}
			bk, err := ag.NewCharacterSpawnBranch(parent, model)
			if err != nil {
				return "", err
			}
			if d.path == pathAPI {
				d.setAPICharacterModel(ag, bk, model)
			}
			return bk, nil
		},
		PrepareCharacterBranch: func(bk, model string) {
			if d.agLazy == nil {
				return
			}
			if ag := d.agLazy(); ag != nil {
				ag.PrepareCharacterSpawnBranch(bk, model)
			}
		},
	}
	if d.path == pathExec {
		// A delegated agent: one-shots run as batches on its own backend.
		// The API one-shot fields (clients, model groups, fallbacks,
		// routing, the API tool-loop budgets) stay unset (#spawn-oneshot).
		spec, _ := delegator.SpecFor(acfg.Backend)
		spawnDeps.Delegated = &tools.DelegatedSpawn{
			Spec: spec,
			Runner: func() tools.BatchRunner {
				if d.agLazy == nil {
					return nil
				}
				if ag := d.agLazy(); ag != nil && ag.DelegatedManager != nil {
					return ag.DelegatedManager
				}
				return nil
			},
		}
	} else {
		spawnDeps.Client = d.client
		spawnDeps.ClientProvider = d.p.clientProvider
		spawnDeps.Bootstrap = d.bootstrap
		spawnDeps.GroupResolver = d.groupResolver
		spawnDeps.FallbackFunc = d.fallbackFn
		spawnDeps.FallbackModel = d.resolvedModel
		spawnDeps.FallbackFormat = d.defaultFormat
		spawnDeps.ProviderRoutingFor = config.RoutingFor(modelDefaultsFn(d.p.cfg.Models))
		spawnDeps.MaxToolLoops = func() int { return live.Load().Loop.MaxToolLoops }
		spawnDeps.ExploreMaxDepth = func() int { return live.Load().Tools.ExploreMaxDepth }
	}
	if depthEnabled(d) {
		// #2624: a reported depth leaf's spawns pass up its tree at each
		// fan-in, held until then.
		spawnDeps.Route = d.getDepth().SpawnRoute
	}
	d.spawner = tools.NewSpawner(spawnDeps, func() tools.SpawnAgent {
		// A nil *agent.Agent must stay a nil interface, or the callers'
		// "agent not available" check could never fire.
		if d.agLazy == nil {
			return nil
		}
		if ag := d.agLazy(); ag != nil {
			return ag
		}
		return nil
	})
	tools.RegisterSpawner(acfg.ID, d.spawner) // #2632: backend prompts route a spawn's to its owner
	if d.out != nil {
		d.out.spawner = d.spawner
	}
	return d.spawner
}
