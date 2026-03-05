package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unicode"
)

// runCmd runs a command via exec.CommandContext with process group kill.
// Returns combined stdout+stderr output.
func runCmd(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = ChildSysProcAttr()
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// resolveGrepBinary detects the best available grep binary.
// Preference: rg > ack > ag > grep.
func resolveGrepBinary() (binaryPath, binaryName string) {
	for _, name := range []string{"rg", "ack", "ag", "grep"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, name
		}
	}
	return "grep", "grep" // fallback even if not found
}

// findBlockedPredicates are dangerous find predicates that are hard-rejected.
var findBlockedPredicates = map[string]bool{
	"-exec":    true,
	"-execdir": true,
	"-ok":      true,
	"-okdir":   true,
	"-delete":  true,
	"-fls":     true,
	"-fprint":  true,
	"-fprint0": true,
	"-fprintf": true,
}

// grepCanonicalFlags are flags accepted by the grep tool.
// Maps flag character to whether it takes an argument.
var grepCanonicalFlags = map[byte]bool{
	'i': false, // case insensitive
	'n': false, // line numbers
	'l': false, // files only
	'c': false, // count
	'w': false, // word match
	'F': false, // fixed string / literal
	'R': false, // recursive (added automatically for grep fallback)
}

// grepArgFlags take a numeric argument.
var grepArgFlags = map[byte]bool{
	'A': true, // after context
	'B': true, // before context
	'C': true, // context
	'm': true, // max matches
}

// NewLsTool creates the ls exploration tool.
func NewLsTool() *Tool {
	return &Tool{
		Name:        "ls",
		Description: "List directory contents.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Directory path to list."
				},
				"params": {
					"type": "string",
					"description": "Optional flags (e.g. '-la', '-ltr'). Passed directly to ls."
				}
			},
			"required": ["path"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Path   string `json:"path"`
				Params string `json:"params"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Path == "" {
				return ToolResult{}, fmt.Errorf("path is required")
			}

			var args []string
			if p.Params != "" {
				parts, _ := splitShellArgs(p.Params)
				args = append(args, parts...)
			}
			args = append(args, p.Path)

			out, err := runCmd(ctx, "ls", args...)
			if err != nil {
				if out != "" {
					return TextResult(out + "\nError: " + err.Error()), nil
				}
				return ToolResult{}, err
			}
			return TextResult(out), nil
		},
	}
}

// NewFindTool creates the find exploration tool.
func NewFindTool() *Tool {
	return &Tool{
		Name:        "find",
		Description: "Search for files in a directory hierarchy.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Starting directory path."
				},
				"params": {
					"type": "string",
					"description": "Find predicates (e.g. '-name \"*.go\" -type f'). Dangerous predicates (-exec, -execdir, -ok, -okdir, -delete, -fls, -fprint, -fprint0, -fprintf) are blocked."
				}
			},
			"required": ["path", "params"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Path   string `json:"path"`
				Params string `json:"params"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Path == "" {
				return ToolResult{}, fmt.Errorf("path is required")
			}
			if p.Params == "" {
				return ToolResult{}, fmt.Errorf("params is required")
			}

			// Check for blocked predicates before splitting
			if blocked := checkFindBlocked(p.Params); blocked != "" {
				return ToolResult{}, fmt.Errorf("blocked predicate: %s (dangerous — not allowed in explore mode)", blocked)
			}

			// Build args: find <path> <params...>
			findArgs, err := splitShellArgs(p.Params)
			if err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			args := append([]string{p.Path}, findArgs...)

			out, err := runCmd(ctx, "find", args...)
			if err != nil {
				if out != "" {
					return TextResult(out + "\nError: " + err.Error()), nil
				}
				return ToolResult{}, err
			}
			return TextResult(out), nil
		},
	}
}

