package ccstream

import (
	"encoding/json"
	"strings"

	"foci/internal/log"
)

// autoApproveRule is a parsed permission auto-approve rule.
type autoApproveRule struct {
	toolName string // e.g. "Bash", "Read", "Edit"
	pattern  string // e.g. "git *", "/home/foci/**" — empty means match any input
}

// CommonReadonlyRules is the built-in list of safe, read-only tools and
// commands that are auto-approved when auto_approve_common_readonly is true.
// Format matches user-facing rules: "ToolName" or "ToolName:pattern".
var CommonReadonlyRules = []string{
	// Read-only tools — blanket access everywhere.
	"Search",
	"Glob",
	"Grep",
	"Read",
	"WebSearch",
	"WebFetch",
	// Basic shell commands — read-only, safe to auto-approve.
	"Bash:ls",
	"Bash:echo",
	"Bash:cat",
	"Bash:head",
	"Bash:tail",
	"Bash:wc",
	"Bash:sort",
	"Bash:cut",
	"Bash:tr",
	"Bash:diff",
	"Bash:stat",
	"Bash:file",
	"Bash:which",
	"Bash:date",
	"Bash:pwd",
	"Bash:id",
	"Bash:uname",
	"Bash:ps",
	"Bash:ss",
	"Bash:du",
	"Bash:df",
	// Search/filter tools.
	"Bash:grep",
	"Bash:rg",
	"Bash:ack",
	"Bash:sed -n",
	// Compressed file inspection.
	"Bash:zcat",
	"Bash:zgrep",
	// Environment and system inspection.
	"Bash:env",
	"Bash:crontab -l",
	"Bash:npm list",
	// System logs.
	"Bash:journalctl",
	// Data tools.
	"Bash:jq",
	"Bash:yq",
	"Bash:mds",
	"Bash:mdq",
	"Bash:sqlite3",
	// Foci shell functions.
	"Bash:foci_todo",
	"Bash:foci_send_to_chat",
	"Bash:foci_memory_search",
	"Bash:foci_http_request",
	"Bash:foci_web_search",
	"Bash:foci_web_fetch",
}

// parseAutoApproveRule splits a rule string into tool name and optional pattern.
// Format: "ToolName" (match any input) or "ToolName:pattern" (match input).
func parseAutoApproveRule(rule string) autoApproveRule {
	if idx := strings.IndexByte(rule, ':'); idx >= 0 {
		return autoApproveRule{
			toolName: rule[:idx],
			pattern:  rule[idx+1:],
		}
	}
	return autoApproveRule{toolName: rule}
}

// parseAutoApproveRules parses a slice of rule strings into compiled rules.
func parseAutoApproveRules(rules []string) []autoApproveRule {
	parsed := make([]autoApproveRule, len(rules))
	for i, r := range rules {
		parsed[i] = parseAutoApproveRule(r)
	}
	return parsed
}

// matchAutoApprove checks whether a permission request matches any auto-approve
// rule. Returns true if the request should be auto-approved.
//
// For Bash commands, the command is split on shell operators (&&, ||, ;, |)
// and every segment must independently match at least one Bash rule. This
// prevents "git status && rm -rf /" from being auto-approved by a "git *" rule.
func matchAutoApprove(rules []autoApproveRule, toolName string, input json.RawMessage) bool {
	if toolName == "Bash" {
		return matchBashAutoApprove(rules, input)
	}
	return matchToolAutoApprove(rules, toolName, input)
}

// matchToolAutoApprove handles non-Bash tools: any single rule match suffices.
func matchToolAutoApprove(rules []autoApproveRule, toolName string, input json.RawMessage) bool {
	for _, r := range rules {
		if r.matchesTool(toolName, input) {
			return true
		}
	}
	return false
}

// matchBashAutoApprove splits a Bash command on shell operators and requires
// every segment to match at least one Bash rule.
func matchBashAutoApprove(rules []autoApproveRule, input json.RawMessage) bool {
	command := extractMatchString("Bash", input)
	if command == "" {
		return false
	}

	segments := splitShellCommand(command)
	if len(segments) == 0 {
		return false
	}

	for _, seg := range segments {
		if !matchBashSegment(rules, seg) {
			return false
		}
	}
	return true
}

// matchBashSegment checks whether a single command segment (no shell operators)
// matches at least one Bash rule.
func matchBashSegment(rules []autoApproveRule, segment string) bool {
	for _, r := range rules {
		if r.toolName != "Bash" {
			continue
		}
		if r.pattern == "" {
			return true // tool-name-only Bash rule: matches any command
		}
		if matchPattern(r.pattern, segment) {
			return true
		}
	}
	return false
}

// matchesTool checks whether this rule matches the given non-Bash tool invocation.
func (r autoApproveRule) matchesTool(toolName string, input json.RawMessage) bool {
	if r.toolName != toolName {
		return false
	}
	if r.pattern == "" {
		return true // tool-name-only rule: match any invocation
	}

	matchStr := extractMatchString(toolName, input)
	if matchStr == "" {
		return false
	}

	return matchPattern(r.pattern, matchStr)
}

