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
	case "wake":
		err = cmdWake(base, args)
	case "status":
		err = cmdStatus(base)
	case "eval":
		err = cmdEval(base, args)
	case "command":
		err = cmdCommand(base, args)
	case "ping":
		err = cmdCommand(base, []string{"/ping"})
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
  wake [text]          Trigger a wake (branch session)
  status               Query agent status
  eval <command>       Ask the agent to run a shell command
  command </cmd>       Dispatch a slash command (e.g. /ping, /cache)
  ping                 Shorthand for 'command /ping'

Environment:
  CLOD_ADDR            Gateway address (default: %s)
`, defaultAddr)
}

func cmdSend(base string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: clod send <message text>")
	}
	text := strings.Join(args, " ")
	return postJSON(base+"/send", map[string]string{"text": text})
}

func cmdWake(base string, args []string) error {
	text := strings.Join(args, " ")
	body := map[string]string{}
	if text != "" {
		body["text"] = text
	}
	return postJSON(base+"/wake", body)
}

func cmdStatus(base string) error {
	resp, err := client.Get(base + "/status")
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return printResponse(resp)
}

func cmdEval(base string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: clod eval <shell command>")
	}
	cmd := strings.Join(args, " ")
	text := fmt.Sprintf("Run this command and show the output:\n```\n%s\n```", cmd)
	return postJSON(base+"/send", map[string]string{"text": text})
}

func cmdCommand(base string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: clod command </cmd> [args]")
	}
	cmd := strings.Join(args, " ")
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	return postJSON(base+"/command", map[string]string{"command": cmd})
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
