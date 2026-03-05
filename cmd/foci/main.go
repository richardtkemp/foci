package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"foci/internal/anthropic"
	"foci/internal/secrets"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

const defaultAddr = "127.0.0.1:18791"

var client = &http.Client{Timeout: 5 * time.Minute}

// Convention: every CLI flag must have a corresponding FOCI_ env var, and every
// FOCI_ env var must have a corresponding CLI flag. Resolution order: flag > env > default.
// When adding new flags, add the env var fallback in parseSendFlags (for send flags)
// or cmdBranch (for branch-specific flags), and update the usage() text for both.

// envDefault returns val if non-empty, otherwise falls back to the env var.
// Checks FOCI_ prefix first, then CLOD_ prefix as legacy fallback.
func envDefault(val, envKey string) string {
	if val != "" {
		return val
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	// Legacy fallback: CLOD_ prefix
	if strings.HasPrefix(envKey, "FOCI_") {
		return os.Getenv("CLOD_" + strings.TrimPrefix(envKey, "FOCI_"))
	}
	return ""
}

// envBool returns true if val is true, or the env var is non-empty.
// Checks FOCI_ prefix first, then CLOD_ prefix as legacy fallback.
func envBool(val bool, envKey string) bool {
	if val || os.Getenv(envKey) != "" {
		return true
	}
	// Legacy fallback: CLOD_ prefix
	if strings.HasPrefix(envKey, "FOCI_") {
		return os.Getenv("CLOD_"+strings.TrimPrefix(envKey, "FOCI_")) != ""
	}
	return false
}

// wantsHelp returns true if args contain -h or --help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// parseFlagValue extracts a flag value from args, returning the value and remaining args.
// Supports both --flag value and --flag=value formats.
func parseFlagValue(args []string, flagName string) (value string, rest []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+flagName && i+1 < len(args) {
			value = args[i+1]
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+2:]...)
			return value, rest
		}
		prefix := "--" + flagName + "="
		if strings.HasPrefix(args[i], prefix) {
			value = args[i][len(prefix):]
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return value, rest
		}
	}
	return "", args
}

// parseAddrFlag extracts --addr from args, returning the address and remaining args.
func parseAddrFlag(args []string) (addr string, rest []string) {
	return parseFlagValue(args, "addr")
}

// parseAPIKeyFlag extracts --api-key from args, returning the key and remaining args.
func parseAPIKeyFlag(args []string) (apiKey string, rest []string) {
	return parseFlagValue(args, "api-key")
}

