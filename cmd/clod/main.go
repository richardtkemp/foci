package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAddr = "127.0.0.1:18791"

var client = &http.Client{Timeout: 5 * time.Minute}

// Convention: every CLI flag must have a corresponding CLOD_ env var, and every
// CLOD_ env var must have a corresponding CLI flag. Resolution order: flag > env > default.
// When adding new flags, add the env var fallback in parseSendFlags (for send flags)
// or cmdBranch (for branch-specific flags), and update the usage() text for both.

// envDefault returns val if non-empty, otherwise falls back to the env var.
func envDefault(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// envBool returns true if val is true, or the env var is non-empty.
func envBool(val bool, envKey string) bool {
	return val || os.Getenv(envKey) != ""
}

// parseAddrFlag extracts --addr from args, returning the address and remaining args.
func parseAddrFlag(args []string) (addr string, rest []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--addr" && i+1 < len(args) {
			addr = args[i+1]
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+2:]...)
			return addr, rest
		}
		if strings.HasPrefix(args[i], "--addr=") {
			addr = args[i][len("--addr="):]
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return addr, rest
		}
	}
	return "", args
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Parse --addr from global args (before command)
	allArgs := os.Args[1:]
	addrFlag, allArgs := parseAddrFlag(allArgs)
	addr := envDefault(addrFlag, "CLOD_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	base := "http://" + addr

	if len(allArgs) == 0 {
		usage()
		os.Exit(1)
	}
	cmd := allArgs[0]
	args := allArgs[1:]

	var err error
	switch cmd {
	case "send":
		err = cmdSend(base, args)
	case "branch", "wake":
		err = cmdBranch(base, args)
	case "status":
		err = cmdStatus(base, args)
	case "eval":
		err = cmdEval(base, args)
	case "command":
		err = cmdCommand(base, args)
	case "ping":
		err = cmdCommand(base, append(args, "/ping"))
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: clod <command> [args...]

Commands:
  send <text>          Send a message to the agent (main session)
  branch [text]        Trigger a branch session
                         --no-compact      Skip compaction if context limit reached
                         --no-reset-hook   Skip pre-reset memory hook
                         --oneshot          Quick task: no compaction, no reset hook
  status               Query agent status
  eval <command>       Ask the agent to run a shell command
  command </cmd>       Dispatch a slash command (e.g. /ping, /cache)
  ping                 Shorthand for 'command /ping'

Flags:
  --addr <host:port>   Gateway address (default: %s)
  -a, --agent <id>     Target a specific agent (default: first agent)
  -s, --session <id>   Target a specific session (default: main)
  --if-active <dur>    Skip if no user activity within duration (e.g. 8h, 30m)
  -mt, --message-text  Message text (default: trailing args)
  -mf, --message-file  Read message from file path

Environment (flag > env var > default):
  CLOD_ADDR            Gateway address (--addr)
  CLOD_AGENT           Target agent (-a)
  CLOD_SESSION         Target session (-s)
  CLOD_IF_ACTIVE       Activity gate duration (--if-active)
  CLOD_MESSAGE_TEXT    Message text (-mt)
  CLOD_MESSAGE_FILE    Message file path (-mf)
  CLOD_NO_COMPACT      Skip compaction (--no-compact, non-empty = true)
  CLOD_NO_RESET_HOOK   Skip reset hook (--no-reset-hook, non-empty = true)
  CLOD_ONESHOT         Oneshot mode (--oneshot, non-empty = true)
`, defaultAddr)
}

// parseAgentFlag extracts -a/--agent from args, returning the agent ID and
// remaining args. Returns empty string if no flag is present.
func parseAgentFlag(args []string) (agentID string, rest []string) {
	for i := 0; i < len(args); i++ {
		if (args[i] == "-a" || args[i] == "--agent") && i+1 < len(args) {
			agentID = args[i+1]
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+2:]...)
			return agentID, rest
		}
		// Handle --agent=value and -a=value
		for _, prefix := range []string{"--agent=", "-a="} {
			if strings.HasPrefix(args[i], prefix) {
				agentID = args[i][len(prefix):]
				rest = append(rest, args[:i]...)
				rest = append(rest, args[i+1:]...)
				return agentID, rest
			}
		}
	}
	// Env var fallback
	if agentID == "" {
		agentID = os.Getenv("CLOD_AGENT")
	}
	return agentID, args
}

type sendFlags struct {
	agent       string
	session     string
	ifActive    string // Go duration for activity gating
	messageText string // explicit --message-text / -mt
	messageFile string // explicit --message-file / -mf
}

func parseSendFlags(args []string) (flags sendFlags, rest []string) {
	var filtered []string
	for i := 0; i < len(args); i++ {
		consumed := false
		if args[i] == "-a" || args[i] == "--agent" {
			if i+1 < len(args) {
				flags.agent = args[i+1]
				i++
				consumed = true
			}
		} else if args[i] == "-s" || args[i] == "--session" {
			if i+1 < len(args) {
				flags.session = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--agent=") {
			flags.agent = args[i][len("--agent="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-a=") {
			flags.agent = args[i][len("-a="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "--session=") {
			flags.session = args[i][len("--session="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-s=") {
			flags.session = args[i][len("-s="):]
			consumed = true
		} else if args[i] == "--if-active" {
			if i+1 < len(args) {
				flags.ifActive = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--if-active=") {
			flags.ifActive = args[i][len("--if-active="):]
			consumed = true
		} else if args[i] == "--message-text" || args[i] == "-mt" {
			if i+1 < len(args) {
				flags.messageText = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--message-text=") {
			flags.messageText = args[i][len("--message-text="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-mt=") {
			flags.messageText = args[i][len("-mt="):]
			consumed = true
		} else if args[i] == "--message-file" || args[i] == "-mf" {
			if i+1 < len(args) {
				flags.messageFile = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--message-file=") {
			flags.messageFile = args[i][len("--message-file="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-mf=") {
			flags.messageFile = args[i][len("-mf="):]
			consumed = true
		}
		if !consumed {
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks (flag > env > default)
	flags.agent = envDefault(flags.agent, "CLOD_AGENT")
	flags.session = envDefault(flags.session, "CLOD_SESSION")
	flags.ifActive = envDefault(flags.ifActive, "CLOD_IF_ACTIVE")
	flags.messageText = envDefault(flags.messageText, "CLOD_MESSAGE_TEXT")
	flags.messageFile = envDefault(flags.messageFile, "CLOD_MESSAGE_FILE")
	return flags, filtered
}

// resolveMessage determines the message text from flags and trailing args.
// Priority: --message-text / --message-file / trailing args (implicit -mt).
// Returns error if both -mt and -mf are set, or if the file cannot be read.
func resolveMessage(flags sendFlags, trailingArgs []string) (string, error) {
	if flags.messageText != "" && flags.messageFile != "" {
		return "", fmt.Errorf("cannot specify both --message-text and --message-file")
	}
	if flags.messageFile != "" {
		data, err := os.ReadFile(flags.messageFile)
		if err != nil {
			return "", fmt.Errorf("reading message file: %w", err)
		}
		return string(data), nil
	}
	if flags.messageText != "" {
		return flags.messageText, nil
	}
	if len(trailingArgs) > 0 {
		return strings.Join(trailingArgs, " "), nil
	}
	return "", nil
}

func cmdSend(base string, args []string) error {
	flags, args := parseSendFlags(args)
	text, err := resolveMessage(flags, args)
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("usage: clod send [-a agent] [-s session] [-mt text | -mf file] <message text>")
	}
	body := map[string]string{"text": text}
	if flags.agent != "" {
		body["agent"] = flags.agent
	}
	if flags.session != "" {
		body["session"] = flags.session
	}
	if flags.ifActive != "" {
		body["if_active"] = flags.ifActive
	}
	return postJSON(base+"/send", body)
}

func cmdBranch(base string, args []string) error {
	agent, args := parseAgentFlag(args)
	noCompact := false
	noResetHook := false
	ifActive := ""
	messageText := ""
	messageFile := ""
	var filtered []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--no-compact":
			noCompact = true
		case args[i] == "--no-reset-hook":
			noResetHook = true
		case args[i] == "--oneshot":
			noCompact = true
			noResetHook = true
		case args[i] == "--if-active" && i+1 < len(args):
			ifActive = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--if-active="):
			ifActive = args[i][len("--if-active="):]
		case (args[i] == "--message-text" || args[i] == "-mt") && i+1 < len(args):
			messageText = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-text="):
			messageText = args[i][len("--message-text="):]
		case strings.HasPrefix(args[i], "-mt="):
			messageText = args[i][len("-mt="):]
		case (args[i] == "--message-file" || args[i] == "-mf") && i+1 < len(args):
			messageFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-file="):
			messageFile = args[i][len("--message-file="):]
		case strings.HasPrefix(args[i], "-mf="):
			messageFile = args[i][len("-mf="):]
		default:
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks for branch-specific flags
	noCompact = envBool(noCompact, "CLOD_NO_COMPACT")
	noResetHook = envBool(noResetHook, "CLOD_NO_RESET_HOOK")
	if envBool(false, "CLOD_ONESHOT") {
		noCompact = true
		noResetHook = true
	}
	ifActive = envDefault(ifActive, "CLOD_IF_ACTIVE")
	messageText = envDefault(messageText, "CLOD_MESSAGE_TEXT")
	messageFile = envDefault(messageFile, "CLOD_MESSAGE_FILE")

	sf := sendFlags{messageText: messageText, messageFile: messageFile}
	text, err := resolveMessage(sf, filtered)
	if err != nil {
		return err
	}
	body := map[string]interface{}{}
	if agent != "" {
		body["agent"] = agent
	}
	if text != "" {
		body["text"] = text
	}
	if noCompact {
		body["no_compact"] = true
	}
	if noResetHook {
		body["no_reset_hook"] = true
	}
	if ifActive != "" {
		body["if_active"] = ifActive
	}
	return postJSON(base+"/wake", body)
}

func cmdStatus(base string, args []string) error {
	agent, _ := parseAgentFlag(args)
	url := base + "/status"
	if agent != "" {
		url += "?agent=" + agent
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return printResponse(resp)
}

func cmdEval(base string, args []string) error {
	agent, args := parseAgentFlag(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: clod eval [-a agent] <shell command>")
	}
	cmd := strings.Join(args, " ")
	text := fmt.Sprintf("Run this command and show the output:\n```\n%s\n```", cmd)
	body := map[string]string{"text": text}
	if agent != "" {
		body["agent"] = agent
	}
	return postJSON(base+"/send", body)
}

func cmdCommand(base string, args []string) error {
	agent, args := parseAgentFlag(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: clod command [-a agent] </cmd> [args]")
	}
	cmd := strings.Join(args, " ")
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	body := map[string]string{"command": cmd}
	if agent != "" {
		body["agent"] = agent
	}
	return postJSON(base+"/command", body)
}

func postJSON(url string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return printResponse(resp)
}

func printResponse(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Try to extract "response" field from JSON
	var result struct {
		Response string `json:"response"`
	}
	if json.Unmarshal(body, &result) == nil && result.Response != "" {
		fmt.Println(result.Response)
		return nil
	}

	// Fallback: print raw body
	fmt.Print(string(body))
	return nil
}
