package app

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	flog "foci/internal/log"
)

// syncBuffer is a bytes.Buffer safe for the logger's writes and the test's
// reads from different goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the captured lines that contain substr.
func (b *syncBuffer) lines(substr string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// eventually polls cond until it holds or d passes.
func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// lockWatchFixture runs the hub's own watchdog, fast, with the app log
// captured and the hub's logs directory in a temp dir.
func lockWatchFixture(t *testing.T) (*Hub, *syncBuffer, string, func()) {
	t.Helper()
	logs := t.TempDir()
	h := newTestHub()
	h.deps.Config = &config.Config{}
	h.deps.Config.Logging.EventFile = filepath.Join(logs, "foci.log")

	out := &syncBuffer{}
	flog.SetOutput(out)
	flog.SetLevel(flog.INFO)

	done := make(chan struct{})
	stopped := make(chan struct{})
	w := h.newLockWatchdog(10*time.Millisecond, 100*time.Millisecond)
	go func() {
		defer close(stopped)
		w.run(done)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
	t.Cleanup(func() {
		stop()
		flog.SetOutput(os.Stderr)
		flog.SetLevel(flog.INFO)
	})
	return h, out, logs, stop
}

// #3171: while the hub's lock cannot be granted, the watchdog logs ONE WARN
// naming a goroutine dump it wrote in the logs directory; when the lock is
// granted again it logs one INFO.
func TestLockWatchdog_WarnsOnceWithDumpThenRecovers(t *testing.T) {
	h, out, logs, stop := lockWatchFixture(t)

	h.mu.Lock() // a writer that never lets go: every read probe blocks
	released := false
	defer func() {
		if !released {
			h.mu.Unlock()
		}
	}()

	if !eventually(5*time.Second, func() bool { return len(out.lines("app hub lock not granted")) > 0 }) {
		t.Fatal("no WARN while the hub lock was held")
	}
	warn := out.lines("app hub lock not granted")[0]
	if !strings.Contains(warn, "WARN") {
		t.Errorf("hang line is not a WARN: %q", warn)
	}
	m := regexp.MustCompile(`goroutine dump written to (\S+)`).FindStringSubmatch(warn)
	if m == nil {
		t.Fatalf("WARN names no dump file: %q", warn)
	}
	dump := m[1]
	if filepath.Dir(dump) != logs {
		t.Errorf("dump %s is not in the logs dir %s", dump, logs)
	}
	body, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read the dump the WARN names: %v", err)
	}
	// A full dump (debug=2) names this test's own goroutine.
	if !strings.Contains(string(body), "goroutine ") || !strings.Contains(string(body), "TestLockWatchdog_WarnsOnceWithDumpThenRecovers") {
		t.Errorf("dump is not a full goroutine dump (%d bytes)", len(body))
	}

	// Many more probe intervals pass: still one WARN, one dump.
	time.Sleep(300 * time.Millisecond)
	if n := len(out.lines("app hub lock not granted")); n != 1 {
		t.Errorf("%d hang WARNs during one hang, want 1", n)
	}
	if files, _ := filepath.Glob(filepath.Join(logs, "hang-*.txt")); len(files) != 1 {
		t.Errorf("dump files = %v, want 1", files)
	}
	if n := len(out.lines("granted again")); n != 0 {
		t.Errorf("recovery logged while the lock is still held: %v", out.lines("granted again"))
	}

	h.mu.Unlock()
	released = true
	if !eventually(5*time.Second, func() bool { return len(out.lines("app hub lock granted again")) > 0 }) {
		t.Fatal("no recovery line after the lock was released")
	}
	if l := out.lines("app hub lock granted again")[0]; !strings.Contains(l, "INFO") {
		t.Errorf("recovery line is not INFO: %q", l)
	}
	stop()
	if n := len(out.lines("app hub lock granted again")); n != 1 {
		t.Errorf("%d recovery lines, want 1", n)
	}
}

// The control: a hub whose lock is free logs nothing and writes no dump.
func TestLockWatchdog_FreeLockIsSilent(t *testing.T) {
	_, out, logs, stop := lockWatchFixture(t)
	time.Sleep(300 * time.Millisecond)
	stop()
	if l := out.lines("app hub lock"); len(l) != 0 {
		t.Errorf("free lock logged %v, want nothing", l)
	}
	if files, _ := filepath.Glob(filepath.Join(logs, "hang-*.txt")); len(files) != 0 {
		t.Errorf("free lock wrote dumps %v", files)
	}
}
