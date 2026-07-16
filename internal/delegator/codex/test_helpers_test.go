package codex

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"foci/internal/log"
)

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	b := &Backend{
		cfg:          map[string]any{},
		lg:           log.NewComponentLogger("codex"),
		pendingRPC:   make(map[int64]chan rpcReply),
		pendingPerms: make(map[int64]*pendingApproval),
	}
	b.writer = NewWriter(nopWriteCloser{&bytes.Buffer{}})
	return b
}

type captureCloser struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	fail bool
}

func (c *captureCloser) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf.Write(p)
	c.mu.Unlock()
	if c.fail {
		return 0, errors.New("test: write failed")
	}
	return len(p), nil
}

func (c *captureCloser) Close() error { return nil }

func (c *captureCloser) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *captureCloser) Reset() {
	c.mu.Lock()
	c.buf.Reset()
	c.mu.Unlock()
}

func panics(fn func()) (yes bool) {
	defer func() {
		if r := recover(); r != nil {
			yes = true
		}
	}()
	fn()
	return
}
