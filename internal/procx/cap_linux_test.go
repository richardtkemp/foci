//go:build linux

package procx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// capField returns the raw hex value of one Cap* field (e.g. "CapAmb") from a
// /proc/<pid>/status document, or "" when the field is absent.
func capField(status, name string) string {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, name+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, name+":"))
		}
	}
	return ""
}

// capEmpty reports whether a Cap* hex value is present and all zeroes. The
// string is fixed-width zero-padded, so an empty set is all zeroes at any
// width.
func capEmpty(hex string) bool { return hex != "" && strings.Trim(hex, "0") == "" }

// threadCapAmb returns CapAmb for every OS thread of this process, keyed by
// tid. A thread that exits between the glob and the read is skipped.
func threadCapAmb(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("/proc/self/task/*/status")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob /proc/self/task/*/status: %v (%d matches)", err, len(paths))
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out[filepath.Base(filepath.Dir(p))] = capField(string(b), "CapAmb")
	}
	return out
}

// requireAmbientSetgid puts the test process in the state foci-gw starts in
// under its systemd unit (AmbientCapabilities=CAP_SETGID): CAP_SETGID in the
// ambient set of every thread. That is the only state in which a clear is
// observable. Run from a foci-gw descendant the cap is already inherited;
// otherwise raise it on all threads (needs CAP_SETGID in the permitted and
// inheritable sets) or skip — a process with no capability at all cannot
// exercise this boundary.
func requireAmbientSetgid(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	if !capEmpty(capField(string(b), "CapAmb")) {
		return
	}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, unix.CAP_SETGID)
	if errno != 0 {
		t.Skipf("needs ambient CAP_SETGID and cannot raise it (%v); run under foci-gw or `setpriv --inh-caps +setgid --ambient-caps +setgid`", errno)
	}
}

// TestClearAmbientCaps proves that clearAmbientCaps() empties the ambient
// capability set on EVERY OS thread — not just the caller's — and that a
// child forked from any of those threads therefore execs with no
// capabilities at all. This is the load-bearing half of the P0-1 fix:
// ambient caps are preserved across execve for non-root processes, so any
// CAP_SETGID left in the ambient set of any thread lets a child forked from
// that thread re-add the foci-secrets group and read secrets.toml (#2007).
//
// Ambient caps are per-thread state and Go forks a child from whichever
// thread the spawning goroutine is on, so the test parks goroutines on their
// own OS threads BEFORE the clear and has those threads do the spawning. A
// clear that only reaches the calling thread (a single prctl) leaves the
// parked threads holding the cap and their children inheriting it, which is
// exactly the red this test exists to produce.
func TestClearAmbientCaps(t *testing.T) {
	requireAmbientSetgid(t)

	const n = 8
	release := make(chan struct{})
	var parked, done sync.WaitGroup
	results := make(chan string, n)
	parked.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			// Exiting while locked kills the thread, so nothing leaks.
			runtime.LockOSThread()
			parked.Done()
			<-release
			out, err := Spawn(context.Background(), Trusted, "cat", "/proc/self/status").Output()
			if err != nil {
				results <- "spawn: " + err.Error()
				return
			}
			results <- string(out)
		}()
	}
	parked.Wait()

	for tid, v := range threadCapAmb(t) {
		if capEmpty(v) {
			t.Fatalf("precondition: thread %s has CapAmb=%s before the clear; the test cannot fail from here", tid, v)
		}
	}

	if err := clearAmbientCaps(); err != nil {
		t.Fatalf("clearAmbientCaps: %v", err)
	}

	for tid, v := range threadCapAmb(t) {
		if !capEmpty(v) {
			t.Errorf("thread %s still has CapAmb=%s after clear", tid, v)
		}
	}

	close(release)
	done.Wait()
	close(results)
	for r := range results {
		if strings.HasPrefix(r, "spawn: ") {
			t.Fatal(r)
		}
		for _, f := range []string{"CapAmb", "CapPrm", "CapEff"} {
			if v := capField(r, f); !capEmpty(v) {
				t.Errorf("child forked from a parked thread has %s=%s; want all zero", f, v)
			}
		}
	}
}
