// foci-call is a small binary used inside exec commands to invoke foci tools
// via the exec bridge unix socket. It reads FOCI_SOCK, connects, sends a JSON
// request, and prints the result to stdout or error to stderr.
//
// Usage: foci-call '<json>'
//
// The JSON argument must contain a "tool" field and a "params" object.
// Example: foci-call '{"tool":"web_search","params":{"query":"golang"}}'
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

const maxResponseBytes = 1024 * 1024 // 1MB

func printUsage() {
	fmt.Fprintf(os.Stderr, `foci-call — invoke foci tools via the exec bridge socket

Usage: foci-call '<json>'

The JSON argument must contain a "tool" field and a "params" object.
Example: foci-call '{"tool":"web_search","params":{"query":"golang"}}'

Environment:
  FOCI_SOCK    Unix socket path for exec bridge (required)

Flags:
  -h, --help       Show this help
  --version, -v    Print version information
`)
}

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			printUsage()
			os.Exit(0)
		case "--version", "-v", "version":
			fmt.Printf("foci-call %s (commit %s, built %s, %s)\n", version, gitCommit, buildTime, goVersion)
			os.Exit(0)
		}
	}

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: foci-call '<json>'")
		os.Exit(1)
	}

	sock := os.Getenv("FOCI_SOCK")
	if sock == "" {
		fmt.Fprintln(os.Stderr, "foci-call: FOCI_SOCK not set")
		os.Exit(1)
	}

	// Validate JSON before sending
	arg := os.Args[1]
	if !json.Valid([]byte(arg)) {
		fmt.Fprintln(os.Stderr, "foci-call: invalid JSON argument")
		os.Exit(1)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "foci-call: connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	// Send request (newline-terminated)
	if _, err := fmt.Fprintf(conn, "%s\n", arg); err != nil {
		fmt.Fprintf(os.Stderr, "foci-call: send: %v\n", err)
		os.Exit(1)
	}

	// Read response
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "foci-call: read: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "foci-call: empty response")
		}
		os.Exit(1)
	}

	var resp struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		fmt.Fprintf(os.Stderr, "foci-call: parse response: %v\n", err)
		os.Exit(1)
	}

	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, resp.Error)
		os.Exit(1)
	}

	fmt.Print(resp.Result)
}
