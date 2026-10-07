package autoapprove

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"foci/internal/execguard"
	"foci/internal/secrets"
	"mvdan.cc/sh/v3/syntax"
)

// Rule is a parsed permission auto-approve rule.
type Rule struct {
	ToolName string // e.g. "Bash", "Read", "Edit"
	Pattern  string // e.g. "git *", "/home/foci/**" — empty means match any input
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
	// Encoding/decoding — read stdin/file, write stdout, no side effects.
	"Bash:base64",
	// Environment and system inspection.
	"Bash:env",
	"Bash:crontab -l",
	"Bash:npm list",
	// System logs.
	"Bash:journalctl",
	// Go read-only subcommands.
	"Bash:go env",
	"Bash:go version",
	"Bash:go doc",
	"Bash:go help",
	"Bash:go list",
	"Bash:go vet",
	// Data tools.
	"Bash:jq",
	"Bash:yq",
	"Bash:mds",
	"Bash:mdq",
	"Bash:sqlite3 -readonly",
	"Bash:/usr/bin/sqlite3 -readonly",
}

// FociShellRulesFor returns auto-approve rules for foci shell functions
// (foci_todo, foci_send_to_chat, foci_remind, etc.) derived from the tools
// registry's ExportedNames. These are always auto-approved — they're foci's
// own wrappers around platform/storage primitives, executed in-process with
// constrained schemas, and have the same risk profile across the set.
//
// Source-of-truth is the registry: any tool registered with ExecExport:true
// gets a rule automatically, and removing one removes its rule. No hand-list
// to drift.
// Compile parses rule strings into compiled Rules.
func Compile(rules []string) []Rule { return parseAutoApproveRules(rules) }

// MatchWithEnv evaluates an auto-approval request using the exact environment
// inherited by the delegated backend. This is required for shell-interceptor
// variable expansion: the gateway process environment may differ from the
// backend's environment after per-agent overrides and BASH_ENV are applied.
// The second return is a human-readable reason present ONLY when the request
// was denied by the substitutability veto despite matching a rule (#1906).
// Empty otherwise, including for an ordinary no-rule-matched denial.
func MatchWithEnv(rules []Rule, toolName string, input json.RawMessage, env map[string]string) (bool, string) {
	return matchAutoApproveWithEnv(rules, toolName, input, env)
}

func FociShellRulesFor(execNames []string) []string {
	rules := make([]string, 0, len(execNames))
	for _, name := range execNames {
		rules = append(rules, "Bash:"+name)
	}
	return rules
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
	// Go development workflow.
	"Bash:go build",
	"Bash:go test",
	"Bash:go install",
	"Bash:go get",
	"Bash:go mod tidy",
	"Bash:go mod download",
	"Bash:go mod edit",
	"Bash:go clean",
	"Bash:go run",
}

// parseAutoApproveRule splits a rule string into tool name and optional pattern.
// Format: "ToolName" (match any input) or "ToolName:pattern" (match input).
func parseAutoApproveRule(rule string) Rule {
	if idx := strings.IndexByte(rule, ':'); idx >= 0 {
		return Rule{
			ToolName: rule[:idx],
			Pattern:  rule[idx+1:],
		}
	}
	return Rule{ToolName: rule}
}

// parseAutoApproveRules parses a slice of rule strings into compiled rules.
func parseAutoApproveRules(rules []string) []Rule {
	parsed := make([]Rule, len(rules))
	for i, r := range rules {
		parsed[i] = parseAutoApproveRule(r)
	}
	return parsed
}

func matchAutoApproveWithEnv(rules []Rule, toolName string, input json.RawMessage, env map[string]string) (bool, string) {
	if toolName == "Bash" {
		return matchBashAutoApprove(rules, input, env)
	}
	return matchToolAutoApprove(rules, toolName, input), ""
}

// EnvironmentFromList converts an exec.Cmd-style environment into the
// effective key/value snapshot used by shell expansion. Later entries for a
// key override earlier entries, matching the environment passed to a child.
func EnvironmentFromList(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}

// pathTypedTools are tools whose match key is a filesystem path. Their candidate
// path is canonicalized (absolute, symlink-resolved, ".." removed) before glob
// matching so a path-scoped rule such as Write:<workspace>/* cannot be bypassed
// by a traversal like <workspace>/../../../etc/cron.d/x — the trailing "*" would
// otherwise swallow the "../" segments (P1-6). Kept in sync with toolMatchKeys
// by TestPathTypedToolsMatchToolMatchKeys.
var pathTypedTools = map[string]bool{
	"Read":         true,
	"Edit":         true,
	"Write":        true,
	"NotebookEdit": true,
}

// matchToolAutoApprove handles non-Bash tools: any single rule match suffices.
func matchToolAutoApprove(rules []Rule, toolName string, input json.RawMessage) bool {
	matchStr := extractMatchString(toolName, input)
	// Canonicalize path-typed candidates once before matching. Comparing in the
	// same canonical space the workspace rule is built in (see
	// buildAutoApproveRules) closes the ".." traversal and symlink-escape
	// bypasses (P1-6) while preserving legitimate nested-path matches.
	if pathTypedTools[toolName] && filepath.IsAbs(matchStr) {
		matchStr = secrets.CanonicalPath(matchStr)
	}
	for _, r := range rules {
		if r.matchesToolStr(toolName, matchStr) {
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

// varSettingCommands are commands that can set shell variables opaquely —
// their effects are invisible to AST analysis. They are always rejected
// (even if a rule matches) because they could set a variable that later
// appears in a bash -c argument, creating an uninspectable code path.
var varSettingCommands = map[string]bool{
	"eval":      true, // executes a string as shell code
	"source":    true, // executes a script file
	".":         true, // POSIX alias for source
	"mapfile":   true, // reads lines into an array variable
	"readarray": true, // alias for mapfile
	"read":      true, // reads input into a variable
	"unset":     true, // removes a previously tracked variable
	"getopts":   true, // writes option state to named variables
}

// matchBashAutoApprove parses a Bash command into an AST and validates every
// command and structural element against the auto-approve rules.
//
// Structural safety checks (AST-level):
//   - Output redirects (>, >>, >|, &>, &>>) are rejected
//   - Process substitution <() is rejected
//   - Command substitution $() and backticks are recursively validated (and
//     an argument of a flag-checked command is rejected outright, #2216)
//   - Brace expansion {a,b} is rejected at the literal level outside
//     flag-checked commands; on flag-checked commands the scanner's brace
//     rule applies in every quoting shape, so braces bash would not expand
//     (double-quoted, separator-free) keep approval (#2216)
//   - Function declarations and coprocesses are rejected
//   - Command wrappers (env, nice, timeout, etc.) with arguments are rejected
//   - Shell interceptors (bash -c, sh -c, etc.) are unwrapped: the inner
//     script is extracted, statically resolved (literals, inline variable
//     assignments, and environment variables), and validated recursively
//     against the same rules
//
// Command-level checks (reusing existing infrastructure):
//   - Each simple command must match at least one Bash auto-approve rule
//   - Commands with known unsafe flags (sed -i, find -exec, sort -o, etc.) are rejected
//   - sed script arguments are scanned for dangerous commands (w, e)
//   - Commands that set variables opaquely (eval, source, read, etc.) are rejected
func matchBashAutoApprove(rules []Rule, input json.RawMessage, env map[string]string) (bool, string) {
	command := extractMatchString("Bash", input)
	if command == "" {
		return false, ""
	}

	stmts, ok := parseShellScript(command)
	if !ok {
		return false, "" // unparseable → fail safe (prompt user)
	}

	vc := newVarCtx(env)
	if validateParsedCommand(rules, stmts, 0, vc) {
		return true, ""
	}
	return false, vc.denialReason()
}

// parseShellScript parses a command string as bash and returns the top-level
// statements. Returns nil, false if the command is unparseable.
func parseShellScript(command string) ([]*syntax.Stmt, bool) {
	p := syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash))
	f, err := p.Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false
	}
	return f.Stmts, true
}

