package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/delegator/accounting"
	"foci/internal/display"
	"foci/internal/modelinfo"
	"foci/internal/platform"
	"foci/internal/procx"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/tempdir"
	"foci/internal/turnevent"
)

// SystemBlocksProvider returns the system prompt blocks (for full context mode).
type SystemBlocksProvider interface {
	SystemBlocks() []provider.SystemBlock
}

// BranchOptions configures optional behavior for a new branch session (tools-side mirror).
// BranchOptions configures a new branch session.
type BranchOptions struct {
	NoResetHook         bool
	BranchType          string
	OrientationTemplate string
}

// SessionBrancher is the session ops needed by spawn inherit mode.
type SessionBrancher interface {
	ForkSession(ctx context.Context, parentKey string, opts BranchOptions) (branchKey string, ok bool, err error)
	SessionPath(key string) (string, error)
}

// SpawnAgent is the agent interface needed by spawn inherit mode.
type SpawnAgent interface {
	HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) error
}

// SpawnSteerer is the optional agent side of spawn_send (#2523): fold text
// into the turn running on sessionKey now, as a typed mid-turn message does.
// False when it could not (no turn in flight, or a backend without live
// steering); the message then waits for the spawn's next step.
type SpawnSteerer interface {
	SteerTurn(ctx context.Context, sessionKey, text string) bool
}

// spawnRawBlacklist lists tools excluded from "raw" mode spawns.
// No character context means no awareness of communication conventions.
// exec and tmux are excluded because they bypass file tool sandboxing —
// the isolated file tools enforce path containment, but shell access
// allows arbitrary filesystem access and symlink creation.
// whoami is excluded because a one-shot spawn has no session of its own:
// its tool calls run on the parent's context, so whoami would report the
// parent's session key, chat and model as if they were the spawn's (#2226).
// close_session is excluded for the same reason (#2573): on the parent's
// context it would close the PARENT's chat on a throwaway sub-call's say-so.
var spawnRawBlacklist = map[string]bool{
	"shell":           true,
	"tmux":            true,
	"send_to_chat":    true,
	"send_to_session": true,
	"scratchpad":      true,
	"todo":            true,
	"whoami":          true,
	"close_session":   true,
}

// spawnCharacterBlacklist lists tools excluded from "character" mode spawns.
// A character spawn is an ephemeral one-shot sub-call with no persistent session
// identity of its own — it returns its result straight to the caller. So it has
// no need to inject into other sessions, and giving a throwaway spawn that reach
// is surface it doesn't need (the real agent can still use send_to_session).
// raw/explore already exclude it; this brings character into line for that tool.
// whoami is excluded for the same reason as raw (#2226): a one-shot's tool
// calls run on the parent's context, so it would report the parent's
// session key, chat and model. close_session likewise (#2573): it would
// close the parent's chat.
var spawnCharacterBlacklist = map[string]bool{
	"send_to_session": true,
	"whoami":          true,
	"close_session":   true,
}

// exploreSystemPrompt is the system prompt for explore spawn mode.
const exploreSystemPrompt = `You are a read-only code explorer. You have access to tools but must NOT write, edit, create, or delete anything.

Use grep for searching file contents. Use find for locating files. Use read for examining files. Use git for commit history, diffs, and blame. Use todo to read and update the todo list.
Use stat, file, wc, head, tail, tree, du for filesystem inspection. Use jq/yq/mdq for structured data queries. Use sqlite for read-only database queries. Use docker, systemctl, crontab, id for system inspection. Not all tools are available in every environment.

Match your response to the question type:
- "Where is X defined?" → file paths and line numbers only
- "Where is X used/called?" → file paths, line numbers, and the calling context (one line)
- "How does X work?" → trace the call chain, summarise the logic, quote key sections
- "What does X depend on?" → list imports, function calls, config references
- "Find all X" → list matches, grouped by file

Keep responses concise. Quote code when it clarifies; don't dump entire files. If the codebase is large, start with directory structure before diving in.`

// spawnExploreAllowed is the explicit allowlist of registry tools for explore mode.
// New tools do NOT leak into this mode — they must be explicitly opted in.
var spawnExploreAllowed = map[string]bool{
	"read":          true,
	"memory_search": true,
	"web_search":    true,
	"web_fetch":     true,
	"todo":          true,
}

