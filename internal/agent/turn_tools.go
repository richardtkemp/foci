package agent

import (
	"context"
	"fmt"
	"time"

	"foci/internal/provider"
	"foci/internal/tools"
)

// processAPIResponse handles post-API-call checks: cache bust detection and
// max_tokens warning. It updates the session metadata cache baseline.
func (a *Agent) processAPIResponse(sessionKey string, sm *sessionMeta, resp *provider.MessageResponse, cost float64, now time.Time, maxOutput int) { // nolint:unparam

	// Cache bust detection: cache_read dropped significantly vs previous request.
	// Only relevant for Anthropic — other providers have different caching semantics.
	format := a.SessionFormat(sessionKey)
	isAnthropic := format == "" || format == "anthropic"
	if a.CacheBustDetect && isAnthropic && len(a.CacheBustAlert) > 0 && sm.prevCacheRead > 0 {
		idleThresh := a.CacheBustIdleThreshold
		if idleThresh == 0 {
			idleThresh = 10 * time.Minute
		}
		idle := !sm.lastMessageTime.IsZero() && now.Sub(sm.lastMessageTime) > idleThresh
		if !idle && resp.Usage.CacheReadInputTokens < sm.prevCacheRead {
			for _, fn := range a.CacheBustAlert {
				fn(sessionKey, sm.prevCacheRead, resp.Usage.CacheReadInputTokens)
			}
		}
	}
	// Update cache baseline after every API call so subsequent iterations
	// within the same tool_use turn don't re-fire the detection.
	sm.prevCacheRead = resp.Usage.CacheReadInputTokens

	// Warn on max_tokens — response was truncated mid-thought
	if resp.StopReason == "max_tokens" {
		warn := fmt.Sprintf("stop_reason=max_tokens on %s (output=%d, limit=%d)", sessionKey, resp.Usage.OutputTokens, maxOutput)
		a.logger().Warnf("%s", warn)
		for _, fn := range a.MaxTokensWarnFunc {
			fn(warn)
		}
	}
}

// notifyResponseBlocks emits thinking blocks and server tool call/result
// notifications to the observer context.
func notifyResponseBlocks(ctx context.Context, content []provider.ContentBlock) {
	for _, block := range content {
		if block.Type == "thinking" {
			notifyThinkingCtx(ctx, block.Thinking)
		}
		if block.Type == "server_tool_use" {
			notifyToolCallCtx(ctx, block.Name, block.Input)
		}
		if block.Type == "web_search_tool_result" || block.Type == "web_fetch_tool_result" {
			notifyToolResultCtx(ctx, block.Type, summarizeServerToolResult(block), false)
		}
	}
}

// executeToolCalls iterates over response content blocks, executes client-side
// tool_use blocks, handles errors, guards oversized results, and redacts secrets.
// If a steer message arrives between tool calls, remaining tools are skipped
// with synthetic error results and the steer text is appended as a [user] block.
func (a *Agent) executeToolCalls(ctx context.Context, td *TurnDetail, turnClient provider.Client, sessionKey, turnModel string, blocks []provider.ContentBlock, messages []provider.Message) ([]provider.ContentBlock, error) {
	toolCtx := tools.WithSessionKey(ctx, sessionKey)

	var toolResults []provider.ContentBlock
	for i := 0; i < len(blocks); i++ {
		block := blocks[i]
		if block.Type != "tool_use" {
			continue
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Check for steer message before executing this tool
		if steer := steerCheckFromCtx(ctx); steer != "" {
			a.logger().Infof("steer: user redirected conversation, skipping remaining tools in session %s", sessionKey)
			// Skip this and all remaining tool_use blocks with synthetic errors
			for j := i; j < len(blocks); j++ {
				if blocks[j].Type == "tool_use" {
					toolResults = append(toolResults, provider.ToolResultBlock(
						blocks[j].ID, "Skipped: user redirected the conversation", true,
					))
				}
			}
			toolResults = append(toolResults, provider.ContentBlock{Type: "text", Text: "[user] " + steer})
			return toolResults, nil
		}

		tool := a.Tools.Get(block.Name)
		if tool == nil {
			a.logger().Warnf("session=%s unknown tool: %s", sessionKey, block.Name)
			toolResults = append(toolResults, provider.ToolResultBlock(
				block.ID, fmt.Sprintf("Unknown tool: %s", block.Name), true,
			))
			signalActivityCtx(ctx)
			continue
		}

		a.logger().Debugf("session=%s tool_use: %s (%d bytes)", sessionKey, block.Name, len(block.Input))
		notifyToolCallCtx(ctx, block.Name, block.Input)
		td.ToolName = block.Name
		result, err := tool.Execute(toolCtx, block.Input)
		td.ToolName = ""
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			a.logger().Debugf("session=%s tool %s error: %v", sessionKey, block.Name, err)
			errMsg := fmt.Sprintf("Error: %s", err)
			if a.Redact != nil {
				errMsg = a.Redact(errMsg)
			}
			toolResults = append(toolResults, provider.ToolResultBlock(
				block.ID, errMsg, true,
			))
			notifyToolResultCtx(ctx, block.Name, errMsg, true)
			signalActivityCtx(ctx)
			continue
		}

		guardedResult := a.guardToolResult(ctx, turnClient, sessionKey, block.Name, turnModel, result, messages)
		if a.Redact != nil {
			guardedResult = a.Redact(guardedResult)
		}
		toolResults = append(toolResults, provider.ToolResultBlock(
			block.ID, guardedResult, false,
		))
		toolResults = append(toolResults, result.ExtraBlocks...)
		notifyToolResultCtx(ctx, block.Name, guardedResult, false)
		signalActivityCtx(ctx)
	}
	return toolResults, nil
}
