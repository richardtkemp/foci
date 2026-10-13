package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/execguard"
	"foci/internal/log"
)

// The invariant that defines #1914 as done: no directory on the daemon's
// trusted PATH is writable by the foci process. It must FAIL loudly on an
// unexplained writable directory — that is the whole alarm.
func TestCheckTrustedPathFooting_UnexplainedWritableIsAViolation(t *testing.T) {
	t.Parallel()
	writable := map[string]bool{"/opt/agentbin": true}
	findings := checkTrustedPathFooting(
		[]string{"/usr/bin", "/opt/agentbin"},
		"/home/foci",
		func(p string) bool { return writable[p] },
	)

	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}
	if findings[0].Writable {
		t.Error("/usr/bin must not be reported writable")
	}
	if !findings[1].Writable || findings[1].Exception != "" {
		t.Fatalf("/opt/agentbin must be reported writable with NO exception, got %+v", findings[1])
	}
}

// ~/.local/bin IS writable and that is correct, because Claude Code's updater
// must be able to rewrite the `claude` foci execs by bare name. The exception
// must be recognised through ~ expansion and must carry its reason, so the
// report explains itself rather than merely staying quiet.
func TestCheckTrustedPathFooting_NamedExceptionIsExplained(t *testing.T) {
	t.Parallel()
	home := "/home/foci"
	localBin := filepath.Join(home, ".local", "bin")

	findings := checkTrustedPathFooting(
		[]string{localBin},
		home,
		func(string) bool { return true },
	)

	if len(findings) != 1 || !findings[0].Writable {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if findings[0].Exception == "" {
		t.Fatalf("%s must be a NAMED exception, not a violation — the ~ in trustedPathExceptions has to expand against home", localBin)
	}
}

// An exception entry must not launder a DIFFERENT directory. If ~ expansion
// silently no-ops (empty home), the allowlist must not match the literal "~/…"
// against a real path.
func TestCheckTrustedPathFooting_ExceptionDoesNotMatchOtherDirs(t *testing.T) {
	t.Parallel()
	findings := checkTrustedPathFooting(
		[]string{"/home/someoneelse/.local/bin"},
		"/home/foci",
		func(string) bool { return true },
	)
	if findings[0].Exception != "" {
		t.Fatal("the ~/.local/bin exception must not cover another user's .local/bin")
	}
}

// The report is the only alarm for a misclassification, so its PATH diff has
// to be right in both directions. Operator-only dirs mean agents can reach
// something foci cannot; trusted-only dirs mean the reverse, and a bare name
// foci hands an agent will not resolve for it.
func TestPathDelta_BothDirections(t *testing.T) {
	t.Parallel()
	trusted := []string{"/usr/bin", "/home/foci/bin"}
	operator := []string{"/home/foci/scripts", "/usr/bin", "/home/foci/bin"}

	if got := pathDelta(operator, trusted); !slices.Equal(got, []string{"/home/foci/scripts"}) {
		t.Errorf("operator-only = %v, want [/home/foci/scripts]", got)
	}
	if got := pathDelta(trusted, operator); len(got) != 0 {
		t.Errorf("trusted-only = %v, want empty", got)
	}
	if got := pathDelta(trusted, []string{"/usr/bin"}); !slices.Equal(got, []string{"/home/foci/bin"}) {
		t.Errorf("trusted-only = %v, want [/home/foci/bin]", got)
	}
}

// The operator's dotfiles routinely prepend a directory that is already on
// PATH (~/.shellcommon does exactly this), and the live daemon's PATH has
// ~/.local/bin and ~/bin twice. A repeated entry in the report is noise that
// hides the real diff, and it would also be double-counted by the footing
// check.
func TestDedupePreservesFirstOccurrenceOrder(t *testing.T) {
	t.Parallel()
	got := dedupe([]string{"/a", "/b", "/a", "", "/c", "/b"})
	if !slices.Equal(got, []string{"/a", "/b", "/c"}) {
		t.Fatalf("dedupe = %v, want [/a /b /c]", got)
	}
}

// "Nothing differs" must be STATED, not shown as a blank the reader has to
// interpret — the report's value is that it is always legible.
func TestFormatListNamesTheEmptyCase(t *testing.T) {
	t.Parallel()
	if got := formatList(nil); got != "(none)" {
		t.Errorf("formatList(nil) = %q, want %q", got, "(none)")
	}
	if got := formatList([]string{"/a", "/b"}); got != "/a /b" {
		t.Errorf("formatList = %q", got)
	}
}

// The startup operator-env report opens with the #1897 half: one WARN per
// built-in group command the match-time veto will refuse, carrying the
// group, the command, and execguard's own reason and path, before the
// population-split lines of the same report (#1914). This is the seam
// main() calls at startup — without it a vetoed built-in falls through to
// an approval prompt with no line anywhere saying so. yq, not mds/mdq, so
// the line is distinguishable from every sibling test's in the shared tap.
func TestReportOperatorEnv_BuiltinVetoRidesTheStartupReport(t *testing.T) {
	t.Parallel()
	// INFO, not WARN: the veto WARNs must be judged alongside the INFO
	// population-split lines of the same report.
	c := captureLog(t, log.INFO)
	dir := t.TempDir()
	env := guardEnvFor([]string{dir}, []string{dir + "/yq"}, []string{dir + "/yq"})
	// One live agent is enough: read-only defaults on, safe-write defaults
	// off. Delegated, because the built-in groups reach a match-time veto
	// only through buildAutoApproveRules (delegated agents only).
	cfg := &config.Config{Agents: []config.AgentConfig{{ID: "main", Backend: "claude-code"}}}

	reportOperatorEnv(cfg, env, "", t.TempDir(), func(string) bool { return false })

	lines := c.Lines("built-in read-only group", `command "yq"`)
	if len(lines) != 1 {
		t.Fatalf("got %d built-in read-only WARN(s) for yq, want 1:\n%s", len(lines), c.String())
	}
	if lines[0].Level != log.WARN || lines[0].Component != "config" {
		t.Errorf("line = %s, want a config-component WARN", lines[0].String())
	}
	line := lines[0].Msg
	if !strings.Contains(line, "approval prompt") {
		t.Errorf("WARN must say the command falls through to an approval prompt: %s", line)
	}
	if _, path, reason := execguard.Substitutable("yq", env); !strings.Contains(line, reason) || !strings.Contains(line, path) {
		t.Errorf("WARN must carry execguard's own reason and path, got %q (want reason %q, path %q)", line, reason, path)
	}
	// The population half of the same report ran too: the #1897 WARNs ride
	// the startup report, they do not replace it.
	if got := c.Lines("spawn populations:"); len(got) != 1 {
		t.Errorf("got %d spawn-populations line(s), want 1:\n%s", len(got), c.String())
	}
}
