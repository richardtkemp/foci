package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/nudge"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/warnings"
	"foci/internal/workspace"
)

const defaultBraindeadWarningPrompt = "You've made many consecutive tool calls. Stop and verify: is what you're doing right now what the user actually asked for?"

// nudgeHeader prefixes automatic nudge messages so the agent understands
// their origin and treats them as background guidance, not user input.
const nudgeHeader = "[system: automatic nudge — this is a behavioral reminder derived from your character configuration. Incorporate the guidance naturally without mentioning this nudge to the user.]\n"

// ReplyFunc is called to deliver intermediate messages during a turn.
// Used by the platform to send early/deferred replies while
// the agent continues working (e.g., "Looking into this...").
type ReplyFunc func(text string)

// ToolCallObserver is called before each tool execution.
// Used by the platform to show which tools the agent is calling.
type ToolCallObserver func(toolName string, params json.RawMessage)

// ToolResultObserver is called after each tool execution with the result.
// Used by the platform to store tool results for inline keyboard expansion.
type ToolResultObserver func(toolName string, result string, isError bool)

// CacheBustFunc is called when a cache bust is detected (cache_read drops
// significantly compared to the previous request).
// session is the session key, prevRead is what we had, curRead is what we got.
type CacheBustFunc func(session string, prevRead, curRead int)

