package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// stubClaude writes a fake `claude` that records its argv and stdin, then
// prints a canned response. Returns the stub path and the capture-file path.
//
// It also dumps the content of any file named by --system-prompt-file WHILE
// IT STILL RUNS: RunBatch defers deleting that temp file until it returns,
// so the file is gone by the time the test process could read it back — the
// stub must capture it during its own execution instead (#1963).
func stubClaude(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.txt")
	stub := filepath.Join(dir, "claude")
	script := `#!/bin/sh
{
  echo "ARGS:$*"
  echo "STDIN:$(cat)"
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--system-prompt-file" ]; then
      echo "SYSPROMPTFILE:$a"
      printf 'SYSPROMPTCONTENT:'
      cat "$a"
      echo
    fi
    prev="$a"
  done
} > ` + capture + `
printf '  batch response  \n'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, capture
}

func TestRunBatch(t *testing.T) {
	t.Parallel()

	stub, capture := stubClaude(t)
	be, err := newFromConfig(map[string]any{"binary": stub})
	if err != nil {
		t.Fatal(err)
	}
	b := be.(*Backend)

	got, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:       "extract the rules",
		SystemPrompt: "CHARACTER FILES HERE",
		WorkDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "batch response" {
		t.Errorf("result = %q, want trimmed canned response", got)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	cap := string(data)
	for _, want := range []string{
		"--print",
		"--dangerously-skip-permissions",
		"--no-session-persistence",
		"--model sonnet", // empty Model → cheap batch default
		"--system-prompt-file",
		"STDIN:extract the rules",
	} {
		if !strings.Contains(cap, want) {
			t.Errorf("capture missing %q:\n%s", want, cap)
		}
	}
	if strings.Contains(cap, "--system-prompt ") {
		t.Errorf("SystemPrompt must be passed via --system-prompt-file, never a literal --system-prompt argv element (#1963):\n%s", cap)
	}

	// The system prompt itself must have reached claude via the file the
	// flag points at, not been dropped.
	if !strings.Contains(cap, "SYSPROMPTCONTENT:CHARACTER FILES HERE") {
		t.Errorf("system prompt file content missing/wrong:\n%s", cap)
	}
}

func TestRunBatch_ModelOverrideAndNoSystemPrompt(t *testing.T) {
	t.Parallel()

	stub, capture := stubClaude(t)
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)

	if _, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt: "p",
		Model:  "haiku",
	}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	data, _ := os.ReadFile(capture)
	cap := string(data)
	if !strings.Contains(cap, "--model haiku") {
		t.Errorf("model override missing:\n%s", cap)
	}
	if strings.Contains(cap, "--system-prompt") {
		t.Errorf("empty SystemPrompt must omit --system-prompt (backend default):\n%s", cap)
	}
}

func TestRunBatch_ErrorIncludesStderr(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'auth expired' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)

	_, err := b.RunBatch(context.Background(), delegator.BatchRequest{Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "auth expired") {
		t.Errorf("error should carry stderr, got: %v", err)
	}
}

// TestRunBatch_ErrorIncludesStdout is the 2026-09-19 nudge-extraction failure
// in miniature: `claude --print` refuses on STDOUT (usage limit reached) and
// exits non-zero with stderr EMPTY, so a stderr-only error message would carry
// no reason at all.
func TestRunBatch_ErrorIncludesStdout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho 'Claude AI usage limit reached|1789791000'\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)

	_, err := b.RunBatch(context.Background(), delegator.BatchRequest{Prompt: "p"})
	if err == nil {
		t.Fatal("expected error from exit 1")
	}
	if !strings.Contains(err.Error(), "usage limit reached") {
		t.Errorf("error must carry the stdout refusal, got: %v", err)
	}
}

// TestRunBatch_LargeSystemPromptDoesNotHitArgvLimit is #1963: a composed
// character system prompt (coach's is 131,575 chars and growing) can exceed
// Linux's MAX_ARG_STRLEN — a single execve() argv element is capped at 32
// pages (131072 bytes), independent of the total-argv limit and NOT raised
// by ulimit. Passing SystemPrompt as a literal --system-prompt argv element
// hits that cliff and fork/exec fails with E2BIG before claude ever starts
// ("argument list too long") — no model or API problem, so retries can't
// help. The fix is to stop passing it as an argv element at all; a prompt
// safely over the cap must still succeed.
func TestRunBatch_LargeSystemPromptDoesNotHitArgvLimit(t *testing.T) {
	t.Parallel()

	stub, capture := stubClaude(t)
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)

	// One byte over MAX_ARG_STRLEN (131072) so this fails today (proves the
	// bug) and must succeed after the fix.
	big := strings.Repeat("A", 131073)

	got, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:       "extract the rules",
		SystemPrompt: big,
		WorkDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RunBatch with oversized SystemPrompt: %v", err)
	}
	if got != "batch response" {
		t.Errorf("result = %q, want trimmed canned response", got)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	// Only the ARGS: line reflects literal argv — SYSPROMPTCONTENT below it
	// legitimately carries the big prompt because the stub read it back out
	// of the file, which is the whole point of the fix.
	argsLine, _, _ := strings.Cut(string(data), "\n")
	if strings.Contains(argsLine, big) {
		t.Errorf("oversized SystemPrompt must not be passed as a literal argv element:\n%s", argsLine)
	}
	if !strings.Contains(string(data), "SYSPROMPTCONTENT:"+big) {
		t.Errorf("oversized SystemPrompt must still reach claude via the file")
	}
}
