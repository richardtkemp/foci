//go:build linux

package procinfo

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const procDir = "/proc"

// MemTotalKB returns total RAM in kB.
func MemTotalKB() (int64, error) {
	return scanField(filepath.Join(procDir, "meminfo"), "MemTotal")
}

// RSSKB returns the resident set size of pid in kB.
func RSSKB(pid int) (int64, error) {
	return scanField(filepath.Join(procDir, strconv.Itoa(pid), "status"), "VmRSS")
}

// Snapshot returns every process whose status file is readable.
func Snapshot() ([]Proc, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procDir, err)
	}
	var out []Proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		p, ok := readStatus(pid)
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// ChildrenFunc returns a lookup of a PID's direct children. On Linux each
// lookup reads /proc/<pid>/task/<pid>/children, so it is always current.
func ChildrenFunc() func(pid int) []int {
	return func(pid int) []int {
		data, err := os.ReadFile(fmt.Sprintf("%s/%d/task/%d/children", procDir, pid, pid))
		if err != nil {
			return nil
		}
		var out []int
		for _, f := range strings.Fields(string(data)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
			}
		}
		return out
	}
}

// Cmdline returns pid's argv.
func Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	s := strings.TrimRight(string(data), "\x00")
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\x00"), nil
}

// Uptime returns the time since boot.
func Uptime() (time.Duration, error) {
	data, err := os.ReadFile(filepath.Join(procDir, "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected format in /proc/uptime")
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func readStatus(pid int) (Proc, bool) {
	f, err := os.Open(filepath.Join(procDir, strconv.Itoa(pid), "status"))
	if err != nil {
		return Proc{}, false
	}
	defer func() { _ = f.Close() }()
	p := Proc{PID: pid}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		switch k {
		case "Name":
			p.Comm = fields[0]
		case "PPid":
			p.PPID, _ = strconv.Atoi(fields[0])
		case "Uid":
			p.UID, _ = strconv.Atoi(fields[0])
		case "VmRSS":
			p.RSSKB, _ = strconv.ParseInt(fields[0], 10, 64)
		}
	}
	return p, true
}

// scanField reads the first integer after "<field>:" in a /proc file.
func scanField(path, field string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	prefix := field + ":"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("unexpected %s line: %s", field, line)
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%s not found in %s", field, path)
}
