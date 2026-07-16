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
func stubClaude(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.txt")
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n{ echo \"ARGS:$*\"; echo \"STDIN:$(cat)\"; } > " + capture + "\nprintf '  batch response  \\n'\n"
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
		"--system-prompt CHARACTER FILES HERE",
		"STDIN:extract the rules",
	} {
		if !strings.Contains(cap, want) {
			t.Errorf("capture missing %q:\n%s", want, cap)
		}
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