// shellInterceptors are shell interpreters that execute a script passed via -c.
// When such a command is encountered, the -c argument is extracted, statically
// resolved (literal strings only — no variable references or substitutions),
// and validated recursively against the same rules. This allows safe commands
// wrapped in e.g. "bash -c 'ls'" to be auto-approved, while dangerous inner
// commands (bash -c 'rm file') are still rejected.
//
// Non-literal -c arguments (variables, command substitutions, etc.) cannot be
// resolved statically and cause the command to be rejected (prompts user).
var shellInterceptors = map[string]bool{
	"bash": true,
	"sh":   true,
	"dash": true,
	"zsh":  true,
	"ksh":  true,
	"ash":  true,
}

// maxCmdSubstDepth limits recursive validation of nested command substitutions.
const maxCmdSubstDepth = 3

// validateParsedCommand walks a list of statements checking structural safety
// and validating each simple command against rules. depth tracks CmdSubst
// nesting to prevent infinite recursion. vc provides variable resolution
// context for shell interceptor (-c) argument resolution.
func validateParsedCommand(rules []Rule, stmts []*syntax.Stmt, depth int, vc *varCtx) bool {
	for _, stmt := range stmts {
		// A compound statement can change variables in ways that are not
		// statically modelled (notably loop iterator variables). Do not use
		// the surrounding symbol table for variables it mutates.
		stmtVC := vc.clone()
		markComplexAssignments(stmt, stmtVC)
		if !validateParsedStmt(rules, stmt, depth, stmtVC) {
			return false
		}
		updateVarCtx(stmt, vc)
	}
	return true
}

// validateParsedStmt validates one statement using the variable context that
// applies at that point in the script. Callers advance the context only after
// a complete top-level statement has been validated.
func validateParsedStmt(rules []Rule, stmt *syntax.Stmt, depth int, vc *varCtx) bool {
	// Walk the AST checking structural safety and collecting simple commands.
	// Brace-expansion literals are NOT checked here: since #2216 a
	// flag-checked command's words are judged by the scanner's brace rule,
	// which is exact about quoting, so the historical one-Lit-at-a-time
	// refusal moved to its own pass (stmtHasBraceLiteral) that exempts
	// those commands.
	var commands []*syntax.CallExpr
	var cmdSubsts []*syntax.CmdSubst
	hasContent := false
	safe := true

	syntax.Walk(stmt, func(node syntax.Node) bool {
		if !safe {
			return false
		}
		switch n := node.(type) {
		case *syntax.Redirect:
			if isOutputRedirect(n.Op) && !isDevNullRedirect(n) {
				safe = false
			}
		case *syntax.ProcSubst:
			safe = false
		case *syntax.CmdSubst:
			// Collect for recursive validation instead of rejecting.
			cmdSubsts = append(cmdSubsts, n)
			return false // don't descend — we'll validate separately
		case *syntax.CallExpr:
			// Inline command-prefix assignments (LD_PRELOAD=x cmd) and bare
			// assignments (LD_PRELOAD=x) are CallExpr.Assigns — not a
			// DeclClause — so they bypass declHasDangerousVar. Scan them
			// here (P1-7).
			for _, a := range n.Assigns {
				if a.Name != nil && isDangerousVarName(a.Name.Value) {
					safe = false
				}
			}
			// Shell interceptors (bash -c, sh -c, etc.): extract the
			// inner script, statically resolve it, and validate
			// recursively. The interceptor CallExpr itself is not added
			// to commands — its inner commands are validated instead.
			if scriptWord, ok := extractShellScript(n); ok {
				script, resolved := resolveStaticWord(scriptWord, vc)
				switch {
				case !resolved:
					safe = false // non-static constructs in -c arg
				case script == "":
					hasContent = true // empty script — harmless
				default:
					innerStmts, parsed := parseShellScript(script)
					if !parsed || !validateParsedCommand(rules, innerStmts, depth+1, vc.clone()) {
						safe = false
					} else {
						hasContent = true
					}
				}
				return false // handled; don't descend into children
			}
			// Reject commands that set variables opaquely (eval, source,
			// read, etc.) — their effects are invisible to the symbol
			// table and could create uninspectable code paths.
			name := commandBaseName(n)
			if varSettingCommands[name] {
				safe = false
				return false
			}
			commands = append(commands, n)
			hasContent = true
		case *syntax.TestClause:
			hasContent = true // [[ ]] — safe, no side effects
		case *syntax.DeclClause:
			if declHasDangerousVar(n) {
				safe = false
			}
			hasContent = true
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

	// Recursively validate command substitutions.
	if depth >= maxCmdSubstDepth {
		return false // too deeply nested — fail safe
	}
	for _, cs := range cmdSubsts {
		if !validateParsedCommand(rules, cs.Stmts, depth+1, vc.clone()) {
			return false
		}
	}

	// Brace-expansion literals outside flag-checked commands' words.
	pr := syntax.NewPrinter()
	if stmtHasBraceLiteral(pr, stmt) {
		return false
	}

	// Validate each simple command against rules.
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

		if !matchBashSegment(rules, cmdStr, cmd, vc) {
			return false
		}
	}
	return true
}

// isDevNullRedirect returns true if the redirect target is /dev/null.
// Redirecting to /dev/null discards output — it cannot exfiltrate data,
// so it is safe to allow even when other output redirects are rejected.
func isDevNullRedirect(r *syntax.Redirect) bool {
	if r.Word == nil {
		return false
	}
	// Word.Parts should be a single Lit with value "/dev/null".
	if len(r.Word.Parts) != 1 {
		return false
	}
	lit, ok := r.Word.Parts[0].(*syntax.Lit)
	return ok && lit.Value == "/dev/null"
}

// isOutputRedirect returns true if the redirect operator writes output to a
// file. Input redirects (<, <<, <<<) and FD duplication (>&N, <&N) are not
// considered output redirects.
func isOutputRedirect(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrOut, // >
		syntax.AppOut,  // >>
		syntax.RdrClob, // >|
		syntax.RdrAll,  // &>
		syntax.AppAll:  // &>>
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

// stmtHasBraceLiteral reports whether the statement contains a
// brace-expansion literal the historical one-Lit-at-a-time scan above
// refuses. Flag-checked commands are exempt (#2216): their words are
// printed, scanned and judged by the scanner's brace rule
// (containsUnsafeFlagWords → braceArgUnsafe), which tracks quoting across
// the whole word, so what the literal scan refused only by accident now
// follows bash — `rg foo {--pre=./x.sh,}` still refuses (the braces are
// live) while `rg "{a,b}" f` approves (double quotes suppress the
// expansion, yet the parser keeps the content as a Lit inside the
// DblQuoted node, indistinguishable from the live form one Lit at a time).
// The exemption covers the CallExpr's argument and assignment words — an
// argument word's braces are the scanner's to judge, and bash never
// brace-expands an assignment word. Everything else keeps the literal-level
// refusal: commands without unsafe-flag machinery (`cat {a,b}`), redirect
// words (they hang off the Stmt, not the CallExpr, and a brace group there
// is at worst an "ambiguous redirect" error), and non-command words such as
// a for-loop word list (`rg foo; for x in {a,b}; do true; done`).
// Shell-interceptor and command-substitution bodies are skipped: they are
// validated recursively, this pass included.
func stmtHasBraceLiteral(pr *syntax.Printer, stmt *syntax.Stmt) bool {
	found := false
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			if _, ok := extractShellScript(n); ok {
				return false // the -c script is validated recursively
			}
			return !callExprFlagChecked(pr, n) // the scanner owns a flag-checked command's words
		case *syntax.CmdSubst:
			return false // inner statements are validated recursively, this pass included
		case *syntax.Lit:
			if litContainsBraceExpansion(n.Value) {
				found = true
			}
		}
		return true
	})
	return found
}

