package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
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

// legacyEnvFallback returns the value of the legacy CLOD_ env var if it exists.
// Used for backward compatibility with old CLOD_ environment variable names.
func legacyEnvFallback(envKey string) string {
	if strings.HasPrefix(envKey, "FOCI_") {
		return os.Getenv("CLOD_" + strings.TrimPrefix(envKey, "FOCI_"))
	}
	return ""
}

// envDefault returns val if non-empty, otherwise falls back to the env var.
// Checks FOCI_ prefix first, then CLOD_ prefix as legacy fallback.
func envDefault(val, envKey string) string {
	if val != "" {
		return val
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return legacyEnvFallback(envKey)
}

// envBool returns true if val is true, or the env var is non-empty.
// Checks FOCI_ prefix first, then CLOD_ prefix as legacy fallback.
func envBool(val bool, envKey string) bool {
	if val || os.Getenv(envKey) != "" {
		return true
	}
	return legacyEnvFallback(envKey) != ""
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
