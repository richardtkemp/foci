package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
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
	// Opening is what the fork ends on before its first turn (#2617, depth
	// leaves); see session.BranchOpening.
	Opening *session.BranchOpening
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

// exploreRolePrompt is the backend-neutral part of the explore system prompt:
// the read-only role and how to shape the answer. A delegated explore batch
// gets it alone (its tools are the backend's own); the API explore spawn adds
// exploreAPIToolsPrompt for foci's tools.
const exploreRolePrompt = `You are a read-only code explorer. You have access to tools but must NOT write, edit, create, or delete anything.

Match your response to the question type:
- "Where is X defined?" → file paths and line numbers only
- "Where is X used/called?" → file paths, line numbers, and the calling context (one line)
- "How does X work?" → trace the call chain, summarise the logic, quote key sections
- "What does X depend on?" → list imports, function calls, config references
- "Find all X" → list matches, grouped by file

Keep responses concise. Quote code when it clarifies; don't dump entire files. If the codebase is large, start with directory structure before diving in.`

// exploreAPIToolsPrompt names the foci tools an API explore spawn has.
const exploreAPIToolsPrompt = `Use grep for searching file contents. Use find for locating files. Use read for examining files. Use git for commit history, diffs, and blame. Use todo to read and update the todo list.
Use stat, file, wc, head, tail, tree, du for filesystem inspection. Use jq/yq/mdq for structured data queries. Use sqlite for read-only database queries. Use docker, systemctl, crontab, id for system inspection. Not all tools are available in every environment.`

// exploreSystemPrompt is the system prompt for an API explore spawn.
const exploreSystemPrompt = exploreRolePrompt + "\n\n" + exploreAPIToolsPrompt

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
	// Delegated is set for an agent on a delegated backend (Claude Code,
	// opencode, codex). Its one-shot modes then run as batches on its own
	// backend (DelegatedSpawn.Runner), never through the API one-shot loop:
	// model groups are an API-backend concept, so Client, ClientProvider,
	// GroupResolver, FallbackFunc/Model/Format and ProviderRoutingFor stay
	// unset and no code path reads them. Nil = an API agent.
	Delegated *DelegatedSpawn
	// CanResume reports whether a branch session still has a conversation a
	// run can continue (a delegated backend's saved resume id; an API
	// session's history), for a spawn a restart interrupted (#2589). Nil =
	// assume it can.
	CanResume func(branchKey string) bool
	// TakeUndelivered returns, and forgets, the inputs written to a branch's
	// backend that it never consumed before foci stopped (#2050), so a resumed
	// spawn gets them in its resume turn instead of a redelivery turn whose
	// reply would land in no spawn. Nil = none.
	TakeUndelivered func(branchKey string) []string
	// Notice shows the user a line in a session's chat without running a turn
	// (which spawns a restart resumed, which it could not). Nil = log only.
	Notice func(sessionKey, text string)
	// Route says who owns the spawns a session started (#2624): a depth
	// leaf's pass up its tree once its report has reached its parent's
	// fan-in, and are held until then. Nil = the starting session owns its
	// spawns, never held.
	Route func(parent string) SpawnRoute
}

