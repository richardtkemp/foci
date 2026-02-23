package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clod/anthropic"
	"clod/compaction"
	"clod/log"
	"clod/memory"
	"clod/session"
	"clod/state"
	"clod/tools"
	"clod/workspace"
)

// ImageData holds a raw image for inclusion in a message.
type ImageData struct {
	MediaType string // "image/jpeg", "image/png", etc.
	Data      []byte // raw bytes (base64-encoded when building content blocks)
}

// sessionMeta tracks per-session state for metadata injection.
type sessionMeta struct {
	lastMessageTime time.Time
	prevCost        float64
	prevInput       int
	prevOutput      int
	prevCacheRead   int
	prevCacheWrite  int
	voiceMode       bool
}

// ReplyFunc is called to deliver intermediate messages during a turn.
// Used by the Telegram bot to send early/deferred replies while
// the agent continues working (e.g., "Looking into this...").
type ReplyFunc func(text string)

// ToolCallObserver is called before each tool execution.
// Used by the Telegram bot to show which tools the agent is calling.
type ToolCallObserver func(toolName string, params json.RawMessage)

// CacheBustFunc is called when a cache bust is detected (cache_read drops
// significantly compared to the previous request).
// session is the session key, prevRead is what we had, curRead is what we got.
type CacheBustFunc func(session string, prevRead, curRead int)

// Agent is the core agent loop.
type Agent struct {
	Client    *anthropic.Client
	Sessions  *session.Store
	Tools     *tools.Registry
	Bootstrap *workspace.Bootstrap
	Compactor *compaction.Compactor // nil disables auto-compaction
	Reminders *memory.ReminderStore // nil disables reminder injection
	Model     string

	ExtraSystemBlocks       []anthropic.SystemBlock // additional system blocks (e.g. skills list), injected before cache marker
	CacheStrategy           string                  // "auto" (top-level) or "explicit" (manual breakpoints)
	CacheBustDetect         bool                    // detect cache busts (cache_read drop >50%)
	CacheBustAlert          CacheBustFunc           // callback for cache bust alerts
	DuplicateMessages       bool                    // send user text twice per API call (improves instruction following)
	MaxResultChars          int                     // max chars for tool result before writing to file (0 disables)
	ToolResultTempDir       string                  // where to write large tool results
	Warnings                *WarningQueue           // nil disables warning injection into session
	ManaWatcher             *ManaWatcher            // nil disables mana threshold warnings
	ManaWarnFunc            func(string)            // callback for mana threshold warnings (e.g. Telegram notification)
	StateStore              *state.Store            // nil disables state persistence
	UsageClient             *anthropic.UsageClient  // nil disables mana metadata
	PromptRules             []CompiledPromptRule    // compiled regex rules for inbound message transformation
	CompactionSummaryPrompt string                  // passed to Compactor.Compact(); empty uses default
	CompactionHandoffMsg    string                  // passed to Compactor.Compact(); empty uses default
	MaxToolLoops            int                     // max tool iterations per turn (default 25)
	MaxOutputTokens         int                     // max tokens in model response (default 8192)

	processing    int32 // atomic: number of in-flight HandleMessage calls
	turnLocksMu   sync.Mutex
	turnLocks     map[string]*sync.Mutex // per-session turn serialization
	metaMu        sync.Mutex
	meta          map[string]*sessionMeta // per-session metadata
	manaCacheMu   sync.Mutex
	manaCached    string
	manaCacheTime time.Time
}

// IsProcessing returns true if the agent is currently handling a message.
func (a *Agent) IsProcessing() bool {
	return atomic.LoadInt32(&a.processing) > 0
}

// SetProcessingForTest sets the processing counter directly. Test-only.
func (a *Agent) SetProcessingForTest(n int32) {
	atomic.StoreInt32(&a.processing, n)
}

// VoiceReplyFunc is called to deliver voice audio during a turn.
type VoiceReplyFunc func(oggData []byte)

// VoiceMode returns whether voice mode is active for the session.
func (a *Agent) VoiceMode(sessionKey string) bool {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.voiceMode
}

// SetVoiceMode toggles voice mode for the session.
func (a *Agent) SetVoiceMode(sessionKey string, on bool) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	sm.voiceMode = on

	if a.StateStore != nil {
		a.StateStore.Set("voice:"+sessionKey, on)
	}
}

