package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary builds the foci-call binary for testing.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "foci-call")
	// -buildvcs=false: VCS stamping shells out to git, which fails when `make test`
	// redirects HOME away from ~/.gitconfig and its safe.directory exception, which
	// git needs when the checkout is owned by a different user than the test runner
	// (#1561). The stamp is worthless to a throwaway test binary anyway.
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	cmd.Dir = filepath.Dir(mustAbs(t, "main.go"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}
	return bin
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// startTestServer creates a unix socket that responds to one request.
func startTestServer(t *testing.T, handler func(req string) string) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64*1024)
		n, _ := conn.Read(buf)
		resp := handler(strings.TrimSpace(string(buf[:n])))
		fmt.Fprintf(conn, "%s\n", resp)
	}()

	return sockPath
}

// TestFociCallSuccess builds the foci-call binary and exercises the happy
// path end-to-end — a successful round-trip over a Unix socket.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallSuccess(t *testing.T) {
	bin := buildBinary(t)
	sockPath := startTestServer(t, func(req string) string {
		resp, _ := json.Marshal(map[string]string{"result": "hello world"})
		return string(resp)
	})

	cmd := exec.Command(bin, `{"tool":"test","params":{}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("foci-call failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "hello world" {
		t.Errorf("output = %q, want %q", string(out), "hello world")
	}
}

// TestFociCallStreamsResultFile verifies that when the server returns a
// result_file pointer (large result spilled to disk), foci-call streams the
// file's full contents to stdout rather than printing the inline preview —
// so a downstream pipe receives the complete body.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallStreamsResultFile(t *testing.T) {
	bin := buildBinary(t)
	full := filepath.Join(t.TempDir(), "body.json")
	fullContent := strings.Repeat("X", 5000) + `{"complete":true}`
	if err := os.WriteFile(full, []byte(fullContent), 0600); err != nil {
		t.Fatal(err)
	}
	sockPath := startTestServer(t, func(req string) string {
		resp, _ := json.Marshal(map[string]any{
			"result":      "PREVIEW-ONLY",
			"result_file": full,
			"result_size": len(fullContent),
		})
		return string(resp)
	})

	cmd := exec.Command(bin, `{"tool":"http_request","params":{}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("foci-call failed: %v\n%s", err, out)
	}
	if string(out) != fullContent {
		t.Errorf("expected full file contents streamed to stdout (%d bytes), got %d bytes; preview leak=%v",
			len(fullContent), len(out), strings.Contains(string(out), "PREVIEW-ONLY"))
	}
}

// TestFociCallResultFileFallback verifies that if the referenced result_file
// can't be opened, foci-call falls back to the inline preview (data is not
// lost from the agent's view) and still exits zero.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallResultFileFallback(t *testing.T) {
	bin := buildBinary(t)
	sockPath := startTestServer(t, func(req string) string {
		resp, _ := json.Marshal(map[string]any{
			"result":      "fallback preview",
			"result_file": "/nonexistent/path/body.json",
			"result_size": 999,
		})
		return string(resp)
	})

	cmd := exec.Command(bin, `{"tool":"http_request","params":{}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("foci-call should fall back, not fail: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "fallback preview") {
		t.Errorf("expected fallback to inline preview, got %q", string(out))
	}
}

// TestFociCallError verifies the binary exits non-zero when the server
// returns an error field in the JSON response.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallError(t *testing.T) {
	bin := buildBinary(t)
	sockPath := startTestServer(t, func(req string) string {
		resp, _ := json.Marshal(map[string]string{"error": "something failed"})
		return string(resp)
	})

	cmd := exec.Command(bin, `{"tool":"test","params":{}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "something failed") {
		t.Errorf("output = %q, want error message", string(out))
	}
}

// TestFociCallNoSocket verifies the binary exits non-zero and mentions
// FOCI_SOCK when the environment variable is absent.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallNoSocket(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, `{"tool":"test","params":{}}`)
	cmd.Env = []string{} // no FOCI_SOCK
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "FOCI_SOCK") {
		t.Errorf("output = %q, want FOCI_SOCK error", string(out))
	}
}

// TestFociCallNoArgs verifies the binary prints a usage message and exits
// non-zero when invoked with no arguments.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallNoArgs(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "FOCI_SOCK=/tmp/test.sock")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "usage") {
		t.Errorf("output = %q, want usage message", string(out))
	}
}

// TestFociCallInvalidJSON verifies the binary rejects malformed JSON input
// with a clear error message and non-zero exit code.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallInvalidJSON(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, `{not valid}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK=/tmp/test.sock")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "invalid JSON") {
		t.Errorf("output = %q, want invalid JSON error", string(out))
	}
}