// callExprFlagChecked reports whether the command's shell-visible name has
// unsafe-argument machinery. It composes the same printer→scanner→gate
// chain as containsUnsafeFlagWords, so the literal scan's exemption and the
// scanner's #2216 rules always agree on which commands they cover.
func callExprFlagChecked(pr *syntax.Printer, ce *syntax.CallExpr) bool {
	cmdStr := callExprCmdString(pr, ce)
	if cmdStr == "" {
		return false
	}
	return flagCheckedCommand(visibleCmdBase(scanShellWords(cmdStr)))
}

// dangerousVars lists environment variables whose modification can lead to
// arbitrary code execution or security bypass. Assignments to these via
// export/declare/readonly/typeset are rejected by the auto-approve walker.
var dangerousVars = map[string]bool{
	"PATH":            true, // redirects command lookups
	"LD_PRELOAD":      true, // injects shared library into every process
	"LD_LIBRARY_PATH": true, // redirects shared library resolution
	"PROMPT_COMMAND":  true, // executed by bash before every prompt
	"BASH_ENV":        true, // executed on non-interactive bash startup
	"ENV":             true, // executed on sh/dash startup
	"HISTFILE":        true, // controls command history location
}

// isDangerousVarName reports whether assigning to the named variable can lead to
// arbitrary code execution or a security bypass. Shared by the export/declare
// path (declHasDangerousVar) and the inline command-prefix / bare-assignment
// path (CallExpr.Assigns). Exported-function names (BASH_FUNC_*, ShellShock
// style) are matched by prefix.
func isDangerousVarName(name string) bool {
	return dangerousVars[name] || strings.HasPrefix(name, "BASH_FUNC_")
}

// declHasDangerousVar checks whether a DeclClause (export, declare, etc.)
// assigns to any security-sensitive variable. Also rejects nameref
// declarations since they can alias dangerous variables indirectly.
func declHasDangerousVar(d *syntax.DeclClause) bool {
	// nameref can alias any variable — always require approval.
	if d.Variant != nil && d.Variant.Value == "nameref" {
		return true
	}
	// declare -n / typeset -n also creates namerefs.
	for _, arg := range d.Args {
		if arg.Naked && arg.Name == nil && arg.Value != nil {
			// Naked args are flags (e.g. -n, -gn). Check for nameref flag.
			var buf strings.Builder
			// Print errors only on broken Writers; strings.Builder never fails.
			_ = syntax.NewPrinter().Print(&buf, arg.Value)
			flag := buf.String()
			if strings.HasPrefix(flag, "-") && strings.ContainsRune(flag, 'n') {
				return true
			}
		}
	}
	for _, arg := range d.Args {
		if arg.Name != nil && isDangerousVarName(arg.Name.Value) {
			return true
		}
	}
	return false
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
		// Print errors only on broken Writers; strings.Builder never fails.
		_ = pr.Print(&buf, arg)
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

// ---------- Shell interceptor unwrapping ----------

// extractShellScript checks whether ce is a shell interceptor (bash, sh, etc.)
// invoked in the strict form "shell -c SCRIPT [ARG...]". Returns the script
// word and true if so. Options before -c are rejected: flags such as bash -i
// and --rcfile can execute startup files before the inspected script runs.
func extractShellScript(ce *syntax.CallExpr) (*syntax.Word, bool) {
	name := commandBaseName(ce)
	if !shellInterceptors[name] {
		return nil, false
	}
	if len(ce.Args) >= 3 && isLitEqual(ce.Args[1], "-c") {
		return ce.Args[2], true
	}
	return nil, false
}

// isLitEqual reports whether w is a single literal word equal to val.
func isLitEqual(w *syntax.Word, val string) bool {
	if len(w.Parts) != 1 {
		return false
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	return ok && lit.Value == val
}

// resolveStaticWord attempts to statically resolve a shell Word to its string
// value. Literal parts (unquoted, single-quoted, double-quoted literals) are
// always resolvable. Variable references ($VAR) are resolved via the varCtx
// when vc is non-nil — from prior inline assignments in the current shell
// scope. When vc is nil, only literals resolve.
//
// Anything that cannot be statically resolved — command substitutions $(),
// arithmetic $(()), process substitution <(), special parameters ($?, $@),
// complex expansions (${VAR:-default}) — returns ("", false). The caller must
// reject (fail closed) in that case.
func resolveStaticWord(w *syntax.Word, vc *varCtx) (string, bool) {
	var sb strings.Builder
	for _, part := range w.Parts {
		s, ok := resolveWordPart(part, vc)
		if !ok {
			return "", false
		}
		sb.WriteString(s)
	}
	return sb.String(), true
}

// resolveWordPart resolves a single word part to its string value.
func resolveWordPart(part syntax.WordPart, vc *varCtx) (string, bool) {
	switch p := part.(type) {
	case *syntax.Lit:
		return p.Value, true
	case *syntax.SglQuoted:
		if p.Dollar {
			// ANSI-C quoting ($'…'): the value is the escape-decoded text.
			// Undecodable content fails closed like every other non-static
			// construct (#2216).
			return decodeAnsiC(p.Value)
		}
		return p.Value, true
	case *syntax.DblQuoted:
		if len(p.Parts) == 0 {
			return "", true // empty double-quoted string
		}
		var sb strings.Builder
		for _, inner := range p.Parts {
			s, ok := resolveWordPart(inner, vc)
			if !ok {
				return "", false
			}
			sb.WriteString(s)
		}
		return sb.String(), true
	case *syntax.ParamExp:
		if vc == nil {
			return "", false // no resolution context during pre-scan
		}
		return vc.resolveParamExp(p)
	default:
		// CmdSubst, ProcSubst, ArithmExp, ExtGlob, etc.
		return "", false
	}
}

// ---------- Variable resolution context ----------

// varCtx holds statically known shell variables at one precise point in a
// script. Variables not assigned in the command fall through to the process
// environment (os.LookupEnv), which the gateway and delegated shell share.
// vetoNote records the FIRST substitutability veto of an evaluation, so the
// caller can explain a denial instead of returning a bare false. It is held
// behind a pointer and shared by clone() ON PURPOSE: clones are made for
// subshells and compound statements, and a veto raised inside one of those is
// exactly as much a reason for the denial as one raised at the top level.
// Copying it by value would silently drop those.
type vetoNote struct {
	segment string
	path    string
	reason  string
}

type varCtx struct {
	symbolTable map[string]string // known literal assignments: name → value
	unknown     map[string]bool   // assignments whose value/scope is not modelled
	environment map[string]string // exact environment inherited by the shell
	veto        *vetoNote         // shared across clones; see vetoNote
}

func newVarCtx(environment map[string]string) *varCtx {
	return &varCtx{symbolTable: make(map[string]string), unknown: make(map[string]bool), environment: environment, veto: &vetoNote{}}
}

// noteVeto records the first veto only. A command with several offending
// segments denies on the first one the walker reaches, and reporting that one
// is both stable and the one to fix first.
func (vc *varCtx) noteVeto(segment, path, reason string) {
	if vc.veto != nil && vc.veto.reason == "" {
		vc.veto.segment, vc.veto.path, vc.veto.reason = segment, path, reason
	}
}

// DenialReason renders the recorded veto for a human, or "" if none was raised.
func (vc *varCtx) denialReason() string {
	if vc.veto == nil || vc.veto.reason == "" {
		return ""
	}
	return fmt.Sprintf("not auto-approved despite matching a rule: %s — %s. Approving here runs it anyway.",
		vc.veto.path, vc.veto.reason)
}

func (vc *varCtx) clone() *varCtx {
	copy := newVarCtx(vc.environment)
	copy.veto = vc.veto // SHARED, not copied — see vetoNote
	for name, value := range vc.symbolTable {
		copy.symbolTable[name] = value
	}
	for name := range vc.unknown {
		copy.unknown[name] = true
	}
	return copy
}

// markComplexAssignments marks variables that may be mutated inside a
// compound statement. The walker does not represent a for iterator as a
// CallExpr assignment, so it is explicitly included here.
func markComplexAssignments(stmt *syntax.Stmt, vc *varCtx) {
	switch stmt.Cmd.(type) {
	case *syntax.CallExpr, *syntax.DeclClause:
		return // direct statements are handled by updateVarCtx after validation
	}
	syntax.Walk(stmt, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			for _, a := range n.Assigns {
				if a.Name != nil {
					vc.unknown[a.Name.Value] = true
				}
			}
		case *syntax.DeclClause:
			for _, arg := range n.Args {
				if arg.Name != nil {
					vc.unknown[arg.Name.Value] = true
				}
			}
		case *syntax.ForClause:
			if loop, ok := n.Loop.(*syntax.WordIter); ok && loop.Name != nil {
				vc.unknown[loop.Name.Value] = true
			}
		}
		return true
	})
}