// Agent is the core agent loop.
type Agent struct {
	Client         provider.Client
	ClientProvider provider.ClientProvider // provides access to API clients for different endpoint:format pairs
	Sessions       *session.Store
	Tools          *tools.Registry
	Bootstrap      *workspace.Bootstrap
	Compactor      *compaction.Compactor // nil disables auto-compaction
	AsyncNotifier  *tools.AsyncNotifier  // nil disables async-pending compaction guard
	Reminders       *memory.ReminderStore  // nil disables reminder injection
	TaskListStore   *memory.TaskListStore  // nil disables task list in state dashboard
	TodoStore       *memory.TodoStore      // nil disables todo count in state dashboard
	ScratchpadStore *memory.Scratchpad     // nil disables scratchpad count in state dashboard
	AgentID         string                 // unique agent identifier (for per-agent DB queries)
	Model          string                // "developer/model_id" format (e.g. "anthropic/claude-opus-4-6")
	Format         string                // wire format resolved at startup (e.g. "anthropic", "gemini", "openai")
	Endpoint       string                // agent's default endpoint (sessions inherit this unless overridden)
	Log            *log.ComponentLogger  // structured logger for this agent

	EnvironmentBlock              string                       // pre-built environment context block (prepended first in system prompt)
	ExtraSystemBlocks             []provider.SystemBlock       // additional system blocks (e.g. skills list), injected before cache marker
	CacheStrategy                 string                       // "auto" (top-level) or "explicit" (manual breakpoints)
	CacheBustDetect               bool                         // detect cache busts (cache_read drop >50%)
	CacheBustIdleThreshold        time.Duration                // suppress cache bust alert if session idle > this (default 10m)
	CacheBustAlert                HookList[CacheBustFunc]      // callbacks for cache bust alerts
	DuplicateMessages             bool                         // send user text twice per API call (improves instruction following)
	BatchPartialAssistantMessages bool                         // accumulate mid-turn text; send concatenated on turn end (default false = send immediately)
	BatchPartialJoiner            string                       // separator between batched partial messages (default "")
	MaxResultChars                int                          // max chars for tool result before writing to file (0 disables)
	ToolResultTempDir             string                       // where to write large tool results
	ModelAliases                  map[string]string            // for resolving "haiku" → full model ID
	SummaryContextTurns           int                          // recent conversation turns for summary context
	SummaryContextChars           int                          // max chars of context to send to Haiku
	MaxSummaryChars               int                          // max chars to auto-summarise (skip Haiku above this)
	MaxSummaryInputChars          int                          // max chars of tool result embedded in summary prompt (0 = no limit)
	GroupResolver                 *config.GroupResolver         // nil = single-model mode (use session model for everything)
	MaxImagePixels                int                          // max pixels (w*h) for images before downscaling; 0 disables
	AutoSummarise                 bool                         // enable auto-summarise of oversized tool results (default true)
	WarningQueue                  *warnings.Queue              // nil disables warning injection into session
	ManaWatcher                   *ManaWatcher                 // nil disables mana threshold warnings
	ManaWarnFunc                  HookList[func(string)]                 // callbacks for mana threshold warnings (e.g. platform notification)
	MaxTokensWarnFunc             HookList[func(string)]                 // callbacks when stop_reason=max_tokens (response truncated)
	RateLimitFunc                 HookList[func(resetTime time.Time)]    // callbacks when API returns 429 (rate limited)
	CompactionManaRefreshThreshold   string                                 // trigger mana-refresh when reset this soon (e.g. "5m")
	CompactionManaRefreshFactor      float64                               // secondary threshold = main threshold × factor (e.g. 0.5)
	CompactionManaRefreshPreserve    *int                                   // messages to preserve in refresh mode (nil = use percentage)
	CompactionManaRefreshPreservePct float64                                // fraction of messages to preserve in refresh mode (0 = default 0.5)
	TaskListNotifyFunc            HookList[func(string, string)]         // callbacks for task list changes (session key, message)
	CompactionMemoryFunc          HookList[func(string)]                 // fires before compaction to save memories (session key)
	CompactionStartFunc           HookList[func(string, string)]         // callbacks for compaction start (session key, message) — sent immediately, not buffered
	CompactionNotifyFunc          HookList[func(string, string)]         // callbacks for compaction notifications (session key, message)
	CompactionDebugFunc           HookList[func(string, string)]         // callbacks for compaction debug (session key, summary text)
	SessionKeyRotatedFunc         HookList[func(string, string)]         // callbacks when session key rotates (oldKey, newKey)
	OnActivity                    HookList[func(string)]                 // callbacks when a session has activity (session key)
	Redact                        func(string) string          // redact secrets from tool output; nil disables
	SessionIndex                  *session.SessionIndex        // nil disables state persistence
	UsageClient                   provider.UsageClient         // nil disables mana metadata
	UsageClientProvider           provider.UsageClientProvider // per-endpoint usage client resolution (nil = use default UsageClient)
	MessageTransforms             []CompiledTransform          // compiled regex rules for inbound message transformation
	CompactionSummaryPromptPath   string                       // file path; read at compaction time via prompts.ResolvePrompt
	CompactionHandoffMsg          string                       // inline handoff message; empty resolves from search dirs or embedded default
	PromptSearchDirs              []string                     // directories to search for prompt files (agent workspace, shared)
	MaxToolLoops                  int                          // max tool iterations per turn (default 25)
	MaxOutputTokens               int                          // max tokens in model response (default 16384)
	BraindeadWarningEnable        bool                         // enable braindead warning (default true)
	BraindeadWarningThreshold     int                          // consecutive tool loops before warning (0 = disabled)
	BraindeadWarningPrompt        string                       // warning text (empty = hardcoded default)
	Nudger                        *nudge.Scheduler             // nil disables nudge reminders
	NudgePreAnswerGate            bool                         // enable pre-answer verification gate
	NudgePreAnswerMinTools        int                          // min tool calls before gate fires (default 2)
	NudgeReloadFunc               func()                       // called after bootstrap reload to refresh nudge rules; nil disables
	FirstRunMessage               atomic.Value                 // string; prepended as separate content block on first HandleMessage, then cleared
	TurnLockWarnThreshold         time.Duration                // warn if turn lock wait exceeds this (default 3m)
	Effort                        string                       // effort level for API requests (empty = omit from request)
	Thinking                      string                       // thinking mode: "off" or "adaptive" (empty/"off" = disabled)
	Speed                         string                       // speed mode: "fast" for Anthropic fast mode (Opus only, empty = standard)
	ShowToolCalls                 string                       // agent-level default: "off"/"preview"/"full" (per-session overrides via /display)
	CacheTTL                      string                       // Anthropic prompt cache TTL: "5m" or "1h" (set on MessageRequest for translate layer)
	Streaming                     bool                         // use streaming API when provider supports it
	ManaInvestInterval            time.Duration                // invest interval for mana good/bad indicator; 0 = no indicator
	ServerTools                   []provider.ToolDef           // server-side tools (web_search, web_fetch) — executed by Anthropic, not client
	DefaultSessionKey             func() string                // returns the main/default session key; reminders only inject into this session

	platforms  map[string]platform.Sender // per-agent platforms (telegram, discord, etc.); key = platform name
	platformMu sync.RWMutex               // protects platforms map access

	rateLimitGates   map[string]*RateLimitGate // per-endpoint gates; key = endpoint name, lazy-init
	rateLimitGatesMu sync.RWMutex              // protects rateLimitGates map access
	processing       int32                     // atomic: number of in-flight HandleMessage calls
	turnDetailsMu    sync.Mutex
	turnDetails      map[uint64]*TurnDetail // keyed by unique turn ID
	turnIDCounter    uint64                 // atomic: monotonic turn ID
	turnLocksMu      sync.Mutex
	turnLocks        map[string]*sync.Mutex // per-session turn serialization
	metaMu           sync.Mutex
	meta             map[string]*sessionMeta // per-session metadata
}

