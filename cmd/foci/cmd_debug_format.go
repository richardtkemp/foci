package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
)

const (
	maxThinkingLines  = 10
	maxToolResultLen  = 500
	maxToolInputLen   = 200
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorDim    = "\033[2m"
	colorBold   = "\033[1m"
	colorCyan   = "\033[36m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

// formatLine parses a JSONL line and returns formatted output.
// Returns empty string if the line should be skipped.
func formatLine(line []byte) string {
	if len(line) == 0 {
		return ""
	}

	// Check for metadata lines
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &probe) == nil {
		switch probe.Type {
		case "session_meta":
			var meta session.SessionMeta
			if json.Unmarshal(line, &meta) == nil {
				return fmt.Sprintf("%s── session_meta: created %s ──%s\n", colorDim, meta.CreatedAt, colorReset)
			}
			return ""
		case "branch_meta":
			var meta struct {
				Type        string `json:"type"`
				ParentKey   string `json:"parent_key"`
				BranchPoint int    `json:"branch_point"`
			}
			if json.Unmarshal(line, &meta) == nil {
				return fmt.Sprintf("%s── branch_meta: parent=%s branch_point=%d ──%s\n",
					colorDim, meta.ParentKey, meta.BranchPoint, colorReset)
			}
			return ""
		}
	}

	// Parse as message
	var msg provider.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Sprintf("%s[unparseable line: %v]%s\n", colorDim, err, colorReset)
	}

	return formatMessage(msg)
}

// formatMessage renders a provider.Message in human-readable format.
func formatMessage(msg provider.Message) string {
	var b strings.Builder

	// Role header
	ts := time.Now().Format("15:04:05")
	switch msg.Role {
	case "user":
		fmt.Fprintf(&b, "\n%s[%s]%s %s%s═══ USER ═══%s\n",
			colorDim, ts, colorReset, colorBold, colorCyan, colorReset)
	case "assistant":
		fmt.Fprintf(&b, "\n%s[%s]%s %s%s═══ ASSISTANT ═══%s\n",
			colorDim, ts, colorReset, colorBold, colorGreen, colorReset)
	default:
		fmt.Fprintf(&b, "\n%s[%s]%s %s%s═══ %s ═══%s\n",
			colorDim, ts, colorReset, colorBold, colorYellow, strings.ToUpper(msg.Role), colorReset)
	}

	// Content blocks
	for _, block := range msg.Content {
		formatBlock(&b, block)
	}

	return b.String()
}

// formatBlock renders a single content block.
func formatBlock(b *strings.Builder, block provider.ContentBlock) {
	switch block.Type {
	case "text":
		fmt.Fprintf(b, "%s\n", block.Text)

	case "thinking":
		formatThinking(b, block.Thinking)

	case "redacted_thinking":
		fmt.Fprintf(b, "%s  [redacted thinking: %d bytes]%s\n", colorDim, len(block.Data), colorReset)

	case "tool_use":
		formatToolUse(b, block)

	case "tool_result":
		formatToolResult(b, block)

	case "image":
		if block.Source != nil {
			fmt.Fprintf(b, "%s  [image: %s, %d bytes]%s\n",
				colorDim, block.Source.MimeType, len(block.Source.Data), colorReset)
		} else {
			fmt.Fprintf(b, "%s  [image]%s\n", colorDim, colorReset)
		}

	case "document":
		if block.Source != nil {
			fmt.Fprintf(b, "%s  [document: %s, %d bytes]%s\n",
				colorDim, block.Source.MimeType, len(block.Source.Data), colorReset)
		} else {
			fmt.Fprintf(b, "%s  [document]%s\n", colorDim, colorReset)
		}

	default:
		// Server tool blocks and unknown types
		if block.Name != "" {
			fmt.Fprintf(b, "%s  [%s: %s]%s\n", colorDim, block.Type, block.Name, colorReset)
		} else {
			fmt.Fprintf(b, "%s  [%s]%s\n", colorDim, block.Type, colorReset)
		}
	}
}

// formatThinking renders thinking content, abbreviated if long.
func formatThinking(b *strings.Builder, thinking string) {
	if thinking == "" {
		return
	}
	lines := strings.Split(thinking, "\n")
	if len(lines) <= maxThinkingLines {
		for _, line := range lines {
			fmt.Fprintf(b, "%s  │ %s%s\n", colorDim, line, colorReset)
		}
		return
	}
	// Show first and last few lines
	show := maxThinkingLines / 2
	for _, line := range lines[:show] {
		fmt.Fprintf(b, "%s  │ %s%s\n", colorDim, line, colorReset)
	}
	fmt.Fprintf(b, "%s  │ ... (%d lines omitted) ...%s\n", colorDim, len(lines)-maxThinkingLines, colorReset)
	for _, line := range lines[len(lines)-show:] {
		fmt.Fprintf(b, "%s  │ %s%s\n", colorDim, line, colorReset)
	}
}

// formatToolUse renders a tool_use block with tool name and compact input.
func formatToolUse(b *strings.Builder, block provider.ContentBlock) {
	input := compactJSON(block.Input, maxToolInputLen)
	fmt.Fprintf(b, "  %s→ %s%s(%s)%s\n",
		colorYellow, colorBold, block.Name, colorReset+input, colorReset)
}

// formatToolResult renders a tool_result block.
func formatToolResult(b *strings.Builder, block provider.ContentBlock) {
	prefix := "←"
	color := colorDim
	if block.IsError {
		prefix = "✗"
		color = colorRed
	}
	content := block.Content
	if len(content) > maxToolResultLen {
		content = content[:maxToolResultLen] + fmt.Sprintf("... (%d bytes total)", len(block.Content))
	}
	idShort := block.ToolUseID
	if len(idShort) > 12 {
		idShort = idShort[:12] + "…"
	}
	fmt.Fprintf(b, "  %s%s [%s] (%d bytes)%s\n", color, prefix, idShort, len(block.Content), colorReset)
	if content != "" {
		// Show first few lines of content
		lines := strings.SplitN(content, "\n", 6)
		for i, line := range lines {
			if i >= 5 {
				fmt.Fprintf(b, "%s    ...%s\n", colorDim, colorReset)
				break
			}
			fmt.Fprintf(b, "%s    %s%s\n", colorDim, line, colorReset)
		}
	}
}

// compactJSON formats JSON input compactly, truncating if too long.
func compactJSON(raw json.RawMessage, maxLen int) string {
	if len(raw) == 0 {
		return ""
	}

	// Try to pretty-format small inputs
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		s := string(raw)
		if len(s) > maxLen {
			return s[:maxLen] + "…"
		}
		return s
	}

	s := string(raw)
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
