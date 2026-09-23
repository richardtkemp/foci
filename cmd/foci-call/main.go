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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const maxResponseBytes = 1024 * 1024 // 1MB — accepted response cap; do not raise (#1933)

// maxMeasureBytes bounds how far we'll keep reading an over-cap response
// purely to report its actual size in the error message below. It is NOT an
// acceptance limit -- data beyond maxResponseBytes is never parsed or used,
// only counted, so an oversized response is still rejected exactly as before.
const maxMeasureBytes = 16 * maxResponseBytes

// errResponseTooLarge reports that a response exceeded maxResponseBytes. It
// names the cap and the measured size so the caller knows the request itself
// was fine and only the RESULT was too big -- unlike the raw
// "bufio.Scanner: token too long" this replaces, which names neither (#1933).
type errResponseTooLarge struct {
	size   int  // bytes read; exact unless approx is true
	approx bool // true if size is a lower bound (measurement itself hit maxMeasureBytes)
}

func (e *errResponseTooLarge) Error() string {
	if e.approx {
		return fmt.Sprintf("response too large (>%d bytes, cap %d bytes)", e.size, maxResponseBytes)
	}
	return fmt.Sprintf("response too large (%d bytes, cap %d bytes)", e.size, maxResponseBytes)
}

// readResponseLine reads one newline-terminated response from conn.
//
// The exec-bridge gateway spills any result over maxResponseBytes to a file
// and sends only a small JSON reference (see resp.ResultFile below), so a
// legitimate response should never approach the cap. If one still does, we
// want a message that names the cap and the actual size rather than
// bufio.Scanner's raw "token too long" -- so unlike a plain bufio.Scanner
// (which discards how much it had read once the token overflows its buffer),
// this keeps reading past the cap, bounded by maxMeasureBytes, purely to
// measure the true size before giving up. The cap on what's ACCEPTED is
// unchanged: anything over maxResponseBytes is always rejected.
func readResponseLine(conn net.Conn) ([]byte, error) {
	r := bufio.NewReaderSize(conn, 64*1024)
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			line := bytes.TrimSuffix(buf, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(line) > maxResponseBytes {
				return nil, &errResponseTooLarge{size: len(line)}
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(buf) > maxMeasureBytes {
				return nil, &errResponseTooLarge{size: len(buf), approx: true}
			}
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return nil, io.EOF
			}
			// Connection closed without a trailing newline: treat what we
			// have as the final token, mirroring bufio.Scanner's behaviour.
			if len(buf) > maxResponseBytes {
				return nil, &errResponseTooLarge{size: len(buf)}
			}
			return buf, nil
		default:
			return nil, err
		}
	}
}

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
	line, err := readResponseLine(conn)
	if err != nil {
		var tooLarge *errResponseTooLarge
		switch {
		case errors.As(err, &tooLarge):
			fmt.Fprintf(os.Stderr, "foci-call: %v -- narrow the request, e.g. a smaller --limit\n", tooLarge)
		case errors.Is(err, io.EOF):
			fmt.Fprintln(os.Stderr, "foci-call: empty response")
		default:
			fmt.Fprintf(os.Stderr, "foci-call: read: %v\n", err)
		}
		os.Exit(1)
	}

	var resp struct {
		Result     string `json:"result"`
		Error      string `json:"error"`
		ResultFile string `json:"result_file"`
		ResultSize int64  `json:"result_size"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "foci-call: parse response: %v\n", err)
		os.Exit(1)
	}

	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, resp.Error)
		os.Exit(1)
	}

	// Large results are spilled to a file by the gateway and referenced here so
	// the full body never has to fit through the socket. Stream it straight to
	// stdout — a pipe (e.g. `| jq`) then sees the complete body, and the calling
	// agent applies its own output truncation to the final result. Falls back to
	// the inline preview if the file can't be opened.
	if resp.ResultFile != "" {
		if f, err := os.Open(resp.ResultFile); err == nil {
			defer func() { _ = f.Close() }()
			if _, err := io.Copy(os.Stdout, f); err != nil {
				fmt.Fprintf(os.Stderr, "foci-call: stream result file %s: %v\n", resp.ResultFile, err)
				os.Exit(1)
			}
			return
		} else {
			fmt.Fprintf(os.Stderr, "foci-call: open result file %s: %v (showing preview)\n", resp.ResultFile, err)
		}
	}

	fmt.Print(resp.Result)
}