// TransformMessage applies compiled message transforms to the text.
// Returns the original text unchanged if no transforms are configured.
func (a *Agent) TransformMessage(text string) string {
	if len(a.MessageTransforms) == 0 {
		return text
	}
	return ApplyTransforms(a.MessageTransforms, text)
}

func (a *Agent) Warnings() *warnings.Queue {
	return a.WarningQueue
}

// logger returns the agent's ComponentLogger, lazily creating a default if nil.
func (a *Agent) logger() *log.ComponentLogger {
	if a.Log != nil {
		return a.Log
	}
	a.Log = log.NewComponentLogger("agent")
	return a.Log
}

// ResolveCallSite resolves a call site to a (client, model, format) triple.
// For ungrouped calls or single-model mode, returns the session's client/model/format.
// Otherwise resolves via GroupResolver and gets the appropriate client.
func (a *Agent) ResolveCallSite(callSite, sessionKey string) (provider.Client, string, string) {
	if a.GroupResolver == nil || a.GroupResolver.IsSingleModel() {
		return a.SessionClient(sessionKey), a.SessionModel(sessionKey), a.Format
	}
	resolved := a.GroupResolver.ResolveCall(callSite)
	if resolved == nil {
		return a.SessionClient(sessionKey), a.SessionModel(sessionKey), a.Format
	}
	client := a.SessionClient(sessionKey)
	if a.ClientProvider != nil {
		if c := a.ClientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
			client = c
		}
	}
	return client, resolved.Developer + "/" + resolved.ModelID, resolved.Format
}

// TurnDetail describes one in-flight turn for shutdown diagnostics.
type TurnDetail struct {
	SessionKey string
	Trigger    string // "user", "keepalive", "wake", "scheduled_wake", "telegram", "async_notify"
	ToolName   string // currently executing tool, or empty
	StartTime  time.Time
}

// IsProcessing returns true if the agent is currently handling a message.
func (a *Agent) IsProcessing() bool {
	return atomic.LoadInt32(&a.processing) > 0
}

// ProcessingDetails returns detail for every in-flight turn.
func (a *Agent) ProcessingDetails() []TurnDetail {
	a.turnDetailsMu.Lock()
	defer a.turnDetailsMu.Unlock()
	out := make([]TurnDetail, 0, len(a.turnDetails))
	for _, d := range a.turnDetails {
		out = append(out, *d)
	}
	return out
}

// registerTurn adds a TurnDetail and returns its ID and pointer (for tool tracking).
func (a *Agent) registerTurn(d *TurnDetail) uint64 {
	id := atomic.AddUint64(&a.turnIDCounter, 1)
	a.turnDetailsMu.Lock()
	if a.turnDetails == nil {
		a.turnDetails = make(map[uint64]*TurnDetail)
	}
	a.turnDetails[id] = d
	a.turnDetailsMu.Unlock()
	return id
}

// unregisterTurn removes a TurnDetail by ID.
func (a *Agent) unregisterTurn(id uint64) {
	a.turnDetailsMu.Lock()
	delete(a.turnDetails, id)
	a.turnDetailsMu.Unlock()
}

// SetProcessingForTest sets the processing counter directly. Test-only.
func (a *Agent) SetProcessingForTest(n int32) {
	atomic.StoreInt32(&a.processing, n)
}

// isSystemMessage returns true if the message is from a system source
// (keepalive, scheduled wake, proactive warnings) rather than a human user.
func isSystemMessage(msg string) bool {
	return strings.HasPrefix(msg, "[KEEPALIVE]") ||
		strings.HasPrefix(msg, "[SCHEDULED WAKE]") ||
		strings.HasPrefix(msg, "[proactive system warnings]")
}

// turnLock returns a per-session mutex that serializes HandleMessage calls.
// This prevents concurrent turns on the same session from interleaving messages
// in the session file, which would invalidate Anthropic's prefix-matched prompt cache.
func (a *Agent) turnLock(sessionKey string) *sync.Mutex {
	a.turnLocksMu.Lock()
	defer a.turnLocksMu.Unlock()
	if a.turnLocks == nil {
		a.turnLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := a.turnLocks[sessionKey]
	if !ok {
		mu = &sync.Mutex{}
		a.turnLocks[sessionKey] = mu
	}
	return mu
}

// HandleMessage processes a text-only user message. Delegates to HandleMessageWithAttachments.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	return a.HandleMessageWithAttachments(ctx, sessionKey, []string{userMessage}, nil)
}

