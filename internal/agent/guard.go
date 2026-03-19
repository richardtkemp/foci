package agent

import (
	"context"
	"crypto/rand"
	"unicode/utf8"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/tools"
)

// missingQueryTools caches which of jq/mdq/yq are not installed.
// Checked once on first guard trigger (via sync.Once).
var (
	missingQueryTools     map[string]bool
	missingQueryToolsOnce sync.Once
)

// checkMissingQueryTools probes PATH for jq, mdq, and yq, returning
// a set of the tool names that are not installed.
func checkMissingQueryTools() map[string]bool {
	missing := make(map[string]bool)
	for _, name := range []string{"jq", "mdq", "yq"} {
		if _, err := exec.LookPath(name); err != nil {
			missing[name] = true
		}
	}
	return missing
}

// getMissingQueryTools returns the cached set of missing tools,
// running the check on first call. Tests can override by setting
// missingQueryTools directly before calling (the Once will then
// populate only if unset).
func getMissingQueryTools() map[string]bool {
	missingQueryToolsOnce.Do(func() {
		missingQueryTools = checkMissingQueryTools()
	})
	return missingQueryTools
}


func detectContentExtension(content string) string {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) > 0 {
		switch trimmed[0] {
		case '{', '[':
			return ".json"
		case '#':
			return ".md"
		case '<':
			if strings.HasPrefix(trimmed, "<?xml") || strings.HasPrefix(trimmed, "<rss") {
				return ".xml"
			}
			return ".html"
		}
	}
	return ".txt"
}

func (a *Agent) guardToolResult(ctx context.Context, client provider.Client, sessionKey, toolName, turnModel string, tr tools.ToolResult, messages []provider.Message) string {
	result := tr.Text
	if a.MaxResultChars <= 0 || utf8.RuneCountInString(result) <= a.MaxResultChars {
		return result
	}

	// If the tool already spilled to a file, use that path instead of writing again.
	fpath := tr.ResultFile
	if fpath == "" {
		if err := os.MkdirAll(a.ToolResultTempDir, 0o700); err != nil {
			a.logger().Warnf("session=%s create tool result temp dir: %v", sessionKey, err)
			return result
		}

		var randBytes [8]byte
		if _, err := rand.Read(randBytes[:]); err != nil {
			a.logger().Warnf("session=%s generate random filename: %v", sessionKey, err)
			return result
		}
		ext := detectContentExtension(result)
		filename := fmt.Sprintf("tool-result-%s-%s%s", toolName, hex.EncodeToString(randBytes[:]), ext)
		fpath = filepath.Join(a.ToolResultTempDir, filename)

		if err := os.WriteFile(fpath, []byte(result), 0o600); err != nil {
			a.logger().Warnf("session=%s write tool result to file: %v", sessionKey, err)
			return result
		}
	}

	resultLen := utf8.RuneCountInString(result)
	if tr.ResultSize > 0 {
		resultLen = int(tr.ResultSize) // use the full size, not the truncated head
	}

	a.logger().Debugf("session=%s tool result guard: %s produced %d chars (limit %d), saved to %s", sessionKey, toolName, resultLen, a.MaxResultChars, fpath)

	// Try to auto-summarise via Haiku (skip if disabled or result exceeds MaxSummaryChars)
	if a.AutoSummarise && client != nil && len(a.ModelConfigs) > 0 && (a.MaxSummaryChars <= 0 || resultLen <= a.MaxSummaryChars) {
		if summary := a.summariseToolResult(ctx, client, sessionKey, toolName, turnModel, result, messages, fpath); summary != "" {
			return summary
		}
	}

	hint := guardHint(result, fpath)
	return fmt.Sprintf("Result too large (%d chars, limit %d). Full output saved to %s.\n%s", resultLen, a.MaxResultChars, fpath, hint)
}