// TestFociCallOversizedResponse verifies that a response over the 1 MB cap
// produces a diagnostic naming the cause, the cap, the actual size, and the
// remedy -- not the raw Go internal ("bufio.Scanner: token too long") that
// names none of those. See #1933.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallOversizedResponse(t *testing.T) {
	bin := buildBinary(t)
	const overBy = 500 * 1024
	oversize := 1024*1024 + overBy // 1.5 MB, well over the 1 MB cap
	sockPath := startTestServer(t, func(req string) string {
		return strings.Repeat("x", oversize)
	})

	cmd := exec.Command(bin, `{"tool":"foci_todo","params":{"limit":500}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit code for an over-cap response, got output %q", out)
	}
	output := string(out)
	if strings.Contains(output, "bufio.Scanner") || strings.Contains(output, "token too long") {
		t.Errorf("output leaks the raw Go internal error: %q", output)
	}
	for _, want := range []string{"too large", "1048576", fmt.Sprintf("%d", oversize), "--limit"} {
		if !strings.Contains(output, want) {
			t.Errorf("output = %q, want it to contain %q", output, want)
		}
	}
}

// TestFociCallHelp verifies that -h, --help, and help all print a Usage:
// header and exit zero.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallHelp(t *testing.T) {
	bin := buildBinary(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(bin, arg)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", arg, err, out)
			}
			if !strings.Contains(string(out), "Usage:") {
				t.Errorf("output %q does not contain 'Usage:'", string(out))
			}
		})
	}
}

// TestFociCallVersion verifies that --version, -v, and version all print a
// line starting with "foci-call " and exit zero.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallVersion(t *testing.T) {
	bin := buildBinary(t)
	for _, arg := range []string{"--version", "-v", "version"} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(bin, arg)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", arg, err, out)
			}
			output := string(out)
			if !strings.HasPrefix(output, "foci-call ") {
				t.Errorf("output %q does not start with 'foci-call '", output)
			}
		})
	}
}

// TestFociCallForwardsOutputHints: the generated foci_* wrappers export
// FOCI_STDOUT_PIPED / FOCI_OUTPUT_FORMAT (#2048); foci-call forwards them to the
// gateway as a "hints" object on the request envelope. With neither set the
// request must go out byte-identical to argv.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestFociCallForwardsOutputHints(t *testing.T) {
	bin := buildBinary(t)
	const arg = `{"tool":"todo","params":{"action":"list"}}`
	for _, c := range []struct {
		name string
		env  []string
		want map[string]any // nil = request must equal arg exactly
	}{
		{"none", nil, nil},
		{"not piped", []string{"FOCI_STDOUT_PIPED=0", "FOCI_OUTPUT_FORMAT="}, nil},
		{"piped", []string{"FOCI_STDOUT_PIPED=1"}, map[string]any{"stdout_piped": true}},
		{"format only", []string{"FOCI_STDOUT_PIPED=0", "FOCI_OUTPUT_FORMAT=jsonl"}, map[string]any{"format": "jsonl"}},
		{"both", []string{"FOCI_STDOUT_PIPED=1", "FOCI_OUTPUT_FORMAT=md"}, map[string]any{"stdout_piped": true, "format": "md"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reqCh := make(chan string, 1)
			sockPath := startTestServer(t, func(req string) string {
				reqCh <- req
				return `{"result":"ok"}`
			})
			cmd := exec.Command(bin, arg)
			env := []string{"FOCI_SOCK=" + sockPath}
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "FOCI_STDOUT_PIPED=") && !strings.HasPrefix(kv, "FOCI_OUTPUT_FORMAT=") && !strings.HasPrefix(kv, "FOCI_SOCK=") {
					env = append(env, kv)
				}
			}
			cmd.Env = append(env, c.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("foci-call: %v\n%s", err, out)
			}
			req := <-reqCh
			if c.want == nil {
				if req != arg {
					t.Errorf("request = %s, want unchanged %s", req, arg)
				}
				return
			}
			var got struct {
				Tool   string          `json:"tool"`
				Params json.RawMessage `json:"params"`
				Hints  map[string]any  `json:"hints"`
			}
			if err := json.Unmarshal([]byte(req), &got); err != nil {
				t.Fatalf("request not JSON: %v: %s", err, req)
			}
			if got.Tool != "todo" || string(got.Params) != `{"action":"list"}` {
				t.Errorf("tool/params altered: %s", req)
			}
			if fmt.Sprint(got.Hints) != fmt.Sprint(c.want) {
				t.Errorf("hints = %v, want %v", got.Hints, c.want)
			}
		})
	}
}