// RestoreVoiceMode loads voice mode from state store if available.
func (a *Agent) RestoreVoiceMode(sessionKey string) {
	if a.StateStore == nil {
		return
	}
	var on bool
	if a.StateStore.Get("voice:"+sessionKey, &on) && on {
		sm := a.getSessionMeta(sessionKey)
		a.metaMu.Lock()
		sm.voiceMode = on
		a.metaMu.Unlock()
		log.Infof("agent", "restored voice mode for %s", sessionKey)
	}
}

// manaString returns a cached mana percentage string (e.g. "75%").
// Returns empty string if UsageClient is nil or on error.
func (a *Agent) manaString() string {
	if a.UsageClient == nil {
		return ""
	}

	a.manaCacheMu.Lock()
	defer a.manaCacheMu.Unlock()

	// Cache for 5 minutes
	if time.Since(a.manaCacheTime) < 5*time.Minute && a.manaCached != "" {
		return a.manaCached
	}

	usage, err := a.UsageClient.GetUsage(context.Background())
	if err != nil {
		log.Debugf("agent", "mana fetch: %v", err)
		return a.manaCached // return stale on error
	}

	mana := anthropic.FormatMana(usage)
	a.manaCached = mana
	a.manaCacheTime = time.Now()
	return mana
}

func (a *Agent) getSessionMeta(key string) *sessionMeta {
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if a.meta == nil {
		a.meta = make(map[string]*sessionMeta)
	}
	m, ok := a.meta[key]
	if !ok {
		m = &sessionMeta{}
		a.meta[key] = m
	}
	return m
}

// buildMetaPrefix creates the metadata line prepended to user messages.
func buildMetaPrefix(now time.Time, model string, mana string, sm *sessionMeta) string {
	gap := "none"
	if !sm.lastMessageTime.IsZero() {
		gap = formatGap(now.Sub(sm.lastMessageTime))
	}

	voiceFlag := ""
	if sm.voiceMode {
		voiceFlag = " voice=on"
	}

	manaFlag := ""
	if mana != "" {
		manaFlag = " mana=" + mana
	}

	if sm.prevCost == 0 && sm.prevInput == 0 {
		// First message in session — no previous turn data
		return fmt.Sprintf("[meta] time=%s gap=%s%s model=%s%s", now.UTC().Format(time.RFC3339), gap, voiceFlag, model, manaFlag)
	}

	return fmt.Sprintf("[meta] time=%s gap=%s%s model=%s prev_cost=$%.4f prev_tokens=in:%d/out:%d/cR:%d/cW:%d%s",
		now.UTC().Format(time.RFC3339), gap, voiceFlag, model,
		sm.prevCost,
		sm.prevInput, sm.prevOutput, sm.prevCacheRead, sm.prevCacheWrite,
		manaFlag)
}

// formatGap formats a duration as human-readable (e.g., "3h12m", "2d4h", "38s").
func formatGap(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

// isSystemMessage returns true if the message is from a system source
// (heartbeat, scheduled wake) rather than a human user.
func isSystemMessage(msg string) bool {
	return strings.HasPrefix(msg, "[HEARTBEAT]") || strings.HasPrefix(msg, "[SCHEDULED WAKE]")
}

// guardToolResult checks if a tool result exceeds the size limit.
// If it does, writes to a temp file and returns a truncated message with instructions.
// If no limit is set or result is small, returns the original result.
func (a *Agent) guardToolResult(toolName string, result string) string {
	if a.MaxResultChars <= 0 || len(result) <= a.MaxResultChars {
		return result
	}

	// Result is too large — write to file
	if err := os.MkdirAll(a.ToolResultTempDir, 0o700); err != nil {
		log.Warnf("agent", "create tool result temp dir: %v", err)
		// Fall back to returning the full result if we can't write the file
		return result
	}

	// Generate a unique filename
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		log.Warnf("agent", "generate random filename: %v", err)
		return result
	}
	filename := fmt.Sprintf("tool-result-%s-%s.txt", toolName, hex.EncodeToString(randBytes[:]))
	filepath := filepath.Join(a.ToolResultTempDir, filename)

	if err := os.WriteFile(filepath, []byte(result), 0o600); err != nil {
		log.Warnf("agent", "write tool result to file: %v", err)
		return result
	}

	// Return truncated result with file path
	truncated := result[:a.MaxResultChars]
	if len(truncated) < len(result) {
		// Find last newline to avoid cutting in the middle of a line
		if lastNewline := truncated[:len(truncated)-200]; len(lastNewline) > 0 {
			if idx := len(truncated) - 1; idx > 0 && truncated[idx] != '\n' {
				if nlIdx := len(truncated) - 1; nlIdx > len(truncated)-200 {
					for i := nlIdx; i >= nlIdx-200 && i >= 0; i-- {
						if truncated[i] == '\n' {
							truncated = truncated[:i+1]
							break
						}
					}
				}
			}
		}
	}

	msg := fmt.Sprintf("[Result too large: %d chars. Full output saved to %s]\nUse `read` tool to inspect sections. First %d chars:\n%s", len(result), filepath, a.MaxResultChars, truncated)
	return msg
}

