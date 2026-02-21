package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"clod/anthropic"
	"clod/compaction"
	"clod/log"
	"clod/memory"
	"clod/session"
	"clod/tools"
	"clod/workspace"
)

// ImageData holds a raw image for inclusion in a message.
type ImageData struct {
	MediaType string // "image/jpeg", "image/png", etc.
	Data      []byte // raw bytes (base64-encoded when building content blocks)
}

const maxToolLoops = 25
const defaultMaxTokens = 8192

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

// CacheBustFunc is called when cache_write exceeds the threshold.
// session is the session key, tokens is the cache_write count, cost is the write cost.
type CacheBustFunc func(session string, tokens int, cost float64)

// Agent is the core agent loop.
type Agent struct {
	Client    *anthropic.Client
	Sessions  *session.Store
	Tools     *tools.Registry
	Bootstrap *workspace.Bootstrap
	Compactor *compaction.Compactor // nil disables auto-compaction
	Reminders *memory.ReminderStore // nil disables reminder injection
	Model     string

	CacheBustThreshold int           // alert when cache_write exceeds this (0 = disabled)
	CacheBustAlert     CacheBustFunc // callback for alerts (set by telegram bot)
	DuplicateMessages  bool          // send user text twice per API call (improves instruction following)

	processing      int32 // atomic: number of in-flight HandleMessage calls
	metaMu          sync.Mutex
	meta            map[string]*sessionMeta // per-session metadata
	replyMu         sync.Mutex
	replyFunc       ReplyFunc      // optional: set per-turn for intermediate replies
	voiceReplyFunc  VoiceReplyFunc // optional: set per-turn for voice note delivery
}

// IsProcessing returns true if the agent is currently handling a message.
func (a *Agent) IsProcessing() bool {
	return atomic.LoadInt32(&a.processing) > 0
}

// SetReplyFunc sets a callback for intermediate replies during a turn.
// The callback is called from the agent loop goroutine.
func (a *Agent) SetReplyFunc(fn ReplyFunc) {
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	a.replyFunc = fn
}

// VoiceReplyFunc is called to deliver voice audio during a turn.
type VoiceReplyFunc func(oggData []byte)

// SetVoiceReplyFunc sets a callback for voice note delivery during a turn.
func (a *Agent) SetVoiceReplyFunc(fn VoiceReplyFunc) {
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	a.voiceReplyFunc = fn
}

// sendVoice sends a voice note if a VoiceReplyFunc is set.
func (a *Agent) sendVoice(data []byte) {
	a.replyMu.Lock()
	fn := a.voiceReplyFunc
	a.replyMu.Unlock()
	if fn != nil && len(data) > 0 {
		fn(data)
	}
}

// GetVoiceReplyFunc returns the current voice reply function (set per-turn by the telegram bot).
func (a *Agent) GetVoiceReplyFunc() VoiceReplyFunc {
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	return a.voiceReplyFunc
}

// sendIntermediate sends an intermediate reply if a ReplyFunc is set.
func (a *Agent) sendIntermediate(text string) {
	a.replyMu.Lock()
	fn := a.replyFunc
	a.replyMu.Unlock()
	if fn != nil && text != "" {
		fn(text)
	}
}

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
func buildMetaPrefix(now time.Time, model string, sm *sessionMeta) string {
	gap := "none"
	if !sm.lastMessageTime.IsZero() {
		gap = formatGap(now.Sub(sm.lastMessageTime))
	}

	voiceFlag := ""
	if sm.voiceMode {
		voiceFlag = " voice=on"
	}

	if sm.prevCost == 0 && sm.prevInput == 0 {
		// First message in session — no previous turn data
		return fmt.Sprintf("[meta] time=%s gap=%s%s model=%s", now.UTC().Format(time.RFC3339), gap, voiceFlag, model)
	}

	return fmt.Sprintf("[meta] time=%s gap=%s%s model=%s prev_cost=$%.4f prev_tokens=in:%d/out:%d/cR:%d/cW:%d",
		now.UTC().Format(time.RFC3339), gap, voiceFlag, model,
		sm.prevCost,
		sm.prevInput, sm.prevOutput, sm.prevCacheRead, sm.prevCacheWrite)
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

// HandleMessage processes a text-only user message. Delegates to HandleMessageWithImages.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	return a.HandleMessageWithImages(ctx, sessionKey, userMessage, nil)
}

