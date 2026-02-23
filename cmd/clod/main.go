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

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	addr := os.Getenv("CLOD_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	base := "http://" + addr

	cmd := os.Args[1]
	args := os.Args[2:]

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
  branch [text]        Trigger a branch session (--no-compact to skip compaction)
  status               Query agent status
  eval <command>       Ask the agent to run a shell command
  command </cmd>       Dispatch a slash command (e.g. /ping, /cache)
  ping                 Shorthand for 'command /ping'

Flags:
  -a, --agent <id>     Target a specific agent (default: first agent)
  -s, --session <id>   Target a specific session (default: main)

Environment:
  CLOD_ADDR            Gateway address (default: %s)
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
	return "", args
}

type sendFlags struct {
	agent   string
	session string
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
		}
		if !consumed {
			filtered = append(filtered, args[i])
		}
	}
	return flags, filtered
}

func cmdSend(base string, args []string) error {
	flags, args := parseSendFlags(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: clod send [-a agent] [-s session] <message text>")
	}
	text := strings.Join(args, " ")
	body := map[string]string{"text": text}
	if flags.agent != "" {
		body["agent"] = flags.agent
	}
	if flags.session != "" {
		body["session"] = flags.session
	}
	return postJSON(base+"/send", body)
}

func cmdBranch(base string, args []string) error {
	agent, args := parseAgentFlag(args)
	noCompact := false
	var filtered []string
	for _, a := range args {
		if a == "--no-compact" {
			noCompact = true
		} else {
			filtered = append(filtered, a)
		}
	}
	text := strings.Join(filtered, " ")
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
