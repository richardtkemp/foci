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

// largestOwned picks the largest-RSS process owned by uid, excluding selfPid.
func largestOwned(procs []procinfo.Proc, uid, selfPid int) (int, string, int64, error) {
	var best procinfo.Proc
	for _, p := range procs {
		if p.UID == uid && p.PID != selfPid && p.RSSKB > best.RSSKB {
			best = p
		}
	}
	if best.PID == 0 {
		return 0, "", 0, fmt.Errorf("no eligible process found for uid %d", uid)
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
//	2 warning  -> 50   the system is compressing/swapping hard
//	4 critical -> 100  the kernel is about to kill processes
//
// With the default threshold of 10, the guard acts at "warning" and above,
// which is the closest macOS analogue of "PSI says tasks are stalling".
// An unknown level is treated as critical rather than silently ignored.
func darwinPressure(level uint32) float64 {
	switch level {
	case 1:
		return 0
	case 2:
		return 50
	default:
		return 100
	}
}
