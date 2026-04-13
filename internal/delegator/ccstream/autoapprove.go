package ccstream

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"foci/internal/log"
	"mvdan.cc/sh/v3/syntax"
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
	"Bash:sed",
	"Bash:find",
	// Shell test expressions — purely conditional, no side effects.
	"Bash:test",
	"Bash:[",
	"Bash:[[",
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

// CommonSafeWriteRules is the built-in list of commands that have side effects
// (network fetches or filesystem writes) but are considered low-risk in a
// workspace-scoped agent. Enabled via auto_approve_common_safe_write (default
// off). Kept distinct from CommonReadonlyRules so operators can opt into
// write/network access separately.
var CommonSafeWriteRules = []string{
	// Network fetches.
	"Bash:curl",
	"Bash:wget",
	// Filesystem scaffolding.
	"Bash:mkdir",
	"Bash:touch",
	// Safe deletion — moves to trash, recoverable.
	"Bash:trash",
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
// For Bash commands, the command is parsed into an AST and every structural
// element and simple command is validated. This prevents bypasses via shell
// features like redirects, process substitution, and command wrappers.
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

// ---------- AST-based Bash command validation ----------

// wrapperCommands are commands that execute their arguments as a subprocess.
// When these appear as the first word of a simple command with additional
// arguments, the command is rejected (prompted to user) because the wrapper
// could be used to execute any arbitrary command.
//
// Bare invocations (e.g. "env" alone to show environment) are not affected —
// only wrapper + arguments triggers rejection.
var wrapperCommands = map[string]bool{
	"env":     true,
	"nice":    true,
	"timeout": true,
	"nohup":   true,
	"flock":   true,
	"script":  true,
	"setsid":  true,
	"taskset": true,
	"ionice":  true,
	"strace":  true,
	"watch":   true,
}

// matchBashAutoApprove parses a Bash command into an AST and validates every
// command and structural element against the auto-approve rules.
//
// Structural safety checks (AST-level):
//   - Output redirects (>, >>, >|, &>, &>>) are rejected
//   - Process substitution <() is rejected
//   - Command substitution $() and backticks are rejected
//   - Brace expansion {a,b} is rejected
//   - Function declarations and coprocesses are rejected
//   - Command wrappers (env, nice, timeout, etc.) with arguments are rejected
//
// Command-level checks (reusing existing infrastructure):
//   - Each simple command must match at least one Bash auto-approve rule
//   - Commands with known unsafe flags (sed -i, find -exec, sort -o, etc.) are rejected
//   - sed script arguments are scanned for dangerous commands (w, e)
func matchBashAutoApprove(rules []autoApproveRule, input json.RawMessage) bool {
	command := extractMatchString("Bash", input)
	if command == "" {
		return false
	}

	// Parse command as bash.
	p := syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash))
	f, err := p.Parse(strings.NewReader(command), "")
	if err != nil {
		return false // unparseable → fail safe (prompt user)
	}

	// Walk the AST checking structural safety and collecting simple commands.
	//
	// Note: the parser treats brace expansion ({a,b}, {1..10}) as literal
	// text in Lit nodes. syntax.SplitBraces exists to convert them into
	// BraceExp nodes, but Walk panics on BraceExp (unsupported node type).
	// So we detect brace expansion by inspecting Lit values directly.
	var commands []*syntax.CallExpr
	hasContent := false
	safe := true

	syntax.Walk(f, func(node syntax.Node) bool {
		if !safe {
			return false
		}
		switch n := node.(type) {
		case *syntax.Redirect:
			if isOutputRedirect(n.Op) {
				safe = false
			}
		case *syntax.ProcSubst:
			safe = false
		case *syntax.CmdSubst:
			safe = false
		case *syntax.Lit:
			if litContainsBraceExpansion(n.Value) {
				safe = false
			}
		case *syntax.CallExpr:
			commands = append(commands, n)
			hasContent = true
		case *syntax.TestClause:
			hasContent = true // [[ ]] — safe, no side effects
		case *syntax.DeclClause:
			hasContent = true // export/local/declare — safe
		case *syntax.ArithmCmd:
			hasContent = true // (( )) — safe
		case *syntax.LetClause:
			hasContent = true // let — safe
		case *syntax.FuncDecl:
			safe = false // function declarations not allowed
		case *syntax.CoprocClause:
			safe = false // coprocesses not allowed
		default:
			_ = n // other node types — recurse normally
		}
		return safe
	})

	if !safe || !hasContent {
		return false
	}

	// Validate each simple command against rules.
	pr := syntax.NewPrinter()
	for _, cmd := range commands {
		cmdStr := callExprCmdString(pr, cmd)
		if cmdStr == "" {
			continue // pure assignment, no command — safe
		}

		// Reject command wrappers with arguments (e.g. "env rm file").
		// Bare wrapper invocations (e.g. "env" alone) are allowed.
		name := commandBaseName(cmd)
		if wrapperCommands[name] && len(cmd.Args) > 1 {
			return false
		}

		if !matchBashSegment(rules, cmdStr) {
			return false
		}
	}
	return true
}

