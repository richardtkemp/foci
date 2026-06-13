package tmux

import (
	"regexp"
	"strings"
)

// detectTUIAgent inspects pane content for known TUI agent markers.
// Returns "cc" for Claude Code, "oc" for OpenCode, or "" if no TUI is detected.
func detectTUIAgent(content string) string {
	// Claude Code markers
	ccMarkers := []string{"Claude Code", "⏵⏵ bypass", "Cooked for", "Crunched for", "Baked for"}
	for _, m := range ccMarkers {
		if strings.Contains(content, m) {
			return "cc"
		}
	}
	// OpenCode markers
	ocMarkers := []string{"OpenCode", "GLM", "Build"}
	for _, m := range ocMarkers {
		if strings.Contains(content, m) {
			return "oc"
		}
	}
	return ""
}

// Compiled regex patterns for TUI cleanup — shared across calls.
var (
	// Common patterns
	reConsecutiveBlankLines = regexp.MustCompile(`\n{3,}`)
	reHorizontalSeparator   = regexp.MustCompile(`^[\s─╌═━╍]+$`)

	// Claude Code patterns
	reCCBoxDrawing    = regexp.MustCompile(`^[─╌━═╰╯╭╮▀▁─]+$`)
	reCCPipeBorder    = regexp.MustCompile(`^\s*│\s?`)
	reCCPipeTrail     = regexp.MustCompile(`\s*│\s*$`)
	reCCStatusHints   = regexp.MustCompile(`(?i)(shift\+tab|ctrl\+o|esc to interrupt|esc to undo|\/help for)`)
	reCCVersionLine   = regexp.MustCompile(`^Claude Code\b.*$`)
	reCCModeIndicator = regexp.MustCompile(`^[⏵⏸]+\s*(bypass|plan mode|auto mode)\s*$`)
	reCCDecoSymbols   = regexp.MustCompile(`^[✻✢\s]+$`)
	reCCLogoBlocks    = regexp.MustCompile(`[▟█▙▄▀▐▌░▒▓]+`)

	// OpenCode patterns
	reOCBorder      = regexp.MustCompile(`^[┃╹╻\s]+$`)
	reOCBoxDrawing  = regexp.MustCompile(`^[─┬┴┼├┤┌┐└┘╭╮╰╯━═╌]+$`)
	reOCStatusHints = regexp.MustCompile(`(?i)(esc to close|ctrl\+[a-z]|alt\+[a-z])`)
	reOCVersionLine = regexp.MustCompile(`^OpenCode\b.*$`)
	reOCSidebar     = regexp.MustCompile(`^(MCP|LSP)\s*[│┃]`)
	reOCBuildLine   = regexp.MustCompile(`^Build\s*[│┃]`)
	reOCErrorRetry  = regexp.MustCompile(`(?i)^(error|retrying)\b.*$`)
	reOCSectionHdr  = regexp.MustCompile(`^(Modified Files|Todo)\s*$`)
	reOCDiffSummary = regexp.MustCompile(`^\d+ files? changed`)
)

// cleanTUIOutput strips TUI chrome from pane content based on the detected agent type.
func cleanTUIOutput(content, agentType string) string {
	lines := strings.Split(content, "\n")
	var cleaned []string

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")

		switch agentType {
		case "cc":
			if shouldStripCC(trimmed) {
				continue
			}
			// Strip pipe borders from content lines
			trimmed = reCCPipeBorder.ReplaceAllString(trimmed, "")
			trimmed = reCCPipeTrail.ReplaceAllString(trimmed, "")
		case "oc":
			if shouldStripOC(trimmed) {
				continue
			}
		}

		// Truncate long horizontal separator lines to save tokens
		// (only for lines that weren't stripped by agent-specific logic)
		if reHorizontalSeparator.MatchString(trimmed) && len(trimmed) > 10 {
			trimmed = trimmed[:10]
		}

		cleaned = append(cleaned, trimmed)
	}

	result := strings.Join(cleaned, "\n")
	// Collapse runs of 3+ blank lines down to 2
	result = reConsecutiveBlankLines.ReplaceAllString(result, "\n\n")
	// Trim leading/trailing whitespace
	result = strings.TrimSpace(result)
	return result
}

// shouldStripCC returns true if the line is Claude Code TUI chrome that should be removed.
func shouldStripCC(line string) bool {
	if reCCBoxDrawing.MatchString(line) {
		return true
	}
	if reCCStatusHints.MatchString(line) {
		return true
	}
	if reCCVersionLine.MatchString(line) {
		return true
	}
	if reCCModeIndicator.MatchString(line) {
		return true
	}
	if reCCDecoSymbols.MatchString(line) {
		return true
	}
	if reCCLogoBlocks.MatchString(line) && len(strings.TrimSpace(line)) < 20 {
		return true
	}
	return false
}

// shouldStripOC returns true if the line is OpenCode TUI chrome that should be removed.
func shouldStripOC(line string) bool {
	if reOCBorder.MatchString(line) {
		return true
	}
	if reOCBoxDrawing.MatchString(line) {
		return true
	}
	if reOCStatusHints.MatchString(line) {
		return true
	}
	if reOCVersionLine.MatchString(line) {
		return true
	}
	if reOCSidebar.MatchString(line) {
		return true
	}
	if reOCBuildLine.MatchString(line) {
		return true
	}
	if reOCErrorRetry.MatchString(line) {
		return true
	}
	if reOCSectionHdr.MatchString(line) {
		return true
	}
	if reOCDiffSummary.MatchString(line) {
		return true
	}
	return false
}
