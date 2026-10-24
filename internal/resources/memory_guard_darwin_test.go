//go:build darwin

package resources

import (
	"os"
	"os/exec"
	"testing"

	"foci/internal/procinfo"
)

func TestLargestOwned(t *testing.T) {
	procs := []procinfo.Proc{
		{PID: 1, PPID: 0, UID: 0, RSSKB: 10, Comm: "launchd"},
		{PID: 10, PPID: 1, UID: 501, RSSKB: 900, Comm: "foci-gw"},
		{PID: 11, PPID: 10, UID: 501, RSSKB: 500, Comm: "node"},
		{PID: 12, PPID: 10, UID: 0, RSSKB: 9000, Comm: "root-thing"},
		{PID: 13, PPID: 11, UID: 501, RSSKB: 700, Comm: "claude"},
		{PID: 20, PPID: 1, UID: 501, RSSKB: 20000, Comm: "firefox"},
		{PID: 21, PPID: 1, UID: 501, RSSKB: 19000, Comm: "golangci-lint"},
	}
	pid, comm, rss, err := largestOwned(procs, 501, 10)
	if err != nil || pid != 13 || comm != "claude" || rss != 700 {
		t.Errorf("largestOwned = (%d, %q, %d, %v), want (13, claude, 700, nil): only foci-gw's own descendants of the uid, never the user's other apps", pid, comm, rss, err)
	}
	if _, _, _, err := largestOwned(procs[:2], 501, 10); err == nil {
		t.Error("largestOwned with no descendants: want error")
	}
}

func TestDarwinPressure(t *testing.T) {
	for _, c := range []struct {
		level uint32
		want  float64
	}{{1, 0}, {2, 5}, {4, 100}, {7, 100}} {
		if got := darwinPressure(c.level); got != c.want {
			t.Errorf("darwinPressure(%d) = %v, want %v", c.level, got, c.want)
		}
	}
	// At the default threshold (10) only critical acts: warning is routine
	// compression on a Mac, not a stall.
	if darwinPressure(2) >= 10 || darwinPressure(4) < 10 {
		t.Error("default threshold 10 must sit between warning and critical")
	}
}

// The live sources must work on a real Mac: these are what the guard reads.
func TestDarwinLiveSources(t *testing.T) {
	total, err := platformMemTotal()
	if err != nil || total <= 0 {
		t.Fatalf("platformMemTotal = (%d, %v), want > 0", total, err)
	}
	user, err := platformUserRSS(os.Getuid())
	if err != nil || user <= 0 {
		t.Fatalf("platformUserRSS = (%d, %v), want > 0", user, err)
	}
	if user > total {
		t.Errorf("user RSS %d kB > total RAM %d kB: unit mismatch", user, total)
	}
	if _, err := platformPressure(); err != nil {
		t.Errorf("platformPressure: %v", err)
	}
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	pid, _, _, err := platformFindLargest(os.Getuid(), os.Getpid())
	if err != nil || pid != child.Process.Pid {
		t.Errorf("platformFindLargest = (%d, %v), want our only child %d", pid, err, child.Process.Pid)
	}
}
