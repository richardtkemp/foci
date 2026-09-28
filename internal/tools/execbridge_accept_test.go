package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"foci/internal/tempdir"
)

// acceptTestBound is a hang guard only, not a timing assertion: each test
// below fails by hanging on the broken code, so the bound turns a hang into a
// failure. It is well above any wait the fixed code can take.
const acceptTestBound = 10 * time.Second

// errListener wraps a real unix listener and makes Accept return injected
// errors before it serves real connections. If forever is set, Accept returns
// the error on every call until the listener is closed.
type errListener struct {
	net.Listener
	mu       sync.Mutex
	errs     []error
	forever  error
	closed   bool
	accepted chan error // gets each injected error as Accept returns it
}

func (l *errListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, fmt.Errorf("accept: %w", net.ErrClosed)
	}
	var err error
	if len(l.errs) > 0 {
		err, l.errs = l.errs[0], l.errs[1:]
	} else if l.forever != nil {
		err = l.forever
	}
	l.mu.Unlock()
	if err != nil {
		select {
		case l.accepted <- err:
		default:
		}
		return nil, err
	}
	return l.Listener.Accept()
}

func (l *errListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.Listener.Close()
}

func emfileErr() error {
	return &net.OpError{Op: "accept", Net: "unix", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
}

// startErrBridge starts a bridge that serves ln wrapped in an errListener.
func startErrBridge(t *testing.T, fl func(*errListener), backoff acceptBackoff) (*ExecBridge, *errListener) {
	t.Helper()
	n := bridgeCounter.Add(1)
	sockPath := fmt.Sprintf("%s/exec-accepttest-%d-%d.sock", tempdir.Dir(), os.Getpid(), n)
	funcsPath := fmt.Sprintf("%s/exec-accepttest-%d-%d-funcs.sh", tempdir.Dir(), os.Getpid(), n)
	inner, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := &errListener{Listener: inner, accepted: make(chan error, 16)}
	fl(ln)
	b, err := startExecBridge(testRegistry(), context.Background(), ln, sockPath, funcsPath, backoff)
	if err != nil {
		t.Fatalf("startExecBridge: %v", err)
	}
	return b, ln
}

// callBridgeBounded is callBridge with a deadline, so a bridge that never
// accepts fails the test instead of hanging it.
func callBridgeBounded(t *testing.T, sockPath, request string) (result, errMsg string) {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(acceptTestBound))
	fmt.Fprintf(conn, "%s\n", request)
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no response from bridge (accept loop not serving?): %v", scanner.Err())
	}
	var resp struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp.Result, resp.Error
}

// waitDone fails the test if done is not closed within the hang guard.
func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(acceptTestBound):
		t.Fatalf("%s did not return within %s", what, acceptTestBound)
	}
}

func TestExecBridgeAcceptErrorKeepsServing(t *testing.T) {
	// #1122 M1: an Accept error that is not ErrClosed (EMFILE under fd
	// exhaustion) must not stop the accept loop. Before the fix the loop
	// returned on any error, the socket stayed bound, and every later call
	// hung with no log line.
	t.Parallel()
	b, ln := startErrBridge(t, func(l *errListener) {
		l.errs = []error{emfileErr(), emfileErr()}
	}, defaultAcceptBackoff)
	defer b.Close()

	// The injected errors come before any real Accept, so this call is only
	// served if the loop survived both of them.
	result, errMsg := callBridgeBounded(t, b.SockPath(), `{"tool":"echo_tool","params":{"text":"after emfile"}}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result != "echo: after emfile" {
		t.Errorf("result = %q, want %q", result, "echo: after emfile")
	}
	ln.mu.Lock()
	left := len(ln.errs)
	ln.mu.Unlock()
	if left != 0 {
		t.Errorf("%d injected accept errors not consumed", left)
	}
}

func TestExecBridgeCloseDuringAcceptBackoff(t *testing.T) {
	// Close must not wait for a loop that sleeps between Accept retries. The
	// backoff is one hour here so that a wait which ignores the bridge ctx
	// holds Close far past the hang guard.
	t.Parallel()
	b, ln := startErrBridge(t, func(l *errListener) {
		l.forever = emfileErr()
	}, acceptBackoff{min: time.Hour, max: time.Hour})

	select {
	case <-ln.accepted:
	case <-time.After(acceptTestBound):
		t.Fatal("injected accept error not reached")
	}
	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	waitDone(t, done, "Close during accept backoff")
}

func TestExecBridgeListenerCloseStopsAcceptLoop(t *testing.T) {
	// The accept loop must still stop when its listener is closed, and on
	// that signal alone: the ctx is not cancelled here, so only the
	// ErrClosed check can end the loop. Then Close still returns and cleans up.
	t.Parallel()
	b, ln := startErrBridge(t, func(*errListener) {}, defaultAcceptBackoff)

	_ = ln.Close()
	loopDone := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(loopDone)
	}()
	waitDone(t, loopDone, "accept loop after listener close")

	closeDone := make(chan struct{})
	go func() {
		b.Close()
		close(closeDone)
	}()
	waitDone(t, closeDone, "Close")
	if _, err := os.Stat(b.SockPath()); !os.IsNotExist(err) {
		t.Errorf("socket file not cleaned up: %v", err)
	}
}