// summariseToolResult calls a cheap model to produce a summary of an oversized tool result.
// Returns the formatted summary string, or empty string on failure (caller falls back).
func (a *Agent) summariseToolResult(ctx context.Context, _ provider.Client, sessionKey, toolName, _ string, result string, messages []provider.Message, savedPath string) string {
	summaryClient, model, _ := a.ResolveCallSite(config.CallSummarizeTool, sessionKey)

	convContext := recentContext(messages, a.SummaryContextTurns, a.SummaryContextChars)

	// Truncate result text embedded in summary prompt to cap memory and tokens.
	// The full result is already on disk at savedPath.
	summaryInput := result
	if a.MaxSummaryInputChars > 0 && utf8.RuneCountInString(summaryInput) > a.MaxSummaryInputChars {
		summaryInput = summaryInput[:a.MaxSummaryInputChars] + // byte slice; may split a multi-byte rune
			fmt.Sprintf("\n[... truncated — full output is %d chars, only first %d shown]", utf8.RuneCountInString(result), a.MaxSummaryInputChars)
	}

	var userText string
	if convContext != "" {
		userText = fmt.Sprintf("<context>\n%s\n</context>\n\n<tool_output tool=%q>\n%s\n</tool_output>\n\nSummarise this tool output. First give a general overview, then list the parts most relevant to the conversation context with exact quotes and their addresses (line numbers, section headers, JSON paths, or key names) so the reader knows exactly where to look for details.",
			convContext, toolName, summaryInput)
	} else {
		userText = fmt.Sprintf("<tool_output tool=%q>\n%s\n</tool_output>\n\nSummarise this tool output. First give a general overview, then list the key sections or data points with exact quotes and their addresses (line numbers, section headers, JSON paths, or key names) so the reader knows exactly where to look for details.",
			toolName, summaryInput)
	}

	req := &provider.MessageRequest{
		Model:     model,
		MaxTokens: 4096,
		System: []provider.SystemBlock{
			{Type: "text", Text: "You are a tool output summarisation assistant. Your job is to summarise oversized tool output so the reader gets useful visibility without the full content in context.\n\nYour summary must have two parts:\n1. **Overview**: A concise general summary of the content (what it is, how large, key structure).\n2. **Relevant details**: Exact quotes from the parts most relevant to the conversation context, each annotated with its address — line number, section header, JSON path, key name, or other locator. These addresses let the reader jump directly to the source if they need more detail.\n\nBe concise. Preserve exact values (numbers, names, paths, error messages) rather than paraphrasing them."},
		},
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent(userText)},
		},
	}

	start := time.Now()
	resp, err := provider.Send(ctx, summaryClient, req, nil,
		a.FallbackFunc, a.ClientProvider, a.logger().Errorf)
	if err != nil {
		a.logger().Warnf("session=%s auto-summary failed for %s: %v", sessionKey, toolName, err)
		return ""
	}

	duration := time.Since(start)
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	a.logger().Infof("session=%s auto-summary model=%s input=%d output=%d cost=$%.4f duration=%s",
		sessionKey, model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	summary := provider.TextOf(resp.Content)
	if summary == "" {
		return ""
	}

	return fmt.Sprintf("[Auto-summary by %s — full output (%d chars) saved to %s]\n\n%s", model, utf8.RuneCountInString(result), savedPath, summary)
}

// recentContext extracts text from the last N conversation turns,
// capped at maxChars. Skips tool_use and tool_result blocks.
func recentContext(messages []provider.Message, maxTurns, maxChars int) string {
	if maxTurns <= 0 || maxChars <= 0 || len(messages) == 0 {
		return ""
	}

	var parts []string
	total := 0
	turns := 0
	for i := len(messages) - 1; i >= 0 && turns < maxTurns; i-- {
		msg := messages[i]
		var text string
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				text = block.Text
				break
			}
		}
		if text == "" {
			continue
		}
		turns++
		remaining := maxChars - total
		rc := utf8.RuneCountInString(text)
		if rc > remaining {
			text = text[:remaining] // byte slice; may split a multi-byte rune
			rc = remaining
		}
		parts = append(parts, fmt.Sprintf("[%s] %s", msg.Role, text))
		total += rc
		if total >= maxChars {
			break
		}
	}
	// Reverse to chronological order
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "\n")
}