// apiOneShotField names the first API one-shot field d sets (the clients,
// model groups, fallbacks, routing and API tool-loop budgets that only
// runAPIOneShot reads), or "" when none is set.
func (d SpawnDeps) apiOneShotField() string {
	switch {
	case d.Client != nil:
		return "Client"
	case d.ClientProvider != nil:
		return "ClientProvider"
	case d.GroupResolver != nil:
		return "GroupResolver"
	case d.FallbackFunc != nil:
		return "FallbackFunc"
	case d.FallbackModel != "" || d.FallbackFormat != "":
		return "FallbackModel/FallbackFormat"
	case d.ProviderRoutingFor != nil:
		return "ProviderRoutingFor"
	case d.Bootstrap != nil:
		return "Bootstrap"
	case d.MaxToolLoops != nil || d.ExploreMaxDepth != nil:
		return "MaxToolLoops/ExploreMaxDepth"
	}
	return ""
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
//
// It panics on a delegated agent's deps that carry any API one-shot field:
// model groups and API clients are an API-backend concept, and a delegated
// spawn must have no way to reach them (DelegatedSpawn).
func NewSpawner(deps SpawnDeps, agentFn func() SpawnAgent) *Spawner {
	if deps.Delegated != nil {
		if f := deps.apiOneShotField(); f != "" {
			panic("tools.NewSpawner: a delegated agent's spawn deps set " + f + ", an API one-shot field")
		}
	}
	// MaxInherit sizes the semaphore once, at construction.
	return &Spawner{deps: deps, agentFn: agentFn, sem: make(chan struct{}, deps.MaxInherit)}
}

// DelegatedSpawn is what a delegated agent's spawn needs beyond the common
// SpawnDeps: the batch runner its one-shot modes run on, and its backend's
// Spec, which decides the modes it offers (DelegatedSpawnModes).
type DelegatedSpawn struct {
	// Runner resolves the agent's DelegatedManager at call time (the exec
	// registry is built before the manager exists). Nil result = not wired.
	Runner func() BatchRunner
	Spec   delegator.Spec
}

// Spawn modes ("context").
const (
	spawnModeRaw       = "raw"
	spawnModeCharacter = "character"
	spawnModeClone     = "clone"
	spawnModeExplore   = "explore"
)

// apiSpawnModes are the modes an API agent's spawn offers, in schema order.
var apiSpawnModes = []string{spawnModeRaw, spawnModeCharacter, spawnModeClone, spawnModeExplore}

// DelegatedSpawnModes returns the modes a delegated agent's spawn offers, in
// schema order, from what its backend declares: raw needs an isolated batch
// (CapIsolatedBatch), clone a fork (CapBranch), explore a read-only batch
// (CapReadOnlyBatch). character runs with the agent's own access, which every
// delegated backend can.
func DelegatedSpawnModes(spec delegator.Spec) []string {
	var modes []string
	if spec.Supports(delegator.CapIsolatedBatch) {
		modes = append(modes, spawnModeRaw)
	}
	modes = append(modes, spawnModeCharacter)
	if spec.Supports(delegator.CapBranch) {
		modes = append(modes, spawnModeClone)
	}
	if spec.Supports(delegator.CapReadOnlyBatch) {
		modes = append(modes, spawnModeExplore)
	}
	return modes
}

// modes returns the modes this agent's spawn offers.
func (s *Spawner) modes() []string {
	if d := s.deps.Delegated; d != nil {
		return DelegatedSpawnModes(d.Spec)
	}
	return apiSpawnModes
}

// defaultMode is the mode a spawn with no context runs: clone where the
// agent can fork, else character (the agent's own abilities, run once).
func defaultMode(modes []string) string {
	if slices.Contains(modes, spawnModeClone) {
		return spawnModeClone
	}
	return spawnModeCharacter
}

// spawnModeDocs describes each mode, for the tool and its context parameter.
// api is the API agent's wording (foci's own tools and model groups);
// delegated is a delegated agent's (its backend's own tools and models).
var spawnModeDocs = map[string]struct{ api, delegated string }{
	spawnModeRaw: {
		api:       "'raw': just your prompt, no system context, file tools confined to a temp dir — send_to_chat and send_to_session excluded (sync)",
		delegated: "'raw': just your prompt, no system context of yours (the backend's own default prompt), file tools confined to a fresh temp dir, no shell, no foci tools (sync)",
	},
	spawnModeCharacter: {
		api:       "'character': your prompt + character files (sync)",
		delegated: "'character': your prompt + your full system prompt, with your own tools and permission rules; anything you would have to ask the user about is refused (sync)",
	},
	spawnModeClone: {
		api:       "'clone': branch session with full tool access — runs asynchronously in the background, result delivered via [SPAWN RESULT] when complete; steer it with spawn_send, check on it with spawn_status",
		delegated: "'clone': a fork of this session (its own process, starting from a copy of your conversation) — runs in the background, result delivered via [SPAWN RESULT] when complete; steer it with spawn_send, check on it with spawn_status",
	},
	spawnModeExplore: {
		api:       "'explore': read-only exploration agent with ls, find, grep, git, read, todo, memory_search, web_search, web_fetch, plus conditional filesystem/data/system inspection tools (sync, no mutation, uses cheap model group — model param ignored)",
		delegated: "'explore': read-only exploration — reads and searches files and the web, writes nothing and runs nothing that changes anything (sync)",
	},
}

// spawnParams builds the spawn tool's parameter schema for these modes.
func spawnParams(modes []string, delegated bool) json.RawMessage {
	docs := make([]string, 0, len(modes))
	for _, m := range modes {
		d := spawnModeDocs[m]
		if delegated {
			docs = append(docs, d.delegated)
		} else {
			docs = append(docs, d.api)
		}
	}
	def := defaultMode(modes)
	modelDoc := "Model group to use: 'powerful', 'fast', 'cheap'. Empty uses the mode's default group. Ignored for clone and explore modes."
	promptDoc := "Self-contained prompt with all necessary context. For raw/character: the model gets only this (synchronous, result returned directly). For clone: injected as the user message in the branch session."
	if delegated {
		modelDoc = "Model for a raw, character or explore spawn, named as your backend names models; passed to it as written. Empty = your own model. Ignored for clone (a fork runs on your session's model)."
		promptDoc = "Self-contained task for the spawn. For raw/character/explore it is the whole request (synchronous, result returned directly). For clone it runs as the first message of a fork that starts with a copy of your conversation."
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": promptDoc},
			"model":  map[string]any{"type": "string", "description": modelDoc},
			"context": map[string]any{
				"type":        "string",
				"enum":        modes,
				"description": "Context mode (default '" + def + "'). " + strings.Join(docs, ". ") + ".",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "How long the spawn may run, in seconds (default 120). Applies to all modes. Give a long job room: when this runs out the spawn is stopped and reported failed.",
			},
		},
		"required": []string{"prompt"},
	}
	out, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("spawn schema: %v", err)) // a static map of strings: cannot fail
	}
	return out
}

