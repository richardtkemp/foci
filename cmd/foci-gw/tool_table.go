package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/config"
	mcpkg "foci/internal/mcp"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/route"
	"foci/internal/secrets"
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
	p          setupParams
	path       toolPath
	registry   *tools.Registry
	agentStore *secrets.Store
	notifier   *tools.AsyncNotifier
	connMgr    platform.ConnectionManager
	agLazy     func() *agent.Agent
	summariser tools.Summariser // APISummariser (API) or CLISummariser (delegated)
	wakeFn     tools.ScheduleWakeFn

	sessionNotify tools.SessionNotifyFn // send_to_session → via=agent
	askDeliver    tools.SessionNotifyFn // ask answer/grader delivery → via=ask-grader
	agentTTS      func() voice.TTS
	blockedPaths  []config.BlockedPath // API-only (write/edit)

	// API-only deps for spawn.
	client           provider.Client
	groupResolver    *config.GroupResolver
	fallbackFn       provider.FallbackFunc
	bootstrap        *workspace.Bootstrap
	promptSearchDirs []string
	resolvedModel    string
	defaultFormat    string

	out *toolOutputs
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
func registerTools(d *toolDeps) {
	for _, e := range toolTable {
		if e.paths&d.path == 0 {
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
	_, err := exec.LookPath("tmux")
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

// toolTable is ordered to match the historical API registration sequence, so the
// model tool-list order (and thus prompt cache) is unchanged on the API path. The
// exec path's order is cosmetic (shell-function definition order).
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
	{name: "browser", paths: pathAPI, enabled: func(d *toolDeps) bool { return d.p.resolved.Browser.Enabled }, // static-cfg:ignore: see comment above
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
			return browser.NewBrowserTool(browser.NewBrowserManager(&bc, fileMode))
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
		}, d.agentTTS)
	}},

	{name: "send_to_session", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		// Loose targets resolve through the shared route ladder (Create
		// disabled: the tool addresses existing sessions, it doesn't mint
		// new ones).
		idx := d.p.sessionIndex
		resolveKeyFn := func(target string) (string, string, error) {
			t, err := route.ParseTarget(target)
			if err != nil {
				return "", "", err
			}
			t.Create = false
			res, err := (&route.Resolver{Index: idx, PreferredPlatform: d.p.cfg.DefaultPlatformFor}).Resolve(t)
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
		})
	}},

	{name: "ask", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		t, router := tools.NewAskTool(
			newAskPresentFn(d.p.acfg.ID, d.connMgr),
			newAskRestoreFn(d.p.acfg.ID, d.connMgr),
			tools.AskDeliverFn(d.askDeliver),
			func(msgID, finalText string) { _ = platform.CancelInteractiveMessage(msgID, finalText) },
			d.p.sessionIndex, d.p.acfg.ID,
			tools.WithBatchPresent(newAskPresentBatchFn(d.p.acfg.ID, d.connMgr)),
			tools.WithOnResolve(func(sk string) {
				if ag := d.agLazy(); ag != nil {
					ag.DrainDeferredInjects(sk)
				}
			}))
		d.out.askRouter = router
		return t
	}},

	// MaxConcurrentSpawns stays baked: it sizes a semaphore channel at
	// construction, which can't be live-resized without a redesign.
	// MaxToolLoops/ExploreMaxDepth are read fresh per spawn call.
	{name: "spawn", paths: pathBoth, enabled: func(d *toolDeps) bool {
		if d.path == pathAPI {
			return true
		}
		// Delegated agents get spawn only when the backend can fork a session
		// (clone mode depends on it; the routing lives in Agent.ForkSession).
		if d.agLazy == nil {
			return false
		}
		ag := d.agLazy()
		return ag != nil && ag.DelegatedManager != nil && ag.DelegatedManager.BackendCanBranch()
	}, build: func(d *toolDeps) *tools.Tool {
		acfg := d.p.acfg
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		tc := d.p.resolved.Tools // static-cfg:ignore: MaxConcurrentSpawns sizes a semaphore channel at construction, can't be live-resized without a redesign
		live := d.p.resolvedLive
		orientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, d.p.cfg.Sessions.BranchOrientationHeadlessPrompt))
		spawnDeps := tools.SpawnDeps{
			Client:              d.client,
			ClientProvider:      d.p.clientProvider,
			Bootstrap:           d.bootstrap,
			Registry:            d.registry,
			Sessions:            &sessionBranchAdapter{store: d.p.sessions, ag: d.agLazy},
			AgentID:             acfg.ID,
			GroupResolver:       d.groupResolver,
			FallbackFunc:        d.fallbackFn,
			FallbackModel:       d.resolvedModel,
			FallbackFormat:      d.defaultFormat,
			MaxInherit:          tc.MaxConcurrentSpawns,
			MaxToolLoops:        func() int { return live.Load().Loop.MaxToolLoops },
			ExploreMaxDepth:     func() int { return live.Load().Tools.ExploreMaxDepth },
			Notifier:            d.notifier,
			OrientationTemplate: prompts.ResolveOrientationTemplate(orientPath, false, d.promptSearchDirs...),
			SetNoCompact:        func(sk string, v bool) { d.agLazy().SetSessionNoCompact(sk, v) },
			FileMode:            fileMode,
			Store:               d.p.store,
		}
		return tools.NewSpawnTool(spawnDeps, func() tools.SpawnAgent { return d.agLazy() })
	}},

	{name: "remind", paths: pathBoth,
		enabled: func(d *toolDeps) bool { return d.p.reminderStore != nil && d.wakeFn != nil },
		build: func(d *toolDeps) *tools.Tool {
			return tools.NewRemindTool(d.p.reminderStore, d.p.acfg.ID, d.wakeFn)
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
}