// isOutputRedirect returns true if the redirect operator writes output to a
// file. Input redirects (<, <<, <<<) and FD duplication (>&N, <&N) are not
// considered output redirects.
func isOutputRedirect(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrOut, // >
		syntax.AppOut, // >>
		syntax.ClbOut, // >|
		syntax.RdrAll, // &>
		syntax.AppAll: // &>>
		return true
	}
	return false
}

// litContainsBraceExpansion checks whether an unquoted literal contains bash
// brace expansion syntax: {a,b,c} (alternatives) or {1..10} (sequences).
// The sh/syntax parser keeps these as Lit text; syntax.SplitBraces can
// convert them to BraceExp nodes, but Walk doesn't support BraceExp, so
// we detect the pattern in the literal value instead.
func litContainsBraceExpansion(s string) bool {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return false
	}
	end := strings.IndexByte(s[start:], '}')
	if end < 0 {
		return false
	}
	inner := s[start : start+end+1]
	return strings.Contains(inner, ",") || strings.Contains(inner, "..")
}

// callExprCmdString returns the command string from a CallExpr, excluding
// any variable assignments. Returns "" for pure assignments with no command.
func callExprCmdString(pr *syntax.Printer, ce *syntax.CallExpr) string {
	if len(ce.Args) == 0 {
		return ""
	}
	var buf strings.Builder
	for i, arg := range ce.Args {
		if i > 0 {
			buf.WriteByte(' ')
		}
		pr.Print(&buf, arg)
	}
	return buf.String()
}

// commandBaseName extracts the base name of the command from a CallExpr.
// For simple literal commands (like "env", "/usr/bin/env"), returns the base
// name ("env"). Returns "" if the command name is not a simple literal (e.g.
// quoted or expanded).
func commandBaseName(ce *syntax.CallExpr) string {
	if len(ce.Args) == 0 || len(ce.Args[0].Parts) == 0 {
		return ""
	}
	lit, ok := ce.Args[0].Parts[0].(*syntax.Lit)
	if !ok {
		return ""
	}
	return filepath.Base(lit.Value)
}

// ---------- Command segment validation ----------