// collectReminders returns due reminders formatted for injection into the user message.
// Returns empty string if no reminders are due or the store is nil.
func (a *Agent) collectReminders() string {
	if a.Reminders == nil {
		return ""
	}

	reminders, err := a.Reminders.Due()
	if err != nil {
		log.Errorf("agent", "fetch reminders: %v", err)
		return ""
	}
	if len(reminders) == 0 {
		return ""
	}

	var block string
	block = "\n[reminders]"
	for _, r := range reminders {
		block += fmt.Sprintf("\n- %s (set %s, due: %s)", r.Text, r.DueTag, r.Created.Format("2006-01-02 15:04"))
	}

	// Auto-dismiss surfaced reminders
	if err := a.Reminders.DismissAll(); err != nil {
		log.Errorf("agent", "dismiss reminders: %v", err)
	}

	return block
}

// collectWarnings returns queued warnings formatted for injection into the user message.
// Returns empty string if no warnings are queued or the queue is nil.
func (a *Agent) collectWarnings() string {
	if a.Warnings == nil {
		return ""
	}

	warnings := a.Warnings.Drain()
	if len(warnings) == 0 {
		return ""
	}

	block := "\n[system warnings]"
	for _, w := range warnings {
		block += "\n- " + w
	}
	return block
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

// HandleMessage processes a text-only user message. Delegates to HandleMessageWithImages.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	return a.HandleMessageWithImages(ctx, sessionKey, userMessage, nil)
}

