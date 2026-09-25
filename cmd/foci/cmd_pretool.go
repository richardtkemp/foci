package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/pretool"
)

// cmdPretool implements `foci pretool list|test` (#2033): check PreToolUse
// rules against the real config file offline, without a gateway or a CC
// session. It loads the config through foci's own parser and validation and
// resolves an agent's rules exactly as the gateway does at each CC launch.
func cmdPretool(args []string, stdout io.Writer) error {
	configPath, args := parseFlagValue(args, "config")
	agentID, args := parseFlagValue(args, "agent")
	if len(args) == 0 || wantsHelp(args) {
		pretoolUsage()
		return nil
	}
	sub, args := args[0], args[1:]
	if sub != "list" && sub != "test" {
		pretoolUsage()
		return fmt.Errorf("unknown pretool subcommand: %s", sub)
	}

	if agentID == "" {
		// Inside an agent session, default to that agent.
		agentID, _, _ = strings.Cut(os.Getenv("FOCI_SESSION_KEY"), "/")
	}
	if agentID == "" {
		return fmt.Errorf("--agent is required (no FOCI_SESSION_KEY to default from)")
	}
	rules, skipped, backend, err := loadPretoolRules(configPath, agentID)
	if err != nil {
		return err
	}
	if backend != "claude-code" {
		fmt.Fprintf(os.Stderr, "note: agent %q uses backend %q; pretool rules are only enforced for claude-code\n", agentID, backend)
	}
	for _, msg := range skipped {
		fmt.Fprintf(os.Stderr, "warning: %s (skipped)\n", msg)
	}

	// Output is built in full and written once, so a write error surfaces.
	var out strings.Builder
	if sub == "list" {
		printPretoolRules(&out, rules)
	} else if err := pretoolTest(&out, rules, args); err != nil {
		return err
	}
	_, err = io.WriteString(stdout, out.String())
	return err
}

func loadPretoolRules(configPath, agentID string) (rules []pretool.Rule, skipped []string, backend string, err error) {
	if configPath == "" {
		configPath = os.Getenv("FOCI_CONFIG")
	}
	if configPath == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, nil, "", fmt.Errorf("resolve home dir: %w", herr)
		}
		configPath = filepath.Join(home, "config", "foci.toml")
	}
	cfg, err := config.Load(configPath, delegator.RegisteredNames())
	if err != nil {
		return nil, nil, "", fmt.Errorf("load config: %w", err)
	}
	rules, skipped, ok := cfg.PreToolRules(agentID)
	if !ok {
		return nil, nil, "", fmt.Errorf("no agent %q in %s", agentID, configPath)
	}
	for _, a := range cfg.Agents {
		if a.ID == agentID {
			backend = a.Backend
		}
	}
	return rules, skipped, backend, nil
}

func pretoolTest(out *strings.Builder, rules []pretool.Rule, args []string) error {
	bashCmd, args := parseFlagValue(args, "bash")
	tool, args := parseFlagValue(args, "tool")
	input, args := parseFlagValue(args, "input")
	cwd, args := parseFlagValue(args, "cwd")
	verbose := false
	for _, a := range args {
		switch a {
		case "-v", "--verbose":
			verbose = true
		default:
			return fmt.Errorf("unexpected argument %q (see foci pretool --help)", a)
		}
	}

	var call pretool.Call
	switch {
	case bashCmd != "" && (tool != "" || input != ""):
		return fmt.Errorf("--bash is shorthand for --tool Bash --input {\"command\":...}; give one or the other")
	case bashCmd == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		bashCmd = strings.TrimSuffix(string(b), "\n")
		fallthrough
	case bashCmd != "":
		enc, _ := json.Marshal(map[string]string{"command": bashCmd})
		call = pretool.Call{Tool: "Bash", Input: enc}
	case tool != "":
		if input == "" {
			input = "{}"
		}
		if !json.Valid([]byte(input)) {
			return fmt.Errorf("--input is not valid JSON")
		}
		call = pretool.Call{Tool: tool, Input: json.RawMessage(input)}
	default:
		return fmt.Errorf("give --bash <command> or --tool <name> [--input <json>]")
	}
	call.Cwd = cwd

	r := pretool.Match(rules, call)
	if r == nil {
		fmt.Fprintln(out, "no match")
	} else {
		fmt.Fprintln(out, r.Name)
	}
	if !verbose {
		return nil
	}
	if call.Tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Input, &in)
		cmds, ok := pretool.Commands(in.Command)
		if !ok {
			fmt.Fprintln(out, "  command does not parse as bash: command patterns cannot match")
		}
		for _, c := range cmds {
			fmt.Fprintf(out, "  command: %s\n", c)
		}
	}
	if r != nil {
		fmt.Fprintf(out, "  reason: %s\n", r.Reason)
	}
	return nil
}

func printPretoolRules(w *strings.Builder, rules []pretool.Rule) {
	for i, r := range rules {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n  tool: %s\n", r.Name, r.Tool)
		fields := make([]string, 0, len(r.Input))
		for f := range r.Input {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		for _, f := range fields {
			printPatterns(w, "input."+f, r.Input[f])
		}
		printPatterns(w, "command", r.Command)
		printPatterns(w, "cwd", r.Cwd)
		fmt.Fprintf(w, "  reason: %s\n", r.Reason)
	}
}

func printPatterns(w *strings.Builder, label string, pats pretool.Patterns) {
	for _, p := range pats {
		fmt.Fprintf(w, "  %s: %s\n", label, p)
	}
}

func pretoolUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci pretool <list|test> [flags]

Check PreToolUse rules against the config file, offline (no gateway, no CC
session). The config is loaded and validated as the gateway loads it, and the
agent's rules are resolved as they are at each CC launch.

Subcommands:
  list                 Print the agent's resolved rules
  test                 Run one sample tool call past the rules. Prints the
                       name of the rule that denies it, or "no match".

Flags:
  --agent <id>         Agent whose rules to use (default: from FOCI_SESSION_KEY)
  --config <path>      Config file (default: $FOCI_CONFIG, else ~/config/foci.toml)

test flags:
  --bash <command>     A Bash call with this command ("-" reads it from stdin)
  --tool <name>        Any other tool, with
  --input <json>       its tool_input object (default {})
  --cwd <dir>          The session working directory the call is made from
  -v, --verbose        Also print the reason, and for Bash the commands the
                       command patterns are matched against

Examples:
  foci pretool list --agent clutch
  foci pretool test --agent clutch --bash 'git add -A'
  foci pretool test --tool Read --input '{"file_path":"/etc/passwd"}'
`)
}
