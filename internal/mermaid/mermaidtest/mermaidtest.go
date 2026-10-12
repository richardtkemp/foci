// Package mermaidtest provides a stand-in for mermaid-cli (mmdc) for tests of
// the mermaid renderer and its callers.
package mermaidtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/procx"
)

// FakeCmd writes a stand-in for mermaid-cli: it writes a fake PNG (ending
// "bg=<the -b background>") to the -o path and logs each run's theme (one line
// per call) to runs. A source containing BAD fails like a real parse error;
// NOBROWSER like a browser that will not start; SLOW takes 300ms; SLEEP hangs.
func FakeCmd(t testing.TB) (cmd, runs string) {
	t.Helper()
	dir := t.TempDir()
	runs = filepath.Join(dir, "runs")
	cmd = filepath.Join(dir, "mmdc")
	script := `#!/bin/sh
in=""; out=""; theme=""; bg=""
while [ $# -gt 0 ]; do
  case "$1" in
    -i) in="$2"; shift ;;
    -o) out="$2"; shift ;;
    -t) theme="$2"; shift ;;
    -b) bg="$2"; shift ;;
  esac
  shift
done
echo "$theme" >> "` + runs + `"
if grep -q BAD "$in"; then
  printf 'Generating single mermaid chart\n\nError: Parse error on line 2:\nA-->\n----^\nExpecting NODE_STRING, got EOF\nParser.parseError (https://mermaid-cli-intercept.invalid/x.mjs:1:2)\n    at #evaluate (file:///x.js:1:2)\n' >&2
  exit 1
fi
if grep -q NOBROWSER "$in"; then
  printf 'Error: Failed to launch the browser process!\nThe setuid sandbox is not running as root.\n' >&2
  exit 1
fi
if grep -q SLEEP "$in"; then sleep 5; fi
if grep -q SLOW "$in"; then sleep 0.3; fi
printf '\211PNG\r\n\032\nfake bg=%s' "$bg" > "$out"
`
	writeExecutable(t, cmd, script)
	return cmd, runs
}

// writeExecutable creates an executable at path holding body, written by a
// child process so the test process never holds a write fd on it. Writing it
// in-process (os.WriteFile) races sibling parallel tests' fork/exec: a child
// forked while the fd is open inherits it until its own exec, and an exec of
// path meanwhile fails with ETXTBSY "text file busy" (golang/go#22315; #2354).
func writeExecutable(t testing.TB, path, body string) {
	t.Helper()
	cmd := procx.Spawn(context.Background(), procx.Trusted, "sh", "-c", `cat > "$1" && chmod 755 "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write %s: %v: %s", path, err, out)
	}
}

func RunCount(t testing.TB, runs string) int {
	t.Helper()
	b, err := os.ReadFile(runs)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}