// updateVarCtx advances the symbol table after a direct top-level assignment.
// Command-prefix assignments (X=value command) do not persist in the parent
// shell and are intentionally not recorded.
func updateVarCtx(stmt *syntax.Stmt, vc *varCtx) {
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if len(cmd.Args) != 0 {
			return
		}
		for _, a := range cmd.Assigns {
			updateAssign(a, vc)
		}
	case *syntax.DeclClause:
		for _, arg := range cmd.Args {
			if arg.Name == nil {
				continue
			}
			name := arg.Name.Value
			if arg.Value == nil {
				vc.unknown[name] = true
				delete(vc.symbolTable, name)
				continue
			}
			if value, ok := resolveStaticWord(arg.Value, vc); ok {
				vc.symbolTable[name] = value
				delete(vc.unknown, name)
			} else {
				vc.unknown[name] = true
				delete(vc.symbolTable, name)
			}
		}
	}
}

func updateAssign(assign *syntax.Assign, vc *varCtx) {
	if assign.Name == nil {
		return
	}
	name := assign.Name.Value
	if assign.Value == nil || assign.Index != nil {
		vc.unknown[name] = true
		delete(vc.symbolTable, name)
		return
	}
	if value, ok := resolveStaticWord(assign.Value, vc); ok {
		vc.symbolTable[name] = value
		delete(vc.unknown, name)
	} else {
		vc.unknown[name] = true
		delete(vc.symbolTable, name)
	}
}

// resolveParamExp resolves a simple variable reference ($VAR) using the
// pre-scanned context. Returns ("", false) for anything that cannot be
// statically determined.
func (vc *varCtx) resolveParamExp(p *syntax.ParamExp) (string, bool) {
	if p.Param == nil {
		return "", false
	}
	name := p.Param.Value
	// Reject special and positional parameters — their values are
	// runtime-determined, not in the environment.
	if isSpecialParam(name) {
		return "", false
	}
	// Only simple variable reference ($name or ${name}) — no index, slice,
	// replacement, expansion, indirect, length, or other complex forms.
	if p.Excl || p.Length || p.Width || p.IsSet ||
		p.NestedParam != nil || p.Index != nil ||
		len(p.Modifiers) > 0 || p.Slice != nil ||
		p.Repl != nil || p.Names != 0 || p.Exp != nil {
		return "", false
	}
	if vc.unknown[name] {
		return "", false
	}
	// A literal assignment that has already executed in this shell scope.
	if val, ok := vc.symbolTable[name]; ok {
		return val, true
	}
	// Not assigned anywhere in the command — fall through to the exact
	// environment snapshot supplied by the backend.
	val, ok := vc.environment[name]
	return val, ok
}

// isSpecialParam reports whether name is a shell special parameter whose
// value is determined at runtime, not from the environment.
func isSpecialParam(name string) bool {
	switch name {
	case "?", "$", "!", "#", "@", "*", "-":
		return true
	}
	// Positional parameters $0–$9.
	if len(name) == 1 && name[0] >= '0' && name[0] <= '9' {
		return true
	}
	return false
}

// ---------- Expansion resolution (#2216) ----------

// wordHasParamExp reports whether a word references a variable: a top-level
// ParamExp, or one inside double quotes. Words without one were already
// checked as literal text; resolution only engages for these.
func wordHasParamExp(w *syntax.Word) bool {
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.ParamExp:
			return true
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if _, ok := inner.(*syntax.ParamExp); ok {
					return true
				}
			}
		}
	}
	return false
}

// resolveArgWord resolves one argument word of a flag-checked command into
// the effective argument(s) the shell would pass: variable references are
// resolved through the existing resolver (varCtx.resolveParamExp via
// resolveWordPart — the same symbol table and environment snapshot, and the
// same fail-closed rules for substitutions, special parameters and complex
// expansions, that the shell-interceptor path uses), and the results are fed
// to the shared quoting model (wordFacts) with their true liveness:
//
//   - unquoted values are LIVE: their glob and brace characters are active
//     (`X='*'; rg foo $X` globs) and their whitespace word-splits
//     (`X='-i x'; sed $X …` becomes two arguments);
//   - quoted values ("$X", '$X', $'…', $"…") are inert text, checked as
//     quoted.
//
// It returns false when any part cannot be resolved exactly — an
// unresolvable variable, a substitution (blocked earlier by substArgUnsafe,
// kept here as a fail-closed backstop), an undecodable ANSI-C string — so
// the caller prompts instead of checking approximate text.
func resolveArgWord(w *syntax.Word, vc *varCtx) ([]shellWord, bool) {
	var f wordFacts
	anyQuoted := false
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			f.addLive(p.Value)
		case *syntax.ParamExp:
			val, ok := vc.resolveParamExp(p)
			if !ok {
				return nil, false
			}
			f.addLive(val)
		case *syntax.SglQuoted:
			anyQuoted = true
			if p.Dollar {
				decoded, ok := decodeAnsiC(p.Value)
				if !ok {
					return nil, false
				}
				f.addQuoted(decoded)
			} else {
				f.addQuoted(p.Value)
			}
		case *syntax.DblQuoted:
			anyQuoted = true
			s, ok := resolveWordPart(p, vc)
			if !ok {
				return nil, false
			}
			f.addQuoted(s)
		default:
			return nil, false // CmdSubst, ArithmExp, … — never approve blindly
		}
	}
	words := f.take()
	if len(words) == 0 && anyQuoted {
		// A quoted word keeps its argument slot even when it resolves empty.
		words = append(words, shellWord{})
	}
	return words, true
}

// expandedArgsUnsafe resolves variable references in the arguments of a
// flag-checked command and re-checks the effective argument vector exactly
// as if it had been typed (#2216) — through the same
// containsUnsafeFlagWords, so the flag tables, the #2213 glob rule and the
// brace rule all apply to resolved values. The command word itself is not
// an argument and is never resolved. Commands without unsafe-flag machinery
// keep their previous behaviour (variables pass through unresolved), and a
// variable that cannot be resolved fails closed: the printed segment keeps
// `$X` unexpanded, so approving it would mean approving an unknown value.
func expandedArgsUnsafe(ce *syntax.CallExpr, segment string, vc *varCtx) bool {
	anyParam := false
	for _, w := range ce.Args[1:] {
		if wordHasParamExp(w) {
			anyParam = true
			break
		}
	}
	if !anyParam {
		return false // literal words — already checked by containsUnsafeFlags
	}
	words := scanShellWords(segment)
	if len(words) == 0 || !flagCheckedCommand(visibleCmdBase(words)) {
		return false
	}
	// The scanner's words must correspond one-to-one with the AST's argument
	// words for resolution to be meaningful; they do for anything the printer
	// produces. Any divergence fails closed.
	if len(ce.Args) != len(words) {
		return true
	}
	effective := []shellWord{words[0]} // the command word is not an argument
	for _, w := range ce.Args[1:] {
		resolved, ok := resolveArgWord(w, vc)
		if !ok {
			return true
		}
		effective = append(effective, resolved...)
	}
	return containsUnsafeFlagWords(effective)
}