// HandleMessageWithImages processes a user message with optional image attachments.
func (a *Agent) HandleMessageWithImages(ctx context.Context, sessionKey string, userMessage string, images []ImageData) (string, error) {
	// Serialize turns on the same session. Without this, concurrent callers
	// (heartbeat, tmux watch, scheduled wakes, exec auto-background) could
	// load the same session state, run concurrent turns, and interleave their
	// messages in the session file. This would break Anthropic's prefix-matched
	// prompt cache — any insertion in the middle of conversation history
	// invalidates all cached tokens after the insertion point.
	sessionLock := a.turnLock(sessionKey)
	sessionLock.Lock()
	defer sessionLock.Unlock()

	atomic.AddInt32(&a.processing, 1)
	defer atomic.AddInt32(&a.processing, -1)

	// Check if context was cancelled while waiting for the turn lock
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	// Load existing messages
	messages, err := a.Sessions.LoadFull(sessionKey)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	// Repair interrupted tool calls (e.g. SIGTERM during tool execution).
	// If the last message is assistant with tool_use but no tool_result follows,
	// inject synthetic error results so the API accepts the message history.
	if repair := repairInterruptedToolCalls(messages); repair != nil {
		messages = append(messages, *repair)
		if err := a.Sessions.Append(sessionKey, *repair); err != nil {
			log.Errorf("agent", "persist tool call repair: %v", err)
		} else {
			log.Infof("agent", "repaired %d interrupted tool calls in %s", len(repair.Content), sessionKey)
		}
	}

	turnModel := a.Model

	// Apply prompt rules (regex find/replace on inbound message)
	if len(a.PromptRules) > 0 {
		userMessage = ApplyPromptRules(a.PromptRules, userMessage)
	}

	// Build metadata prefix and prepend to user message
	now := time.Now()
	sm := a.getSessionMeta(sessionKey)
	mana := a.manaString()

	// Check mana thresholds and notify user for active conversations only
	// (not heartbeats or scheduled wakes)
	if a.ManaWatcher != nil && !isSystemMessage(userMessage) {
		a.ManaWatcher.CheckAndWarn(mana, func(warn string) {
			if a.ManaWarnFunc != nil {
				a.ManaWarnFunc(warn)
			}
		})
	}

	metaPrefix := buildMetaPrefix(now, turnModel, mana, sm)
	reminderBlock := a.collectReminders()
	warningBlock := a.collectWarnings()
	msgBody := userMessage
	if a.DuplicateMessages {
		msgBody = userMessage + "\n\n" + userMessage
	}
	annotatedMessage := metaPrefix + reminderBlock + warningBlock + "\n" + msgBody

	// Build content blocks: images first, then text
	var contentBlocks []anthropic.ContentBlock
	for _, img := range images {
		contentBlocks = append(contentBlocks, anthropic.ImageBlock(
			img.MediaType,
			base64.StdEncoding.EncodeToString(img.Data),
		))
	}
	contentBlocks = append(contentBlocks, anthropic.ContentBlock{Type: "text", Text: annotatedMessage})

	// Append user message with metadata
	userMsg := anthropic.Message{
		Role:    "user",
		Content: contentBlocks,
	}
	messages = append(messages, userMsg)

	// Track new messages to save. The defer flushes unsaved messages on
	// shutdown (e.g. SIGTERM during a tool call like "systemctl restart clod").
	// Normal exits set newMessages=nil after saving, so the defer is a no-op.
	var newMessages []anthropic.Message
	newMessages = append(newMessages, userMsg)
	defer func() {
		if len(newMessages) > 0 {
			if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
				log.Errorf("agent", "flush in-flight messages: %v", err)
			} else {
				log.Infof("agent", "flushed %d in-flight messages for %s", len(newMessages), sessionKey)
			}
		}
	}()

	system := a.Bootstrap.SystemBlocks()
	useAutoCache := a.CacheStrategy == "auto"

	if useAutoCache {
		// Auto caching: strip all cache_control from system blocks — top-level handles it.
		if len(a.ExtraSystemBlocks) > 0 {
			system = append(system, a.ExtraSystemBlocks...)
		}
		cleanSystem := make([]anthropic.SystemBlock, len(system))
		copy(cleanSystem, system)
		for i := range cleanSystem {
			cleanSystem[i].CacheControl = nil
		}
		system = cleanSystem
	} else if len(a.ExtraSystemBlocks) > 0 && len(system) > 0 {
		// Explicit caching: insert extra blocks before the last block (which has cache_control).
		combined := make([]anthropic.SystemBlock, 0, len(system)+len(a.ExtraSystemBlocks))
		combined = append(combined, system[:len(system)-1]...)
		combined = append(combined, a.ExtraSystemBlocks...)
		combined = append(combined, system[len(system)-1])
		system = combined
	}
	toolDefs := a.Tools.ToolDefs()

	maxLoops := a.MaxToolLoops
	if maxLoops <= 0 {
		maxLoops = 25 // default
	}
	maxOutput := a.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = 8192 // default
	}
	for i := 0; i < maxLoops; i++ {
		var reqMessages []anthropic.Message
		if useAutoCache {
			reqMessages = messages
		} else {
			reqMessages = withCacheBreakpoint(messages)
		}
		req := &anthropic.MessageRequest{
			Model:     turnModel,
			MaxTokens: maxOutput,
			System:    system,
			Messages:  reqMessages,
			Tools:     toolDefs,
		}
		if useAutoCache {
			req.CacheControl = anthropic.Ephemeral()
		}

		// Debug: log cache_control placement
		logCacheDebug(system, reqMessages, turnModel)

		start := time.Now()
		resp, err := a.Client.SendMessage(ctx, req)
		duration := time.Since(start)

		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("send message: %w", err)
		}

		// Check for cancellation after API call
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		cost := log.CalculateCost(turnModel,
			resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

		log.Infof("agent", "stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f",
			resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens, cost)

		log.API(log.APIEntry{
			Timestamp:  start.UTC(),
			Session:    sessionKey,
			Model:      turnModel,
			Input:      resp.Usage.InputTokens,
			Output:     resp.Usage.OutputTokens,
			CacheRead:  resp.Usage.CacheReadInputTokens,
			CacheWrite: resp.Usage.CacheCreationInputTokens,
			CostUSD:    cost,
			DurationMS: duration.Milliseconds(),
			StopReason: resp.StopReason,
		})

		// Full payload logging (opt-in)
		if log.PayloadEnabled() {
			reqJSON, _ := json.Marshal(req)
			respJSON, _ := json.Marshal(resp)
			log.Payload(log.PayloadEntry{
				Timestamp:  start.UTC(),
				Session:    sessionKey,
				Model:      turnModel,
				Request:    reqJSON,
				Response:   respJSON,
				DurationMS: duration.Milliseconds(),
			})
		}

		// Cache bust detection: cache_read dropped significantly vs previous request.
		// Skip first request (no baseline) — prevCacheRead will be 0.
		if a.CacheBustDetect && a.CacheBustAlert != nil && sm.prevCacheRead > 0 {
			if resp.Usage.CacheReadInputTokens < sm.prevCacheRead/2 {
				a.CacheBustAlert(sessionKey, sm.prevCacheRead, resp.Usage.CacheReadInputTokens)
			}
		}

		// Build assistant message from response
		assistantMsg := anthropic.Message{
			Role:    resp.Role,
			Content: resp.Content,
		}
		messages = append(messages, assistantMsg)
		newMessages = append(newMessages, assistantMsg)

		if resp.StopReason != "tool_use" {
			// Done — save all new messages and return text
			if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
				return "", fmt.Errorf("save session: %w", err)
			}
			newMessages = nil // saved — defer won't double-save

			// Update session metadata for next turn
			sm.lastMessageTime = now
			sm.prevCost = cost
			sm.prevInput = resp.Usage.InputTokens
			sm.prevOutput = resp.Usage.OutputTokens
			sm.prevCacheRead = resp.Usage.CacheReadInputTokens
			sm.prevCacheWrite = resp.Usage.CacheCreationInputTokens

			// Check if compaction is needed
			if a.Compactor != nil && a.Compactor.ShouldCompact(messages, &resp.Usage) {
				if err := a.Compactor.Compact(ctx, sessionKey, system, a.CompactionSummaryPrompt, a.CompactionHandoffMsg); err != nil {
					log.Errorf("agent", "compaction failed: %v", err)
				}
				// Reload system prompt — compaction may have changed memory files
				a.Bootstrap.Reload()
				// Reset cache baseline — next request will have a different prefix
				sm.prevCacheRead = 0
			}

			return anthropic.TextOf(resp.Content), nil
		}

		// Send any text in the response as an intermediate reply
		// (the agent said something before/alongside tool calls)
		if intermediateText := anthropic.TextOf(resp.Content); intermediateText != "" {
			sendIntermediateCtx(ctx, intermediateText)
		}

		// Build tool execution context: inject voice reply func so TTS
		// tool can extract it without importing agent (via tools.VoiceReplyFuncFromContext).
		toolCtx := ctx
		if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.VoiceReplyFunc != nil {
			toolCtx = tools.WithVoiceReplyFunc(ctx, tools.VoiceReplyFunc(cb.VoiceReplyFunc))
		}

		// Execute tool calls
		var toolResults []anthropic.ContentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}

			// Check for cancellation between tool calls
			if ctx.Err() != nil {
				return "", ctx.Err()
			}

			tool := a.Tools.Get(block.Name)
			if tool == nil {
				log.Warnf("agent", "unknown tool: %s", block.Name)
				toolResults = append(toolResults, anthropic.ToolResultBlock(
					block.ID, fmt.Sprintf("Unknown tool: %s", block.Name), true,
				))
				signalActivityCtx(ctx)
				continue
			}

			log.Debugf("agent", "tool_use: %s", block.Name)
			notifyToolCallCtx(ctx, block.Name, block.Input)
			result, err := tool.Execute(toolCtx, block.Input)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if err != nil {
				log.Warnf("agent", "tool %s error: %v", block.Name, err)
				toolResults = append(toolResults, anthropic.ToolResultBlock(
					block.ID, fmt.Sprintf("Error: %s", err), true,
				))
				signalActivityCtx(ctx)
				continue
			}

			// Guard against oversized tool results
			guardedResult := a.guardToolResult(block.Name, result)
			toolResults = append(toolResults, anthropic.ToolResultBlock(
				block.ID, guardedResult, false,
			))
			signalActivityCtx(ctx)
		}

		// Append tool results as user message
		toolMsg := anthropic.Message{
			Role:    "user",
			Content: toolResults,
		}
		messages = append(messages, toolMsg)
		newMessages = append(newMessages, toolMsg)
	}

	// Max loops reached — save what we have and return last text
	log.Warnf("agent", "max tool call depth reached for session %s", sessionKey)
	if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	newMessages = nil // saved — defer won't double-save
	return "Max tool call depth reached.", nil
}

