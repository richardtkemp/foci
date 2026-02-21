package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"clod/anthropic"
	"clod/log"
	"clod/session"
	"clod/tools"
	"clod/workspace"
)

const maxToolLoops = 25
const defaultMaxTokens = 8192

// Agent is the core agent loop.
type Agent struct {
	Client    *anthropic.Client
	Sessions  *session.Store
	Tools     *tools.Registry
	Bootstrap *workspace.Bootstrap
	Model     string

	processing int32 // atomic: number of in-flight HandleMessage calls
}

// IsProcessing returns true if the agent is currently handling a message.
func (a *Agent) IsProcessing() bool {
	return atomic.LoadInt32(&a.processing) > 0
}

// HandleMessage processes a user message in the given session and returns the final text response.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	atomic.AddInt32(&a.processing, 1)
	defer atomic.AddInt32(&a.processing, -1)

	// Load existing messages
	messages, err := a.Sessions.LoadFull(sessionKey)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	// Append user message
	userMsg := anthropic.Message{
		Role:    "user",
		Content: anthropic.TextContent(userMessage),
	}
	messages = append(messages, userMsg)

	// Track new messages to save
	var newMessages []anthropic.Message
	newMessages = append(newMessages, userMsg)

	system := a.Bootstrap.SystemBlocks()
	toolDefs := a.Tools.ToolDefs()

	for i := 0; i < maxToolLoops; i++ {
		req := &anthropic.MessageRequest{
			Model:     a.Model,
			MaxTokens: defaultMaxTokens,
			System:    system,
			Messages:  withCacheBreakpoint(messages),
			Tools:     toolDefs,
		}

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

		cost := log.CalculateCost(a.Model,
			resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

		log.Infof("agent", "stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f",
			resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens, cost)

		log.API(log.APIEntry{
			Timestamp:  start.UTC(),
			Session:    sessionKey,
			Model:      a.Model,
			Input:      resp.Usage.InputTokens,
			Output:     resp.Usage.OutputTokens,
			CacheRead:  resp.Usage.CacheReadInputTokens,
			CacheWrite: resp.Usage.CacheCreationInputTokens,
			CostUSD:    cost,
			DurationMS: duration.Milliseconds(),
			StopReason: resp.StopReason,
		})

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
			return anthropic.TextOf(resp.Content), nil
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

// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage anthropic.Usage
}
