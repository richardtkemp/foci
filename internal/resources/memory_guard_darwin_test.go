//go:build darwin

package resources

import (
	"os"
	"testing"

	"foci/internal/procinfo"
)

func TestLargestOwned(t *testing.T) {
	procs := []procinfo.Proc{
		{PID: 10, UID: 501, RSSKB: 900, Comm: "self"},
		{PID: 11, UID: 501, RSSKB: 500, Comm: "node"},
		{PID: 12, UID: 0, RSSKB: 9000, Comm: "root-thing"},
		{PID: 13, UID: 501, RSSKB: 700, Comm: "claude"},
	}
	pid, comm, rss, err := largestOwned(procs, 501, 10)
	if err != nil || pid != 13 || comm != "claude" || rss != 700 {
		t.Errorf("largestOwned = (%d, %q, %d, %v), want (13, claude, 700, nil): self and other uids excluded", pid, comm, rss, err)
	}
	if _, _, _, err := largestOwned(procs[:1], 501, 10); err == nil {
		t.Error("largestOwned with only self: want error")
	}
}

func TestDarwinPressure(t *testing.T) {
	for _, c := range []struct {
		level uint32
		want  float64
	}{{1, 0}, {2, 50}, {4, 100}, {7, 100}} {
		if got := darwinPressure(c.level); got != c.want {
			t.Errorf("darwinPressure(%d) = %v, want %v", c.level, got, c.want)
		}
	}
	// At the default threshold (10) normal must not act and warning must.
	if darwinPressure(1) >= 10 || darwinPressure(2) < 10 {
		t.Error("default threshold 10 must sit between normal and warning")
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
	if _, _, _, err := platformFindLargest(os.Getuid(), os.Getpid()); err != nil {
		t.Errorf("platformFindLargest: %v", err)
	}
}
