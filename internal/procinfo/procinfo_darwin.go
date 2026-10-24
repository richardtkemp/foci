//go:build darwin

package procinfo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"foci/internal/procx"
)

// psTimeout bounds one ps run. A full process listing takes tens of
// milliseconds; a hung ps must not stall a monitor goroutine forever.
const psTimeout = 10 * time.Second

// MemTotalKB returns total RAM in kB.
func MemTotalKB() (int64, error) {
	b, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	return int64(b / 1024), nil //nolint:gosec // RAM in kB fits in int64
}

// RSSKB returns the resident set size of pid in kB.
func RSSKB(pid int) (int64, error) {
	out, err := ps("-o", "rss=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return 0, fmt.Errorf("pid %d: no such process", pid)
	}
	return strconv.ParseInt(s, 10, 64)
}

// Snapshot returns every process from one ps run.
func Snapshot() ([]Proc, error) {
	out, err := ps("-axo", "pid=,ppid=,uid=,rss=,comm=")
	if err != nil {
		return nil, err
	}
	return parsePS(out), nil
}

// ChildrenFunc takes one snapshot and returns a lookup over it. A process
// started after the snapshot is not seen; callers that kill a tree call this
// again after each kill round if they need to catch late forks.
func ChildrenFunc() func(pid int) []int {
	procs, err := Snapshot()
	if err != nil {
		return func(int) []int { return nil }
	}
	byParent := make(map[int][]int)
	for _, p := range procs {
		byParent[p.PPID] = append(byParent[p.PPID], p.PID)
	}
	return func(pid int) []int { return byParent[pid] }
}

// Cmdline returns pid's argv, read from sysctl kern.procargs2 (no exec).
func Cmdline(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2 %d: %w", pid, err)
	}
	return parseProcargs2(raw)
}

// Uptime returns the time since boot, from sysctl kern.boottime.
func Uptime() (time.Duration, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.boottime: %w", err)
	}
	return time.Since(time.Unix(tv.Unix())), nil
}

func ps(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := procx.Spawn(ctx, procx.Trusted, "/bin/ps", args...).Output()
	if err != nil {
		// ps -p <dead pid> exits 1 with empty output: report as no process.
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
			return "", nil
		}
		return "", fmt.Errorf("ps %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// parsePS parses `ps -axo pid=,ppid=,uid=,rss=,comm=`. comm can contain
// spaces ("Google Chrome Helper"), so it is the rest of the line after the
// four numeric columns. Malformed lines are skipped.
func parsePS(out string) []Proc {
	var procs []Proc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		uid, e3 := strconv.Atoi(f[2])
		rss, e4 := strconv.ParseInt(f[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, UID: uid, RSSKB: rss,
			Comm: filepath.Base(strings.Join(f[4:], " "))})
	}
	return procs
}

// parseProcargs2 decodes the kern.procargs2 buffer: a native-endian int32
// argc, the executable path, NUL padding, then argc NUL-terminated args
// (followed by the environment, which is ignored).
func parseProcargs2(raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, errors.New("procargs2: short buffer")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4])) // darwin is little-endian on arm64 and amd64
	rest := raw[4:]
	// Skip the exec path and its NUL padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errors.New("procargs2: no exec path terminator")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			args = append(args, string(rest))
			break
		}
		args = append(args, string(rest[:j]))
		rest = rest[j+1:]
	}
	return args, nil
}