// splitShellCommand splits a command string on unquoted shell operators
// (&&, ||, ;, |) into individual command segments. Respects single and
// double quotes, and backslash escapes. Each segment is trimmed.
// Returns nil if parsing fails (unmatched quotes).
func splitShellCommand(cmd string) []string {
	var segments []string
	var cur strings.Builder
	i := 0

	for i < len(cmd) {
		ch := cmd[i]

		// Backslash escape — skip next char.
		if ch == '\\' && i+1 < len(cmd) {
			cur.WriteByte(ch)
			cur.WriteByte(cmd[i+1])
			i += 2
			continue
		}

		// Quoted strings — consume until matching quote.
		if ch == '\'' || ch == '"' {
			end := indexUnescapedQuote(cmd, i+1, ch)
			if end < 0 {
				return nil // unmatched quote → fail safe (prompt user)
			}
			cur.WriteString(cmd[i : end+1])
			i = end + 1
			continue
		}

		// Check for shell operators.
		if ch == '&' && i+1 < len(cmd) && cmd[i+1] == '&' {
			segments = appendSegment(segments, &cur)
			i += 2
			continue
		}
		if ch == '|' && i+1 < len(cmd) && cmd[i+1] == '|' {
			segments = appendSegment(segments, &cur)
			i += 2
			continue
		}
		if ch == '|' || ch == ';' {
			segments = appendSegment(segments, &cur)
			i++
			continue
		}

		// $(...) and backtick subshells are dangerous — fail safe.
		if ch == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
			return nil
		}
		if ch == '`' {
			return nil
		}

		cur.WriteByte(ch)
		i++
	}

	segments = appendSegment(segments, &cur)

	if len(segments) == 0 {
		return nil
	}
	return segments
}

// indexUnescapedQuote returns the index of the next unescaped quote character
// starting from position start, or -1 if not found.
func indexUnescapedQuote(s string, start int, quote byte) int {
	for i := start; i < len(s); i++ {
		if s[i] == '\\' && quote == '"' {
			i++ // skip escaped char in double quotes
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

// appendSegment trims and appends the current builder content as a segment.
func appendSegment(segments []string, cur *strings.Builder) []string {
	s := strings.TrimSpace(cur.String())
	cur.Reset()
	if s != "" {
		segments = append(segments, s)
	}
	return segments
}

// toolMatchKeys maps CC tool names to the JSON input field used for pattern
// matching. Tools not listed here only support tool-name-only rules.
var toolMatchKeys = map[string]string{
	"Bash":         "command",
	"Read":         "file_path",
	"Edit":         "file_path",
	"Write":        "file_path",
	"NotebookEdit": "file_path",
	"Glob":         "pattern",
	"Grep":         "pattern",
	"WebFetch":     "url",
	"WebSearch":    "query",
}

// extractMatchString extracts the string to match against from the tool input JSON.
// Returns "" if the tool has no match key or the field is missing/unparseable.
func extractMatchString(toolName string, input json.RawMessage) string {
	key, ok := toolMatchKeys[toolName]
	if !ok || len(input) == 0 {
		return ""
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}

	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// matchPattern checks whether str matches pattern. The pattern supports two modes:
//   - If the pattern contains * or ?: glob matching where * matches any sequence
//     of characters (including / and spaces) and ? matches any single character.
//   - Otherwise: command-prefix matching — str must equal pattern exactly, or
//     start with pattern followed by a space (word boundary).
func matchPattern(pattern, str string) bool {
	if strings.ContainsAny(pattern, "*?") {
		return globMatch(pattern, str)
	}
	// Prefix match with word boundary.
	return str == pattern || strings.HasPrefix(str, pattern+" ")
}

// globMatch implements simple glob matching where * matches any sequence of
// characters (including path separators and spaces) and ? matches exactly
// one character. All other characters are matched literally.
func globMatch(pattern, str string) bool {
	return doGlob(pattern, str)
}

// doGlob is the recursive glob matcher. It uses the standard two-pointer
// backtracking algorithm for O(n*m) worst case.
func doGlob(pattern, str string) bool {
	// Iterative backtracking — avoids stack overflow on long inputs.
	px, sx := 0, 0           // pattern and string cursors
	starPx, starSx := -1, -1 // last * position for backtracking

	for sx < len(str) {
		switch {
		case px < len(pattern) && pattern[px] == '*':
			// Record * position for backtracking.
			starPx = px
			starSx = sx
			px++
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == str[sx]):
			px++
			sx++
		case starPx >= 0:
			// Backtrack: advance the match position of the last *.
			starSx++
			sx = starSx
			px = starPx + 1
		default:
			return false
		}
	}
	// Consume trailing *s in pattern.
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// autoApprovePermission checks the request against compiled rules and, if
// matched, sends an allow response directly. Returns true if auto-approved.
func (b *Backend) autoApprovePermission(msg *PermissionRequest) bool {
	if len(b.autoApproveRules) == 0 {
		return false
	}

	if !matchAutoApprove(b.autoApproveRules, msg.Request.ToolName, msg.Request.Input) {
		return false
	}

	summary := msg.Request.Summary()
	log.Infof("ccstream/perm", "auto-approved: tool=%s summary=%q req_id=%s", msg.Request.ToolName, summary, msg.RequestID)

	resp := &PermissionAllow{
		Behavior:               "allow",
		UpdatedInput:           json.RawMessage(`{}`),
		ToolUseID:              msg.Request.ToolUseID,
		DecisionClassification: "user_temporary",
	}
	if err := b.writer.SendControlResponse(msg.RequestID, resp); err != nil {
		log.Warnf("ccstream/perm", "auto-approve send failed: %v", err)
		return false
	}

	return true
}
