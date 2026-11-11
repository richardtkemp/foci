package tools

import (
	"strings"
	"testing"
	"time"
)

// TestTodoAddShellReadsStdinAsBody is the #3013 repro: `foci_todo add --title
// T <<EOF ... EOF` printed "Added #N" with exit 0 and filed a title-only item,
// because the add branch never read stdin. Ruling (Dick, 2026-11-11): read
// piped stdin as the body when no --body/--text is given and stdin is not a
// terminal. Piped stdin is therefore the same as --body, bare-word title
// included.
func TestTodoAddShellReadsStdinAsBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, invocation string
		title, text      any // nil = key absent
	}{
		{"heredoc with --title", "foci_todo add --title T --priority low --tag x <<'EOF'\nline one\nline two\nEOF", "T", "line one\nline two"},
		{"pipe with no text", "printf 'piped body' | foci_todo add", nil, "piped body"},
		{"bare words are the title, like --body", "printf 'piped body' | foci_todo add fix the thing", "fix the thing", "piped body"},
		{"create alias", "printf 'B' | foci_todo create --title T", "T", "B"},
		// Empty stdin (/dev/null, the usual non-interactive stdin) changes nothing.
		{"empty stdin keeps bare words as text", "foci_todo add some words </dev/null", nil, "some words"},
		{"empty stdin keeps a title-only add", "foci_todo add --title T </dev/null", "T", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, out, code := todoAddParams(t, c.invocation)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if p["title"] != c.title || p["text"] != c.text {
				t.Errorf("want title=%v text=%q, got %v\n%s", c.title, c.text, p, out)
			}
		})
	}
}

// TestTodoAddShellRejectsStdinBesideText: when the body already came as
// --text or --body, piped content would be dropped (bare words are not a body
// here: with piped stdin they become the title, above). That silent drop is
// what #3013 is about, so it fails loudly, as the generic StdinParam guard does.
func TestTodoAddShellRejectsStdinBesideText(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`--text A`, `--body A`, `--title T --body A`} {
		t.Run(args, func(t *testing.T) {
			t.Parallel()
			_, out, code := todoAddParams(t, "printf 'lost' | foci_todo add "+args)
			if code == 0 {
				t.Fatalf("expected a non-zero exit\n%s", out)
			}
			if !strings.Contains(out, "stdin") {
				t.Errorf("error does not mention stdin\n%s", out)
			}
			if strings.Contains(out, "FOCI_CALL_REACHED") {
				t.Errorf("a rejected add still reached foci-call\n%s", out)
			}
		})
	}
}

// TestTodoAddShellStdinReadIsBounded: a stdin that never closes (an fd a
// supervisor left open, a `tail -f`) must not hang the add. The read is
// bounded by FOCI_STDIN_WAIT and warns; with a --title the item is still filed
// with what had arrived. The fifo's writer is backgrounded with its own fds,
// so the script exits when foci_todo does (the TestAskStdinReadIsBounded shape).
func TestTodoAddShellStdinReadIsBounded(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	start := time.Now()
	p, out, code := todoAddParams(t, `export FOCI_STDIN_WAIT=0.5
__d=$(mktemp -d); mkfifo "$__d/f"
{ printf 'partial'; sleep 20; } > "$__d/f" 2>/dev/null &
foci_todo add --title T < "$__d/f"; __rc=$?
rm -rf "$__d"
exit $__rc`)
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("add took %v: the stdin read is not bounded", el)
	}
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "warning: stdin was still open") {
		t.Errorf("no cut-off warning\n%s", out)
	}
	if p["title"] != "T" || p["text"] != "partial" {
		t.Errorf("want title=T text=partial, got %v", p)
	}
}
