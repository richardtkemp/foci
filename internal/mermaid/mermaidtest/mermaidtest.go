// Package mermaidtest provides a stand-in for mermaid-cli (mmdc) for tests of
// the mermaid renderer and its callers.
package mermaidtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, runs
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
