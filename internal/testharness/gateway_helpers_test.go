package testharness

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFirstLine proves the helper returns the trimmed first line for
// multi-line, single-line, CRLF, and empty inputs.
func TestFirstLine(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"module foci\n\ngo 1.25.0\n", "module foci"},
		{"single line no newline", "single line no newline"},
		{"  padded  \nrest", "padded"},
		{"crlf line\r\nrest", "crlf line"},
		{"", ""},
		{"\nsecond", ""},
	}
	for _, tt := range tests {
		if got := firstLine(tt.in); got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestTitleFirst proves the first ASCII letter is upper-cased and that
// empty, already-capitalised, and non-letter-leading inputs pass through.
func TestTitleFirst(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"clutch", "Clutch"},
		{"Already", "Already"},
		{"x", "X"},
		{"9lives", "9lives"},
	}
	for _, tt := range tests {
		if got := titleFirst(tt.in); got != tt.want {
			t.Errorf("titleFirst(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSyncBuffer_ConcurrentWrites proves the buffer is safe under
// concurrent writers (run with -race) and loses no bytes: the final
// String contains every written chunk.
func TestSyncBuffer_ConcurrentWrites(t *testing.T) {
	buf := newSyncBuffer()
	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			chunk := fmt.Sprintf("[writer-%d]", id)
			n, err := buf.Write([]byte(chunk))
			if err != nil || n != len(chunk) {
				t.Errorf("Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
			}
		}(i)
	}
	wg.Wait()

	got := buf.String()
	for i := 0; i < writers; i++ {
		chunk := fmt.Sprintf("[writer-%d]", i)
		if !strings.Contains(got, chunk) {
			t.Errorf("buffer missing chunk %q; got %q", chunk, got)
		}
	}
}

// TestWaitForReady_AlreadyReady proves the poll returns nil immediately
// when the terminal "started N agent(s):" line is already in the buffer.
func TestWaitForReady_AlreadyReady(t *testing.T) {
	buf := newSyncBuffer()
	if _, err := buf.Write([]byte("boot...\nstarted 2 agent(s): clutch, fotini\n")); err != nil {
		t.Fatalf("seed buffer: %v", err)
	}
	stopped := make(chan struct{})
	if err := waitForReady(buf, stopped, time.Second); err != nil {
		t.Errorf("waitForReady = %v, want nil for ready buffer", err)
	}
}

// TestWaitForReady_ProcessExited proves a closed stoppedCh (gateway died
// before signalling) is reported as an "exited before ready" error, not a
// timeout.
func TestWaitForReady_ProcessExited(t *testing.T) {
	buf := newSyncBuffer()
	stopped := make(chan struct{})
	close(stopped)
	err := waitForReady(buf, stopped, time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited before signalling ready") {
		t.Errorf("waitForReady = %v, want 'exited before signalling ready' error", err)
	}
}

// TestWaitForReady_Timeout proves the poll gives up with a timeout error
// when the ready line never appears and the process keeps running.
func TestWaitForReady_Timeout(t *testing.T) {
	buf := newSyncBuffer()
	if _, err := buf.Write([]byte("still booting, no ready line\n")); err != nil {
		t.Fatalf("seed buffer: %v", err)
	}
	stopped := make(chan struct{}) // never closed: process still alive
	err := waitForReady(buf, stopped, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("waitForReady = %v, want timeout error", err)
	}
}

// TestPickFreePort proves the kernel-assigned port is in the valid range
// and actually bindable immediately after release.
func TestPickFreePort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port = %d, want 1..65535", port)
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d not bindable after pickFreePort: %v", port, err)
	}
	_ = l.Close()
}

// TestFindRepoRoot proves the upward go.mod walk lands on the directory
// whose go.mod declares "module foci" and which contains this test's
// working directory.
func TestFindRepoRoot(t *testing.T) {
	root := findRepoRoot(t)

	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod at reported root %s: %v", root, err)
	}
	if firstLine(string(b)) != "module foci" {
		t.Errorf("go.mod first line = %q, want %q", firstLine(string(b)), "module foci")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if !strings.HasPrefix(wd, root) {
		t.Errorf("working dir %s not under reported repo root %s", wd, root)
	}
}
