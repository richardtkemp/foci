// Package procinfo answers the few process and memory questions foci asks of
// the host OS — total RAM, a process's resident size, the process table, a
// process's children and command line, and system uptime — behind one API, so
// callers do not read /proc directly.
//
// Sources per platform:
//
//	                Linux                           macOS
//	MemTotalKB      /proc/meminfo                   sysctl hw.memsize
//	RSSKB           /proc/<pid>/status VmRSS        ps -o rss= -p <pid>
//	Snapshot        /proc/<pid>/status (all pids)   ps -axo pid,ppid,uid,rss,comm
//	Children        /proc/<pid>/task/<pid>/children one Snapshot, indexed by ppid
//	Cmdline         /proc/<pid>/cmdline             sysctl kern.procargs2
//	Uptime          /proc/uptime                    sysctl kern.boottime
//
// Other platforms return ErrUnsupported from every call. Callers treat an
// error as "this feature is unavailable", never as fatal.
package procinfo

import "errors"

// ErrUnsupported is returned on platforms with no implementation.
var ErrUnsupported = errors.New("procinfo: not supported on this platform")

// Proc is one row of the process table.
type Proc struct {
	PID   int
	PPID  int
	UID   int // real UID
	RSSKB int64
	Comm  string // executable base name
}

// Descendants returns every descendant of the given PIDs (not the PIDs
// themselves), depth-first. PID 1 and below are never returned, so a walk
// can never escape into init's whole tree.
func Descendants(pids []int) []int {
	children := ChildrenFunc()
	seen := make(map[int]bool)
	var out []int
	var walk func(int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		for _, c := range children(pid) {
			if c <= 1 {
				continue
			}
			out = append(out, c)
			walk(c)
		}
	}
	for _, p := range pids {
		walk(p)
	}
	return out
}
