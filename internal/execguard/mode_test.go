package execguard

import (
	"strings"
	"testing"
)

// selfEnv models the daemon binary at /opt/foci/bin/foci-gw, with the binary's
// directory writable or not.
func selfEnv(selfWritable bool) Env {
	var writable []string
	if selfWritable {
		writable = []string{"/opt/foci/bin"}
	}
	return testEnv([]string{"/opt/foci/bin", "/usr/bin"}, writable, []string{"/opt/foci/bin/foci-gw"})
}

const self = "/opt/foci/bin/foci-gw"

func TestResolve_AutoTurnsOffWhenFociCanReplaceItself(t *testing.T) {
	for _, mode := range []string{ModeAuto, ""} {
		on, why := Resolve(mode, self, selfEnv(true))
		if on {
			t.Fatalf("mode %q, writable self: enforced, want off", mode)
		}
		if !strings.Contains(why, self) {
			t.Errorf("mode %q: reason %q does not name the binary", mode, why)
		}
	}
}

func TestResolve_AutoKeepsCheckWhenSelfIsProtected(t *testing.T) {
	if on, _ := Resolve(ModeAuto, self, selfEnv(false)); !on {
		t.Fatal("auto, read-only self: off, want enforced")
	}
}

func TestResolve_AutoUnknownSelfKeepsCheck(t *testing.T) {
	if on, _ := Resolve(ModeAuto, "", selfEnv(true)); !on {
		t.Fatal("auto, unknown self: off, want enforced (fail closed)")
	}
}

func TestResolve_ExplicitModesOverrideThePremise(t *testing.T) {
	if on, _ := Resolve(ModeOn, self, selfEnv(true)); !on {
		t.Error("on, writable self: off, want enforced")
	}
	if on, why := Resolve(ModeOff, self, selfEnv(false)); on || why == "" {
		t.Errorf("off, read-only self: enforced=%v why=%q, want off with a reason", on, why)
	}
}

// An Env already marked Off must not make Resolve's own premise check vacuous.
func TestResolve_IgnoresIncomingOff(t *testing.T) {
	env := selfEnv(false)
	env.Off = true
	if on, _ := Resolve(ModeAuto, self, env); !on {
		t.Fatal("auto with env.Off set: off, want the premise judged afresh")
	}
}

func TestOffEnvJudgesNothing(t *testing.T) {
	env := shimmedPath()
	if sub, _, _ := Substitutable("sqlite3", env); !sub {
		t.Fatal("control: sqlite3 in writable ~/.local/bin should be substitutable")
	}
	env.Off = true
	if sub, _, _ := Substitutable("sqlite3", env); sub {
		t.Error("Off env: sqlite3 still substitutable")
	}
	if UnresolvedBareName("no-such-cmd", env) {
		t.Error("Off env: unresolved bare name still reported")
	}
}

func TestLiveFollowsSetEnforced(t *testing.T) {
	t.Cleanup(func() { SetEnforced(true) })
	if Live().Off {
		t.Fatal("default Live() is Off, want enforced")
	}
	SetEnforced(false)
	if !Live().Off {
		t.Error("after SetEnforced(false), Live().Off = false")
	}
}
