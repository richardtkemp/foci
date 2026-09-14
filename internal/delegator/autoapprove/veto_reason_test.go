package autoapprove

import (
	"strings"
	"testing"

	"foci/internal/execguard"
)

// writableShim is an Env in which exactly one command is substitutable.
func writableShim(dir, name string) execguard.Env {
	full := dir + "/" + name
	return execguard.Env{
		CanWrite:     func(p string) bool { return p == full },
		PathDirs:     []string{dir, "/usr/bin"},
		IsExecutable: func(p string) bool { return p == full },
		HomeDir:      "/home/foci",
	}
}

// #1906. A veto is a rule that MATCHED being overridden by the guard. Before
// this, that produced a bare false: the user was asked to approve a command
// sitting on their own allowlist, with nothing anywhere explaining why. The
// reason has to come back out so the prompt can say it.
func TestVetoReason_IsReportedWhenAMatchingRuleIsOverridden(t *testing.T) {
	withGuardEnv(t, writableShim("/home/foci/bin", "mdq"))
	rules := Compile([]string{"Bash:mdq *"})

	ok, reason := MatchWithEnv(rules, "Bash", bashInput(t, "mdq '# Heading' notes.md"), nil)
	if ok {
		t.Fatal("precondition: a writable executable must not be auto-approved")
	}
	if reason == "" {
		t.Fatal("a veto must explain itself — an unexplained prompt for an allowlisted command is the #1906 defect")
	}
	if !strings.Contains(reason, "/home/foci/bin/mdq") {
		t.Errorf("the reason must name the offending file so it can be fixed; got %q", reason)
	}
}

// Control, and the one that gives the reason its meaning: an ORDINARY denial —
// no rule matched at all — must report NOTHING. If every denial carried a
// reason, the field would say only "you were denied", which the user already
// knows from being asked.
func TestVetoReason_IsEmptyForAnOrdinaryNoRuleMatch(t *testing.T) {
	withGuardEnv(t, execguard.Env{
		CanWrite:     func(string) bool { return false },
		PathDirs:     []string{"/usr/bin"},
		IsExecutable: func(p string) bool { return p == "/usr/bin/curl" },
		HomeDir:      "/home/foci",
	})
	rules := Compile([]string{"Bash:mdq *"})

	ok, reason := MatchWithEnv(rules, "Bash", bashInput(t, "curl https://example.com"), nil)
	if ok {
		t.Fatal("precondition: curl matches no rule here")
	}
	if reason != "" {
		t.Errorf("an ordinary no-match must carry no veto reason; got %q", reason)
	}
}

// The reason for holding the veto note behind a POINTER shared by clone().
// Clones are made for subshells and compound statements, so a veto raised
// inside one is reached through a different varCtx than the caller's. Copying
// the note by value loses it: the denial still happens, but it goes back to
// being unexplained — the exact bug, restored silently for the commands most
// likely to be doing something interesting.
func TestVetoReason_SurvivesASubshellClone(t *testing.T) {
	withGuardEnv(t, writableShim("/home/foci/bin", "mdq"))
	rules := Compile([]string{"Bash:mdq *", "Bash:echo *"})

	ok, reason := MatchWithEnv(rules, "Bash", bashInput(t, "(mdq '# H' notes.md)"), nil)
	if ok {
		t.Fatal("precondition: the subshell's command is substitutable, so this must be denied")
	}
	if reason == "" {
		t.Fatal("a veto raised inside a subshell must still reach the caller — clone() shares the note by pointer for exactly this")
	}
}