// ---------- Command segment validation ----------

// guardEnv supplies the filesystem/PATH view for the substitutability check
// below. It is package-level rather than a parameter because it would otherwise
// have to thread through three backends' call sites, and tests swap it.
//
// IT IS A FUNCTION, AND THAT IS THE WHOLE POINT (#1900). It used to be
// `var guardEnv = execguard.Live()` — a value — whose comment claimed PATH did
// not need re-reading because CanWrite/IsExecutable hit the filesystem on every
// check. That premise was wrong twice over. Go evaluates package-level
// initialisers before main(), and cmd/foci-gw/main.go calls shellenv.Apply()
// from inside main(), so the value captured the PATH the daemon was EXECd with
// and never the one shellenv installed from the operator's dotfiles — which is
// the PATH every tool shell actually uses. Re-reading PATH is exactly what
// matters: which directories are searched decides WHICH FILE gets judged, and a
// root-owned /usr/bin/git passing tells you nothing when an agent-writable
// ~/scripts/git is what runs. Calling Live() per check means there is one PATH,
// whatever shellenv last installed, with no list to keep in sync.
var guardEnv = execguard.Live

// commandIsSubstitutable reports whether the foci process could swap out the
// executable this segment will run. Vetoing here — at match time — is what
// makes the check a control rather than a report: a startup verdict is stale
// the moment a file changes, and foci runs continuously long after startup.
//
// The veto is deliberately independent of WHICH rule matched, including the
// built-in read-only group. The question "can this binary be swapped" does not
// depend on the provenance of the rule that allowed it.
// The path and reason are returned so the caller can SAY why it declined. A
// veto is otherwise invisible: the command simply falls through to an ordinary
// approval prompt, and the user is asked about a command that is on the
// allowlist with nothing anywhere explaining the discrepancy (#1906).
func commandIsSubstitutable(segment string) (bool, string, string) {
	tokens := tokenizeCommand(segment)
	if len(tokens) == 0 {
		return false, "", ""
	}
	return execguard.Substitutable(tokens[0], guardEnv())
}

// matchBashSegment checks whether a single command string matches at least one
// Bash rule. The match is rejected regardless of which rule matched when the
// segment contains flags or arguments known to make an otherwise safe command
// unsafe — compared on shell-visible text, so quoting cannot hide them —
// when any argument is an unquoted glob whose expansion could begin with '-'
// (#2213), when an argument of a flag-checked command carries a live brace
// group or substitution or a variable that resolves to unsafe or unresolvable
// text (#2216), or when the segment's executable could be substituted by this
// process. ce is the CallExpr the segment was printed from, for the variable
// resolution.
func matchBashSegment(rules []Rule, segment string, ce *syntax.CallExpr, vc *varCtx) bool {
	if containsUnsafeFlags(segment) {
		return false
	}
	if expandedArgsUnsafe(ce, segment, vc) {
		return false
	}
	if sub, path, reason := commandIsSubstitutable(segment); sub {
		vc.noteVeto(segment, path, reason)
		return false
	}
	for _, r := range rules {
		if r.ToolName != "Bash" {
			continue
		}
		if r.Pattern == "" {
			return true // tool-name-only Bash rule: matches any command
		}
		if matchPattern(r.Pattern, segment) {
			return true
		}
	}
	return false
}