// SpawnDeps holds the dependencies for the spawn tool, wired at registration time.
type SpawnDeps struct {
	Client         provider.Client
	ClientProvider provider.ClientProvider // provides access to clients for different endpoint:format pairs
	Bootstrap      SystemBlocksProvider
	Registry       *Registry // tool registry for one-shot tool access
	Sessions       SessionBrancher
	AgentID        string
	GroupResolver  *config.GroupResolver // resolves model groups for spawn modes
	FallbackFunc   provider.FallbackFunc // nil disables automatic model fallback on transient errors
	FallbackModel  string                // agent's default model (developer/model_id) for single-model mode fallback
	FallbackFormat string                // agent's default format for single-model mode fallback
	// ProviderRoutingFor returns the [models.*.provider] routing
	// configured for a canonical developer/model_id, or nil. It stamps
	// every request of a one-shot's tool loop (and its fallback hops) with
	// the model's own routing. Nil = no routing, today's behaviour.
	ProviderRoutingFor  func(model string) *provider.ProviderRouting
	MaxInherit          int                                 // semaphore size (from config) — fixed at construction, can't be live-resized
	MaxToolLoops        func() int                          // max tool loops for raw/character spawns, read fresh per call
	ExploreMaxDepth     func() int                          // max tool loops for explore spawns, read fresh per call
	Notifier            *AsyncNotifier                      // async result delivery for inherit mode
	OrientationTemplate string                              // orientation template for branch sessions ({branch_key}, {parent_key}, {branch_type} resolved at creation)
	SetNoCompact        func(sessionKey string, value bool) // marks branch sessions as no_compact (prevents compaction)
	FileMode            os.FileMode                         // permission bits for files created by spawned sessions
	Store               *secrets.Store                      // secrets store for blocked-path enforcement in isolated tools
	// Tracker registers each background clone spawn so /stop can stop it
	// (#2139). Nil = untracked (a nil tracker's methods are no-ops).
	Tracker *SpawnTracker
	// Observer is told each background clone spawn's start, text and end, so
	// the app lists it beside the agent's subagents (#2139). Nil = not shown.
	Observer SpawnObserver
	// ModelFor returns the model a branch session runs on, for the observer.
	ModelFor func(sessionKey string) string
	// CloneOnly offers only the clone mode: a delegated (Claude Code) agent's
	// spawn, whose one-shot modes would need the direct API client it has not
	// got (#1311, #2523). The schema then has no model or context parameter.
	CloneOnly bool
}

// maxToolLoops calls d.MaxToolLoops(), or returns 0 if unset (tests that
// don't exercise the tool-loop budget).
func (d SpawnDeps) maxToolLoops() int {
	if d.MaxToolLoops == nil {
		return 0
	}
	return d.MaxToolLoops()
}

// exploreMaxDepth calls d.ExploreMaxDepth(), or returns 0 if unset.
func (d SpawnDeps) exploreMaxDepth() int {
	if d.ExploreMaxDepth == nil {
		return 0
	}
	return d.ExploreMaxDepth()
}

// Spawner owns what the spawn tools share: the dependencies, the lazy agent
// getter and the semaphore that bounds concurrent background runs, so a
// follow-up run started by spawn_send counts against the same limit as spawn's
// own (#2523).
type Spawner struct {
	deps    SpawnDeps
	agentFn func() SpawnAgent
	sem     chan struct{}
}

// NewSpawner builds the shared state of spawn, spawn_send and spawn_status.
// agentFn is a lazy getter for the agent (resolved at call time, since the
// agent struct is assigned after tool registration).
func NewSpawner(deps SpawnDeps, agentFn func() SpawnAgent) *Spawner {
	// MaxInherit sizes the semaphore once, at construction.
	return &Spawner{deps: deps, agentFn: agentFn, sem: make(chan struct{}, deps.MaxInherit)}
}

// cloneOnlySpawnParams is the schema of a CloneOnly spawn: no model, no context.
const cloneOnlySpawnParams = `{
	"type": "object",
	"properties": {
		"prompt": {
			"type": "string",
			"description": "Self-contained task for the spawn. It runs as the first message of a branch session that starts with a copy of your conversation."
		},
		"timeout": {
			"type": "integer",
			"description": "How long the spawn may run, in seconds (default 120). Give a long wait room: when this runs out the spawn is stopped and reported failed."
		}
	},
	"required": ["prompt"]
}`

