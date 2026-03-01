package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"foci/anthropic"
	"foci/log"
)

// SystemBlocksProvider returns the system prompt blocks (for full context mode).
type SystemBlocksProvider interface {
	SystemBlocks() []anthropic.SystemBlock
}

// knownModels maps short names to full model IDs.
var knownModels = map[string]string{
	"haiku":  "claude-haiku-4-5",
	"sonnet": "claude-sonnet-4-5",
	"opus":   "claude-opus-4-6",
}

// BranchOptions configures optional behavior for a new branch session (tools-side mirror).
type BranchOptions struct {
	NoResetHook        bool
	OrientationMessage string
}

// SessionBrancher is the session ops needed by spawn inherit mode.
type SessionBrancher interface {
	CreateBranch(parentKey, branchKey string, opts BranchOptions) error
}

// SpawnAgent is the agent interface needed by spawn inherit mode.
type SpawnAgent interface {
	HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error)
}

// spawnNoneBlacklist lists tools excluded from "none" mode spawns.
// No character context means no awareness of communication conventions.
// exec and tmux are excluded because they bypass file tool sandboxing —
// the isolated file tools enforce path containment, but shell access
// allows arbitrary filesystem access and symlink creation.
var spawnNoneBlacklist = map[string]bool{
	"exec":            true,
	"tmux":            true,
	"send_telegram":   true,
	"send_to_session": true,
	"scratchpad":      true,
	"todo":            true,
}

