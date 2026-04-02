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
func matchAutoApprove(rules []autoApproveRule, toolName string, input json.RawMessage) bool {
	for _, r := range rules {
		if r.matches(toolName, input) {
			return true
		}
	}
	return false
}

// matches checks whether this rule matches the given tool invocation.
func (r autoApproveRule) matches(toolName string, input json.RawMessage) bool {
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

// extractMatchString extracts the string to match against from the tool input JSON.
// For Bash: the "command" field. For Edit/Write: the "file_path" field.
func extractMatchString(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}

	var key string
	switch toolName {
	case "Bash":
		key = "command"
	case "Edit", "Write":
		key = "file_path"
	default:
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