// authTransport is an http.RoundTripper that injects an Authorization: Bearer
// header on every request. Wraps an underlying transport.
type authTransport struct {
	key  string
	base http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.key)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Handle "foci auth" and "foci setup" before normal command dispatch — they don't need a gateway.
	if os.Args[1] == "auth" {
		if err := cmdAuth(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "setup" {
		if err := cmdSetup(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "secrets" {
		if err := cmdSecrets(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "secrets: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Parse --addr and --api-key from global args (before command)
	allArgs := os.Args[1:]
	addrFlag, allArgs := parseAddrFlag(allArgs)
	apiKeyFlag, allArgs := parseAPIKeyFlag(allArgs)
	addr := envDefault(addrFlag, "FOCI_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	apiKey := envDefault(apiKeyFlag, "FOCI_API_KEY")
	if apiKey != "" {
		client.Transport = &authTransport{key: apiKey, base: client.Transport}
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
		if wantsHelp(args) {
			pingUsage()
		} else {
			err = cmdCommand(base, append(args, "/ping"))
		}
	case "auth":
		// Already handled above main dispatch, but list here for completeness.
		err = cmdAuth(args)
	case "help", "--help", "-h":
		usage()
	case "version", "--version", "-v":
		fmt.Printf("foci %s (commit %s, built %s, %s)\n", version, gitCommit, buildTime, goVersion)
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
	fmt.Fprintf(os.Stderr, `Usage: foci <command> [args...]

Commands:
  setup                First-run setup wizard (config, auth, character files)
  auth                 Authenticate with Anthropic (setup token from Claude Code)
  secrets              Manage secrets (list, get, set, delete)
  send <text>          Send a message to the agent (main session)
  branch [text]        Trigger a branch session
                         --no-compact      Skip compaction if context limit reached
                         --no-reset-hook   Skip pre-reset memory hook
                         --oneshot          Quick task: no compaction, no reset hook
  status               Query agent status
  eval <command>       Ask the agent to run a shell command
  command </cmd>       Dispatch a slash command (e.g. /ping, /cache)
  ping                 Shorthand for 'command /ping'
  version              Print version information

Flags:
  --addr <host:port>   Gateway address (default: %s)
  --api-key <key>      HTTP API key for authentication
  -a, --agent <id>     Target a specific agent (default: first agent)
  -s, --session <id>   Target a specific session (default: main)
  --if-active <dur>    Skip if no user activity within duration (e.g. 8h, 30m)
  -mt, --message-text  Message text (default: trailing args)
  -mf, --message-file  Read message from file path

Environment (flag > env var > default):
  FOCI_ADDR            Gateway address (--addr)
  FOCI_API_KEY         HTTP API key (--api-key)
  FOCI_AGENT           Target agent (-a)
  FOCI_SESSION         Target session (-s)
  FOCI_IF_ACTIVE       Activity gate duration (--if-active)
  FOCI_SYNC            Wait for response (--sync/--wait, non-empty = true)
  FOCI_ASYNC           Fire-and-forget (--async/--no-wait, non-empty = true)
  FOCI_MESSAGE_TEXT    Message text (-mt)
  FOCI_MESSAGE_FILE    Message file path (-mf)
  FOCI_NO_COMPACT      Skip compaction (--no-compact, non-empty = true)
  FOCI_NO_RESET_HOOK   Skip reset hook (--no-reset-hook, non-empty = true)
  FOCI_ONESHOT         Oneshot mode (--oneshot, non-empty = true)
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
		agentID = envDefault("", "FOCI_AGENT")
	}
	return agentID, args
}

type sendFlags struct {
	agent       string
	session     string
	ifActive    string // Go duration for activity gating
	ifInactive  string // Go duration for inactivity gating (opposite of ifActive)
	messageText string // explicit --message-text / -mt
	messageFile string // explicit --message-file / -mf
	async       bool   // fire-and-forget mode
	sync        bool   // wait for response (overrides async)
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
		} else if args[i] == "--if-inactive" {
			if i+1 < len(args) {
				flags.ifInactive = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--if-inactive=") {
			flags.ifInactive = args[i][len("--if-inactive="):]
			consumed = true
		} else if args[i] == "--message-text" || args[i] == "--mt" || args[i] == "-mt" {
			if i+1 < len(args) {
				flags.messageText = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--message-text=") {
			flags.messageText = args[i][len("--message-text="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "--mt=") || strings.HasPrefix(args[i], "-mt=") {
			flags.messageText = args[i][strings.Index(args[i], "=")+1:]
			consumed = true
		} else if args[i] == "--message-file" || args[i] == "--mf" || args[i] == "-mf" {
			if i+1 < len(args) {
				flags.messageFile = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--message-file=") {
			flags.messageFile = args[i][len("--message-file="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "--mf=") || strings.HasPrefix(args[i], "-mf=") {
			flags.messageFile = args[i][strings.Index(args[i], "=")+1:]
			consumed = true
		} else if args[i] == "--async" || args[i] == "--no-wait" {
			flags.async = true
			consumed = true
		} else if args[i] == "--sync" || args[i] == "--wait" {
			flags.sync = true
			consumed = true
		}
		if !consumed {
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks (flag > env > default)
	flags.agent = envDefault(flags.agent, "FOCI_AGENT")
	flags.session = envDefault(flags.session, "FOCI_SESSION")
	flags.ifActive = envDefault(flags.ifActive, "FOCI_IF_ACTIVE")
	flags.ifInactive = envDefault(flags.ifInactive, "FOCI_IF_INACTIVE")
	flags.messageText = envDefault(flags.messageText, "FOCI_MESSAGE_TEXT")
	flags.messageFile = envDefault(flags.messageFile, "FOCI_MESSAGE_FILE")
	flags.async = envBool(flags.async, "FOCI_ASYNC")
	flags.sync = envBool(flags.sync, "FOCI_SYNC")
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

func sendUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci send [-a agent] [-s session] [--if-active <dur>] [--if-inactive <dur>] [--sync] [-mt text | -mf file] <message>

Send a message to the agent's session.

By default, send is asynchronous (fire-and-forget): the CLI returns immediately
and the agent's response is delivered to Telegram. Use --sync/--wait to block
until the response is available.

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
  -s, --session <id>      Target session (env: FOCI_SESSION, default: main)
  --if-active <dur>       Skip if no user activity within duration (env: FOCI_IF_ACTIVE)
  --if-inactive <dur>     Skip if user was active within duration (env: FOCI_IF_INACTIVE)
  --sync, --wait          Wait for the response (env: FOCI_SYNC)
  --async, --no-wait      Fire-and-forget (default) (env: FOCI_ASYNC)
  -mt, --message-text     Message text (env: FOCI_MESSAGE_TEXT)
  -mf, --message-file     Read message from file (env: FOCI_MESSAGE_FILE)

Trailing args without a flag are treated as implicit --message-text.
Cannot use both -mt and -mf.
`)
}

func cmdSend(base string, args []string) error {
	if wantsHelp(args) {
		sendUsage()
		return nil
	}
	flags, args := parseSendFlags(args)
	text, err := resolveMessage(flags, args)
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("usage: foci send [-a agent] [-s session] [-mt text | -mf file] <message text>")
	}
	// Default async=true unless --sync/--wait or FOCI_SYNC is set
	async := !flags.sync
	if flags.async {
		async = true // explicit --async overrides
	}
	body := map[string]interface{}{"text": text, "async": async}
	if flags.agent != "" {
		body["agent"] = flags.agent
	}
	if flags.session != "" {
		body["session"] = flags.session
	}
	if flags.ifActive != "" {
		body["if_active"] = flags.ifActive
	}
	if flags.ifInactive != "" {
		body["if_inactive"] = flags.ifInactive
	}
	return postJSON(base+"/send", body)
}

func branchUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci branch [-a agent] [--if-active <dur>] [--if-inactive <dur>] [--no-compact] [--no-reset-hook] [--oneshot] [--sync] [-mt text | -mf file] [text]

Fork a branch session from the agent's main chat.

By default, branch is asynchronous (fire-and-forget): the CLI returns immediately
and the agent's response is delivered to Telegram. Use --sync/--wait to block
until the response is available.

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
  --if-active <dur>       Skip if no user activity within duration (env: FOCI_IF_ACTIVE)
  --if-inactive <dur>     Skip if user was active within duration (env: FOCI_IF_INACTIVE)
  --no-compact            Skip compaction if context limit reached (env: FOCI_NO_COMPACT)
  --no-reset-hook         Skip pre-reset memory hook (env: FOCI_NO_RESET_HOOK)
  --oneshot               Shorthand for --no-compact --no-reset-hook (env: FOCI_ONESHOT)
  --sync, --wait          Wait for the response (env: FOCI_SYNC)
  --async, --no-wait      Fire-and-forget (default) (env: FOCI_ASYNC)
  -mt, --message-text     Message text (env: FOCI_MESSAGE_TEXT)
  -mf, --message-file     Read message from file (env: FOCI_MESSAGE_FILE)

Aliased as 'wake'.
`)
}

func cmdBranch(base string, args []string) error {
	if wantsHelp(args) {
		branchUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	noCompact := false
	noResetHook := false
	silent := false
	asyncFlag := false
	syncFlag := false
	ifActive := ""
	ifInactive := ""
	messageText := ""
	messageFile := ""
	var filtered []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--no-compact":
			noCompact = true
		case args[i] == "--no-reset-hook":
			noResetHook = true
		case args[i] == "--silent":
			silent = true
		case args[i] == "--oneshot":
			noCompact = true
			noResetHook = true
			silent = true
		case args[i] == "--async" || args[i] == "--no-wait":
			asyncFlag = true
		case args[i] == "--sync" || args[i] == "--wait":
			syncFlag = true
		case args[i] == "--if-active" && i+1 < len(args):
			ifActive = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--if-active="):
			ifActive = args[i][len("--if-active="):]
		case args[i] == "--if-inactive" && i+1 < len(args):
			ifInactive = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--if-inactive="):
			ifInactive = args[i][len("--if-inactive="):]
		case (args[i] == "--message-text" || args[i] == "--mt" || args[i] == "-mt") && i+1 < len(args):
			messageText = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-text="):
			messageText = args[i][len("--message-text="):]
		case strings.HasPrefix(args[i], "--mt=") || strings.HasPrefix(args[i], "-mt="):
			messageText = args[i][strings.Index(args[i], "=")+1:]
		case (args[i] == "--message-file" || args[i] == "--mf" || args[i] == "-mf") && i+1 < len(args):
			messageFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-file="):
			messageFile = args[i][len("--message-file="):]
		case strings.HasPrefix(args[i], "--mf=") || strings.HasPrefix(args[i], "-mf="):
			messageFile = args[i][strings.Index(args[i], "=")+1:]
		default:
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks for branch-specific flags
	noCompact = envBool(noCompact, "FOCI_NO_COMPACT")
	noResetHook = envBool(noResetHook, "FOCI_NO_RESET_HOOK")
	if envBool(false, "FOCI_ONESHOT") {
		noCompact = true
		noResetHook = true
	}
	asyncFlag = envBool(asyncFlag, "FOCI_ASYNC")
	syncFlag = envBool(syncFlag, "FOCI_SYNC")
	ifActive = envDefault(ifActive, "FOCI_IF_ACTIVE")
	ifInactive = envDefault(ifInactive, "FOCI_IF_INACTIVE")
	messageText = envDefault(messageText, "FOCI_MESSAGE_TEXT")
	messageFile = envDefault(messageFile, "FOCI_MESSAGE_FILE")

	// Default async=true unless --sync/--wait or FOCI_SYNC is set
	async := !syncFlag
	if asyncFlag {
		async = true // explicit --async overrides
	}

	sf := sendFlags{messageText: messageText, messageFile: messageFile}
	text, err := resolveMessage(sf, filtered)
	if err != nil {
		return err
	}
	body := map[string]interface{}{"async": async}
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
	if ifInactive != "" {
		body["if_inactive"] = ifInactive
	}
	if silent {
		body["silent"] = true
	}
	return postJSON(base+"/wake", body)
}

func statusUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci status [-a agent]

Query agent status (session info, model, uptime).

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func cmdStatus(base string, args []string) error {
	if wantsHelp(args) {
		statusUsage()
		return nil
	}
	agent, _ := parseAgentFlag(args)
	url := base + "/status"
	if agent != "" {
		url += "?agent=" + agent
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return printResponse(resp)
}

func evalUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci eval [-a agent] <shell command>

Ask the agent to run a shell command and show output.

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func cmdEval(base string, args []string) error {
	if wantsHelp(args) {
		evalUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: foci eval [-a agent] <shell command>")
	}
	cmd := strings.Join(args, " ")
	text := fmt.Sprintf("Run this command and show the output:\n```\n%s\n```", cmd)
	body := map[string]string{"text": text}
	if agent != "" {
		body["agent"] = agent
	}
	return postJSON(base+"/send", body)
}

func commandUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci command [-a agent] </cmd> [args]

Dispatch a slash command via the gateway (bypasses agent conversation).

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func cmdCommand(base string, args []string) error {
	if wantsHelp(args) {
		commandUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: foci command [-a agent] </cmd> [args]")
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

func pingUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci ping [-a agent]

Liveness check (shorthand for 'foci command /ping').

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func authUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci auth [--config <path>] [--addr <host:port>]

Authenticate with Anthropic using a Claude Code setup token.
Run 'claude setup-token' in another terminal, then paste the token.
Token is saved to secrets.toml.

If a foci gateway is running, the new credentials are hot-reloaded
immediately (no restart needed).

Flags:
  --config <path>       Path to foci.toml (secrets.toml is written alongside it)
                        Default secrets path: ~/config/secrets.toml
  --addr <host:port>    Gateway address for credential hot-reload notification
                        Env: FOCI_ADDR / Default: %s

The HTTP API key (http.api_key in secrets.toml) is read automatically
to authenticate the reload request to the gateway.
`, defaultAddr)
}

func cmdAuth(args []string) error {
	if wantsHelp(args) {
		authUsage()
		return nil
	}

	// Parse --config and --addr flags
	configPath := ""
	addr := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config" && i+1 < len(args):
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			configPath = args[i][len("--config="):]
		case args[i] == "--addr" && i+1 < len(args):
			addr = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--addr="):
			addr = args[i][len("--addr="):]
		}
	}
	addr = envDefault(addr, "FOCI_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	var secretsPath string
	if configPath != "" {
		// --config explicitly provided: derive secrets path from it
		secretsPath = filepath.Join(filepath.Dir(configPath), "secrets.toml")
	} else {
		// Default: ~/config/secrets.toml
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		secretsPath = filepath.Join(home, "config", "secrets.toml")
	}

	// If the file doesn't exist, confirm path with the user before writing
	if _, err := os.Stat(secretsPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Secrets file will be created at: %s\nConfirm? [Y/n] ", secretsPath)
		var answer string
		fmt.Scanln(&answer)
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			return fmt.Errorf("aborted")
		}
		if err := os.MkdirAll(filepath.Dir(secretsPath), 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", filepath.Dir(secretsPath), err)
		}
	}

	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
	}
	if err := anthropic.RunSetupTokenFlow(store); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Setup token saved to %s\n", secretsPath)

	// Read HTTP API key from secrets for gateway notification auth
	httpAPIKey, _ := store.Get("http.api_key")

	// Notify running gateway to hot-reload credentials.
	notifyGatewayReload(addr, httpAPIKey)
	return nil
}

// notifyGatewayReload sends a POST to the gateway's /-/reload-credentials endpoint.
// Best-effort: if the gateway isn't running, prints a note and continues.
func notifyGatewayReload(addr, apiKey string) {
	u := fmt.Sprintf("http://%s/-/reload-credentials", addr)
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gateway not reachable at %s — restart to use new credentials.\n", addr)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gateway not reachable at %s — restart to use new credentials.\n", addr)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		fmt.Fprintf(os.Stderr, "Gateway credentials hot-reloaded.\n")
	} else {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Gateway reload returned HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func secretsUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci secrets <subcommand> [args...]

Manage secrets in secrets.toml without a running gateway.

Subcommands:
  list                          List secret names (no values)
  get <section.key>             Print secret value to stdout
  set <section.key> <value>     Add or update a secret
  delete <section.key>          Remove a secret

Flags:
  --config <path>       Path to foci.toml (secrets.toml is resolved alongside it)
                        Default secrets path: ~/config/secrets.toml
`)
}

func cmdSecrets(args []string) error {
	if len(args) == 0 {
		secretsUsage()
		return nil
	}
	// Show top-level secrets help only when -h/--help is the first arg
	// (not a subcommand). Subcommands handle their own help.
	if args[0] == "-h" || args[0] == "--help" {
		secretsUsage()
		return nil
	}

	// Parse --config flag
	configPath := ""
	var filtered []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config" && i+1 < len(args):
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			configPath = args[i][len("--config="):]
		default:
			filtered = append(filtered, args[i])
		}
	}

	// Resolve secrets path (same pattern as cmdAuth)
	var secretsPath string
	if configPath != "" {
		secretsPath = filepath.Join(filepath.Dir(configPath), "secrets.toml")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		secretsPath = filepath.Join(home, "config", "secrets.toml")
	}

	if len(filtered) == 0 {
		secretsUsage()
		return nil
	}

	sub := filtered[0]
	subArgs := filtered[1:]

	switch sub {
	case "list":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets list\n\nList all secret names (values are not shown).\n")
			return nil
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		names := store.Names()
		if len(names) == 0 {
			fmt.Fprintf(os.Stderr, "no secrets in %s\n", secretsPath)
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil

	case "get":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets get <section.key>\n\nPrint the value of a secret to stdout.\n")
			return nil
		}
		if len(subArgs) != 1 {
			return fmt.Errorf("usage: foci secrets get <section.key>")
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		val, ok := store.Get(subArgs[0])
		if !ok {
			return fmt.Errorf("secret %q not found", subArgs[0])
		}
		fmt.Print(val)
		return nil

	case "set":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets set <section.key> <value>\n\nAdd or update a secret. Key must be in section.key format (e.g. custom.github_token).\n")
			return nil
		}
		if len(subArgs) != 2 {
			return fmt.Errorf("usage: foci secrets set <section.key> <value>")
		}
		if !strings.Contains(subArgs[0], ".") {
			return fmt.Errorf("key must be in section.key format (e.g. custom.github_token)")
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		store.Set(subArgs[0], subArgs[1])
		if err := store.Save(); err != nil {
			return fmt.Errorf("save secrets: %w", err)
		}
		fmt.Fprintf(os.Stderr, "set %s in %s\n", subArgs[0], secretsPath)
		return nil

	case "delete":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets delete <section.key>\n\nRemove a secret by name.\n")
			return nil
		}
		if len(subArgs) != 1 {
			return fmt.Errorf("usage: foci secrets delete <section.key>")
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		if !store.Remove(subArgs[0]) {
			return fmt.Errorf("secret %q not found", subArgs[0])
		}
		if err := store.Save(); err != nil {
			return fmt.Errorf("save secrets: %w", err)
		}
		fmt.Fprintf(os.Stderr, "deleted %s from %s\n", subArgs[0], secretsPath)
		return nil

	default:
		return fmt.Errorf("unknown subcommand: %s\nRun 'foci secrets --help' for usage", sub)
	}
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
	defer func() { _ = resp.Body.Close() }()
	return printResponse(resp)
}

func printResponse(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Try to extract "response" or "status" field from JSON
	var result struct {
		Response string `json:"response"`
		Status   string `json:"status"`
	}
	if json.Unmarshal(body, &result) == nil {
		if result.Response != "" {
			fmt.Println(result.Response)
			return nil
		}
		if result.Status != "" {
			fmt.Println(result.Status)
			return nil
		}
	}

	// Fallback: print raw body
	fmt.Print(string(body))
	return nil
}