// matchesToolStr checks whether this rule matches the given non-Bash tool
// invocation, against a match string already extracted (and canonicalized, for
// path-typed tools) by matchToolAutoApprove.
func (r Rule) matchesToolStr(toolName, matchStr string) bool {
	if r.ToolName != toolName {
		return false
	}
	if r.Pattern == "" {
		return true // tool-name-only rule: match any invocation
	}
	if matchStr == "" {
		return false
	}
	return matchPattern(r.Pattern, matchStr)
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
// The set of keys (plus sqlite3, which has its own whole-vector check in
// containsUnsafeFlagWords) also defines which commands the #2213 glob rule
// and the #2216 brace, substitution and variable rules apply to; see
// flagCheckedCommand.
var unsafeFlags = map[string]unsafeCmdFlags{
	"sed": {
		shortFlags: "if",
		longFlags:  []string{"--in-place", "--file"},
		argCheck:   sedArgUnsafe,
	},
	"find": {
		wordFlags: []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fls", "-fprintf"},
	},
	"sort": {
		shortFlags: "o",
		longFlags:  []string{"--output", "--compress-program"},
	},
	"rg": {
		longFlags: []string{"--pre"},
	},
	"go": {
		wordFlags: []string{"-vettool"},
	},
	"yq": {
		shortFlags: "i",
		longFlags:  []string{"--inplace"},
	},
	// git -c sets an arbitrary config value for one command (core.sshCommand,
	// core.pager, alias.*) — a direct arbitrary-exec vector; --config-env is the
	// env-backed form; the `config` subcommand writes those same values. -C
	// (run-in-dir) is deliberately NOT flagged: it is not itself an exec vector
	// and is a very common safe pattern. (P2-8.)
	"git": {
		shortFlags: "c",
		longFlags:  []string{"--config-env"},
		argCheck:   gitArgUnsafe,
	},
	// printf -v writes formatted output to a shell variable, enabling
	// opaque variable assignment that bypasses symbol-table tracking.
	"printf": {
		shortFlags: "v",
	},
}

// gitArgUnsafe flags the git `config` subcommand, which can persist arbitrary
// exec hooks (core.sshCommand / core.pager) and shell aliases (alias.x = !cmd).
func gitArgUnsafe(arg string) bool {
	return arg == "config"
}

// flagCheckedCommand reports whether a command has unsafe-argument machinery
// at all: an entry in unsafeFlags, or sqlite3's own whole-vector check. This
// set is also the gate for the #2213 glob rule and the #2216 brace,
// substitution and variable rules, so a command added to unsafeFlags
// automatically joins them — and a command without machinery keeps its
// previous behaviour.
func flagCheckedCommand(cmdBase string) bool {
	_, ok := unsafeFlags[cmdBase]
	return ok || cmdBase == "sqlite3"
}

// globArgUnsafe reports whether any argument word of the segment can expand
// into a flag (#2213) — see shellWord.canYieldFlag for the word shapes. The
// command word itself is not an argument and is skipped.
func globArgUnsafe(words []shellWord) bool {
	for _, w := range words[1:] {
		if w.canYieldFlag() {
			return true
		}
	}
	return false
}

// braceArgUnsafe reports whether any argument word carries a brace group the
// shell would expand (#2216) — see shellWord.hasLiveBrace for the exact
// shape. An expanded group can assemble a dangerous flag out of parts that
// are each individually harmless (`rg foo {"--pre=./x.sh",}` runs
// `rg foo --pre=./x.sh`), and quoting parts of the group does not suppress
// the expansion, so liveness is tracked across the quotes. The command word
// itself is not an argument and is skipped.
//
// Known follow-up: commands WITHOUT unsafe-flag machinery are not covered
// here (`cat {"a",b}` still approves) — their brace handling remains the
// AST-level litContainsBraceExpansion, which only sees one literal at a time.
func braceArgUnsafe(words []shellWord) bool {
	for _, w := range words[1:] {
		if w.hasLiveBrace {
			return true
		}
	}
	return false
}

// substArgUnsafe reports whether any argument word carries a live
// substitution (#2216) — $(…), $((…)) or a backtick, none of them single-
// quoted or backslash-escaped. The walker recursively approves a
// substitution whose inner commands are approved, but the OUTER command
// receives the substituted TEXT as an argument: `rg foo $(echo --pre=./x.sh)`
// feeds rg a flag the tables never saw. So for flag-checked commands the
// argument prompts instead. The command word itself is not an argument and
// is skipped.
func substArgUnsafe(words []shellWord) bool {
	for _, w := range words[1:] {
		if w.hasLiveSubst {
			return true
		}
	}
	return false
}

// visibleCmdBase returns the base name of a scanned command's first word in
// shell-visible text — the name every piece of flag machinery gates on.
func visibleCmdBase(words []shellWord) string {
	if len(words) == 0 {
		return ""
	}
	return filepath.Base(words[0].visible)
}

// containsUnsafeFlags checks whether a command string contains flags or
// arguments that make it unsafe for auto-approval: it scans the segment into
// shell words and applies containsUnsafeFlagWords — see that function for
// the rules and the shell-visible-text principle. This segment-level entry
// is the form matchBashSegment reaches (the variable resolver then re-scans
// the same segment with the same scanner, so the two passes cannot disagree).
func containsUnsafeFlags(segment string) bool {
	return containsUnsafeFlagWords(scanShellWords(segment))
}

// containsUnsafeFlagWords checks whether a command's scanned words contain
// flags or arguments that make it unsafe for auto-approval. It is the single
// flag-checking function: containsUnsafeFlags feeds it the literal segment's
// scanned words, and the #2216 variable resolver feeds it the resolved
// argument vector, so literal and expanded arguments are judged by one code
// path.
// Returns true if any of:
//
//   - a flag matching the command's unsafeFlags entry — short flags
//     (including bundled forms like -ni), word flags (single-dash
//     multi-letter flags like -exec), long flags (including --flag=value
//     forms) — or dangerous argument content (via the optional argCheck);
//   - an unquoted, unescaped glob argument whose expansion could begin with
//     '-' (#2213): the shell expands globs before the command runs, so a
//     file name planted in the working directory (--pre=./x.sh in a cloned
//     repo) would arrive as a flag;
//   - a brace group the shell would expand (#2216), which can assemble a
//     flag out of quoted parts — see braceArgUnsafe;
//   - a live substitution argument (#2216) — see substArgUnsafe;
//   - a command-specific whole-vector finding: sqlite3 dot-commands
//     (sqliteCommandUnsafe) or a git grep pager flag (gitGrepPagerUnsafe).
//
// Every check compares the words' shell-visible text — quoting a flag
// (`sed '-i'`) or assembling it from quoted parts does not change what the
// program receives, so it must not change the verdict (#2216). The command
// name is read from the visible text too, so `"sed" -i …` is flag-checked.
// Only flag-checked commands (see flagCheckedCommand) are covered by the
// glob, brace, substitution and flag-table rules; quoting or escaping a glob
// keeps approval, and there is deliberately no `--` exemption — some
// commands (find) do not honour it for every later argument.
func containsUnsafeFlagWords(words []shellWord) bool {
	if len(words) == 0 {
		return false
	}

	cmdBase := visibleCmdBase(words)
	if cmdBase == "sqlite3" && sqliteCommandUnsafe(visibleTexts(words)) {
		return true
	}
	if cmdBase == "git" && gitGrepPagerUnsafe(words) {
		return true
	}
	if !flagCheckedCommand(cmdBase) {
		return false
	}
	if globArgUnsafe(words) || braceArgUnsafe(words) || substArgUnsafe(words) {
		return true
	}
	spec, ok := unsafeFlags[cmdBase]
	if !ok {
		return false // sqlite3 — its whole argument vector is checked above
	}

	for _, w := range words[1:] {
		tok := w.visible
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
					if tok == wf || strings.HasPrefix(tok, wf+"=") {
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

// sqliteCommandUnsafe rejects SQLite CLI forms that can execute shell commands
// or read commands from standard input. The readonly database flag does not
// constrain dot-commands such as .system, .shell, .load, or .output.
//
// Auto-approved SQLite calls must include both a database argument and an
// explicit SQL argument. Any dot-command or additional CLI option is prompted
// instead of being interpreted as safe SQL. The tokens are the words'
// shell-visible text (#2216): quoting (`'.'shell`) does not change what
// sqlite3 receives.
func sqliteCommandUnsafe(tokens []string) bool {
	// The built-in rules require "sqlite3 -readonly". Require a database and
	// SQL argument as well, so stdin cannot supply a dot-command.
	if len(tokens) < 4 || tokens[1] != "-readonly" {
		return true
	}
	for _, token := range tokens[2:] {
		arg := strings.TrimSpace(token)
		if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, ".") {
			return true
		}
		// The CLI recognises dot-commands at the beginning of an input line;
		// reject one embedded after an SQL statement as well.
		for _, line := range strings.Split(arg, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), ".") {
				return true
			}
		}
	}
	return false
}

// gitGrepPagerUnsafe reports whether words run git's grep subcommand with a
// pager flag (-O, -O<pager>, --open-files-in-pager[=<cmd>]): the pager is
// executed over the matched files and may be an arbitrary command (#2213).
//
// Both facts are tested on the words' shell-VISIBLE text, because quoting
// does not change what git receives: git grep '-Oevil.sh', "-O"evil and
// \-Oevil.sh all deliver a pager flag. Detection is presence-based — a word
// resolving to `grep` together with a pager-flag word, wherever they sit —
// so global options before the subcommand (git -C dir grep -O,
// git --attr-source X grep -O) or a quoted subcommand (git "grep" -O) cannot
// move either past the check. git diff -O<order> and git log --grep=<pat>
// carry no `grep` word and stay approved. Deliberate over-approximation:
// `git diff -Oorder grep` (a path named grep) and a `--`-separated pattern
// that merely looks like -O are prompted, not approved — same fail-safe
// direction as the glob rule.
func gitGrepPagerUnsafe(words []shellWord) bool {
	isGrep, hasPager := false, false
	for _, w := range words {
		switch {
		case w.visible == "grep":
			isGrep = true
		case strings.HasPrefix(w.visible, "-O"), // -O and -O<pager>
			w.visible == "--open-files-in-pager",
			strings.HasPrefix(w.visible, "--open-files-in-pager="):
			hasPager = true
		}
	}
	return isGrep && hasPager
}

// ---------- sed script argument analysis ----------

// sedArgUnsafe checks if a sed script argument contains potentially dangerous
// sed commands or flags. The argument is the word's shell-visible text
// (#2216) — quoting part of a script (`sed 's/a/b/w'out`) does not change
// what sed receives — and shell-visible text carries no quote delimiters, so
// the whole-token strip below is a no-op there; it stays for text that still
// carries them. Returns true if the argument contains:
//   - A 'w'/'W' command (write matched lines to file)
//   - An 'e'/'E' command (execute pattern space as shell command)
//   - A substitute command with 'e' flag: s/pattern/replacement/e
//   - A substitute command with 'w' flag: s/pattern/replacement/w file
func sedArgUnsafe(arg string) bool {
	arg = stripOuterQuotes(arg)
	for _, command := range splitSedCommands(arg) {
		if sedCommandUnsafe(command) {
			return true
		}
	}
	return false
}

// stripOuterQuotes removes a matching pair of surrounding single or double
// quotes from a token, when they wrap the WHOLE token. Quoting that wraps
// only part of a word is resolved by the scanner before arguments arrive
// here, so this only normalises whole-token-quoted text.
func stripOuterQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"')) {
		return s[1 : len(s)-1]
	}
	return s
}

