//go:build darwin

package resources

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"foci/internal/procinfo"
)

// macOS has no procfs. Total RAM and the process table come from procinfo
// (sysctl hw.memsize and one ps snapshot). Pressure comes from sysctl
// kern.memorystatus_vm_pressure_level, mapped onto the PSI avg10 scale the
// threshold is configured in (see darwinPressure).

func platformMemTotal() (int64, error) { return procinfo.MemTotalKB() }

func platformUserRSS(uid int) (int64, error) {
	procs, err := procinfo.Snapshot()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range procs {
		if p.UID == uid {
			total += p.RSSKB
		}
	}
	return total, nil
}

func platformSelfRSS() (int64, error) { return procinfo.RSSKB(os.Getpid()) }

func platformFindLargest(uid, selfPid int) (int, string, int64, error) {
	procs, err := procinfo.Snapshot()
	if err != nil {
		return 0, "", 0, err
	}
	return largestOwned(procs, uid, selfPid)
}

// largestOwned picks the largest-RSS process owned by uid that descends from
// selfPid (foci-gw). On macOS foci runs as the login user, so "owned by uid"
// alone covers the user's own apps (browser, IDE, a build over ssh): the guard
// once killed a 20 GB golangci-lint that foci had not started. Only foci's own
// process tree is foci's to kill. (On Linux the uid is the dedicated foci user,
// so ownership already means foci's.)
func largestOwned(procs []procinfo.Proc, uid, selfPid int) (int, string, int64, error) {
	children := make(map[int][]int)
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	ours := make(map[int]bool)
	stack := append([]int(nil), children[selfPid]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ours[pid] {
			continue
		}
		ours[pid] = true
		stack = append(stack, children[pid]...)
	}
	var best procinfo.Proc
	for _, p := range procs {
		if ours[p.PID] && p.UID == uid && p.RSSKB > best.RSSKB {
			best = p
		}
	}
	if best.PID == 0 {
		return 0, "", 0, fmt.Errorf("no eligible process found for uid %d under pid %d", uid, selfPid)
	}
	return best.PID, best.Comm, best.RSSKB, nil
}

func platformPressure() (float64, error) {
	lvl, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.memorystatus_vm_pressure_level: %w", err)
	}
	return darwinPressure(lvl), nil
}

// darwinPressure maps the kernel's coarse pressure level onto the PSI avg10
// scale (percent of time stalled, 0-100) that memory_pressure_threshold is
// written in. macOS reports only three levels, so the mapping is a step:
//
//	1 normal   -> 0    never satisfies a positive threshold
//	2 warning  -> 5    below the default threshold of 10
//	4 critical -> 100  the kernel is about to kill processes
//
// "warning" means macOS has started compressing memory, which is routine on a
// busy Mac (a 20 GB golangci-lint on a 48 GB machine reached it), not the
// sustained stall that PSI >= 10 means on Linux. So by default the guard acts
// only at "critical". An unknown level is treated as critical.
func darwinPressure(level uint32) float64 {
	switch level {
	case 1:
		return 0
	case 2:
		return 5
	default:
		return 100
	}
}