// HandleMessageWithAttachments processes one or more user messages with optional attachments
// (images, PDFs, or convertible documents like docx/xlsx/pptx/HTML/CSV).
// Multiple texts are batched into a single turn with separate content blocks.
func (a *Agent) HandleMessageWithAttachments(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (string, error) {
	// Gate check: resolve session's endpoint and check its gate.
	// Only that endpoint's sessions are blocked when rate-limited.
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	endpoint := sm.modelEndpoint
	if endpoint == "" {
		endpoint = a.Endpoint // agent default
	}
	a.metaMu.Unlock()

	gate := a.getOrCreateRateLimitGate(endpoint)
	if limited, until := gate.IsLimited(); limited {
		trigger := TriggerFromContext(ctx)
		gate.Enqueue(sessionKey, texts[0], trigger)
		a.logger().Infof("rate limit gate (%s): queued message for session=%s trigger=%s (resets %s)", endpoint, sessionKey, trigger, until.Format(time.Kitchen))
		return "", &RateLimitedError{Until: until}
	}

	// Serialize turns on the same session. Without this, concurrent callers
	// (keepalive, tmux watch, scheduled wakes, exec auto-background) could
	// load the same session state, run concurrent turns, and interleave their
	// messages in the session file. This would break Anthropic's prefix-matched
	// prompt cache — any insertion in the middle of conversation history
	// invalidates all cached tokens after the insertion point.
	sessionLock := a.turnLock(sessionKey)
	waiterTrigger := TriggerFromContext(ctx)
	a.logger().Debugf("turn_lock_wait session=%s trigger=%s", sessionKey, waiterTrigger)
	lockStart := time.Now()
	sessionLock.Lock()
	lockDur := time.Since(lockStart)
	a.logTurnLockWait(sessionKey, lockDur, waiterTrigger)
	defer sessionLock.Unlock()

	atomic.AddInt32(&a.processing, 1)
	defer atomic.AddInt32(&a.processing, -1)

	td := &TurnDetail{
		SessionKey: sessionKey,
		Trigger:    TriggerFromContext(ctx),
		StartTime:  time.Now(),
	}
	turnID := a.registerTurn(td)
	defer a.unregisterTurn(turnID)

	// Check if context was cancelled while waiting for the turn lock
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	// Log received message for conversation log (platform-agnostic).
	meta := TurnMetadataFromContext(ctx)
	if meta == nil {
		meta = &TurnMetadata{}
	}
	convChatID := meta.ChatID
	if convChatID == 0 {
		convChatID = session.ChatIDFromKey(sessionKey)
	}
	log.Conversation(log.ConversationEntry{
		Direction: "recv",
		UserID:    meta.UserID,
		Username:  meta.Username,
		ChatID:    convChatID,
		Text:      strings.Join(texts, "\n"),
		Session:   sessionKey,
	})

	// Touch session activity for index tracking.
	for _, fn := range a.OnActivity {
		fn(sessionKey)
	}

	// Load existing messages
	messages, err := a.Sessions.LoadFull(sessionKey)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	if loadStats := provider.ComputeSessionStats(messages); loadStats.Messages > 0 {
		a.logger().Debugf("session_loaded session=%s messages=%d blocks=%d bytes=%d tokens≈%d",
			sessionKey, loadStats.Messages, loadStats.Blocks, loadStats.ApproxBytes, loadStats.ApproxTokens())
	}

	// Repair interrupted tool calls (e.g. SIGTERM during tool execution).
	// If the last message is assistant with tool_use but no tool_result follows,
	// inject synthetic error results so the API accepts the message history.
	if repair := repairInterruptedToolCalls(messages); repair != nil {
		messages = append(messages, *repair)
		writer := a.Sessions.For(sessionKey)
		if err := writer.Append(sessionKey, *repair); err != nil {
			a.logger().Errorf("session=%s persist tool call repair: %v", sessionKey, err)
		} else {
			a.logger().Infof("session=%s repaired %d interrupted tool calls", sessionKey, len(repair.Content))
		}
	}

	// Repair duplicate tool_use/tool_result IDs in memory. The Anthropic API
	// rejects duplicate IDs with a 400 error. This can happen due to session
	// corruption (e.g., partial write + defer safety-net replay) or Gemini
	// synthesising identical IDs for same-name tool calls.
	// Not persisted: runs on every load (cheap O(n) scan), and Replace on branch
	// sessions would incorrectly write the full parent+branch chain to the branch file.
	messages, _ = repairDuplicateToolIDs(messages, func(format string, args ...any) {
		a.logger().Warnf("session=%s "+format, append([]any{sessionKey}, args...)...)
	})

	// Repair missing assistant messages (consecutive user messages or empty
	// assistant content). Caused by API errors where the defer safety-net
	// flushed the user message without a matching assistant response.
	if repaired, n := repairMissingAssistantMessages(messages); n > 0 {
		messages = repaired
		a.logger().Warnf("session=%s repaired %d missing/empty assistant messages", sessionKey, n)
	}

	turnModel := a.SessionModel(sessionKey)
	turnClient := a.SessionClient(sessionKey)
	turnEffort := a.SessionEffort(sessionKey)
	turnThinking := a.SessionThinking(sessionKey)
	turnSpeed := a.SessionSpeed(sessionKey)

	// When extended thinking is active with effort above "low", duplicate_messages
	// wastes tokens — thinking already produces high-quality first responses.
	effectiveDuplicate := a.DuplicateMessages
	if effectiveDuplicate && turnThinking != "" && turnThinking != "off" && turnEffort != "low" {
		effectiveDuplicate = false
		a.logger().Debugf("session=%s duplicate_messages suppressed: thinking=%s effort=%s", sessionKey, turnThinking, turnEffort)
	}

	now := time.Now()
	sm = a.getSessionMeta(sessionKey)

	userMsg := a.prepareUserMessage(ctx, sessionKey, texts, turnModel, attachments, effectiveDuplicate)
	messages = append(messages, userMsg)

	// Track new messages to save. The defer flushes unsaved messages on
	// shutdown (e.g. SIGTERM during a tool call like "systemctl restart foci").
	// Normal exits set newMessages=nil after saving, so the defer is a no-op.
	var newMessages []provider.Message
	newMessages = append(newMessages, userMsg)
	defer func() {
		if len(newMessages) > 0 {
			writer := a.Sessions.For(sessionKey)
			if err := writer.AppendAll(sessionKey, newMessages); err != nil {
				a.logger().Errorf("session=%s flush in-flight messages: %v", sessionKey, err)
			} else {
				a.logger().Infof("flushed %d in-flight messages for %s", len(newMessages), sessionKey)
			}
		}
	}()

	system := a.buildSystemBlocks(sessionKey)
	toolDefs := a.Tools.ToolDefs()
	if len(a.ServerTools) > 0 {
		toolDefs = append(toolDefs, a.ServerTools...)
	}

	maxLoops := a.MaxToolLoops
	if maxLoops <= 0 {
		maxLoops = 25 // default
	}
	maxOutput := a.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = 16384 // default
	}
	braindeadWarningThreshold := a.BraindeadWarningThreshold
	braindeadWarned := false
	displayNoted := false // true after injecting tool_display note
	verified := false     // pre-answer gate: true after one verification pass
	var sameToolStreak int
	var lastToolName string
	var lastToolError bool
	if a.Nudger != nil {
		a.Nudger.StartTurn(texts[0])
		// Prepend match nudges as ContentBlocks before the user's text/attachment blocks.
		if matchNudges := a.Nudger.CheckMatch(); len(matchNudges) > 0 {
			var nudgeBlocks []provider.ContentBlock
			for _, r := range matchNudges {
				nudgeBlocks = append(nudgeBlocks, provider.ContentBlock{Type: "text", Text: nudgeHeader + r})
			}
			userMsg.Content = append(nudgeBlocks, userMsg.Content...)
			// Update the already-appended message in messages and newMessages slices.
			messages[len(messages)-1] = userMsg
			newMessages[len(newMessages)-1] = userMsg
			a.logger().Infof("nudge: %d match trigger(s) prepended to user message for session %s", len(matchNudges), sessionKey)
		}
	}
	var batchedText strings.Builder // accumulates intermediate text when BatchPartialAssistantMessages=true

	// sendOrBatchText delivers any text from a response as an intermediate
	// reply, respecting batch mode. Used before nudge continues and for
	// text mixed into tool_use responses.
	sendOrBatchText := func(r provider.MessageResponse) {
		if text := provider.TextOf(r.Content); text != "" {
			if a.BatchPartialAssistantMessages {
				if batchedText.Len() > 0 {
					batchedText.WriteString(a.BatchPartialJoiner)
				}
				batchedText.WriteString(text)
			} else {
				sendIntermediateCtx(ctx, text)
				a.logConversationSent(convChatID, meta, sessionKey, text)
			}
		}
	}

	for i := 0; i < maxLoops; i++ {
		// Remove empty text blocks that would cause API 400 errors.
		messages = sanitizeEmptyTextBlocks(messages)

		req := &provider.MessageRequest{
			Model:         turnModel,
			MaxTokens:     maxOutput,
			System:        system,
			Messages:      messages,
			Tools:         toolDefs,
			CacheStrategy: a.CacheStrategy,
			CacheTTL:      a.CacheTTL,
		}
		// Set effort/thinking unconditionally — each provider's SendMessage
		// handles or ignores unsupported fields. The error-and-retry fallback
		// below strips them if the model rejects with a 400.
		if turnEffort != "" && turnEffort != "off" {
			req.Output = &provider.OutputConfig{Effort: turnEffort}
		}
		if turnThinking == "adaptive" {
			req.Thinking = &provider.ThinkingConfig{Type: "adaptive"}
		}
		if turnSpeed == "fast" {
			req.Speed = "fast"
		}

		// Debug: log cache placement
		logCacheDebug(sessionKey, system, messages, turnModel)

		a.logger().Debugf("api_request session=%s model=%s messages=%d tools=%d system_blocks=%d",
			sessionKey, turnModel, len(messages), len(toolDefs), len(system))

		start := time.Now()
		a.logger().Debugf("api_call_start session=%s model=%s streaming=%v", sessionKey, turnModel, a.Streaming)

		var resp *provider.MessageResponse
		var err error

		// Use streaming if enabled; provider.Send auto-selects streaming
		// when the client supports it, falling back to SendMessage otherwise.
		var handler *provider.StreamHandler
		if a.Streaming {
			handler = &provider.StreamHandler{
				OnTextDelta: func(delta string) {
					notifyTextDeltaCtx(ctx, delta)
					signalActivityCtx(ctx) // keep typing indicator alive
				},
				OnThinkingDelta: func(delta string) {
					notifyThinkingDeltaCtx(ctx, delta)
				},
			}
		}

		// Attach retry notification callbacks to context
		ctx = provider.WithRetryCallbacks(ctx, &provider.RetryCallbacks{
			OnFirstRetry: func(endpoint string) {
				notifyRetryCtx(ctx, endpoint)
			},
			OnSuccess: func() {
				notifyRetrySuccessCtx(ctx)
			},
		})

		resp, err = provider.Send(ctx, turnClient, req, handler)

		duration := time.Since(start)
		keySuffix := ""
		if resp != nil {
			keySuffix = resp.KeySuffix
		}
		a.logger().Debugf("api_call_done session=%s duration=%s key=%s err=%v", sessionKey, duration, keySuffix, err)

		// Error-and-retry: if a 400 suggests unsupported thinking/effort,
		// strip the offending params and retry once.
		if err != nil {
			if req.Thinking != nil || req.Output != nil || req.Speed != "" {
				var apiErr *provider.APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == 400 {
					body := strings.ToLower(apiErr.Body)
					stripped := false
					if req.Thinking != nil && strings.Contains(body, "thinking") {
						a.logger().Warnf("session=%s model %s rejected thinking param, retrying without it", sessionKey, turnModel)
						req.Thinking = nil
						stripped = true
					}
					if req.Output != nil && (strings.Contains(body, "effort") || strings.Contains(body, "output")) {
						a.logger().Warnf("session=%s model %s rejected effort param, retrying without it", sessionKey, turnModel)
						req.Output = nil
						stripped = true
					}
					if req.Speed != "" && strings.Contains(body, "speed") {
						a.logger().Warnf("session=%s model %s rejected speed param, retrying without it", sessionKey, turnModel)
						req.Speed = ""
						stripped = true
					}
					if stripped {
						resp, err = provider.Send(ctx, turnClient, req, handler)
						duration = time.Since(start)
					}
				}
			}
			if err != nil {
				// Log the failed request payload for debugging.
				a.logErrorPayload(sessionKey, turnModel, start, duration, req, err)

				// Append a synthetic assistant error message so the session
				// maintains role alternation (user→assistant). Without this,
				// the defer safety-net flushes only the user message, causing
				// consecutive user messages on the next turn — which the API
				// rejects with 400, creating a permanent cascade.
				errMsg := provider.Message{
					Role:    "assistant",
					Content: provider.TextContent("(API error — response unavailable)"),
				}
				newMessages = append(newMessages, errMsg)

				// Resolve endpoint for error classification
				a.metaMu.Lock()
				endpoint := sm.modelEndpoint
				if endpoint == "" {
					endpoint = a.Endpoint
				}
				a.metaMu.Unlock()
				return "", a.classifyAPIError(ctx, err, sessionKey, endpoint, duration)
			}
		}

		// Check for cancellation after API call
		if ctx.Err() != nil {
			// Synthetic assistant message for role alternation (same as API error above).
			errMsg := provider.Message{
				Role:    "assistant",
				Content: provider.TextContent("(API error — response unavailable)"),
			}
			newMessages = append(newMessages, errMsg)
			return "", ctx.Err()
		}

		cost := a.logAPIResponse(sessionKey, turnModel, start, duration, req, resp, len(messages))
		a.processAPIResponse(sessionKey, sm, resp, cost, now, maxOutput)

		// Build assistant message from response
		assistantMsg := provider.Message{
			Role:    resp.Role,
			Content: resp.Content,
		}
		messages = append(messages, assistantMsg)
		newMessages = append(newMessages, assistantMsg)

		notifyResponseBlocks(ctx, resp.Content)

		// pause_turn: server paused a long-running turn — continue without client-side tool execution
		if resp.StopReason == "pause_turn" {
			continue
		}

		if resp.StopReason != "tool_use" {
			// Pre-answer verification gate: if the model wants to end the
			// turn and pre_answer rules exist, inject a reminder and let
			// it reconsider once.
			if !verified && a.Nudger != nil && a.NudgePreAnswerGate && i >= a.NudgePreAnswerMinTools {
				if reminder := a.Nudger.CheckPreAnswer(); reminder != "" {
					verifyMsg := provider.Message{
						Role:    "user",
						Content: provider.TextContent(nudgeHeader + reminder),
					}
					messages = append(messages, verifyMsg)
					newMessages = append(newMessages, verifyMsg)
					verified = true
					a.logger().Infof("nudge: pre-answer gate fired at loop %d for session %s", i, sessionKey)
					sendOrBatchText(*resp)
					continue
				}
			}
			// Done — save all new messages and return text
			writer := a.Sessions.For(sessionKey)
			if err := writer.AppendAll(sessionKey, newMessages); err != nil {
				return "", fmt.Errorf("save session: %w", err)
			}
			newMessages = nil // saved — defer won't double-save

			endStats := provider.ComputeSessionStats(messages)
			a.logger().Debugf("turn_end session=%s messages=%d blocks=%d bytes=%d tokens≈%d",
				sessionKey, endStats.Messages, endStats.Blocks, endStats.ApproxBytes, endStats.ApproxTokens())

			// Update session metadata for next turn
			sm.lastMessageTime = now
			sm.prevCost = cost
			sm.prevInput = resp.Usage.InputTokens
			sm.prevOutput = resp.Usage.OutputTokens
			sm.prevCacheWrite = resp.Usage.CacheCreationInputTokens

			a.maybeCompact(ctx, sessionKey, messages, system, &resp.Usage, sm)

			finalText := provider.TextOf(resp.Content)
			if a.BatchPartialAssistantMessages && batchedText.Len() > 0 {
				if finalText != "" {
					batchedText.WriteString(a.BatchPartialJoiner)
					batchedText.WriteString(finalText)
				}
				sentText := batchedText.String()
				a.logConversationSent(convChatID, meta, sessionKey, sentText)
				return sentText, nil
			}
			a.logConversationSent(convChatID, meta, sessionKey, finalText)
			return finalText, nil
		}

		// Handle text in tool_use responses: either send immediately or accumulate for batch delivery.
		sendOrBatchText(*resp)

		// If this is the last allowed iteration, don't execute tools —
		// instead inject descriptive error results so the session JSONL
		// ends with a proper tool_use / tool_result pair.
		if i+1 >= maxLoops {
			var toolResults []provider.ContentBlock
			errMsg := fmt.Sprintf("Tool call not executed: max tool loop depth reached (limit: %d)", maxLoops)
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}
				toolResults = append(toolResults, provider.ToolResultBlock(block.ID, errMsg, true))
			}
			toolMsg := provider.Message{Role: "user", Content: toolResults}
			newMessages = append(newMessages, toolMsg)
			break
		}

		// Execute tool calls. server_tool_use blocks are skipped — already
		// executed by Anthropic's servers.
		toolResults, err := a.executeToolCalls(ctx, td, turnClient, sessionKey, turnModel, resp.Content, messages)
		if err != nil {
			return "", err
		}

		// Steer rewrite: if some tool_use blocks in the assistant message
		// have no matching tool_result (steer skipped them), strip them so
		// the model never sees tools it didn't run. Safe because the
		// assistant message hasn't been flushed to disk yet.
		if filtered, stripped := stripUnmatchedToolUse(resp.Content, toolResults); stripped {
			messages[len(messages)-1].Content = filtered
			newMessages[len(newMessages)-1].Content = filtered
		}

		// Track tool streak and error state for nudge triggers.
		lastToolError = false
		batchToolName := ""
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				batchToolName = block.Name
				break
			}
		}
		if batchToolName != "" && batchToolName == lastToolName {
			sameToolStreak++
		} else {
			sameToolStreak = 1
		}
		lastToolName = batchToolName
		for _, tr := range toolResults {
			if tr.Type == "tool_result" && tr.IsError {
				lastToolError = true
				break
			}
		}

		// Braindead warning detection: fold warning into tool results to avoid
		// a separate user message that breaks tool_use/tool_result adjacency.
		if a.BraindeadWarningEnable && !braindeadWarned && braindeadWarningThreshold > 0 && i+1 >= braindeadWarningThreshold {
			prompt := a.BraindeadWarningPrompt
			if prompt == "" {
				prompt = defaultBraindeadWarningPrompt
			}
			toolResults = append(toolResults, provider.ContentBlock{Type: "text", Text: "[system] " + prompt})
			braindeadWarned = true
			a.logger().Infof("braindead warning injected at loop %d for session %s", i+1, sessionKey)
		}

		// Tool display note: tell the agent once per turn whether the user can see tool results.
		if !displayNoted {
			toolResults = append(toolResults, provider.ContentBlock{Type: "text", Text: toolDisplayNote(a.SessionShowToolCalls(sessionKey))})
			displayNoted = true
		}

		// Nudge reminders: inject behavioral reminders from character file rules.
		if a.Nudger != nil {
			if reminders := a.Nudger.CheckAfterTools(i, sameToolStreak, lastToolError); len(reminders) > 0 {
				for _, r := range reminders {
					toolResults = append(toolResults, provider.ContentBlock{Type: "text", Text: nudgeHeader + r})
				}
				a.logger().Debugf("nudge: injected %d reminder(s) at loop %d for session %s", len(reminders), i, sessionKey)
			}
		}

		// Steer check: catch messages that arrive after all tools in a batch
		// finish but before the next API call. Saves one full round-trip.
		if blocks := steerBlocks(ctx); len(blocks) > 0 {
			toolResults = append(toolResults, blocks...)
			a.logger().Infof("steer: injected %d user message(s) after tool batch for session %s", len(blocks), sessionKey)
		}

		// Append tool results as user message
		toolMsg := provider.Message{
			Role:    "user",
			Content: toolResults,
		}
		messages = append(messages, toolMsg)
		newMessages = append(newMessages, toolMsg)
	}

	// Max loops reached — save what we have and return last text
	sessionFile := sessionKey
	if p, err := a.Sessions.SessionPath(sessionKey); err == nil {
		sessionFile = p
	}
	a.logger().Warnf("max tool call depth reached for session %s", sessionFile)
	writer := a.Sessions.For(sessionKey)
	if err := writer.AppendAll(sessionKey, newMessages); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	newMessages = nil // saved — defer won't double-save

	endStats := provider.ComputeSessionStats(messages)
	a.logger().Debugf("turn_end session=%s messages=%d blocks=%d bytes=%d tokens≈%d",
		sessionKey, endStats.Messages, endStats.Blocks, endStats.ApproxBytes, endStats.ApproxTokens())

	return "Max tool call depth reached.", nil
}

// toolDisplayNote returns a short system note describing whether the user can see tool results.
func toolDisplayNote(mode string) string {
	switch mode {
	case "full":
		return "[display] tool_results=visible — the user can see your tool calls, inputs, and outputs."
	case "preview":
		return "[display] tool_results=preview — the user sees tool names but not inputs or outputs."
	default:
		return "[display] tool_results=hidden — the user cannot see tool calls or results. Narrate important actions and findings in your replies."
	}
}

// logConversationSent logs an outbound conversation entry with the turn's metadata.
func (a *Agent) logConversationSent(chatID int64, meta *TurnMetadata, sessionKey, text string) {
	if text == "" {
		return
	}
	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    meta.UserID,
		Username:  meta.Username,
		ChatID:    chatID,
		Text:      text,
		Session:   sessionKey,
	})
}

// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage provider.Usage
}