// spawnToolDescription is the spawn tool's description for these modes.
func spawnToolDescription(modes []string) string {
	return "Spawn a sub-call to a model. Context modes: " + strings.Join(modes, ", ") + " (see the context parameter). " +
		"Use 'raw'/'character' for one-shot queries, 'clone' to delegate complex multi-step tasks in the background, 'explore' for read-only codebase research. Only the modes listed are available."
}

// SpawnTool is the unified spawn tool (it replaced request_model).
func (s *Spawner) SpawnTool() *Tool {
	modes := s.modes()
	return &Tool{
		Name:        "spawn",
		ExecExport:  true,
		Positional:  []string{"prompt"},
		Description: spawnToolDescription(modes),
		Parameters:  spawnParams(modes, s.deps.Delegated != nil),
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
				p.Context = defaultMode(modes)
			}
			if !slices.Contains(modes, p.Context) {
				return ToolResult{}, fmt.Errorf("invalid context: %q (use %s)", p.Context, strings.Join(modes, ", "))
			}
			timeout := ResolveTimeout(p.Timeout, TimeoutConfig{DefaultSec: 120})

			if p.Context == spawnModeClone {
				return s.startClone(ctx, p.Prompt, timeout)
			}
			if s.deps.Delegated != nil {
				return s.runBatchOneShot(ctx, p.Context, p.Prompt, p.Model, timeout)
			}
			return s.runAPIOneShot(ctx, p.Context, p.Prompt, p.Model, timeout)
		},
	}
}

// inSpawnTempDir runs a raw spawn in a fresh temp dir and appends a list of
// the files it left there to its result; an empty dir is removed.
func inSpawnTempDir(run func(dir string) (string, error)) (ToolResult, error) {
	tempDir, err := tempdir.SpawnMkdir("foci-spawn-*")
	if err != nil {
		return ToolResult{}, fmt.Errorf("create temp dir: %w", err)
	}
	result, err := run(tempDir)
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
}