// NewGrepTool creates the grep exploration tool.
// binary and name are the resolved grep binary path and name.
func NewGrepTool(binary, name string) *Tool {
	desc := fmt.Sprintf("Search file contents using %s.", name)
	return &Tool{
		Name:        "grep",
		Description: desc,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {
					"type": "string",
					"description": "Search pattern (regex by default, use -F for literal)."
				},
				"path": {
					"type": "string",
					"description": "File or directory to search (default '.')."
				},
				"params": {
					"type": "string",
					"description": "Optional flags: -i (case insensitive), -n (line numbers), -l (files only), -c (count), -w (word match), -F (literal), -A N/-B N/-C N (context), -m N (max matches), --hidden, --glob=PATTERN."
				}
			},
			"required": ["pattern"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Params  string `json:"params"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Pattern == "" {
				return ToolResult{}, fmt.Errorf("pattern is required")
			}
			if p.Path == "" {
				p.Path = "."
			}

			translated, notices := translateGrepFlags(p.Params, name)

			// For grep fallback, add -R automatically (recursive by default)
			if name == "grep" {
				translated = append([]string{"-R"}, translated...)
			}

			args := append(translated, p.Pattern, p.Path)
			out, err := runCmd(ctx, binary, args...)
			result := prependNotices(notices, out)

			if err != nil {
				// grep returns exit 1 for no matches — not an error
				if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					if result == "" {
						return TextResult("(no matches)"), nil
					}
					return TextResult(result), nil
				}
				if result != "" {
					return TextResult(result + "\nError: " + err.Error()), nil
				}
				return ToolResult{}, err
			}
			return TextResult(result), nil
		},
	}
}

// gitAllowedSubcommands are the read-only git subcommands allowed in explore mode.
var gitAllowedSubcommands = map[string]bool{
	"log":      true,
	"show":     true,
	"diff":     true,
	"status":   true,
	"blame":    true,
	"branch":   true,
	"tag":      true,
	"shortlog": true,
	"rev-parse": true,
	"ls-files": true,
	"ls-tree":  true,
}

// NewGitTool creates a read-only git exploration tool.
// Only a safe subset of git subcommands is allowed.
func NewGitTool() *Tool {
	return &Tool{
		Name:        "git",
		Description: "Run read-only git commands. Allowed subcommands: log, show, diff, status, blame, branch, tag, shortlog, rev-parse, ls-files, ls-tree.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "Git subcommand and arguments (e.g. 'log --oneline -20', 'show HEAD', 'diff --stat HEAD~3', 'blame src/main.go')."
				}
			},
			"required": ["command"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Command == "" {
				return ToolResult{}, fmt.Errorf("command is required")
			}

			args, err := splitShellArgs(p.Command)
			if err != nil {
				return ToolResult{}, fmt.Errorf("parse command: %w", err)
			}
			if len(args) == 0 {
				return ToolResult{}, fmt.Errorf("command is required")
			}

			subcmd := args[0]
			if !gitAllowedSubcommands[subcmd] {
				return ToolResult{}, fmt.Errorf("git subcommand %q is not allowed in explore mode (allowed: log, show, diff, status, blame, branch, tag, shortlog, rev-parse, ls-files, ls-tree)", subcmd)
			}

			out, err := runCmd(ctx, "git", args...)
			if err != nil {
				if out != "" {
					return TextResult(out + "\nError: " + err.Error()), nil
				}
				return ToolResult{}, err
			}
			return TextResult(out), nil
		},
	}
}

// checkFindBlocked checks if any blocked predicates appear in the params string.
// Returns the first blocked predicate found, or empty string if none.
func checkFindBlocked(params string) string {
	parts, _ := splitShellArgs(params)
	for _, part := range parts {
		if findBlockedPredicates[part] {
			return part
		}
	}
	return ""
}

// translateGrepFlags parses a params string and translates flags to the active
// binary's dialect. Returns translated args and notices for stripped flags.
func translateGrepFlags(params, binaryName string) (args []string, notices []string) {
	if params == "" {
		return nil, nil
	}

	parts, _ := splitShellArgs(params)
	i := 0
	for i < len(parts) {
		part := parts[i]

		// Handle --long-flags
		if strings.HasPrefix(part, "--") {
			handled, nextI, notice := handleLongFlag(parts, i, binaryName)
			if notice != "" {
				notices = append(notices, notice)
			}
			args = append(args, handled...)
			i = nextI
			continue
		}

		// Handle -short flags
		if strings.HasPrefix(part, "-") && len(part) > 1 {
			flagStr := part[1:]
			j := 0
			for j < len(flagStr) {
				ch := flagStr[j]

				// Check if it's a flag that takes an argument
				if grepArgFlags[ch] {
					// Argument can be attached (-C3) or separate (-C 3)
					argVal := ""
					if j+1 < len(flagStr) {
						// Attached: -C3
						argVal = flagStr[j+1:]
					} else if i+1 < len(parts) {
						// Separate: -C 3
						i++
						argVal = parts[i]
					} else {
						notices = append(notices, fmt.Sprintf("-%c requires an argument, ignored", ch))
						j++
						continue
					}
					args = append(args, fmt.Sprintf("-%c", ch), argVal)
					j = len(flagStr) // consumed rest of this part
					continue
				}

				// Check canonical simple flags
				if _, ok := grepCanonicalFlags[ch]; ok {
					translated := translateSingleFlag(ch, binaryName)
					if translated != "" {
						args = append(args, translated)
					}
					// else: translated to no-op
				} else {
					notices = append(notices, fmt.Sprintf("-%c was ignored, not supported", ch))
				}
				j++
			}
			i++
			continue
		}

		// Not a flag — skip with notice
		notices = append(notices, fmt.Sprintf("%s was ignored, not a flag", part))
		i++
	}
	return args, notices
}

// translateSingleFlag translates a single canonical flag character to the
// active binary's dialect. Returns empty string for no-ops.
func translateSingleFlag(ch byte, binaryName string) string {
	switch ch {
	case 'F':
		// -F = fixed string. ack and ag use -Q instead.
		if binaryName == "ack" || binaryName == "ag" {
			return "-Q"
		}
		return "-F"
	default:
		// i, n, l, c, w, R — universal, pass through
		return fmt.Sprintf("-%c", ch)
	}
}

// handleLongFlag handles --long-flags and translates to the active binary.
func handleLongFlag(parts []string, i int, binaryName string) (args []string, nextI int, notice string) {
	part := parts[i]
	nextI = i + 1

	switch {
	case part == "--hidden":
		switch binaryName {
		case "rg", "ack", "ag":
			return []string{"--hidden"}, nextI, ""
		default:
			// grep: hidden is implicit (no dotfile skip)
			return nil, nextI, ""
		}

	case strings.HasPrefix(part, "--glob="):
		pattern := strings.TrimPrefix(part, "--glob=")
		switch binaryName {
		case "rg":
			return []string{"--glob=" + pattern}, nextI, ""
		case "grep":
			return []string{"--include=" + pattern}, nextI, ""
		case "ack":
			// ack doesn't have a direct equivalent; best-effort skip
			return nil, nextI, "--glob was ignored, not supported by ack"
		case "ag":
			// ag uses -G for file pattern (regex, not glob — best-effort)
			return []string{"-G", pattern}, nextI, ""
		default:
			return nil, nextI, "--glob was ignored, not supported"
		}

	default:
		return nil, nextI, fmt.Sprintf("%s was ignored, not supported", part)
	}
}

// prependNotices prepends system notices to the output if any exist.
func prependNotices(notices []string, output string) string {
	if len(notices) == 0 {
		return output
	}
	var sb strings.Builder
	for _, n := range notices {
		fmt.Fprintf(&sb, "[system message: %s]\n", n)
	}
	sb.WriteString(output)
	return sb.String()
}

// splitShellArgs splits a string into shell-like arguments, respecting
// single and double quotes. This is a simple parser — no escape sequences.
func splitShellArgs(s string) ([]string, error) {
	var args []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case unicode.IsSpace(rune(ch)) && !inSingle && !inDouble:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	if inSingle || inDouble {
		return args, fmt.Errorf("unclosed quote")
	}
	return args, nil
}
