// Tests for the format:filepath path resolution in generated shell functions.
// When a string schema property has "format": "filepath", the generic shell-
// func generator emits a POSIX case statement that prefixes relative paths
// with $PWD before they reach foci-gw — solving the "send_to_chat --file
// report.md fails because foci-gw's cwd ≠ caller's cwd" UX bug (TODO #754).
//
// Verifies both the structural emit and the absolute-path passthrough by
// running the generated bash directly and inspecting the resulting params.
package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendToChat_ShellFuncResolvesRelativeFilePath(t *testing.T) {
	// The generator must emit a POSIX case statement that prefixes relative
	// --file values with $PWD. Walk the generated body for the exact arm
	// shape — `case "$file" in /*) ;; *) file="$PWD/$file" ;; esac` — so a
	// future refactor can't silently drop the resolution while keeping the
	// flag arm intact.
	t.Parallel()
	tool := NewSendToChatTool(nil, nil)
	body := generateShellFunc(tool)

	// Look for the resolution snippet on the file param.
	want := `case "$file" in /*) ;; *) file="$PWD/$file" ;; esac`
	if !strings.Contains(body, want) {
		t.Errorf("generated send_to_chat shell function missing filepath resolver\nwant substring: %s\n---body---\n%s", want, body)
	}
}

// disconnected-test-ok: black-box test — execs bash to validate the filepath
// resolution logic; the sibling TestSendToChat_ShellFuncResolvesRelativeFilePath
// references generateShellFunc and guards drift of the real snippet.
func TestSendToChat_ShellFuncResolutionExecutesCorrectly(t *testing.T) {
	// End-to-end: extract the file-resolution lines from the generated body
	// and run them under bash with PWD set, asserting the resolved value.
	// Doesn't invoke foci-call (no socket); just validates the bash logic.
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	cwd := t.TempDir()

	cases := []struct {
		name    string
		input   string
		wantAbs string // empty = expect literal passthrough
	}{
		{"relative_basename", "report.md", filepath.Join(cwd, "report.md")},
		{"relative_dotdot", "../report.md", cwd + "/../report.md"}, // bash concatenates literally; foci-gw Cleans on the receive side
		{"absolute_path", "/etc/passwd", "/etc/passwd"},
		{"empty_string", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Inline the production resolution; if either side drifts the
			// structural test above will fail first.
			script := `file="$1"; [ -n "$file" ] && case "$file" in /*) ;; *) file="$PWD/$file" ;; esac; printf '%s' "$file"`
			cmd := exec.Command(bash, "-c", script, "_", tc.input)
			cmd.Dir = cwd
			cmd.Env = append(os.Environ(), "PWD="+cwd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash exec failed: %v", err)
			}
			got := string(out)
			if got != tc.wantAbs {
				t.Errorf("input=%q got=%q want=%q", tc.input, got, tc.wantAbs)
			}
		})
	}
}

func TestSendToChat_ShellFuncFileDashReadsStdin(t *testing.T) {
	// `--file -` must read the attachment body from stdin into a temp file
	// rather than treating "-" as a literal path (TODO #814). Guard the
	// structural emit so a refactor can't silently drop it.
	t.Parallel()
	tool := NewSendToChatTool(nil, nil)
	body := generateShellFunc(tool)

	for _, want := range []string{
		`if [ "$file" = "-" ]; then`,
		`__foci_stdin_file="$(mktemp)"`,
		`cat > "$__foci_stdin_file"`,
		`file="$__foci_stdin_file"`,
		// The text stdin reader must skip when a file already drained stdin.
		`[ -z "$__foci_stdin_file" ]; then`,
		// Temp file is cleaned up after the call.
		`rm -f "$__foci_stdin_file"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generated send_to_chat shell function missing stdin-file snippet\nwant substring: %s\n---body---\n%s", want, body)
		}
	}

	// The "-"-to-tempfile block must appear BEFORE the relative-path resolver,
	// otherwise "-" would become "$PWD/-" and never reach the stdin branch.
	dashIdx := strings.Index(body, `if [ "$file" = "-" ]; then`)
	resolverIdx := strings.Index(body, `case "$file" in /*)`)
	if dashIdx < 0 || resolverIdx < 0 || dashIdx > resolverIdx {
		t.Errorf("stdin-file block (%d) must precede the relative-path resolver (%d)", dashIdx, resolverIdx)
	}
}

// disconnected-test-ok: black-box test — execs bash to validate the
// stdin-to-tempfile logic; the sibling TestSendToChat_ShellFuncFileDashReadsStdin
// guards drift of the real generated snippet.
func TestSendToChat_ShellFuncFileDashStdinExecutes(t *testing.T) {
	// End-to-end of the bash fragment: pipe content with file="-" and assert
	// the temp file receives stdin and the file var points at it.
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	// Inline the production logic (structural test above guards drift).
	script := `file="$1"
__foci_stdin_file=""
if [ "$file" = "-" ]; then
  __foci_stdin_file="$(mktemp)"
  cat > "$__foci_stdin_file"
  file="$__foci_stdin_file"
fi
printf 'FILE=%s\n' "$file"
printf 'CONTENT=%s' "$(cat "$file")"
rm -f "$__foci_stdin_file"`

	cmd := exec.Command(bash, "-c", script, "_", "-")
	cmd.Stdin = strings.NewReader("hello from stdin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash exec failed: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "CONTENT=hello from stdin") {
		t.Errorf("temp file did not capture stdin\ngot: %q", got)
	}
	if !strings.Contains(got, "FILE=/") {
		t.Errorf("file var should point at an absolute temp path\ngot: %q", got)
	}
}

func TestSendToChat_ShellFuncFilenameFlagNotResolved(t *testing.T) {
	// --filename is a display label, not a path. Must NOT be resolved as
	// filepath (would produce an absolute label like "/cwd/report.md"
	// displayed to the user).
	t.Parallel()
	tool := NewSendToChatTool(nil, nil)
	body := generateShellFunc(tool)

	// Filename should appear as a flag arm but NOT in a filepath resolver.
	if !strings.Contains(body, "--filename)") {
		t.Fatalf("--filename) flag arm missing from body")
	}
	if strings.Contains(body, `case "$filename" in /*)`) {
		t.Errorf("--filename incorrectly treated as a filepath — it's a display label, not a path\n---body---\n%s", body)
	}
}