// runBatchOneShot runs a delegated agent's raw, character or explore spawn
// as a batch on its own backend (DelegatedManager.RunBatch): synchronous,
// its answer returned as the tool result. The model string goes to the
// backend as written; empty = the agent's own. Nothing here reads a model
// group or an API client.
//
//   - raw: AccessIsolated in a fresh temp dir (file tools confined to it, no
//     commands, no foci tools), with the backend's own default system prompt.
//     A blank prompt is the aim, but Claude Code — the one backend that can
//     isolate a session — rejects a blank system prompt (API 400 "system:
//     text content blocks must contain non-whitespace text", claude 2.1.311),
//     so it gets its default.
//   - character: AccessParent with the agent's own composed system prompt:
//     the agent's tools and rules; what would prompt is refused.
//   - explore: AccessReadOnly with the read-only explorer prompt.
func (s *Spawner) runBatchOneShot(ctx context.Context, mode, prompt, model string, timeout time.Duration) (ToolResult, error) {
	runner := s.deps.Delegated.Runner()
	if runner == nil {
		return ToolResult{}, fmt.Errorf("spawn %s: no batch runner (delegated manager not configured)", mode)
	}
	req := delegator.BatchRequest{
		Prompt:          prompt,
		Model:           model,
		OwnerSessionKey: SessionKeyFromContext(ctx),
	}
	run := func() (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		text, err := runner.RunBatch(callCtx, req)
		if err != nil {
			return "", fmt.Errorf("spawn %s: %w", mode, err)
		}
		return text, nil
	}
	switch mode {
	case spawnModeRaw:
		req.Access = delegator.BatchAccessIsolated
		req.Purpose = delegator.BatchPurposeSpawnRaw
		return inSpawnTempDir(func(dir string) (string, error) {
			req.WorkDir = dir
			return run()
		})
	case spawnModeCharacter:
		req.Access = delegator.BatchAccessParent
		req.AgentPrompt = true
		req.Purpose = delegator.BatchPurposeSpawnCharacter
	case spawnModeExplore:
		req.Access = delegator.BatchAccessReadOnly
		req.SystemPrompt = exploreRolePrompt
		req.Purpose = delegator.BatchPurposeSpawnExplore
	default:
		return ToolResult{}, fmt.Errorf("spawn: %q is not a one-shot mode", mode)
	}
	text, err := run()
	if err != nil {
		return ToolResult{}, err
	}
	return TextResult(text), nil
}

// runAPIOneShot runs an API agent's raw, character or explore spawn through
// foci's own one-shot tool loop (spawnOneShot), on the model its group
// resolves to.
func (s *Spawner) runAPIOneShot(ctx context.Context, mode, prompt, modelGroup string, timeout time.Duration) (ToolResult, error) {
	deps := s.deps
	switch mode {
	case spawnModeRaw:
		client, model, format := resolveSpawnGroup(deps.GroupResolver, modelGroup, config.CallSpawnRaw, deps.ClientProvider, deps.Client, deps.FallbackModel, deps.FallbackFormat)
		return inSpawnTempDir(func(tempDir string) (string, error) {
			toolDefs, tools := spawnIsolatedToolSet(deps.Registry, spawnRawBlacklist, deps.Store, tempDir, deps.FileMode)
			return spawnOneShot(ctx, oneShotParams{
				client:         client,
				model:          model,
				format:         format,
				prompt:         prompt,
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
		})

	case spawnModeCharacter:
		client, model, format := resolveSpawnGroup(deps.GroupResolver, modelGroup, config.CallSpawnCharacter, deps.ClientProvider, deps.Client, deps.FallbackModel, deps.FallbackFormat)
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
			prompt:         prompt,
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

	case spawnModeExplore:
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
			prompt:         prompt,
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
	}
	return ToolResult{}, fmt.Errorf("spawn: %q is not a one-shot mode", mode)
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
		if spawnToolNames[t.Name] {
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
		if spawnToolNames[t.Name] {
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
		deps.Tracker.addWithPrompt(parentSession, running, cancel, timeout, prompt)
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
	var stopped, interrupted bool
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
			next, done, wasStopped, cut, n := tr.finishOrNext(parent, id, runErr != nil || spawnCtx.Err() != nil)
			if done {
				stopped, interrupted, dropped = wasStopped, cut, n
				break
			}
			for _, t := range next {
				tr.appendLog(parent, id, "message", "(follow-up turn) "+t)
			}
			texts = next
		}
		switch {
		case interrupted:
			// foci is shutting down: the persisted record stays as it is
			// and the next process resumes the spawn (#2589).
			spawnLog.Infof("spawn=%s branch=%s cut short by shutdown — left for the next process to resume", id, branchKey)
			close(signal)
			return
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
			case interrupted:
				return ""
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
			if n := tr.resumeCount(parent, id); n > 0 {
				msg += fmt.Sprintf("\n\n(This spawn was resumed %d time(s) after a foci restart.)", n)
			}
			return msg
		},
		Deliver:       func(msg string) { s.deliver(parent, id, msg) },
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