// withCacheBreakpoint returns a deep copy of messages with cache_control set
// on exactly one place: the last content block of the second-to-last message.
// All other cache_control markers are stripped. This ensures exactly 1 message
// breakpoint per API call (plus the system prompt breakpoint = 2 total).
//
// Deep copy is critical: the originals are saved to session history and must
// never have cache_control persisted, or it accumulates across turns and
// mutates the prefix (causing cache misses).
func withCacheBreakpoint(messages []anthropic.Message) []anthropic.Message {
	// Deep copy all messages, stripping any existing cache_control
	result := make([]anthropic.Message, len(messages))
	for i, msg := range messages {
		content := make([]anthropic.ContentBlock, len(msg.Content))
		copy(content, msg.Content)
		for j := range content {
			content[j].CacheControl = nil
		}
		result[i] = anthropic.Message{Role: msg.Role, Content: content}
	}

	// Add the one breakpoint to second-to-last message
	if len(result) >= 2 {
		idx := len(result) - 2
		if len(result[idx].Content) > 0 {
			result[idx].Content[len(result[idx].Content)-1].CacheControl = anthropic.Ephemeral()
		}
	}

	return result
}

// logCacheDebug logs cache_control placement and warns about minimum token thresholds.
func logCacheDebug(system []anthropic.SystemBlock, messages []anthropic.Message, model string) {
	// Estimate tokens: ~4 chars per token (rough heuristic)
	const charsPerToken = 4

	var systemChars int
	var systemCacheIdx = -1
	for i, block := range system {
		systemChars += len(block.Text)
		if block.CacheControl != nil {
			systemCacheIdx = i
		}
	}
	systemTokensEst := systemChars / charsPerToken

	var msgCacheIdx = -1
	for i, msg := range messages {
		for _, block := range msg.Content {
			if block.CacheControl != nil {
				msgCacheIdx = i
				break
			}
		}
	}

	log.Debugf("agent", "cache: system=%d blocks, ~%d tokens, breakpoint=%d; messages=%d, breakpoint=%d",
		len(system), systemTokensEst, systemCacheIdx, len(messages), msgCacheIdx)

	// Warn about minimum token thresholds
	minTokens := 2048 // Haiku default
	if model == "claude-sonnet-4-5" || model == "claude-opus-4-6" {
		minTokens = 1024
	}

	if len(system) > 0 && systemTokensEst < minTokens {
		log.Warnf("agent", "system prompt ~%d tokens is below %s minimum of %d for caching — cache will not activate",
			systemTokensEst, model, minTokens)
	}
}

// repairInterruptedToolCalls checks if the last message in the history is an
// assistant message with tool_use blocks that have no following tool_result.
// This happens when SIGTERM kills the process during tool execution — the defer
// flushes the assistant message but no tool_result was ever created.
// Returns a synthetic tool_result message to append, or nil if no repair needed.
func repairInterruptedToolCalls(messages []anthropic.Message) *anthropic.Message {
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" {
		return nil
	}

	var toolUseIDs []string
	for _, block := range last.Content {
		if block.Type == "tool_use" {
			toolUseIDs = append(toolUseIDs, block.ID)
		}
	}
	if len(toolUseIDs) == 0 {
		return nil
	}

	var results []anthropic.ContentBlock
	for _, id := range toolUseIDs {
		results = append(results, anthropic.ToolResultBlock(id, "Tool call interrupted by service restart", true))
	}
	return &anthropic.Message{Role: "user", Content: results}
}

// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage anthropic.Usage
}
