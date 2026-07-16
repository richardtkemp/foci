package autoapprove

import (
	"encoding/json"
	"path/filepath"
	"strings"

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

// Match checks whether a permission request matches any auto-approve rule.
// Exported wrapper around the internal matcher for cross-backend use.
func Match(rules []Rule, toolName string, input json.RawMessage) bool {
	return matchAutoApprove(rules, toolName, input)
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

// matchAutoApprove checks whether a permission request matches any auto-approve
// rule. Returns true if the request should be auto-approved.
//
// For Bash commands, the command is parsed into an AST and every structural
// element and simple command is validated. This prevents bypasses via shell
// features like redirects, process substitution, and command wrappers.
func matchAutoApprove(rules []Rule, toolName string, input json.RawMessage) bool {
	if toolName == "Bash" {
		return matchBashAutoApprove(rules, input)
	}
	return matchToolAutoApprove(rules, toolName, input)
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
//   - Command substitution $() and backticks are recursively validated
//   - Brace expansion {a,b} is rejected
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
func matchBashAutoApprove(rules []Rule, input json.RawMessage) bool {
	command := extractMatchString("Bash", input)
	if command == "" {
		return false
	}

	stmts, ok := parseShellScript(command)
	if !ok {
		return false // unparseable → fail safe (prompt user)
	}

	return validateParsedCommand(rules, stmts, 0, newVarCtx())
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
	//
	// Note: the parser treats brace expansion ({a,b}, {1..10}) as literal
	// text in Lit nodes. syntax.SplitBraces exists to convert them into
	// BraceExp nodes, but Walk panics on BraceExp (unsupported node type).
	// So we detect brace expansion by inspecting Lit values directly.
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
		case *syntax.Lit:
			if litContainsBraceExpansion(n.Value) {
				safe = false
			}
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
// script. It deliberately never reads the gateway environment: permission
// validation must not assume it is byte-identical to the delegated shell's
// environment.
type varCtx struct {
	symbolTable map[string]string // known literal assignments: name → value
	unknown     map[string]bool   // assignments whose value/scope is not modelled
}

func newVarCtx() *varCtx {
	return &varCtx{symbolTable: make(map[string]string), unknown: make(map[string]bool)}
}

func (vc *varCtx) clone() *varCtx {
	copy := newVarCtx()
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
	return "", false
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

// ---------- Command segment validation ----------

// matchBashSegment checks whether a single command string matches at least one
// Bash rule. If the command contains flags or arguments that are known to be
// unsafe (e.g. sed -i, sort -o), the match is rejected regardless of which
// rule matched.
func matchBashSegment(rules []Rule, segment string) bool {
	if containsUnsafeFlags(segment) {
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

// ---------- Pattern matching ----------

// (autoApprovePermission — the ccstream Backend method that sends the
// permission response — lives in ccstream/autoapprove.go. This shared
// package provides only the matching engine.)