// matchBashSegment checks whether a single command string matches at least one
// Bash rule. If the command contains flags or arguments that are known to be
// unsafe (e.g. sed -i, sort -o), the match is rejected regardless of which
// rule matched.
func matchBashSegment(rules []autoApproveRule, segment string) bool {
	if containsUnsafeFlags(segment) {
		return false
	}
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

// ---------- Unsafe flag and argument detection ----------

// unsafeCmdFlags describes flags and argument patterns that make an otherwise
// safe command unsafe for auto-approval.
type unsafeCmdFlags struct {
	shortFlags string            // unsafe single-letter flags, e.g. "i" for -i
	wordFlags  []string          // unsafe single-dash word flags, e.g. "-exec", "-delete"
	longFlags  []string          // long flag stems (matched as prefix for --flag=value)
	argCheck   func(string) bool // optional: check non-flag arguments for dangerous content
}

// unsafeFlags maps command base names to their unsafe flag/argument specs.
// Only commands listed here are checked — all other commands pass through.
var unsafeFlags = map[string]unsafeCmdFlags{
	"sed": {
		shortFlags: "i",
		longFlags:  []string{"--in-place"},
		argCheck:   sedArgUnsafe,
	},
	"find": {
		wordFlags: []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fls", "-fprintf"},
	},
	"sort": {
		shortFlags: "o",
		longFlags:  []string{"--output"},
	},
}

// containsUnsafeFlags checks whether a command string contains flags or
// arguments that make it unsafe for auto-approval. Returns true if any unsafe
// flag or dangerous argument content is detected.
//
// The check tokenises the command, looks up the command base name in
// unsafeFlags, and scans tokens for matching short flags (including bundled
// forms like -ni), word flags (single-dash multi-letter flags like -exec),
// long flags (including --flag=value forms), and dangerous argument content
// (via the optional argCheck function).
func containsUnsafeFlags(segment string) bool {
	tokens := tokenizeCommand(segment)
	if len(tokens) == 0 {
		return false
	}

	cmdBase := filepath.Base(tokens[0])
	spec, ok := unsafeFlags[cmdBase]
	if !ok {
		return false
	}

	for _, tok := range tokens[1:] {
		if len(tok) >= 2 && tok[0] == '-' {
			// Flag token.
			if strings.HasPrefix(tok, "--") {
				// Long flag: --in-place or --in-place=.bak
				for _, lf := range spec.longFlags {
					if tok == lf || strings.HasPrefix(tok, lf+"=") {
						return true
					}
				}
			} else {
				// Word flag: single-dash multi-letter flags matched exactly,
				// e.g. find's -exec, -delete.
				for _, wf := range spec.wordFlags {
					if tok == wf {
						return true
					}
				}
				// Short flag(s): -i, -i.bak, -ni, etc.
				// Everything after the leading '-' up to the first non-alpha
				// character is the flag bundle. For -i.bak the bundle is "i"
				// (the dot terminates it, rest is the suffix argument).
				if spec.shortFlags != "" {
					bundle := tok[1:]
					for j := 0; j < len(bundle); j++ {
						ch := bundle[j]
						if ch < 'A' || (ch > 'Z' && ch < 'a') || ch > 'z' {
							break // non-letter terminates the flag bundle
						}
						if strings.IndexByte(spec.shortFlags, ch) >= 0 {
							return true
						}
					}
				}
			}
		} else if spec.argCheck != nil {
			// Non-flag token — check with custom argument checker.
			if spec.argCheck(tok) {
				return true
			}
		}
	}
	return false
}

// ---------- sed script argument analysis ----------

// sedArgUnsafe checks if a sed script argument contains potentially dangerous
// sed commands or flags. Returns true if the argument contains:
//   - A 'w'/'W' command (write matched lines to file)
//   - An 'e'/'E' command (execute pattern space as shell command)
//   - A substitute command with 'e' flag: s/pattern/replacement/e
//   - A substitute command with 'w' flag: s/pattern/replacement/w file
func sedArgUnsafe(arg string) bool {
	// Strip outer quotes if present.
	if len(arg) >= 2 {
		if (arg[0] == '\'' && arg[len(arg)-1] == '\'') ||
			(arg[0] == '"' && arg[len(arg)-1] == '"') {
			arg = arg[1 : len(arg)-1]
		}
	}
	if arg == "" {
		return false
	}
	// Scan past optional sed address prefix (line numbers, /regex/, $, ranges).
	i := skipSedAddress(arg)
	if i >= len(arg) {
		return false
	}
	switch arg[i] {
	case 'w', 'W': // write command — writes matched lines to file
		return true
	case 'e', 'E': // execute command — runs pattern space as shell command
		return true
	case 's': // substitute — check flags after third delimiter for e/w
		return sedSubstHasUnsafeFlags(arg[i:])
	}
	return false
}

// sedSubstHasUnsafeFlags checks whether a sed substitute command
// (s/pattern/replacement/flags) contains the 'e' (execute) or 'w' (write)
// flag. The delimiter is the character immediately after 's' and can be any
// character (/, |, #, etc.).
func sedSubstHasUnsafeFlags(s string) bool {
	if len(s) < 2 {
		return false
	}
	delim := s[1]
	// Find the third delimiter (end of replacement) to reach the flags.
	count := 0
	i := 2
	for i < len(s) && count < 2 {
		if s[i] == '\\' && i+1 < len(s) {
			i += 2 // skip escaped char
			continue
		}
		if s[i] == delim {
			count++
		}
		i++
	}
	if count < 2 {
		return false // incomplete substitute — no flags section
	}
	// Everything from i onward is flags (and optional w filename).
	for j := i; j < len(s); j++ {
		switch s[j] {
		case 'e', 'E':
			return true
		case 'w', 'W':
			return true
		}
	}
	return false
}

// skipSedAddress skips a sed address prefix in a script string, returning
// the index of the first command character. Handles line numbers, $ (last
// line), /regex/ delimiters, \cregexc alternate delimiters, address ranges
// (,), and step (~).
func skipSedAddress(s string) int {
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch >= '0' && ch <= '9' || ch == ',' || ch == '~' || ch == '$' || ch == ' ' {
			i++
			continue
		}
		if ch == '/' {
			// Skip /regex/ address.
			end := strings.IndexByte(s[i+1:], '/')
			if end >= 0 {
				i = i + 1 + end + 1
				continue
			}
			break // unterminated regex — stop
		}
		if ch == '\\' && i+1 < len(s) {
			// Skip \cregexc alternate delimiter.
			delim := s[i+1]
			end := strings.IndexByte(s[i+2:], delim)
			if end >= 0 {
				i = i + 2 + end + 1
				continue
			}
			break // unterminated — stop
		}
		break
	}
	return i
}

// ---------- Command tokenization ----------

// tokenizeCommand splits a command string into whitespace-delimited tokens,
// respecting single and double quotes and backslash escapes.
func tokenizeCommand(cmd string) []string {
	var tokens []string
	var cur strings.Builder
	i := 0
	for i < len(cmd) {
		ch := cmd[i]

		// Skip whitespace between tokens.
		if ch == ' ' || ch == '\t' {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
			i++
			continue
		}

		// Quoted string — consume through matching quote.
		if ch == '\'' || ch == '"' {
			end := indexUnescapedQuote(cmd, i+1, ch)
			if end < 0 {
				// Unmatched quote — take rest of string.
				cur.WriteString(cmd[i:])
				i = len(cmd)
			} else {
				cur.WriteString(cmd[i : end+1])
				i = end + 1
			}
			continue
		}

		// Backslash escape.
		if ch == '\\' && i+1 < len(cmd) {
			cur.WriteByte(cmd[i+1])
			i += 2
			continue
		}

		cur.WriteByte(ch)
		i++
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
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

// ---------- Tool input extraction ----------

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

// ---------- Pattern matching ----------

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

// doGlob is the iterative glob matcher. It uses the standard two-pointer
// backtracking algorithm for O(n*m) worst case.
func doGlob(pattern, str string) bool {
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

// ---------- Permission handling ----------

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
