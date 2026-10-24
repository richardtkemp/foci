//go:build linux || darwin

package procinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// These run against the live OS on both supported platforms: the point of the
// package is that the same calls give the same kind of answer everywhere.

func TestMemTotalAndSelfRSS(t *testing.T) {
	total, err := MemTotalKB()
	if err != nil || total <= 0 {
		t.Fatalf("MemTotalKB = (%d, %v), want > 0", total, err)
	}
	rss, err := RSSKB(os.Getpid())
	if err != nil || rss <= 0 {
		t.Fatalf("RSSKB(self) = (%d, %v), want > 0", rss, err)
	}
	if rss > total {
		t.Errorf("self RSS %d kB > total %d kB: unit mismatch", rss, total)
	}
}

func TestRSSKBDeadPID(t *testing.T) {
	cmd := exec.Command("true") //nolint:forbidigo // test helper child
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := RSSKB(cmd.Process.Pid); err == nil {
		t.Error("RSSKB of an exited pid: want error")
	}
}

func TestSnapshotHasSelf(t *testing.T) {
	procs, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	for _, p := range procs {
		if p.PID == self {
			if p.UID != os.Getuid() || p.PPID != os.Getppid() || p.RSSKB <= 0 || p.Comm == "" {
				t.Errorf("self row = %+v; want uid %d ppid %d rss>0 comm set", p, os.Getuid(), os.Getppid())
			}
			return
		}
	}
	t.Fatalf("own pid %d not in snapshot of %d procs", self, len(procs))
}

func TestDescendantsAndCmdline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// sh -> sleep: the sleep is a grandchild of this test.
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.CommandContext(ctx, "sh", "-c", "touch "+marker+"; sleep 30 & wait") //nolint:forbidigo // test helper child
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	var desc []int
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(marker); err == nil {
			if desc = Descendants([]int{cmd.Process.Pid}); len(desc) > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(desc) == 0 {
		t.Fatal("Descendants found no child of sh (want the sleep)")
	}
	args, err := Cmdline(desc[0])
	if err != nil {
		t.Fatalf("Cmdline(%d): %v", desc[0], err)
	}
	if len(args) != 2 || filepath.Base(args[0]) != "sleep" || args[1] != "30" {
		t.Errorf("Cmdline(sleep) = %q, want [sleep 30]", args)
	}
}

func TestUptime(t *testing.T) {
	up, err := Uptime()
	if err != nil {
		t.Fatal(err)
	}
	if up <= 0 || up > 10*365*24*time.Hour {
		t.Errorf("Uptime = %v, want a positive plausible duration", up)
	}
}
