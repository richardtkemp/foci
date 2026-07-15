package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "foci/internal/delegator/ccstream" // register claude-code backend
	_ "foci/internal/delegator/cctmux"   // register claude-code-tmux backend (unsupported, filtered out)
	_ "foci/internal/delegator/opencode" // register opencode backend (HTTP/SSE; WIP, see OPENCODE_DELEGATOR_PLAN.md)
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

// unixSocketTransport returns an http.Transport that dials a Unix domain socket.
// No API key is needed — the kernel verifies peer credentials on the socket.
func unixSocketTransport(sockPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}
}

// parseSocketFlag extracts --socket from args, returning the path and remaining args.
func parseSocketFlag(args []string) (sockPath string, rest []string) {
	return parseFlagValue(args, "socket")
}

// resolveGWSocket returns the gateway Unix socket path if one is available.
// Resolution order: --socket flag > FOCI_GW_SOCK env var > default path.
// Returns empty string if no socket is found.
func resolveGWSocket(flagValue string) string {
	sock := envDefault(flagValue, "FOCI_GW_SOCK")
	if sock != "" {
		if isSocket(sock) {
			return sock
		}
		return ""
	}
	// Default: $HOME/data/foci-gw.sock
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	sock = filepath.Join(home, "data", "foci-gw.sock")
	if isSocket(sock) {
		return sock
	}
	return ""
}

// isSocket returns true if path exists and is a Unix socket.
func isSocket(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Handle "foci auth" and "foci first-run" before normal command dispatch — they don't need a gateway.
	if os.Args[1] == "auth" {
		if err := cmdAuth(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "first-run" {
		if err := cmdSetup(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "first-run failed: %v\n", err)
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
	if os.Args[1] == "debug" {
		if err := cmdDebug(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "debug: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Parse --addr, --api-key, and --socket from global args (before command).
	allArgs := os.Args[1:]
	addrFlag, allArgs := parseAddrFlag(allArgs)
	apiKeyFlag, allArgs := parseAPIKeyFlag(allArgs)
	socketFlag, allArgs := parseSocketFlag(allArgs)

	// Transport resolution: prefer Unix socket (no secret needed), fall back to TCP + API key.
	var base string
	if sock := resolveGWSocket(socketFlag); sock != "" {
		client.Transport = unixSocketTransport(sock)
		// URL host is ignored by the unix transport; use a placeholder.
		base = "http://foci-gw"
	} else {
		addr := envDefault(addrFlag, "FOCI_ADDR")
		if addr == "" {
			addr = defaultAddr
		}
		apiKey := envDefault(apiKeyFlag, "FOCI_API_KEY")
		if apiKey != "" {
			client.Transport = &authTransport{key: apiKey, base: client.Transport}
		}
		base = "http://" + addr
	}

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
	case "branch":
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
	case "pair-key", "pairkey":
		if wantsHelp(args) {
			pairKeyUsage()
		} else {
			err = cmdCommand(base, append([]string{"/pairkey"}, args...))
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
  first-run            First-run setup wizard (config, auth, character files)
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
  pair-key [host]      Mint a single-use Android pairing key (-a for agent)
  debug session <key>  Tail a session file with formatted output
  version              Print version information

Flags:
  --socket <path>      Gateway Unix socket (auto-detected from ~/data/foci-gw.sock)
  --addr <host:port>   Gateway address (default: %s)
  --api-key <key>      HTTP API key (for TCP auth; not needed with Unix socket)
  -a, --agent <id>     Target a specific agent (default: first agent)
  -s, --session <id>   Target a specific session (default: main)
  --if-active <dur>    Skip if no user activity within duration (e.g. 8h, 30m)
  -mt, --message-text  Message text (default: trailing args)
  -mf, --message-file  Read message from file path

Environment (flag > env var > default):
  FOCI_GW_SOCK         Gateway Unix socket path (--socket, auto-detected)
  FOCI_ADDR            Gateway address (--addr)
  FOCI_API_KEY         HTTP API key (--api-key, not needed with Unix socket)
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

// postSend* bound and instrument the connection-retry loop in postJSON (#998):
// when the CLI is invoked from cron/a script while the daemon is mid-restart,
// retry the dial every 5s for up to a minute before failing loudly. The last
// two are test seams.
var (
	postSendRetryEvery  = 5 * time.Second
	postSendGiveUpAfter = time.Minute
	postSendSleep       = time.Sleep
	postSendNow         = time.Now
)

func postJSON(url string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	// Retry ONLY transport-level failures (daemon unreachable — dial refused).
	// A returned *http.Response, even a non-2xx one, means the daemon is up and
	// is handled by printResponse; those are never retried.
	start := postSendNow()
	var resp *http.Response
	for {
		resp, err = client.Post(url, "application/json", bytes.NewReader(data))
		if err == nil {
			break
		}
		if postSendNow().Sub(start) >= postSendGiveUpAfter {
			fmt.Fprintf(os.Stderr, "\n*** foci unreachable — gave up after %s. Message NOT sent. Last error: %v ***\n",
				postSendGiveUpAfter, err)
			return fmt.Errorf("foci daemon unreachable after %s: %w", postSendGiveUpAfter, err)
		}
		fmt.Fprintf(os.Stderr, "foci unreachable (%v) — retrying in %s...\n", err, postSendRetryEvery)
		postSendSleep(postSendRetryEvery)
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

	// Try to extract "response" or "status" field from JSON. The routing
	// receipt (which session the message landed in, and which resolution
	// rung matched) goes to stderr so cron logs show where a send actually
	// went without polluting stdout for pipelines.
	var result struct {
		Response    string `json:"response"`
		Status      string `json:"status"`
		Session     string `json:"session"`
		ResolvedVia string `json:"resolved_via"`
	}
	if json.Unmarshal(body, &result) == nil {
		if result.Session != "" {
			fmt.Fprintf(os.Stderr, "session: %s (%s)\n", result.Session, result.ResolvedVia)
		}
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