// splitSedCommands separates commands joined by an unescaped semicolon or
// newline. Each resulting command is inspected independently so a later `e`
// command cannot hide behind an earlier harmless one.
func splitSedCommands(program string) []string {
	commands := make([]string, 0, 1)
	start := 0
	escaped := false
	for i := 0; i < len(program); i++ {
		if escaped {
			escaped = false
			continue
		}
		if program[i] == '\\' {
			escaped = true
			continue
		}
		if program[i] == ';' || program[i] == '\n' {
			commands = append(commands, program[start:i])
			start = i + 1
		}
	}
	commands = append(commands, program[start:])
	return commands
}

// sedCommandUnsafe checks one simple sed command for write or execution
// behaviour. Callers split compound programs first.
func sedCommandUnsafe(arg string) bool {
	arg = strings.TrimSpace(arg)
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

// shellWord is one shell word of a command string, recording the quoting
// facts the #2213/#2216 argument controls need on top of the token text
// tokenizeCommand has always produced.
type shellWord struct {
	text          string // token text (quotes kept, backslash escapes resolved) — tokenizeCommand's exact output
	visible       string // shell-visible text (quote delimiters dropped, $'…' decoded) — what the program receives
	firstLiveGlob bool   // first visible character is an unquoted, unescaped * ? [
	hasLiveGlob   bool   // the word contains an unquoted, unescaped * ? [
	hasLiveBrace  bool   // the word contains a brace group the shell would expand (#2216)
	hasLiveSubst  bool   // the word contains a live $(, $(( or ` substitution (#2216)
}

// canYieldFlag reports whether glob expansion of the word can produce a word
// beginning with '-' (#2213): the word starts with a live glob character (a
// matched file name becomes the whole argument), or it visibly starts with
// '-' and contains a live glob (matched file names extend the flag-shaped
// prefix). Quoted or backslash-escaped metacharacters are literal to the
// shell and never live, whatever the visible text looks like.
func (w shellWord) canYieldFlag() bool {
	return w.firstLiveGlob || (len(w.visible) > 0 && w.visible[0] == '-' && w.hasLiveGlob)
}

// wordFacts accumulates the shell-visible text and quoting facts of one shell
// word while it is being built. It is the single quoting model behind both
// producers (#2216): scanShellWords feeds it the characters of a printed
// segment, and the flag-checked argument resolver feeds it resolved variable
// values — so "what would the shell do to this text" is computed once, in one
// place.
//
// Live means quote- and escape-transparent: a live character is one the shell
// itself sees and acts on (glob metacharacters, brace expansion syntax,
// substitution introducers). Quoted or backslash-escaped text is literal and
// only ever contributes bytes to the visible text.
type wordFacts struct {
	visible strings.Builder // shell-visible text of the word being built
	split   []shellWord     // words completed by unquoted whitespace in resolved values

	firstLiveGlob bool
	hasLiveGlob   bool
	hasLiveBrace  bool
	hasLiveSubst  bool

	braceDepth int  // open unquoted { count
	braceSep   bool // unquoted , or .. seen inside the open braces
	dotRun     int  // consecutive unquoted dots — a second one inside braces is a {x..y} separator
}

// addQuoted appends inert text: the bytes reach the program verbatim and no
// character inside them is live. Quoted content interrupts a dot run, because
// a quoted dot never completes a {x..y} sequence — bash scans for the
// separator in the unquoted text (`{1".."10}` stays literal).
func (f *wordFacts) addQuoted(s string) {
	f.visible.WriteString(s)
	f.dotRun = 0
}

// addLive appends shell-active text: every character is one the shell acts
// on. Unquoted whitespace ends the current word — bash field-splits the
// result of an unquoted expansion — which is how a resolved variable value
// carrying spaces becomes several arguments.
func (f *wordFacts) addLive(s string) {
	for i := 0; i < len(s); i++ {
		f.addLiveByte(s[i])
	}
}

// addLiveByte records one shell-active character: its visible byte and its
// quoting facts.
func (f *wordFacts) addLiveByte(ch byte) {
	switch ch {
	case ' ', '\t': // reachable only from addLive — the scanner splits first
		if f.visible.Len() > 0 {
			f.finishWord()
		}
		return
	case '*', '?', '[':
		f.hasLiveGlob = true
	case '{':
		f.braceDepth++
		if f.braceDepth == 1 {
			f.braceSep = false // a fresh outermost group needs its own separator
		}
	case '}':
		if f.braceDepth > 0 {
			f.braceDepth--
			if f.braceDepth == 0 && f.braceSep {
				f.hasLiveBrace = true
			}
		}
	case ',':
		if f.braceDepth > 0 {
			f.braceSep = true
		}
	case '.':
		f.dotRun++
		if f.dotRun >= 2 && f.braceDepth > 0 {
			f.braceSep = true
		}
	}
	if ch != '.' {
		f.dotRun = 0
	}
	f.visible.WriteByte(ch)
	if f.hasLiveGlob && f.visible.Len() == 1 {
		f.firstLiveGlob = true // this live glob is the word's first visible character
	}
}

// noteSubst records a live command or arithmetic substitution introducer.
func (f *wordFacts) noteSubst() { f.hasLiveSubst = true }

// current returns the word being built (its token text is the caller's to
// fill in — the accumulator only knows the shell-visible side).
func (f *wordFacts) current() shellWord {
	return shellWord{
		visible:       f.visible.String(),
		firstLiveGlob: f.firstLiveGlob,
		hasLiveGlob:   f.hasLiveGlob,
		hasLiveBrace:  f.hasLiveBrace,
		hasLiveSubst:  f.hasLiveSubst,
	}
}

// finishWord completes the word being built into the split list.
func (f *wordFacts) finishWord() {
	f.split = append(f.split, f.current())
	f.resetCurrent()
}

// resetCurrent clears the word being built without emitting it.
func (f *wordFacts) resetCurrent() {
	f.visible.Reset()
	f.firstLiveGlob, f.hasLiveGlob = false, false
	f.hasLiveBrace, f.hasLiveSubst = false, false
	f.braceDepth, f.braceSep, f.dotRun = 0, false, 0
}

// reset clears all state, including words split out of resolved values.
func (f *wordFacts) reset() {
	f.resetCurrent()
	f.split = nil
}

// take returns every word held — any words split out of unquoted whitespace,
// plus the word being built if it has visible content — and clears the
// accumulator.
func (f *wordFacts) take() []shellWord {
	if f.visible.Len() > 0 {
		f.finishWord()
	}
	words := f.split
	f.split = nil
	return words
}

// scanShellWords splits a command string into whitespace-delimited shell
// words, respecting single and double quotes, backslash escapes, ANSI-C
// ($'…') and locale ($"…") runs, and records each word's quoting facts.
// Word boundaries, the token text (quotes kept, escapes resolved) and the
// escape/quote-end handling are byte-for-byte tokenizeCommand's historical
// behaviour, pinned by TestTokenizeCommand; visible and the liveness flags
// are the #2213/#2216 additions:
//
//   - visible drops the `$` of $'…' and $"…" and decodes ANSI-C content, so
//     a flag assembled through those forms (`rg $'--pre=./x.sh'`) is seen by
//     the flag tables;
//   - hasLiveBrace/hasLiveSubst record brace groups and substitutions the
//     shell would expand (see wordFacts).
//
// A variable reference is NOT resolved here — the scanner cannot know the
// value. Arguments of flag-checked commands are resolved separately by
// expandedArgsUnsafe (#2216), against the symbol table and the environment
// snapshot; anything unresolvable fails closed there.
func scanShellWords(cmd string) []shellWord {
	var words []shellWord
	var text strings.Builder
	var facts wordFacts
	flush := func() {
		if text.Len() > 0 {
			w := facts.current()
			w.text = text.String()
			words = append(words, w)
		}
		text.Reset()
		facts.reset()
	}
	i := 0
	for i < len(cmd) {
		ch := cmd[i]

		// Skip whitespace between words.
		if ch == ' ' || ch == '\t' {
			flush()
			i++
			continue
		}

		// ANSI-C ($'…') and locale ($"…") runs: an unescaped $ immediately
		// before a quote starts the extended form. The $ is token text but
		// not shell-visible; the run's content reaches the visible text
		// through the form's own semantics. Only visible and the facts are
		// affected — the byte-for-byte text and word boundaries follow the
		// historical tokenizer by construction, so a pathological run (an
		// escaped quote inside $'…') partitions exactly as before and can
		// only over-block, never hide: the residue fails closed below.
		if ch == '$' && i+1 < len(cmd) && (cmd[i+1] == '\'' || cmd[i+1] == '"') {
			quote := cmd[i+1]
			end := indexUnescapedQuote(cmd, i+2, quote)
			text.WriteByte('$')
			if end < 0 {
				// Unmatched quote — take rest of string, as the plain-quote
				// path does. Printed segments cannot produce this; fail
				// closed rather than guess the decoded residue.
				text.WriteString(cmd[i+1:])
				facts.noteSubst()
				facts.addQuoted(cmd[i+2:])
				i = len(cmd)
				continue
			}
			content := cmd[i+2 : end]
			text.WriteString(cmd[i+1 : end+1])
			if quote == '\'' {
				if decoded, ok := decodeAnsiC(content); ok {
					facts.addQuoted(decoded)
				} else {
					// Undecodable escape content — fail closed instead of
					// checking approximate text.
					facts.noteSubst()
					facts.addQuoted(content)
				}
			} else {
				facts.addQuoted(content)
				if quotedSubstLive(content) {
					facts.noteSubst()
				}
			}
			i = end + 1
			continue
		}

		// Quoted run — consume through the matching quote. Content goes
		// into the token text with its delimiters (as always) and into the
		// visible text without them; nothing inside single quotes is live.
		// Inside double quotes globs and braces stay literal too, but
		// substitutions still expand, so they are live there.
		if ch == '\'' || ch == '"' {
			end := indexUnescapedQuote(cmd, i+1, ch)
			if end < 0 {
				// Unmatched quote — take rest of string.
				content := cmd[i+1:]
				text.WriteString(cmd[i:])
				facts.addQuoted(content)
				if ch == '"' && quotedSubstLive(content) {
					facts.noteSubst()
				}
				i = len(cmd)
			} else {
				content := cmd[i+1 : end]
				text.WriteString(cmd[i : end+1])
				facts.addQuoted(content)
				if ch == '"' && quotedSubstLive(content) {
					facts.noteSubst()
				}
				i = end + 1
			}
			continue
		}

		// Backslash escape — the next character is literal, never live.
		if ch == '\\' && i+1 < len(cmd) {
			text.WriteByte(cmd[i+1])
			facts.addQuoted(string(cmd[i+1]))
			i += 2
			continue
		}

		text.WriteByte(ch)
		facts.addLiveByte(ch)
		if ch == '`' {
			facts.noteSubst()
		}
		if ch == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
			facts.noteSubst() // covers both $(…) and $((…)
		}
		i++
	}
	flush()
	return words
}