// SpawnDeps holds the dependencies for the spawn tool, wired at registration time.
type SpawnDeps struct {
	Client             *anthropic.Client
	Bootstrap          SystemBlocksProvider
	Registry           *Registry // tool registry for one-shot tool access
	Sessions           SessionBrancher
	AgentID            string
	Model              string                                   // parent's default model
	MaxInherit         int                                      // semaphore size (from config)
	Notifier           *AsyncNotifier                           // async result delivery for inherit mode
	OrientationBuilder func(branchKey, parentKey string) string // builds orientation text for branch sessions
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
		Description: "Spawn a sub-call to a model. Three context modes: 'none' (just your prompt, no system context — send_telegram and send_to_session excluded), 'character_only' (your prompt + character files), 'clone_current' (branch session — a headless self-fork). All modes have tool access. Use 'none'/'character_only' for one-shot queries to different models. Use 'clone_current' to delegate complex multi-step tasks.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {
					"type": "string",
					"description": "Self-contained prompt with all necessary context. For none/character_only: the model gets only this (synchronous, result returned directly). For clone_current: injected as the user message in the branch session."
				},
				"model": {
					"type": "string",
					"description": "Model to use: 'opus', 'sonnet', 'haiku', or a full model ID. Empty uses the current model. Ignored for clone_current mode (inherits parent model)."
				},
				"context": {
					"type": "string",
					"enum": ["none", "character_only", "clone_current"],
					"description": "Context mode. 'none': just your prompt, no system context (sync). 'character_only': your prompt + character files (sync). 'clone_current' (default): branch session with full tool access — runs asynchronously in the background, result delivered via [SPAWN RESULT] when complete."
				},
				"timeout": {
					"type": "integer",
					"description": "Timeout in seconds (default 120). Applies to all modes."
				}
			},
			"required": ["prompt"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Prompt  string `json:"prompt"`
				Model   string `json:"model"`
				Context string `json:"context"`
				Timeout int    `json:"timeout"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.Prompt == "" {
				return "", fmt.Errorf("prompt is required")
			}
			if p.Context == "" {
				p.Context = "clone_current"
			}
			if p.Timeout <= 0 {
				p.Timeout = 120
			}

			// Resolve model short name
			model := p.Model
			if full, ok := knownModels[model]; ok {
				model = full
			}
			if model == "" {
				model = deps.Model
			}

			timeout := time.Duration(p.Timeout) * time.Second

			switch p.Context {
			case "none":
				tempDir, err := os.MkdirTemp("", "foci-spawn-*")
				if err != nil {
					return "", fmt.Errorf("create temp dir: %w", err)
				}
				toolDefs, tools := spawnIsolatedToolSet(deps.Registry, spawnNoneBlacklist, tempDir)
				result, err := spawnOneShot(ctx, deps.Client, model, nil, p.Prompt, timeout, toolDefs, tools)
				if err != nil {
					return "", err
				}
				filesCreated := listCreatedFiles(tempDir)
				if filesCreated != "" {
					result += "\n\n---\nFiles created in " + tempDir + "/:\n" + filesCreated
				}
				return result, nil

			case "character_only":
				var system []anthropic.SystemBlock
				if deps.Bootstrap != nil {
					system = deps.Bootstrap.SystemBlocks()
				}
				toolDefs, tools := spawnToolSet(deps.Registry, nil)
				return spawnOneShot(ctx, deps.Client, model, system, p.Prompt, timeout, toolDefs, tools)

			case "clone_current":
				return spawnInherit(ctx, deps, agentFn, sem, p.Prompt, timeout)

			default:
				return "", fmt.Errorf("invalid context: %q (use none, character_only, or clone_current)", p.Context)
			}
		},
	}
}

// spawnToolSet builds API tool definitions and a name→Tool map from the
// registry, excluding any tools in the blacklist. Returns nil slices if
// registry is nil.
func spawnToolSet(reg *Registry, blacklist map[string]bool) ([]anthropic.ToolDef, map[string]*Tool) {
	if reg == nil {
		return nil, nil
	}
	all := reg.All()
	defs := make([]anthropic.ToolDef, 0, len(all))
	tools := make(map[string]*Tool, len(all))
	for _, t := range all {
		if blacklist[t.Name] {
			continue
		}
		if t.Name == "spawn" {
			continue
		}
		defs = append(defs, anthropic.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
		tools[t.Name] = t
	}
	return defs, tools
}

func spawnIsolatedToolSet(reg *Registry, blacklist map[string]bool, baseDir string) ([]anthropic.ToolDef, map[string]*Tool) {
	if reg == nil {
		return nil, nil
	}
	all := reg.All()
	defs := make([]anthropic.ToolDef, 0, len(all))
	tools := make(map[string]*Tool, len(all))
	for _, t := range all {
		if blacklist[t.Name] {
			continue
		}
		if t.Name == "spawn" {
			continue
		}
		defs = append(defs, anthropic.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
		switch t.Name {
		case "read":
			tools[t.Name] = NewIsolatedReadTool(nil, baseDir)
		case "write":
			tools[t.Name] = NewIsolatedWriteTool(nil, baseDir)
		case "edit":
			tools[t.Name] = NewIsolatedEditTool(nil, baseDir)
		case "exec":
			tools[t.Name] = NewExecTool(nil, nil, 0, nil, baseDir, nil)
		case "http_request":
			tools[t.Name] = NewIsolatedHTTPRequestTool(t)
		default:
			tools[t.Name] = t
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
		fmt.Fprintf(&out, "  %s (%s)\n", e.Name(), formatBytes(info.Size()))
	}
	return out.String()
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

const maxSpawnToolLoops = 25

// spawnOneShot makes API calls with optional tool access (none/character_only modes).
func spawnOneShot(ctx context.Context, client *anthropic.Client, model string, system []anthropic.SystemBlock, prompt string, timeout time.Duration, toolDefs []anthropic.ToolDef, tools map[string]*Tool) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Infof("spawn", "one-shot model=%s system_blocks=%d tools=%d prompt=%d chars", model, len(system), len(toolDefs), len(prompt))

	messages := []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent(prompt)},
	}

	for i := 0; i < maxSpawnToolLoops; i++ {
		req := &anthropic.MessageRequest{
			Model:     model,
			MaxTokens: 16384,
			System:    system,
			Messages:  messages,
			Tools:     toolDefs,
		}

		resp, err := client.SendMessage(callCtx, req)
		if err != nil {
			return "", fmt.Errorf("spawn %s: %w", model, err)
		}

		cost := log.CalculateCost(model,
			resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

		log.Infof("spawn", "model=%s input=%d output=%d cost=$%.4f stop=%s",
			model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, resp.StopReason)

		// If no tool use, return text.
		if resp.StopReason != "tool_use" {
			text := anthropic.TextOf(resp.Content)
			if text == "" {
				return "(empty response)", nil
			}
			return text, nil
		}

		// Append assistant response.
		messages = append(messages, anthropic.Message{
			Role:    "assistant",
			Content: resp.Content,
		})

		// Execute tool calls.
		var toolResults []anthropic.ContentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			if callCtx.Err() != nil {
				return "", callCtx.Err()
			}
			tool, ok := tools[block.Name]
			if !ok {
				toolResults = append(toolResults, anthropic.ToolResultBlock(
					block.ID, fmt.Sprintf("Unknown tool: %s", block.Name), true,
				))
				continue
			}
			log.Debugf("spawn", "tool_use: %s", block.Name)
			result, err := tool.Execute(callCtx, block.Input)
			if err != nil {
				toolResults = append(toolResults, anthropic.ToolResultBlock(
					block.ID, fmt.Sprintf("Error: %s", err), true,
				))
				continue
			}
			toolResults = append(toolResults, anthropic.ToolResultBlock(
				block.ID, result, false,
			))
		}

		messages = append(messages, anthropic.Message{
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
func spawnInherit(ctx context.Context, deps SpawnDeps, agentFn func() SpawnAgent, sem chan struct{}, prompt string, timeout time.Duration) (string, error) {
	// No-recursion guard: reject inherit calls from inside a spawn inherit session.
	if IsSpawnInherit(ctx) {
		return "", fmt.Errorf("nested inherit spawns not allowed — use context='none' or context='full' instead")
	}

	parentSession := SessionKeyFromContext(ctx)
	if parentSession == "" {
		return "", fmt.Errorf("spawn inherit: no parent session in context")
	}

	// Build unique branch key.
	branchKey := fmt.Sprintf("agent:%s:spawn:spawn-%d", deps.AgentID, time.Now().UnixNano())

	// Build orientation text for the branch.
	var orientText string
	if deps.OrientationBuilder != nil {
		orientText = deps.OrientationBuilder(branchKey, parentSession)
	}

	// Create branch with NoResetHook (ephemeral session).
	if err := deps.Sessions.CreateBranch(parentSession, branchKey, BranchOptions{
		NoResetHook:        true,
		OrientationMessage: orientText,
	}); err != nil {
		return "", fmt.Errorf("spawn inherit: create branch: %w", err)
	}

	agent := agentFn()
	if agent == nil {
		return "", fmt.Errorf("spawn inherit: agent not available")
	}

	log.Infof("spawn", "inherit branch=%s parent=%s prompt=%d chars timeout=%s",
		branchKey, parentSession, len(prompt), timeout)

	// Async path: launch goroutine, return immediately.
	if deps.Notifier != nil {
		// Acquire semaphore slot (non-blocking check against context).
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}

		deps.Notifier.MarkPending(parentSession)
		go func() {
			defer func() { <-sem }()
			defer deps.Notifier.MarkDone(parentSession)

			// Detached context — survives parent turn ending.
			spawnCtx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			spawnCtx = WithSpawnInherit(spawnCtx)
			spawnCtx = WithSessionKey(spawnCtx, branchKey)

			result, err := agent.HandleMessage(spawnCtx, branchKey, prompt)
			if err != nil {
				msg := fmt.Sprintf("[SPAWN RESULT] Branch %s failed:\n\n%v", branchKey, err)
				deps.Notifier.Notify(parentSession, msg)
				return
			}
			if result == "" {
				result = "(empty response)"
			}
			msg := fmt.Sprintf("[SPAWN RESULT] Branch %s completed:\n\n%s", branchKey, result)
			deps.Notifier.Notify(parentSession, msg)
		}()

		promptPreview := prompt
		if len(promptPreview) > 100 {
			promptPreview = promptPreview[:100] + "..."
		}
		return fmt.Sprintf("Spawn started in background.\nBranch: %s\nPrompt: %s\nResults will be delivered when complete.", branchKey, promptPreview), nil
	}

	// Synchronous fallback (nil notifier — for tests).
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	spawnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	spawnCtx = WithSpawnInherit(spawnCtx)
	spawnCtx = WithSessionKey(spawnCtx, branchKey)

	result, err := agent.HandleMessage(spawnCtx, branchKey, prompt)
	if err != nil {
		return "", fmt.Errorf("spawn inherit: %w", err)
	}
	if result == "" {
		return "(empty response)", nil
	}
	return result, nil
}
