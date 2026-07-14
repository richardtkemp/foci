package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/config"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/tempdir"
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

// spawnRawBlacklist lists tools excluded from "raw" mode spawns.
// No character context means no awareness of communication conventions.
// exec and tmux are excluded because they bypass file tool sandboxing —
// the isolated file tools enforce path containment, but shell access
// allows arbitrary filesystem access and symlink creation.
var spawnRawBlacklist = map[string]bool{
	"shell":           true,
	"tmux":            true,
	"send_to_chat":    true,
	"send_to_session": true,
	"scratchpad":      true,
	"todo":            true,
}

// spawnCharacterBlacklist lists tools excluded from "character" mode spawns.
// A character spawn is an ephemeral one-shot sub-call with no persistent session
// identity of its own — it returns its result straight to the caller. So it has
// no need to inject into other sessions, and giving a throwaway spawn that reach
// is surface it doesn't need (the real agent can still use send_to_session).
// raw/explore already exclude it; this brings character into line for that tool.
var spawnCharacterBlacklist = map[string]bool{
	"send_to_session": true,
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
	Client              provider.Client
	ClientProvider      provider.ClientProvider // provides access to clients for different endpoint:format pairs
	Bootstrap           SystemBlocksProvider
	Registry            *Registry // tool registry for one-shot tool access
	Sessions            SessionBrancher
	AgentID             string
	GroupResolver       *config.GroupResolver               // resolves model groups for spawn modes
	FallbackFunc        provider.FallbackFunc               // nil disables automatic model fallback on transient errors
	FallbackModel       string                              // agent's default model (developer/model_id) for single-model mode fallback
	FallbackFormat      string                              // agent's default format for single-model mode fallback
	MaxInherit          int                                 // semaphore size (from config) — fixed at construction, can't be live-resized
	MaxToolLoops        func() int                          // max tool loops for raw/character spawns, read fresh per call
	ExploreMaxDepth     func() int                          // max tool loops for explore spawns, read fresh per call
	Notifier            *AsyncNotifier                      // async result delivery for inherit mode
	OrientationTemplate string                              // orientation template for branch sessions ({branch_key}, {parent_key}, {branch_type} resolved at creation)
	SetNoCompact        func(sessionKey string, value bool) // marks branch sessions as no_compact (prevents compaction)
	FileMode            os.FileMode                         // permission bits for files created by spawned sessions
	Store               *secrets.Store                      // secrets store for blocked-path enforcement in isolated tools
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

// NewSpawnTool creates the unified spawn tool that replaces request_model.
// agentFn is a lazy getter for the agent (resolved at call time, since the
// agent struct is assigned after tool registration).
func NewSpawnTool(deps SpawnDeps, agentFn func() SpawnAgent) *Tool {
	// Semaphore for limiting concurrent inherit spawns.
	sem := make(chan struct{}, deps.MaxInherit)

	return &Tool{
		Name:        "spawn",
		ExecExport:  true,
		Positional:  []string{"prompt"},
		Description: "Spawn a sub-call to a model. Four context modes: 'raw' (just your prompt, no system context — send_to_chat and send_to_session excluded), 'character' (your prompt + character files), 'clone' (branch session — a headless self-fork), 'explore' (read-only exploration — ls, find, grep, git, read, todo, memory_search, web_search, web_fetch, plus conditional tools like stat, file, head, tail, jq, sqlite when available — no file mutation, no shell exec, no messaging). Use 'raw'/'character' for one-shot queries. Use 'clone' to delegate complex multi-step tasks. Use 'explore' for codebase research and exploration.",
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
				result, err := spawnOneShot(ctx, client, model, format, nil, p.Prompt, timeout, toolDefs, tools, deps.Sessions, spawnMaxResultChars, deps.maxToolLoops(), deps.FallbackFunc, deps.ClientProvider)
				if err != nil {
					return ToolResult{}, err
				}
				filesCreated := listCreatedFiles(tempDir)
				if filesCreated != "" {
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
				result, err := spawnOneShot(ctx, client, model, format, system, p.Prompt, timeout, toolDefs, tools, deps.Sessions, spawnMaxResultChars, deps.maxToolLoops(), deps.FallbackFunc, deps.ClientProvider)
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
				result, err := spawnOneShot(ctx, client, model, format, system, p.Prompt, timeout, toolDefs, tools, deps.Sessions, spawnExploreMaxResultChars, deps.exploreMaxDepth(), deps.FallbackFunc, deps.ClientProvider)
				if err != nil {
					return ToolResult{}, err
				}
				return TextResult(result), nil

			case "clone":
				return spawnInherit(ctx, deps, agentFn, sem, p.Prompt, timeout)

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
		if binPath, err := exec.LookPath(opt.binary); err == nil {
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

// spawnOneShot makes API calls with optional tool access (raw/character/explore modes).
func spawnOneShot(ctx context.Context, client provider.Client, model, format string, system []provider.SystemBlock, prompt string, timeout time.Duration, toolDefs []provider.ToolDef, tools map[string]*Tool, sessions SessionBrancher, maxResultChars int, maxLoops int, fallbackFn provider.FallbackFunc, clientProvider provider.ClientProvider) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sessionKey := SessionKeyFromContext(ctx)
	spawnLog.Infof("session=%s one-shot model=%s system_blocks=%d tools=%d prompt=%d chars", sessionKey, model, len(system), len(toolDefs), len(prompt))

	messages := []provider.Message{
		{Role: "user", Content: provider.TextContent(prompt)},
	}

	for i := 0; i < maxLoops; i++ {
		req := &provider.MessageRequest{
			Model:     model,
			MaxTokens: 16384,
			System:    system,
			Messages:  messages,
			Tools:     toolDefs,
		}

		start := time.Now()
		resp, err := provider.Send(callCtx, client, req, nil,
			fallbackFn, clientProvider, func(f string, args ...any) {
				spawnLog.Errorf(f, args...)
			})
		if err != nil {
			return "", fmt.Errorf("spawn %s: %w", model, err)
		}

		duration := time.Since(start)
		cost := modelinfo.Cost(model,
			resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

		spawnLog.Infof("session=%s model=%s input=%d output=%d cost=$%.4f stop=%s",
			sessionKey, model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, resp.StopReason)
		var sessionFile string
		if sessions != nil {
			if p, err := sessions.SessionPath(sessionKey); err == nil {
				sessionFile = p
			}
		}
		log.API(log.APIEntry{
			Timestamp:   start,
			Provider:    format,
			Session:     sessionKey,
			Model:       model,
			Input:       resp.Usage.InputTokens,
			Output:      resp.Usage.OutputTokens,
			CacheRead:   resp.Usage.CacheReadInputTokens,
			CacheWrite:  resp.Usage.CacheCreationInputTokens,
			CostUSD:     cost,
			DurationMS:  duration.Milliseconds(),
			StopReason:  resp.StopReason,
			CallType:    "spawn",
			SessionFile: sessionFile,
		})

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
			tool, ok := tools[block.Name]
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
			guarded := spawnGuardResult(block.Name, result.Text, maxResultChars)
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

// spawnInherit creates a branch session and runs HandleMessage on it.
// When a notifier is available, the spawn runs asynchronously in a background
// goroutine and delivers results via the notifier. When notifier is nil, it
// falls back to synchronous execution (for tests).
func spawnInherit(ctx context.Context, deps SpawnDeps, agentFn func() SpawnAgent, sem chan struct{}, prompt string, timeout time.Duration) (ToolResult, error) {
	// No-recursion guard: reject inherit calls from inside a spawn inherit session.
	if IsSpawnInherit(ctx) {
		return ToolResult{}, fmt.Errorf("nested inherit spawns not allowed — use context='raw' or context='character' instead")
	}

	parentSession := SessionKeyFromContext(ctx)
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

	agent := agentFn()
	if agent == nil {
		return ToolResult{}, fmt.Errorf("spawn inherit: agent not available")
	}

	spawnLog.Infof("inherit branch=%s parent=%s prompt=%d chars timeout=%s",
		branchKey, parentSession, len(prompt), timeout)

	// Async path: launch goroutine, return immediately.
	if deps.Notifier != nil {
		// Acquire semaphore slot (non-blocking check against context).
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}

		var spawnResult string
		var spawnErr error
		signal := make(chan struct{})
		go func() {
			spawnCtx, cancel := buildSpawnContext(ctx, timeout, branchKey, true)
			defer cancel()
			buf := turnevent.NewBufferSink()
			spawnCtx = turnevent.WithSink(spawnCtx, buf)
			spawnErr = agent.HandleMessage(spawnCtx, branchKey, []string{prompt}, nil)
			spawnResult = buf.FinalText()
			close(signal)
		}()

		promptPreview := truncatePromptPreview(prompt)
		return RunInBackground(ctx, BackgroundParams{
			SessionKey:    parentSession,
			Notifier:      deps.Notifier,
			ThresholdSecs: 0, // always async
			Done:          signal,
			NotifyMessage: func() string {
				if spawnErr != nil {
					return fmt.Sprintf("[SPAWN RESULT] Branch %s failed:\n\n%v", branchKey, spawnErr)
				}
				if spawnResult == "" {
					spawnResult = "(empty response)"
				}
				return fmt.Sprintf("[SPAWN RESULT] Branch %s completed:\n\n%s", branchKey, spawnResult)
			},
			Cleanup:       func() { <-sem },
			PendingResult: TextResult(fmt.Sprintf("Spawn started in background.\nBranch: %s\nPrompt: %s\nResults will be delivered when complete.", branchKey, promptPreview)),
		})
	}

	// Synchronous fallback (nil notifier — for tests).
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
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