// SpawnTool is the unified spawn tool (it replaced request_model).
func (s *Spawner) SpawnTool() *Tool {
	deps := s.deps
	if deps.CloneOnly {
		return &Tool{
			Name:        "spawn",
			ExecExport:  true,
			Positional:  []string{"prompt"},
			Description: "Start a background spawn: a fork of this session (its own process, starting from a copy of your conversation) that works on the prompt while you carry on. Use it for work that must wait on something slow without holding your turn. Its result arrives once, as a [SPAWN RESULT] message, when it finishes. Send it more instructions with spawn_send; check on it with spawn_status. Prints the spawn id.",
			Parameters:  json.RawMessage(cloneOnlySpawnParams),
			Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
				p, err := UnmarshalParams[struct {
					Prompt  string `json:"prompt"`
					Timeout int    `json:"timeout"`
				}](params)
				if err != nil {
					return ToolResult{}, err
				}
				if p.Prompt == "" {
					return ToolResult{}, fmt.Errorf("prompt is required")
				}
				return s.startClone(ctx, p.Prompt, ResolveTimeout(p.Timeout, TimeoutConfig{DefaultSec: 120}))
			},
		}
	}

	return &Tool{
		Name:        "spawn",
		ExecExport:  true,
		Positional:  []string{"prompt"},
		Description: "Spawn a sub-call to a model. Four context modes: 'raw' (just your prompt, no system context — send_to_chat and send_to_session excluded), 'character' (your prompt + character files), 'clone' (branch session — a headless self-fork), 'explore' (read-only exploration — ls, find, grep, git, read, todo, memory_search, web_search, web_fetch, plus conditional tools like stat, file, head, tail, jq, sqlite when available — no file mutation, no shell exec, no messaging). Use 'raw'/'character' for one-shot queries. Use 'clone' to delegate complex multi-step tasks; steer a running clone with spawn_send and check on it with spawn_status. Use 'explore' for codebase research and exploration.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {
					"type": "string",
					"description": "Self-contained prompt with all necessary context. For raw/character: the model gets only this (synchronous, result returned directly). For clone: injected as the user message in the branch session."
				},
				"model": {
					"type": "string",
					"description": "Model group to use: 'powerful', 'fast', 'cheap'. Empty uses the mode's default group. Ignored for clone and explore modes."
				},
				"context": {
					"type": "string",
					"enum": ["raw", "character", "clone", "explore"],
					"description": "Context mode. 'raw': just your prompt, no system context (sync). 'character': your prompt + character files (sync). 'clone' (default): branch session with full tool access — runs asynchronously in the background, result delivered via [SPAWN RESULT] when complete. 'explore': read-only exploration agent with ls, find, grep, git, read, todo, memory_search, web_search, web_fetch, plus conditional filesystem/data/system inspection tools (sync, no mutation, uses cheap model group — model param ignored)."
				},
				"timeout": {
					"type": "integer",
					"description": "Timeout in seconds (default 120). Applies to all modes."
				}
			},
			"required": ["prompt"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Prompt  string `json:"prompt"`
				Model   string `json:"model"`
				Context string `json:"context"`
				Timeout int    `json:"timeout"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			if p.Prompt == "" {
				return ToolResult{}, fmt.Errorf("prompt is required")
			}
			if p.Context == "" {
				p.Context = "clone"
			}
			timeout := ResolveTimeout(p.Timeout, TimeoutConfig{DefaultSec: 120})

			switch p.Context {
			case "raw":
				client, model, format := resolveSpawnGroup(deps.GroupResolver, p.Model, config.CallSpawnRaw, deps.ClientProvider, deps.Client, deps.FallbackModel, deps.FallbackFormat)
				tempDir, err := tempdir.SpawnMkdir("foci-spawn-*")
				if err != nil {
					return ToolResult{}, fmt.Errorf("create temp dir: %w", err)
				}
				toolDefs, tools := spawnIsolatedToolSet(deps.Registry, spawnRawBlacklist, deps.Store, tempDir, deps.FileMode)
				result, err := spawnOneShot(ctx, oneShotParams{
					client:         client,
					model:          model,
					format:         format,
					prompt:         p.Prompt,
					timeout:        timeout,
					toolDefs:       toolDefs,
					tools:          tools,
					sessions:       deps.Sessions,
					maxResultChars: spawnMaxResultChars,
					maxLoops:       deps.maxToolLoops(),
					fallbackFn:     deps.FallbackFunc,
					clientProvider: deps.ClientProvider,
					routingFor:     deps.ProviderRoutingFor,
				})
				if err != nil {
					return ToolResult{}, err
				}
				filesCreated := listCreatedFiles(tempDir)
				if filesCreated != "" {
					// tempDir is now durably quoted in conversation history (same
					// class of reference as a tool-results/ path or an app blob's
					// path), but — unlike those — it is NOT excluded from
					// cleanStaleRoot's unconditional wipe of spawn/'s children
					// (internal/tempdir/cleanup.go). That's a deliberate choice,
					// not an oversight (#1506): a later turn re-reading this path
					// across a gateway restart is rare in practice, and excluding
					// it would need a new age-based sweep to avoid an unbounded
					// leak (no session-end hook retires a raw spawn's tempDir).
					// So a raw-mode spawn's created files do not survive a restart.
					result += "\n\n---\nFiles created in " + tempDir + "/:\n" + filesCreated
				} else {
					_ = os.Remove(tempDir) // clean up empty spawn dir
				}
				return TextResult(result), nil

			case "character":
				client, model, format := resolveSpawnGroup(deps.GroupResolver, p.Model, config.CallSpawnCharacter, deps.ClientProvider, deps.Client, deps.FallbackModel, deps.FallbackFormat)
				var system []provider.SystemBlock
				if deps.Bootstrap != nil {
					system = deps.Bootstrap.SystemBlocks()
				}
				toolDefs, tools := spawnToolSet(deps.Registry, spawnCharacterBlacklist)
				result, err := spawnOneShot(ctx, oneShotParams{
					client:         client,
					model:          model,
					format:         format,
					system:         system,
					prompt:         p.Prompt,
					timeout:        timeout,
					toolDefs:       toolDefs,
					tools:          tools,
					sessions:       deps.Sessions,
					maxResultChars: spawnMaxResultChars,
					maxLoops:       deps.maxToolLoops(),
					fallbackFn:     deps.FallbackFunc,
					clientProvider: deps.ClientProvider,
					routingFor:     deps.ProviderRoutingFor,
				})
				if err != nil {
					return ToolResult{}, err
				}
				return TextResult(result), nil

			case "explore":
				client, model, format := resolveSpawnGroup(deps.GroupResolver, "", config.CallSpawnExplore, deps.ClientProvider, deps.Client, deps.FallbackModel, deps.FallbackFormat)
				system := []provider.SystemBlock{
					{Type: "text", Text: exploreSystemPrompt},
				}
				toolDefs, tools := spawnExploreToolSet(deps.Registry)
				result, err := spawnOneShot(ctx, oneShotParams{
					client:         client,
					model:          model,
					format:         format,
					system:         system,
					prompt:         p.Prompt,
					timeout:        timeout,
					toolDefs:       toolDefs,
					tools:          tools,
					sessions:       deps.Sessions,
					maxResultChars: spawnExploreMaxResultChars,
					maxLoops:       deps.exploreMaxDepth(),
					fallbackFn:     deps.FallbackFunc,
					clientProvider: deps.ClientProvider,
					routingFor:     deps.ProviderRoutingFor,
				})
				if err != nil {
					return ToolResult{}, err
				}
				return TextResult(result), nil

			case "clone":
				return s.startClone(ctx, p.Prompt, timeout)

			default:
				return ToolResult{}, fmt.Errorf("invalid context: %q (use raw, character, clone, or explore)", p.Context)
			}
		},
	}
}

// spawnToolSet builds API tool definitions and a name→Tool map from the
// registry, excluding any tools in the blacklist. Returns nil slices if
// registry is nil.
func spawnToolSet(reg *Registry, blacklist map[string]bool) ([]provider.ToolDef, map[string]*Tool) {
	if reg == nil {
		return nil, nil
	}
	all := reg.All()
	defs := make([]provider.ToolDef, 0, len(all))
	tools := make(map[string]*Tool, len(all))
	for _, t := range all {
		if blacklist[t.Name] {
			continue
		}
		if t.Name == "spawn" {
			continue
		}
		defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
		tools[t.Name] = t
	}
	return defs, tools
}

func spawnIsolatedToolSet(reg *Registry, blacklist map[string]bool, store *secrets.Store, baseDir string, fileMode os.FileMode) ([]provider.ToolDef, map[string]*Tool) {
	if reg == nil {
		return nil, nil
	}
	all := reg.All()
	defs := make([]provider.ToolDef, 0, len(all))
	tools := make(map[string]*Tool, len(all))
	for _, t := range all {
		if blacklist[t.Name] {
			continue
		}
		if t.Name == "spawn" {
			continue
		}
		defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
		switch t.Name {
		case "read":
			tools[t.Name] = NewIsolatedReadTool(store, baseDir)
		case "write":
			tools[t.Name] = NewIsolatedWriteTool(store, baseDir, fileMode)
		case "edit":
			tools[t.Name] = NewIsolatedEditTool(store, baseDir, fileMode)
		case "http_request":
			tools[t.Name] = NewIsolatedHTTPRequestTool(t, store, baseDir)
		case "summary":
			tools[t.Name] = NewIsolatedSummaryTool(t, store, baseDir)
		default:
			tools[t.Name] = t
		}
	}
	return defs, tools
}

// optionalExploreTool maps a binary name to a tool constructor for conditional explore tools.
type optionalExploreTool struct {
	binary string
	create func(binPath string) *Tool
}

// optionalExploreTools lists tools conditionally added to explore mode if their binary exists.
var optionalExploreTools = []optionalExploreTool{
	{"file", func(p string) *Tool { return newPathTool("file", p, "Identify file type.", true, nil) }},
	{"stat", func(p string) *Tool { return newPathTool("stat", p, "Display file status/metadata.", true, nil) }},
	{"wc", func(p string) *Tool { return newPathTool("wc", p, "Count lines, words, and characters.", true, nil) }},
	{"head", func(p string) *Tool { return newPathTool("head", p, "Display first lines of a file.", true, nil) }},
	{"tail", func(p string) *Tool {
		return newPathTool("tail", p, "Display last lines of a file. --follow/-f/-F is blocked.", true, tailValidate)
	}},
	{"tree", func(p string) *Tool { return newPathTool("tree", p, "Display directory tree structure.", false, nil) }},
	{"du", func(p string) *Tool { return newPathTool("du", p, "Estimate disk usage.", false, nil) }},
	{"jq", func(p string) *Tool { return newFilterTool("jq", p, "Query and filter JSON data.", "filter") }},
	{"yq", func(p string) *Tool { return newFilterTool("yq", p, "Query and filter YAML data.", "expression") }},
	{"mdq", func(p string) *Tool { return newFilterTool("mdq", p, "Query and filter Markdown.", "query") }},
	{"docker", func(p string) *Tool {
		return newSubcmdTool("docker", p, "Run read-only docker commands. Allowed: images, inspect, logs, network, ps, stats, volume.", dockerAllowedSubcommands)
	}},
	{"systemctl", func(p string) *Tool {
		return newSubcmdTool("systemctl", p, "Run read-only systemctl commands. Allowed: is-active, is-enabled, list-timers, list-units, status.", systemctlAllowedSubcommands)
	}},
	{"sqlite3", NewSQLiteTool},
	{"crontab", NewCrontabTool},
	{"id", NewIDTool},
}

// spawnExploreToolSet builds a tool set for explore spawn mode.
// It creates ls/find/grep/git tools fresh (not in the registry), adds
// conditional tools if their binary is in PATH, and pulls allowed tools
// from the registry via the explicit allowlist.
func spawnExploreToolSet(reg *Registry) ([]provider.ToolDef, map[string]*Tool) {
	defs := make([]provider.ToolDef, 0, 16)
	tools := make(map[string]*Tool, 16)

	// Create core exploration tools (not in the main registry).
	lsTool := NewLsTool()
	findTool := NewFindTool()
	grepBin, grepName := resolveGrepBinary()
	grepTool := NewGrepTool(grepBin, grepName)
	gitTool := NewGitTool()

	for _, t := range []*Tool{lsTool, findTool, grepTool, gitTool} {
		defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
		tools[t.Name] = t
	}

	// Add conditional tools if their binary is available.
	for _, opt := range optionalExploreTools {
		// Operator: optional explore tools are agent-facing.
		if binPath, err := procx.LookPath(procx.Operator, opt.binary); err == nil {
			t := opt.create(binPath)
			defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
			tools[t.Name] = t
		}
	}

	// Pull allowed tools from the registry.
	if reg != nil {
		for _, t := range reg.All() {
			if spawnExploreAllowed[t.Name] {
				defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
				tools[t.Name] = t
			}
		}
	}

	return defs, tools
}

func listCreatedFiles(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(&out, "  %s (%s)\n", e.Name(), display.FormatBytes(info.Size()))
	}
	return out.String()
}

// spawnMaxResultChars is the threshold for writing oversize tool results
// to a temp file instead of including them inline. Applied in spawnOneShot
// to prevent large tool outputs from bloating the spawn's context window.
const spawnMaxResultChars = 15000

// spawnExploreMaxResultChars is the threshold for explore mode (4x normal).
// Explore agents read more raw output since they're doing research.
const spawnExploreMaxResultChars = spawnMaxResultChars * 4

// spawnGuardResult checks if a tool result exceeds the given limit.
// If so, writes the full result to a temp file and returns a guard message
// with the file path. No summarisation — the spawn agent reads the file itself.
func spawnGuardResult(toolName, result string, limit int) string {
	if len(result) <= limit {
		return result
	}
	f, err := tempdir.Create("spawn-result-" + toolName + "-*.txt")
	if err != nil {
		return result // fallback: return original
	}
	if _, err := f.WriteString(result); err != nil {
		_ = f.Close() // #nosec G104 - best effort cleanup
		return result
	}
	_ = f.Close() // #nosec G104 - file already written successfully
	return fmt.Sprintf("Result too large (%d chars). Full output saved to %s. Use the read tool to inspect it.", len(result), f.Name())
}

// oneShotParams carries everything a one-shot spawn needs, named. The
// three call modes (raw/character/explore) differ in almost every field,
// so a positional parameter list left every call site a same-typed wall.
type oneShotParams struct {
	client         provider.Client
	model          string
	format         string
	system         []provider.SystemBlock
	prompt         string
	timeout        time.Duration
	toolDefs       []provider.ToolDef
	tools          map[string]*Tool
	sessions       SessionBrancher
	maxResultChars int
	maxLoops       int
	fallbackFn     provider.FallbackFunc
	clientProvider provider.ClientProvider
	routingFor     func(model string) *provider.ProviderRouting
}

// spawnOneShot makes API calls with optional tool access (raw/character/explore modes).
func spawnOneShot(ctx context.Context, p oneShotParams) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	sessionKey := SessionKeyFromContext(ctx)
	spawnLog.Infof("session=%s one-shot model=%s system_blocks=%d tools=%d prompt=%d chars", sessionKey, p.model, len(p.system), len(p.toolDefs), len(p.prompt))

	messages := []provider.Message{
		{Role: "user", Content: provider.TextContent(p.prompt)},
	}
	spawnStart := time.Now()
	agentID := session.AgentIDFromKey(sessionKey)
	turnID := accounting.MintTurnID(sessionKey, accounting.KindSpawn, spawnStart)

	// The spawn model is loop-invariant, so resolve its [models.*.provider]
	// routing once; every loop request carries it, plus the lookup so any
	// fallback hop lands on the hop model's own routing. The primary triple
	// is loop-invariant the same way — each loop's booking pairs with the
	// tuple that served that request (ServedTuple).
	var routing *provider.ProviderRouting
	if p.routingFor != nil {
		routing = p.routingFor(p.model)
	}
	primary := provider.ModelTuple{Model: p.model, Format: p.format}

	for i := 0; i < p.maxLoops; i++ {
		req := &provider.MessageRequest{
			Model:           p.model,
			MaxTokens:       16384,
			System:          p.system,
			Messages:        messages,
			Tools:           p.toolDefs,
			ProviderRouting: routing,
			RoutingFor:      p.routingFor,
			SessionKey:      sessionKey,
		}

		start := time.Now()
		resp, err := provider.Send(callCtx, p.client, req, nil,
			p.fallbackFn, p.clientProvider, func(f string, args ...any) {
				spawnLog.Errorf(f, args...)
			})
		if err != nil {
			return "", fmt.Errorf("spawn %s: %w", p.model, err)
		}

		duration := time.Since(start)
		served := provider.ServedTuple(primary, resp)
		cost, _ := modelinfo.CostAsOf(served.Model, time.Now(), resp.Usage.Tokens())

		spawnLog.Infof("session=%s model=%s input=%d output=%d cost=$%.4f stop=%s",
			sessionKey, served.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, resp.StopReason)
		var sessionFile string
		if p.sessions != nil {
			if path, err := p.sessions.SessionPath(sessionKey); err == nil {
				sessionFile = path
			}
		}
		// Each loop is one direct-API call, booked on the turn whose tool call
		// spawned it — or, spawned by no turn, all on one system turn of the
		// spawn's own.
		turn := accounting.TurnFor(ctx, accounting.OwnTurn(
			turnID, sessionKey, agentID, accounting.SourceSystem, spawnStart, time.Now()))
		if err := accounting.Record(turn,
			accounting.APIResponse{
				ID: resp.ID, Kind: accounting.KindSpawn, Provider: served.Format, Model: served.Model,
				Session: sessionKey, AgentID: agentID, TurnID: turn.TurnID,
				Start: start, Duration: duration, Tokens: resp.Usage.Tokens(), StopReason: resp.StopReason,
				SessionFile: sessionFile,
			}.Call()); err != nil {
			spawnLog.Errorf("session=%s book spawn call: %v", sessionKey, err)
		}

		// If no tool use, return text.
		if resp.StopReason != "tool_use" {
			text := provider.TextOf(resp.Content)
			if text == "" {
				return "(empty response)", nil
			}
			return text, nil
		}

		// Append assistant response.
		messages = append(messages, provider.Message{
			Role:    "assistant",
			Content: resp.Content,
		})

		// Execute tool calls.
		var toolResults []provider.ContentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			if callCtx.Err() != nil {
				return "", callCtx.Err()
			}
			tool, ok := p.tools[block.Name]
			if !ok {
				toolResults = append(toolResults, provider.ToolResultBlock(
					block.ID, fmt.Sprintf("Unknown tool: %s", block.Name), true,
				))
				continue
			}
			spawnLog.Debugf("session=%s tool_use: %s", sessionKey, block.Name)
			result, err := tool.Execute(callCtx, block.Input)
			if err != nil {
				toolResults = append(toolResults, provider.ToolResultBlock(
					block.ID, fmt.Sprintf("Error: %s", err), true,
				))
				continue
			}
			guarded := spawnGuardResult(block.Name, result.Text, p.maxResultChars)
			toolResults = append(toolResults, provider.ToolResultBlock(
				block.ID, guarded, false,
			))
			toolResults = append(toolResults, result.ExtraBlocks...)
		}

		messages = append(messages, provider.Message{
			Role:    "user",
			Content: toolResults,
		})
	}

	return "Max tool call depth reached.", nil
}

// startClone creates a branch session and runs the prompt on it. When a
// notifier is available, the spawn runs asynchronously in a background
// goroutine and delivers its result via the notifier. When notifier is nil, it
// falls back to synchronous execution (for tests).
func (s *Spawner) startClone(ctx context.Context, prompt string, timeout time.Duration) (ToolResult, error) {
	deps := s.deps
	parentSession := SessionKeyFromContext(ctx)
	// No-recursion guard: reject clone calls from inside a clone. An API
	// spawn's tool calls carry the inherit marker; a delegated spawn's come
	// through its own exec bridge with just its branch key (#2523).
	if IsSpawnInherit(ctx) || s.isSpawnBranch(parentSession) {
		return ToolResult{}, fmt.Errorf("nested inherit spawns not allowed — use context='raw' or context='character' instead")
	}
	if parentSession == "" {
		return ToolResult{}, fmt.Errorf("spawn inherit: no parent session in context")
	}

	// Create branch with NoResetHook (ephemeral session).
	branchKey, ok, err := deps.Sessions.ForkSession(ctx, parentSession, BranchOptions{
		NoResetHook:         true,
		BranchType:          "spawn",
		OrientationTemplate: deps.OrientationTemplate,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("spawn inherit: fork: %w", err)
	}
	if !ok {
		return ToolResult{}, fmt.Errorf("spawn inherit: backend cannot fork this session")
	}

	// Prevent compaction on branch sessions — they're short-lived and should
	// never compact independently or send notifications to the main chat.
	if deps.SetNoCompact != nil {
		deps.SetNoCompact(branchKey, true)
	}

	agent := s.agentFn()
	if agent == nil {
		return ToolResult{}, fmt.Errorf("spawn inherit: agent not available")
	}

	spawnLog.Infof("inherit branch=%s parent=%s prompt=%d chars timeout=%s",
		branchKey, parentSession, len(prompt), timeout)

	// Async path: launch goroutine, return immediately.
	if deps.Notifier != nil {
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}
		// Registered before the goroutine starts, so a /stop that lands at
		// once still finds it (#2139).
		spawnCtx, cancel := buildSpawnContext(ctx, timeout, branchKey, true)
		id := newSpawnID()
		var model string
		if deps.ModelFor != nil {
			model = deps.ModelFor(branchKey)
		}
		running := RunningSpawn{
			ID: id, BranchKey: branchKey, Description: spawnDescription(prompt),
			Model: model, Started: time.Now(),
		}
		deps.Tracker.add(parentSession, running, cancel, timeout)
		deps.Tracker.appendLog(parentSession, id, "prompt", prompt)
		if deps.Observer != nil {
			deps.Observer.SpawnStarted(parentSession, running, prompt)
		}
		pending := fmt.Sprintf("Spawn started in background.\nSpawn: %s\nBranch: %s\nPrompt: %s\nResults will be delivered when complete. Send it more with spawn_send; check on it with spawn_status.",
			id, branchKey, truncatePromptPreview(prompt))
		return s.runBackground(ctx, agent, parentSession, running, []string{prompt}, spawnCtx, cancel, pending)
	}

	// Synchronous fallback (nil notifier — for tests).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}

	spawnCtx, cancel := buildSpawnContext(ctx, timeout, branchKey, false)
	defer cancel()

	buf := turnevent.NewBufferSink()
	spawnCtx = turnevent.WithSink(spawnCtx, buf)
	if err := agent.HandleMessage(spawnCtx, branchKey, []string{prompt}, nil); err != nil {
		return ToolResult{}, fmt.Errorf("spawn inherit: %w", err)
	}
	if buf.FinalText() == "" {
		return TextResult("(empty response)"), nil
	}
	return TextResult(buf.FinalText()), nil
}

// isSpawnBranch reports whether sessionKey is a tracked spawn's own branch.
func (s *Spawner) isSpawnBranch(sessionKey string) bool {
	_, _, ok := s.deps.Tracker.ForBranch(sessionKey)
	return ok
}

// runBackground runs texts on the spawn's branch in a goroutine, holding a
// semaphore slot the caller already took, and delivers ONE [SPAWN RESULT] to
// the parent when the run ends (#2523 keeps that push as the only one). The
// spawn is already registered with the tracker under spawnCtx's cancel.
//
// Parent messages (spawn_send) that its turn did not take — the API turn
// drains them at tool boundaries through the Steerer below; a delegated turn
// gets them folded in by SteerTurn instead, or not at all if its turn had not
// started — run as a follow-up turn on the same branch before the result is
// delivered. A stopped, failed or timed-out run takes no follow-up.
func (s *Spawner) runBackground(ctx context.Context, agent SpawnAgent, parent string, running RunningSpawn,
	texts []string, spawnCtx context.Context, cancel context.CancelFunc, pendingResult string) (ToolResult, error) {
	tr, obs := s.deps.Tracker, s.deps.Observer
	id, branchKey := running.ID, running.BranchKey

	var result string
	var runErr error
	var stopped bool
	var dropped int
	signal := make(chan struct{})
	go func() {
		defer cancel()
		runCtx := turnevent.WithSteerer(spawnCtx, turnevent.SteererFunc(func() []string {
			return tr.drainPending(parent, id)
		}))
		for {
			buf := turnevent.NewBufferSink()
			sink := &spawnTextSink{BufferSink: buf, obs: obs, tracker: tr, parent: parent, id: id}
			runErr = agent.HandleMessage(turnevent.WithSink(runCtx, sink), branchKey, texts, nil)
			result = buf.FinalText()
			next, done, wasStopped, n := tr.finishOrNext(parent, id, runErr != nil || spawnCtx.Err() != nil)
			if done {
				stopped, dropped = wasStopped, n
				break
			}
			for _, t := range next {
				tr.appendLog(parent, id, "message", "(follow-up turn) "+t)
			}
			texts = next
		}
		switch {
		case stopped:
			tr.setOutcome(parent, id, "stopped")
		case runErr != nil:
			tr.setOutcome(parent, id, "failed")
			tr.appendLog(parent, id, "error", runErr.Error())
		default:
			tr.setOutcome(parent, id, "completed")
		}
		if obs != nil {
			obs.SpawnEnded(parent, id)
		}
		close(signal)
	}()

	return RunInBackground(ctx, BackgroundParams{
		SessionKey:    parent,
		Notifier:      s.deps.Notifier,
		ThresholdSecs: 0, // always async
		Done:          signal,
		NotifyMessage: func() string {
			var msg string
			switch {
			case stopped:
				msg = fmt.Sprintf("[SPAWN RESULT] Branch %s (spawn %s) was stopped by the user.", branchKey, id)
			case runErr != nil:
				msg = fmt.Sprintf("[SPAWN RESULT] Branch %s (spawn %s) failed:\n\n%v", branchKey, id, runErr)
			default:
				if result == "" {
					result = "(empty response)"
				}
				msg = fmt.Sprintf("[SPAWN RESULT] Branch %s (spawn %s) completed:\n\n%s", branchKey, id, result)
			}
			if dropped > 0 {
				msg += fmt.Sprintf("\n\n(%d message(s) sent with spawn_send arrived too late and were not delivered.)", dropped)
			}
			return msg
		},
		Cleanup:       func() { <-s.sem },
		PendingResult: TextResult(pendingResult),
	})
}

// resolveSpawnGroup resolves a spawn call to (client, model, format) using the group resolver.
// The returned model is in developer/model_id format (e.g. "openrouter/stepfun/step-3.5-flash")
// so that buildParams can strip exactly one prefix level.
// userGroup is the user-provided group name (may be empty). defaultCallSite is the call site
// constant that determines the default group.
func resolveSpawnGroup(gr *config.GroupResolver, userGroup, defaultCallSite string, clientProvider provider.ClientProvider, fallbackClient provider.Client, fallbackModel, fallbackFormat string) (provider.Client, string, string) {
	var resolved *config.ResolvedModel
	if gr != nil {
		if userGroup != "" {
			resolved = gr.ResolveGroup(userGroup)
		}
		if resolved == nil {
			resolved = gr.ResolveCall(defaultCallSite)
		}
	}
	if resolved == nil {
		// Ungrouped call or nil resolver: use agent's model (keep developer/model_id
		// format so buildParams strips exactly once).
		return fallbackClient, fallbackModel, fallbackFormat
	}
	client := fallbackClient
	if clientProvider != nil {
		if c := clientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
			client = c
		}
	}
	return client, resolved.Developer + "/" + resolved.ModelID, resolved.Format
}

// buildSpawnContext creates a spawn context with timeout and session/inherit markers.
func buildSpawnContext(ctx context.Context, timeout time.Duration, branchKey string, detached bool) (context.Context, context.CancelFunc) {
	var baseCtx context.Context
	if detached {
		baseCtx = context.Background()
	} else {
		baseCtx = ctx
	}
	spawnCtx, cancel := context.WithTimeout(baseCtx, timeout)
	spawnCtx = WithSpawnInherit(spawnCtx)
	spawnCtx = WithSessionKey(spawnCtx, branchKey)
	return spawnCtx, cancel
}

// truncatePromptPreview returns a truncated preview of the prompt (max 100 chars with ellipsis).
func truncatePromptPreview(prompt string) string {
	if len(prompt) > 100 {
		return prompt[:100] + "..."
	}
	return prompt
}