// guardHint returns a contextual suggestion for how to extract data from a
// saved tool result file, based on content sniffing. Includes the file path
// in example commands so the agent can copy-paste. If the suggested tool is
// not installed, appends an install recommendation.
func guardHint(content, path string) string {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) == 0 {
		return fmt.Sprintf("Use the `summary` tool to extract specific information from %s.", path)
	}
	// Check TOML before JSON — both can start with '[' but TOML sections start with [letter
	if looksLikeTOML(trimmed) {
		return withInstallHint(fmt.Sprintf("Use `yq` to query, e.g. `yq '.section.key' %s`.", path), "yq")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return withInstallHint(fmt.Sprintf("Use `jq` to query, e.g. `jq 'keys' %s` or `jq '.items[:3]' %s`.", path, path), "jq")
	}
	if trimmed[0] == '#' {
		return withInstallHint(fmt.Sprintf("Use `mdq` to query sections, e.g. `mdq '# Section' %s`.", path), "mdq")
	}
	if detectContentExtension(content) == ".xml" {
		return withInstallHint(fmt.Sprintf("Use `yq` to query, e.g. `yq -p xml '.' %s`.", path), "yq")
	}
	if looksLikeYAML(trimmed) {
		return withInstallHint(fmt.Sprintf("Use `yq` to query, e.g. `yq '.key' %s`.", path), "yq")
	}
	hint := fmt.Sprintf("Use the `summary` tool to extract specific information from %s.", path)
	// For plain text, suggest installing any missing query tools that could help
	// with structured data next time.
	missing := getMissingQueryTools()
	if len(missing) > 0 {
		var names []string
		for _, name := range []string{"jq", "mdq", "yq"} {
			if missing[name] {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			hint += fmt.Sprintf("\nTip: install %s for more efficient querying of structured files.", strings.Join(names, ", "))
		}
	}
	return hint
}

// withInstallHint appends an install recommendation if the named tool
// is not found in PATH.
func withInstallHint(hint, toolName string) string {
	missing := getMissingQueryTools()
	if !missing[toolName] {
		return hint
	}
	return hint + fmt.Sprintf("\nNote: `%s` is not installed. Install it to use this approach, or use the `summary` tool instead.", toolName)
}

// looksLikeTOML checks if content starts with TOML-like patterns (e.g. [section] or key = value).
func looksLikeTOML(trimmed string) bool {
	if len(trimmed) == 0 {
		return false
	}
	// [section] at start of line — must start with a letter (not digit, quote, brace)
	if trimmed[0] == '[' && len(trimmed) > 1 && isLetter(trimmed[1]) {
		if idx := strings.IndexByte(trimmed, ']'); idx > 1 && idx < 80 {
			return true
		}
	}
	// key = value pattern on first line
	firstLine := trimmed
	if nl := strings.IndexByte(trimmed, '\n'); nl > 0 {
		firstLine = trimmed[:nl]
	}
	if strings.Contains(firstLine, " = ") {
		return true
	}
	return false
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// looksLikeYAML checks if content starts with YAML-like patterns (e.g. key: value or ---).
func looksLikeYAML(trimmed string) bool {
	if len(trimmed) == 0 {
		return false
	}
	if strings.HasPrefix(trimmed, "---") {
		return true
	}
	firstLine := trimmed
	if nl := strings.IndexByte(trimmed, '\n'); nl > 0 {
		firstLine = trimmed[:nl]
	}
	// key: value (but not URLs like http:)
	if idx := strings.Index(firstLine, ": "); idx > 0 && !strings.Contains(firstLine[:idx], "//") {
		return true
	}
	return false
}
