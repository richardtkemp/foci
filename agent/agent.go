package agent

import (
	"context"
	"fmt"
	"log"

	"clod/anthropic"
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
}

// HandleMessage processes a user message in the given session and returns the final text response.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
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
			Messages:  messages,
			Tools:     toolDefs,
		}

		resp, err := a.Client.SendMessage(ctx, req)
		if err != nil {
			return "", fmt.Errorf("send message: %w", err)
		}

		log.Printf("[agent] stop_reason=%s input=%d output=%d cache_read=%d cache_write=%d",
			resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

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

			tool := a.Tools.Get(block.Name)
			if tool == nil {
				toolResults = append(toolResults, anthropic.ToolResultBlock(
					block.ID, fmt.Sprintf("Unknown tool: %s", block.Name), true,
				))
				continue
			}

			log.Printf("[agent] tool_use: %s", block.Name)
			result, err := tool.Execute(ctx, block.Input)
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

		// Append tool results as user message
		toolMsg := anthropic.Message{
			Role:    "user",
			Content: toolResults,
		}
		messages = append(messages, toolMsg)
		newMessages = append(newMessages, toolMsg)
	}

	// Max loops reached — save what we have and return last text
	if err := a.Sessions.AppendAll(sessionKey, newMessages); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	return "Max tool call depth reached.", nil
}

// LastUsage returns the usage from the most recent API response.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage anthropic.Usage
}
