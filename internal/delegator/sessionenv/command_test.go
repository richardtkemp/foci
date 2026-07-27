package sessionenv

import (
	"os/exec"
	"strings"
	"testing"
)

var testEntry = Entry{
	SessionKey: "agent/c42",
	FociSock:   "/tmp/foci/exec-agent-c42-1-1.sock",
	BashEnv:    "/tmp/foci/exec-agent-c42-1-1-funcs.sh",
}

// The unwrap is the only thing keeping foci's command display and its
// auto-approve matching honest once the hook starts rewriting, so it has to
// return the original byte-for-byte — including the shell metacharacters that
// make naive prefix-stripping wrong.
func TestWrapUnwrap_RoundTrip(t *testing.T) {
	cases := []string{
		"echo hi",
		`echo "it's here" && ls | wc -l`,
		`git commit -m 'fix: don'"'"'t break'`,
		"printf '%s\\n' a b > /tmp/x; cat /tmp/x",
		"bash -c 'echo nested'",
		"env FOO=1 echo hi",
		"echo 'trailing space '",
	}
	for _, orig := range cases {
		wrapped := WrapCommand(orig, testEntry)
		got, ok := UnwrapCommand(wrapped)
		if !ok {
			t.Errorf("UnwrapCommand(%q) not recognised (wrapped=%q)", orig, wrapped)
			continue
		}
		if got != orig {
			t.Errorf("round-trip mismatch:\n orig = %q\n got  = %q\n wrap = %q", orig, got, wrapped)
		}
	}
}

func TestWrapCommand_NoOpWithoutVars(t *testing.T) {
	if got := WrapCommand("echo hi", Entry{}); got != "echo hi" {
		t.Errorf("zero entry must not wrap, got %q", got)
	}
	if got := WrapCommand("", testEntry); got != "" {
		t.Errorf("empty command must not wrap, got %q", got)
	}
}

func TestUnwrapCommand_LeavesForeignCommandsAlone(t *testing.T) {
	// An agent's own `env ... bash -c ...` must survive untouched: no foci
	// variable, so it isn't ours.
	foreign := "env FOO=1 BAR=2 bash -c 'echo hi'"
	if got, ok := UnwrapCommand(foreign); ok || got != foreign {
		t.Errorf("foreign command claimed: got=%q ok=%v", got, ok)
	}
	for _, s := range []string{"echo hi", "env", "env FOCI_SOCK=/x sh -c 'y'", "echo 'unterminated"} {
		if got, ok := UnwrapCommand(s); ok || got != s {
			t.Errorf("UnwrapCommand(%q) = %q, %v; want unchanged, false", s, got, ok)
		}
	}
}

// Codex reports the argv it built, not the tool input: `/bin/bash -lc "<input>"`.
func TestUnwrapDisplayCommand(t *testing.T) {
	orig := `echo "it's $HOME" && ls`
	display := "/bin/bash -lc " + ShellQuote(WrapCommand(orig, testEntry))

	got := UnwrapDisplayCommand(display)
	want := "/bin/bash -lc " + ShellQuote(orig)
	if got != want {
		t.Errorf("UnwrapDisplayCommand:\n got  = %q\n want = %q", got, want)
	}
}

func TestUnwrapDisplayCommand_PassesThroughUnwrapped(t *testing.T) {
	for _, s := range []string{
		`/bin/bash -lc 'echo hi'`,
		`/bin/bash -lc "grep -r foo ."`,
		"short",
	} {
		if got := UnwrapDisplayCommand(s); got != s {
			t.Errorf("UnwrapDisplayCommand(%q) = %q, want unchanged", s, got)
		}
	}
}

// The wrap only matters if the shell agrees with it. These assert the two
// properties an agent would notice if the wrap were wrong: the exit status is
// the original command's, and stdout/stderr stay separate.
func TestWrapCommand_ExecutesWithEnvAndPreservesExitStatus(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	e := Entry{SessionKey: "agent/cLIVE", FociSock: "/tmp/sock-live"}

	wrapped := WrapCommand(`echo "key=$FOCI_SESSION_KEY sock=$FOCI_SOCK"; echo oops >&2; exit 7`, e)
	cmd := exec.Command("/bin/bash", "-lc", wrapped)
	cmd.Env = append(cmd.Environ(), "FOCI_SESSION_KEY=WRONG", "FOCI_SOCK=WRONG")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()

	var exit *exec.ExitError
	if err == nil {
		t.Fatal("expected non-zero exit")
	} else if !asExitError(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("exit status not preserved: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "key=agent/cLIVE sock=/tmp/sock-live" {
		t.Errorf("stdout = %q; the wrap must override the inherited (wrong) values", got)
	}
	if strings.TrimSpace(stderr.String()) != "oops" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "oops")
	}
}

func TestWrapCommand_QuotingSurvivesRealShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// Single quotes, a pipeline and && in one original — the shapes that break
	// a naive concatenation.
	wrapped := WrapCommand(`printf '%s\n' "it's" | tr -d "\n" && printf ok`, testEntry)
	out, err := exec.Command("/bin/bash", "-lc", wrapped).Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != "it'sok" {
		t.Errorf("output = %q, want %q", out, "it'sok")
	}
}

func asExitError(err error, dst **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*dst = e
	}
	return ok
}

// Captured verbatim from codex-cli 0.145.0 item/started, with the hook
// installed: codex re-quotes the argv it built, mixing double- and
// single-quoted runs inside one word. A hand-rolled unwrap that only handled
// its own quoting style would silently pass every other test here and fail on
// this, which is the only shape that actually reaches production.
func TestUnwrapDisplayCommand_LiveCodexFixture(t *testing.T) {
	const live = `/bin/bash -lc "env FOCI_SESSION_KEY=agent/cAAA FOCI_SOCK=/tmp/sock-agent/cAAA bash -c 'echo \"MARKER-A="'$FOCI_SESSION_KEY|$FOCI_SOCK"'"'"`
	const original = `echo "MARKER-A=$FOCI_SESSION_KEY|$FOCI_SOCK"`

	got := UnwrapDisplayCommand(live)
	want := "/bin/bash -lc " + ShellQuote(original)
	if got != want {
		t.Errorf("\n got  = %s\n want = %s", got, want)
	}
}
