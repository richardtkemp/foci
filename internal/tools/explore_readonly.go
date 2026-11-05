package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/delegator/autoapprove"
)

// Explore's read-only shell (#2702, Dick 2026-11-01: "ok they can have a
// shell with those read-only tools. Their prompt should clearly specify the
// whitelist, and that everything else is unavailable. They are invited to note
// in their response if there is a tool missing which they believe they and
// other explore agents would benefit from.").
//
// A delegated explore batch (BatchAccessReadOnly) gets a shell. Every command
// it runs is decided by foci's read-only auto-approve set alone
// (ReadOnlyBatchRules): matched → run, anything else → denied at once, never
// prompted and never sent up to the spawner. The prompt's whitelist is
// generated from the same rule list (ExploreShellPrompt), so the two cannot
// drift.

// readOnlyTodoActions are the todo actions a read-only session may run.
var readOnlyTodoActions = map[string]bool{
	"list":   true,
	"search": true,
	"get":    true,
}

// readOnlyTodoTool returns a copy of the todo tool that refuses every action
// but list, search and get (#2702; #397's "read-only whitelist" covered the
// tool, not its actions, so an explore spawn could add, complete and drop
// todos). The schema is unchanged, so the shell function generated from it
// is the same; only Execute refuses.
func readOnlyTodoTool(t *Tool) *Tool {
	ro := *t
	inner := t.Execute
	ro.Description = "Read the todo list (read-only here: list, search and get only). " + t.Description
	ro.Execute = func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
		var p struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return ToolResult{}, err
		}
		if a := resolveTodoAction(p.Action); !readOnlyTodoActions[a] {
			return ToolResult{}, fmt.Errorf("todo %s: unavailable in a read-only session; only list, search and get may run", p.Action)
		}
		return inner(ctx, params)
	}
	return &ro
}

// readOnlyTool returns the form of a registry tool a read-only session gets:
// todo is narrowed to its read actions, the rest are shared as they are.
func readOnlyTool(t *Tool) *Tool {
	if t.Name == "todo" {
		return readOnlyTodoTool(t)
	}
	return t
}

// readOnlyRegistry is the explore allowlist of reg (spawnExploreAllowed),
// each tool in its read-only form.
func readOnlyRegistry(reg *Registry) *Registry {
	out := NewRegistry()
	for _, t := range reg.All() {
		if spawnExploreAllowed[t.Name] {
			out.Register(readOnlyTool(t))
		}
	}
	return out
}

// ReadOnlyBatchRules returns the only auto-approve rules a read-only batch
// session is decided by: foci's built-in read-only set
// (autoapprove.CommonReadonlyRules, whatever the agent's own toggles say),
// explore-only cd/uniq rules, and a rule for each foci shell function its
// exec bridge carries (the read-only registry, RegistryForAccess). Nothing
// from the agent's own configuration is added. reg may be nil (no bridge).
func ReadOnlyBatchRules(reg *Registry) []string {
	rules := readOnlyShellRules()
	if ro := readOnlyFociFuncs(reg); len(ro) > 0 {
		rules = append(rules, autoapprove.FociShellRulesFor(ro)...)
	}
	return rules
}

// readOnlyShellRules is shared by enforcement and the generated prompt.
// cd is explore-only: the matcher does not track cwd for path-scoped rules
// an ordinary agent might combine with it. uniq stays local to this extension
// too; autoapprove guards its output operand even under an explicit rule.
func readOnlyShellRules() []string {
	rules := append([]string(nil), autoapprove.CommonReadonlyRules...)
	return append(rules, "Bash:cd", "Bash:uniq")
}

// readOnlyFociFuncs returns the foci shell function names a
// read-only session's exec bridge exports (foci_todo, foci_memory_search,
// …), or nil when reg is nil.
func readOnlyFociFuncs(reg *Registry) []string {
	if reg == nil {
		return nil
	}
	return readOnlyRegistry(reg).ExportedNames()
}

// readOnlyShellCommands returns the command patterns of the read-only rule
// set (the "Bash:" rules of readOnlyShellRules), in order, as
// autoapprove.DisplayRule renders them (the read-only git rule names its
// subcommands).
func readOnlyShellCommands() []string {
	var cmds []string
	for _, r := range readOnlyShellRules() {
		if strings.HasPrefix(r, "Bash:") {
			cmds = append(cmds, autoapprove.DisplayRule(r))
		}
	}
	return cmds
}

// ExploreShellPrompt is the part of a delegated explore spawn's system prompt
// that states its shell whitelist: the read-only commands (from
// readOnlyShellRules) and the foci shell functions fociFuncs,
// and that everything else is unavailable. It ends with the invitation to
// name a missing tool.
func ExploreShellPrompt(fociFuncs []string) string {
	var b strings.Builder
	b.WriteString("## Your shell: a read-only whitelist\n\n")
	b.WriteString("Besides your file, search and web tools you have a shell, limited to this whitelist. A command runs only when every command in it is one of these (a listed entry with several words, like `crontab -l`, must be used with those words):\n\n")
	for _, c := range readOnlyShellCommands() {
		fmt.Fprintf(&b, "- `%s`\n", c)
	}
	if len(fociFuncs) > 0 {
		b.WriteString("\nfoci functions (run `<name> --help` for usage):\n\n")
		for _, f := range fociFuncs {
			if f == "foci_todo" {
				fmt.Fprintf(&b, "- `%s` (list, search and get only)\n", f)
				continue
			}
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}
	b.WriteString("\nEverything else is unavailable and is denied at once, with no one to ask: any other command, ")
	b.WriteString("output redirection to a file (`> file`; `2>/dev/null` is fine), and the writing or executing flags of listed commands ")
	b.WriteString("(`uniq INPUT OUTPUT`, `sed -i`, `find -exec`/`-delete`, `sort -o`, sqlite3 dot-commands like `.shell`, git `--output`/`--ext-diff`/`--textconv`). Pipes between listed commands work. For uniq, put options before its single input file; optional flag values must be attached, and abbreviated or unknown flags are unavailable. ")
	b.WriteString("Use `sqlite3 -readonly <db> '<query>'` for SQLite and `jq` for JSON/JSONL. A denial is final: do not retry the same command; find another way with the whitelist.\n\n")
	b.WriteString("If a tool you needed is missing from this whitelist and you believe you and other explore agents would benefit from it, say so at the end of your answer: name the tool and what you would have used it for.")
	return b.String()
}
