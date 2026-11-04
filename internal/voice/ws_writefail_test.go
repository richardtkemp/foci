package voice

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/log"

	"github.com/gorilla/websocket"
)

// hangGuard bounds a wait for an event that a correct build reaches at once.
const hangGuard = 15 * time.Second

var errHalfDead = errors.New("half-dead socket")

// faultConn fails writes once fail is set while reads keep working: a socket
// the client can still send on but the server can no longer answer on.
type faultConn struct {
	net.Conn
	fail *atomic.Bool
}

func (f faultConn) Write(b []byte) (int, error) {
	if f.fail.Load() {
		return 0, errHalfDead
	}
	return f.Conn.Write(b)
}

type faultListener struct {
	net.Listener
	fail *atomic.Bool
}

func (l faultListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return faultConn{Conn: c, fail: l.fail}, nil
}

// logCapture collects the event log and lets a test wait for a line.
type logCapture struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written chan struct{}
}

func captureLog(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{written: make(chan struct{}, 1)}
	log.SetOutput(lc)
	t.Cleanup(func() { log.SetOutput(nil) })
	return lc
}

func (lc *logCapture) Write(p []byte) (int, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	n, err := lc.buf.Write(p)
	select {
	case lc.written <- struct{}{}:
	default:
	}
	return n, err
}

func (lc *logCapture) String() string {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.buf.String()
}

// waitFor returns once the log holds a line with every part, or fails the test.
func (lc *logCapture) waitFor(t *testing.T, parts ...string) {
	t.Helper()
	guard := time.After(hangGuard)
	for {
		for _, line := range strings.Split(lc.String(), "\n") {
			matched := true
			for _, p := range parts {
				matched = matched && strings.Contains(line, p)
			}
			if matched {
				return
			}
		}
		select {
		case <-lc.written:
		case <-guard:
			t.Fatalf("no log line with %q; log:\n%s", parts, lc.String())
		}
	}
}

// startFaultServer serves cfg on a listener whose writes fail once the returned
// flag is set, and returns a client past select_agent.
func startFaultServer(t *testing.T, cfg HandlerConfig) (*websocket.Conn, *atomic.Bool) {
	t.Helper()
	fail := &atomic.Bool{}
	srv := httptest.NewUnstartedServer(Handler(cfg))
	srv.Listener = faultListener{Listener: srv.Listener, fail: fail}
	srv.Start()
	t.Cleanup(srv.Close)

	ws := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/voice")
	t.Cleanup(func() { _ = ws.Close() })

	var connected ConnectedMsg
	readJSON(t, ws, &connected)
	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)
	return ws, fail
}

// assertClosedByServer proves the server tore the connection down: the client
// read ends with an error rather than sitting until its own deadline.
func assertClosedByServer(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(hangGuard))
	if _, data, err := ws.ReadMessage(); err == nil {
		t.Fatalf("expected a closed connection, read frame %q", data)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server left the connection open after a failed write: %v", err)
	}
}

// TestWriteFailure_ReplyLost covers the audit case: the agent's whole answer
// fails to write. The loss must be logged, the connection torn down so the
// client can reconnect, and TTS must not be paid for an answer nobody can get.
func TestWriteFailure_ReplyLost(t *testing.T) {
	lc := captureLog(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var ttsCalls atomic.Int32
	ws, fail := startFaultServer(t, testConfig(func(c *HandlerConfig) {
		c.HandleMessage = func(context.Context, string, string, string) (string, error) {
			close(entered)
			<-release
			return "the whole answer", nil
		}
		c.AgentTTS = func(string) TTS {
			ttsCalls.Add(1)
			return &mockTTS{result: []byte("fake-mp3")}
		}
	}))

	sendJSON(t, ws, TextMsg{Type: "text", Content: "hi"})
	var start ResponseStartMsg
	readJSON(t, ws, &start)
	<-entered
	fail.Store(true)
	close(release)

	assertClosedByServer(t, ws)
	lc.waitFor(t, "voice.ResponseTextMsg", errHalfDead.Error())
	lc.waitFor(t, "reply lost", "16 bytes")
	if n := ttsCalls.Load(); n != 0 {
		t.Errorf("TTS resolved %d times for an undeliverable reply, want 0", n)
	}
}

// TestWriteFailure_BeforeAgentCall proves a socket that cannot carry
// response_start does not start an agent turn whose answer could not be sent.
func TestWriteFailure_BeforeAgentCall(t *testing.T) {
	lc := captureLog(t)
	var agentCalls atomic.Int32
	ws, fail := startFaultServer(t, testConfig(func(c *HandlerConfig) {
		c.HandleMessage = func(context.Context, string, string, string) (string, error) {
			agentCalls.Add(1)
			return "unreachable", nil
		}
	}))

	fail.Store(true)
	sendJSON(t, ws, TextMsg{Type: "text", Content: "hi"})

	assertClosedByServer(t, ws)
	lc.waitFor(t, "turn dropped")
	if n := agentCalls.Load(); n != 0 {
		t.Errorf("agent called %d times on a dead socket, want 0", n)
	}
}

// TestWriteFailure_DuringAudio proves a failure after the text was delivered is
// also reported and tears the connection down.
func TestWriteFailure_DuringAudio(t *testing.T) {
	lc := captureLog(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	ws, fail := startFaultServer(t, testConfig(func(c *HandlerConfig) {
		c.AgentTTS = func(string) TTS {
			return ttsFunc(func(context.Context, string) ([]byte, error) {
				close(entered)
				<-release
				return []byte("fake-mp3"), nil
			})
		}
	}))

	sendJSON(t, ws, TextMsg{Type: "text", Content: "hi"})
	var start ResponseStartMsg
	readJSON(t, ws, &start)
	var text ResponseTextMsg
	readJSON(t, ws, &text)
	<-entered
	fail.Store(true)
	close(release)

	assertClosedByServer(t, ws)
	lc.waitFor(t, "voice.AudioStartOutMsg", errHalfDead.Error())
}

// TestWriteFailure_Ping proves an idle half-dead socket, found only by a failed
// ping, is reported and closed rather than left for the read deadline.
func TestWriteFailure_Ping(t *testing.T) {
	lc := captureLog(t)
	fail := &atomic.Bool{}
	loopDone := make(chan struct{})
	ticks := make(chan time.Time)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		c := &conn{ws: ws, cfg: &HandlerConfig{}, connID: "ping-test"}
		c.pingLoop(context.Background(), ticks)
		c.writeFailed("later frame", errHalfDead) // sticky errors: later writes fail too
		close(loopDone)
	}))
	srv.Listener = faultListener{Listener: srv.Listener, fail: fail}
	srv.Start()
	t.Cleanup(srv.Close)
	ws := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/voice")
	t.Cleanup(func() { _ = ws.Close() })

	fail.Store(true)
	ticks <- time.Now()

	select {
	case <-loopDone:
	case <-time.After(hangGuard):
		t.Fatal("pingLoop did not return after a failed ping")
	}
	assertClosedByServer(t, ws)
	lc.waitFor(t, "write ping failed", "conn=ping-test", errHalfDead.Error())
	if n := strings.Count(lc.String(), "closing connection"); n != 1 {
		t.Errorf("logged %d write failures, want only the first:\n%s", n, lc.String())
	}
}

type ttsFunc func(context.Context, string) ([]byte, error)

func (f ttsFunc) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return f(ctx, text)
}
