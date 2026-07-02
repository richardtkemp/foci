package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	mcpkg "foci/internal/mcp"
	"foci/internal/platform"
	"foci/internal/provider"
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

	sessionNotify tools.SessionNotifyFn
	agentTTS      voice.TTS
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
	tmuxMigrateKey func(string, string)
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
		tc := d.p.resolved.Tools
		return shell.NewExecTool(d.agentStore, d.p.bwStore, tc.ExecAutoBackground, d.notifier,
			d.p.acfg.Workspace, d.registry, d.p.resolved.Summary.MaxResultChars, d.p.cfg.Tools.TempDir,
			execExtraEnv(d.p))
	}},

	{name: "tmux", paths: pathAPI, enabled: tmuxAvailable, build: func(d *toolDeps) *tools.Tool {
		tc := d.p.resolved.Tools
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
		wc, t, clear, migrate := tmux.NewTmuxTool(d.p.cfg.Tools.TmuxCols, d.p.cfg.Tools.TmuxRows,
			d.notifier, d.p.sessionIndex, d.p.acfg.ID, tc.TmuxAutopilot, watchSec, ttl, "")
		d.out.tmuxWatchCount, d.out.tmuxTool, d.out.tmuxClearAll, d.out.tmuxMigrateKey = wc, t, clear, migrate
		return t
	}},

	{name: "browser", paths: pathAPI, enabled: func(d *toolDeps) bool { return d.p.resolved.Browser.Enabled },
		build: func(d *toolDeps) *tools.Tool {
			bc := d.p.resolved.Browser
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
		return tools.NewReadTool(d.agentStore, d.p.acfg.Workspace, d.p.resolved.Tools.MaxFileReadBytes)
	}},
	{name: "write", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		return tools.NewWriteTool(d.agentStore, d.p.acfg.Workspace, d.blockedPaths, fileMode)
	}},
	{name: "edit", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		return tools.NewEditTool(d.agentStore, d.p.acfg.Workspace, d.blockedPaths, fileMode,
			d.p.resolved.Tools.MaxFileReadBytes)
	}},

	{name: "summary", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		return tools.NewSummaryTool(d.agentStore, d.summariser, d.p.acfg.Workspace)
	}},

	{name: "http_request", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		tc := d.p.resolved.Tools
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		return tools.NewHTTPRequestTool(d.agentStore, d.p.bwStore, d.p.cfg.Tools.TempDir,
			tc.ExecAutoBackground, tc.MaxUploadFileSize, tc.HTTPMaxSpillBytes, d.notifier, fileMode)
	}},

	{name: "web_search", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		if d.path == pathAPI && d.p.resolved.Tools.SearchProvider == "anthropic" {
			d.out.serverTools = append(d.out.serverTools, buildServerTool("web_search_20250305", "web_search",
				d.p.cfg.Tools.WebSearchMaxUses, d.p.cfg.Tools.WebSearchAllowedDomains, d.p.cfg.Tools.WebSearchBlockedDomains))
			return nil
		}
		// brave (API only when provider=="brave"; exec path always uses brave)
		if (d.path == pathExec || d.p.resolved.Tools.SearchProvider == "brave") && d.p.braveKey != "" {
			return tools.NewWebSearchTool(d.p.braveKey)
		}
		return nil
	}},

	{name: "web_fetch", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		if d.path == pathAPI && d.p.resolved.Tools.FetchProvider == "anthropic" {
			d.out.serverTools = append(d.out.serverTools, buildServerTool("web_fetch_20250910", "web_fetch",
				d.p.cfg.Tools.WebFetchMaxUses, d.p.cfg.Tools.WebFetchAllowedDomains, d.p.cfg.Tools.WebFetchBlockedDomains))
			return nil
		}
		return tools.NewWebFetchTool()
	}},

	{name: "memory_search", paths: pathBoth, enabled: func(d *toolDeps) bool { return len(d.p.memBackends) > 0 },
		build: func(d *toolDeps) *tools.Tool {
			return tools.NewMemorySearchTool(d.p.memBackends, d.p.resolved.MemorySearch.SearchBackend, d.p.convReader)
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
		var resolveKeyFn tools.SessionKeyResolverFn
		if d.p.sessionIndex != nil {
			resolveKeyFn = d.p.sessionIndex.ResolveLooseKey
		}
		return tools.NewSendToSessionTool(d.p.sessions, d.notifier, d.sessionNotify, resolveKeyFn)
	}},

	{name: "ask", paths: pathBoth, build: func(d *toolDeps) *tools.Tool {
		t, router := tools.NewAskTool(
			newAskPresentFn(d.p.acfg.ID, d.connMgr),
			newAskRestoreFn(d.p.acfg.ID, d.connMgr),
			tools.AskDeliverFn(d.sessionNotify),
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

	{name: "spawn", paths: pathAPI, build: func(d *toolDeps) *tools.Tool {
		acfg := d.p.acfg
		fileMode, _ := config.ParseFileMode(d.p.cfg.FileMode)
		al := d.p.resolved.Loop
		tc := d.p.resolved.Tools
		orientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, d.p.cfg.Sessions.BranchOrientationHeadlessPrompt))
		spawnDeps := tools.SpawnDeps{
			Client:              d.client,
			ClientProvider:      d.p.clientProvider,
			Bootstrap:           d.bootstrap,
			Registry:            d.registry,
			Sessions:            &sessionBranchAdapter{store: d.p.sessions},
			AgentID:             acfg.ID,
			GroupResolver:       d.groupResolver,
			FallbackFunc:        d.fallbackFn,
			FallbackModel:       d.resolvedModel,
			FallbackFormat:      d.defaultFormat,
			MaxInherit:          tc.MaxConcurrentSpawns,
			MaxToolLoops:        al.MaxToolLoops,
			ExploreMaxDepth:     tc.ExploreMaxDepth,
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
}