// decodeAnsiC decodes the escape sequences of a $'…' (ANSI-C quoted) string
// to the bytes bash would produce. It returns false for constructs it cannot
// decode exactly — a trailing backslash, a truncated \c, \x, \u or \U — so
// callers fail closed instead of checking approximate text. Unknown letter
// escapes keep their backslash, as bash does.
func decodeAnsiC(s string) (string, bool) {
	var sb strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			sb.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", false // trailing backslash
		}
		switch e := s[i+1]; e {
		case '\\', '\'', '"', '?':
			sb.WriteByte(e)
			i += 2
		case 'a':
			sb.WriteByte('\a')
			i += 2
		case 'b':
			sb.WriteByte('\b')
			i += 2
		case 'e', 'E':
			sb.WriteByte('\x1b')
			i += 2
		case 'f':
			sb.WriteByte('\f')
			i += 2
		case 'n':
			sb.WriteByte('\n')
			i += 2
		case 'r':
			sb.WriteByte('\r')
			i += 2
		case 't':
			sb.WriteByte('\t')
			i += 2
		case 'v':
			sb.WriteByte('\v')
			i += 2
		case 'c':
			// \cX: the control character of a letter. Anything else has no
			// exact short form worth reproducing — fail closed.
			if i+2 >= len(s) || !isASCIILetter(s[i+2]) {
				return "", false
			}
			sb.WriteByte(s[i+2] & 0x1f)
			i += 3
		case 'x':
			j := i + 2
			for j < len(s) && j < i+4 && isHexDigit(s[j]) {
				j++
			}
			if j == i+2 {
				return "", false // \x with no digits
			}
			v, _ := strconv.ParseUint(s[i+2:j], 16, 8)
			sb.WriteByte(byte(v))
			i = j
		case 'u', 'U':
			width := 4
			if e == 'U' {
				width = 8
			}
			if i+2+width > len(s) {
				return "", false // truncated \u or \U
			}
			v, err := strconv.ParseUint(s[i+2:i+2+width], 16, 32)
			if err != nil || v > unicode.MaxRune {
				return "", false
			}
			sb.WriteRune(rune(v))
			i += 2 + width
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// Octal: up to three digits, or \0 followed by up to three more.
			digits := 3
			if e == '0' {
				digits = 4
			}
			j := i + 1
			for j-i-1 < digits && j < len(s) && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[i+1:j], 8, 16)
			if v > 0xff {
				return "", false
			}
			sb.WriteByte(byte(v))
			i = j
		default:
			// Unknown escape — bash keeps the backslash and the character.
			sb.WriteByte('\\')
			sb.WriteByte(e)
			i += 2
		}
	}
	return sb.String(), true
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// quotedSubstLive reports whether double-quoted content contains a live
// substitution: `$(…)` (which covers `$((…))`), or a backtick, neither
// backslash-escaped. Inside double quotes globs and brace metacharacters are
// literal, but substitutions still expand.
func quotedSubstLive(content string) bool {
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '\\':
			if i+1 < len(content) {
				i++ // skip the escaped character
			}
		case '`':
			return true
		case '$':
			if i+1 < len(content) && content[i+1] == '(' {
				return true
			}
		}
	}
	return false
}

// wordTexts returns just the token text of the words — the exact tokens
// tokenizeCommand has always produced (nil for empty input).
func wordTexts(words []shellWord) []string {
	if len(words) == 0 {
		return nil
	}
	tokens := make([]string, len(words))
	for i, w := range words {
		tokens[i] = w.text
	}
	return tokens
}

// visibleTexts returns the shell-visible text of each word — the argv the
// executed program receives, with quoting resolved.
func visibleTexts(words []shellWord) []string {
	if len(words) == 0 {
		return nil
	}
	tokens := make([]string, len(words))
	for i, w := range words {
		tokens[i] = w.visible
	}
	return tokens
}

// tokenizeCommand splits a command string into whitespace-delimited tokens,
// respecting single and double quotes and backslash escapes.
func tokenizeCommand(cmd string) []string {
	return wordTexts(scanShellWords(cmd))
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

// ---------- Pattern matching ----------

// (autoApprovePermission — the ccstream Backend method that sends the
// permission response — lives in ccstream/autoapprove.go. This shared
// package provides only the matching engine.)