// HandleMessageWithImages processes a user message with optional image attachments.
func (a *Agent) HandleMessageWithImages(ctx context.Context, sessionKey string, userMessage string, images []ImageData) (string, error) {
	atomic.AddInt32(&a.processing, 1)
	defer atomic.AddInt32(&a.processing, -1)

	// Load existing messages
	messages, err := a.Sessions.LoadFull(sessionKey)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	turnModel := a.Model

	// Build metadata prefix and prepend to user message
	now := time.Now()
	sm := a.getSessionMeta(sessionKey)
	metaPrefix := buildMetaPrefix(now, turnModel, sm)
	reminderBlock := a.collectReminders()
	msgBody := userMessage
	if a.DuplicateMessages {
		msgBody = userMessage + "\n\n" + userMessage
	}
	annotatedMessage := metaPrefix + reminderBlock + "\n" + msgBody

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

	// Track new messages to save
	var newMessages []anthropic.Message
	newMessages = append(newMessages, userMsg)

	system := a.Bootstrap.SystemBlocks()
	toolDefs := a.Tools.ToolDefs()

	for i := 0; i < maxToolLoops; i++ {
		cachedMessages := withCacheBreakpoint(messages)
		req := &anthropic.MessageRequest{
			Model:     turnModel,
			MaxTokens: defaultMaxTokens,
			System:    system,
			Messages:  cachedMessages,
			Tools:     toolDefs,
		}

		// Debug: log cache_control placement
		logCacheDebug(system, cachedMessages, turnModel)

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

		// Cache bust alert
		if a.CacheBustThreshold > 0 && resp.Usage.CacheCreationInputTokens > a.CacheBustThreshold && a.CacheBustAlert != nil {
			writeCost := log.CalculateCost(turnModel, 0, 0, 0, resp.Usage.CacheCreationInputTokens)
			a.CacheBustAlert(sessionKey, resp.Usage.CacheCreationInputTokens, writeCost)
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

			// Update session metadata for next turn
			sm.lastMessageTime = now
			sm.prevCost = cost
			sm.prevInput = resp.Usage.InputTokens
			sm.prevOutput = resp.Usage.OutputTokens
			sm.prevCacheRead = resp.Usage.CacheReadInputTokens
			sm.prevCacheWrite = resp.Usage.CacheCreationInputTokens

			// Check if compaction is needed
			if a.Compactor != nil && a.Compactor.ShouldCompact(messages, &resp.Usage) {
				if err := a.Compactor.Compact(ctx, sessionKey, system); err != nil {
					log.Errorf("agent", "compaction failed: %v", err)
				}
				// Reload system prompt — compaction may have changed memory files
				a.Bootstrap.Reload()
			}

			return anthropic.TextOf(resp.Content), nil
		}

		// Send any text in the response as an intermediate reply
		// (the agent said something before/alongside tool calls)
		if intermediateText := anthropic.TextOf(resp.Content); intermediateText != "" {
			a.sendIntermediate(intermediateText)
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
				continue
			}

			log.Debugf("agent", "tool_use: %s", block.Name)
			result, err := tool.Execute(ctx, block.Input)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if err != nil {
				log.Warnf("agent", "tool %s error: %v", block.Name, err)
				toolResults = append(toolResults, anthropic.ToolResultBlock(
					block.ID, fmt.Sprintf("Error: %s", err), true,
				))
				continue
			}

			toolResults = append(toolResults, anthropic.ToolResultBlock(
				block.ID, result, false,
			))
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
	return "Max tool call depth reached.", nil
}

// withCacheBreakpoint returns a copy of messages with cache_control: ephemeral
// set on the last content block of the second-to-last message. This creates a
// cache breakpoint at the conversation history boundary, so the API caches
// system prompt + history and only processes the latest turn. For branch
// sessions, this means the shared prefix gets cache hits instead of rewrites.
// Returns a shallow copy — original messages are not modified.
func withCacheBreakpoint(messages []anthropic.Message) []anthropic.Message {
	if len(messages) < 2 {
		return messages
	}

	result := make([]anthropic.Message, len(messages))
	copy(result, messages)

	// Add cache_control to last content block of second-to-last message
	idx := len(result) - 2
	if len(result[idx].Content) > 0 {
		content := make([]anthropic.ContentBlock, len(result[idx].Content))
		copy(content, result[idx].Content)
		content[len(content)-1].CacheControl = anthropic.Ephemeral()
		result[idx].Content = content
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

// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage anthropic.Usage
}
