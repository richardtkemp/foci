//go:build unix

package procx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

const rlimitHelperEnv = "FOCI_PROCX_RLIMIT_HELPER"

// childSoftLimit runs this test binary with a soft open-file limit of 256 and
// the hard limit left alone (launchd's default for foci-gw: 256 soft,
// unlimited hard), as TestRlimitHelper. That copy
// optionally calls PassOpenFileLimitToChildren, then spawns `sh -c 'ulimit -n'`
// through Spawn and prints what its child got.
func childSoftLimit(t *testing.T, pass bool) string {
	t.Helper()
	mode := "skip"
	if pass {
		mode = "pass"
	}
	cmd := Spawn(context.Background(), Trusted, "/bin/sh", "-c",
		`ulimit -S -n 256 && exec "$0" -test.run='^TestRlimitHelper$'`, os.Args[0])
	cmd.Env = append(cmd.Env, rlimitHelperEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper (%s): %v\n%s", mode, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "CHILD_NOFILE="); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("helper (%s) printed no CHILD_NOFILE line:\n%s", mode, out)
	return ""
}

// TestPassOpenFileLimitToChildren is the 2026-11-05 regression: foci-gw ran
// with a soft limit of ~184k but the codex app-server it spawned got 256 —
// the Go runtime restores the original soft limit in every child — and ran
// out of file descriptors. The control (no call) shows the runtime's restore
// is real in this environment, so the positive case is not passing by luck.
func TestPassOpenFileLimitToChildren(t *testing.T) {
	if got := childSoftLimit(t, false); got != "256" {
		t.Fatalf("control: without the call a child got %q, want 256 (the runtime's restore); the test cannot show the fix here", got)
	}
	got := childSoftLimit(t, true)
	t.Logf("child soft open-file limit with the call: %s", got)
	if got != "unlimited" {
		n, err := strconv.ParseUint(got, 10, 64)
		if err != nil {
			t.Fatalf("child soft limit %q is not a number", got)
		}
		if n <= 256 {
			t.Fatalf("with PassOpenFileLimitToChildren a child still got %d open files, want foci's raised limit", n)
		}
	}
}

// TestRlimitHelper is the helper process for TestPassOpenFileLimitToChildren;
// it does nothing in a normal run.
func TestRlimitHelper(t *testing.T) {
	mode := os.Getenv(rlimitHelperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	if mode == "pass" {
		if _, err := PassOpenFileLimitToChildren(); err != nil {
			t.Fatalf("PassOpenFileLimitToChildren: %v", err)
		}
	}
	out, err := Spawn(context.Background(), Trusted, "/bin/sh", "-c", "ulimit -S -n").CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	os.Stdout.WriteString("CHILD_NOFILE=" + strings.TrimSpace(string(out)) + "\n")
}
