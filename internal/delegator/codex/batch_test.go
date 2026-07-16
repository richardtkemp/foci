package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// stubCodex writes a fake `codex` that records argv+stdin, writes the reply
// to the --output-last-message file, and prints progress noise to stdout
// (which RunBatch must ignore in favour of the file).
func stubCodex(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.txt")
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
{ printf 'ARGS:%s\n' "$*"; printf 'STDIN:%s\n' "$(cat)"; } > ` + capture + `
printf 'tokens used\n1234\n'
printf '  final reply from file  ' > "$out"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, capture
}

func TestRunBatch(t *testing.T) {
	t.Parallel()

	stub, capture := stubCodex(t)
	be, err := newFromConfig(map[string]any{"binary": stub})
	if err != nil {
		t.Fatal(err)
	}
	b := be.(*Backend)

	wd := t.TempDir()
	got, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:       "extract the rules",
		SystemPrompt: "CHARACTER \"FILES\"\nLINE TWO",
		Model:        "gpt-5.3-codex",
		WorkDir:      wd,
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "final reply from file" {
		t.Errorf("result = %q, want trimmed --output-last-message content", got)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	cap := string(data)
	for _, want := range []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--output-last-message",
		`instructions="CHARACTER \"FILES\"\nLINE TWO"`, // TOML-escaped
		"-m gpt-5.3-codex",
		"-C " + wd,
		"STDIN:extract the rules",
	} {
		if !strings.Contains(cap, want) {
			t.Errorf("capture missing %q:\n%s", want, cap)
		}
	}
}

func TestRunBatch_Defaults(t *testing.T) {
	t.Parallel()

	stub, capture := stubCodex(t)
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)

	if _, err := b.RunBatch(context.Background(), delegator.BatchRequest{Prompt: "p"}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	data, _ := os.ReadFile(capture)
	cap := string(data)
	if strings.Contains(cap, "instructions=") {
		t.Errorf("empty SystemPrompt must omit instructions override:\n%s", cap)
	}
	if strings.Contains(cap, " -m ") {
		t.Errorf("empty Model must omit -m (codex config default):\n%s", cap)
	}
}

func TestTomlBasicString(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{"a\nb", `"a\nb"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"tab\there", `"tab\there"`},
		{"bell\x07", `"bell\u0007"`},
	}
	for _, c := range cases {
		if got := tomlBasicString(c.in); got != c.want {
			t.Errorf("tomlBasicString(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
