package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/log"
	"foci/internal/platform"
)

// TestRecoverApp_SwallowsPanic verifies recoverApp, used as a deferred call,
// stops a panic from propagating and logs it. Reaching the assertions at all
// proves the panic did not crash the test binary.
func TestRecoverApp_SwallowsPanic(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	func() {
		defer recoverApp("test-site")
		panic("boom")
	}()

	if got := buf.String(); !strings.Contains(got, "recovered panic in test-site") {
		t.Errorf("log = %q, want it to mention the recovered panic site", got)
	}
}

// TestSafeGo_RecoversPanic verifies a panic inside a safeGo goroutine is
// recovered rather than crashing the process (an unrecovered goroutine panic
// would take down the whole test binary).
func TestSafeGo_RecoversPanic(t *testing.T) {
	ran := make(chan struct{})
	safeGo("test-goroutine", func() {
		defer close(ran)
		panic("boom in goroutine")
	})

	select {
	case <-ran:
		// Goroutine executed; if safeGo had not recovered, the propagating
		// panic would have crashed the test binary before we got here.
	case <-time.After(2 * time.Second):
		t.Fatal("safeGo goroutine did not run")
	}
}

// serveWithHub runs fn behind withHub on a real net/http server, because what the
// client sees after a handler panic is decided by net/http, not by a recorder.
// The returned channel yields the value withHub panicked with once the handler
// has finished, which also orders the caller's log read after the handler's write.
// after, if not nil, runs once the handler has finished and before net/http
// handles the panic.
func serveWithHub(t *testing.T, fn func(*Hub, http.ResponseWriter, *http.Request), after func()) (*httptest.Server, <-chan any) {
	t.Helper()
	setActiveHub(newTestHub())
	t.Cleanup(func() { setActiveHub(nil) })

	finished := make(chan any, 1)
	h := withHub(fn)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if after != nil {
				after()
			}
			finished <- v
			if v != nil {
				panic(v)
			}
		}()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, finished
}

// TestWithHub_HandlerPanicBeforeWriteFailsClient verifies a panic before any
// write reaches the client as a failure, not as a 200 with an empty body, which
// a client of POST /app/blob would read as success (#2895). The panic stays
// contained: net/http treats http.ErrAbortHandler as a request abort, and the
// log line still names the site and carries the original value.
func TestWithHub_HandlerPanicBeforeWriteFailsClient(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	srv, finished := serveWithHub(t, func(*Hub, http.ResponseWriter, *http.Request) {
		panic("handler boom")
	}, nil)

	resp, err := http.Post(srv.URL+"/app/test", "text/plain", strings.NewReader("x"))
	if err == nil {
		resp.Body.Close()
		t.Errorf("client got status %d, want a transport failure", resp.StatusCode)
	}

	if v := <-finished; v != http.ErrAbortHandler {
		t.Errorf("withHub panicked with %v, want http.ErrAbortHandler", v)
	}
	got := buf.String()
	if !strings.Contains(got, "recovered panic in http POST /app/test") || !strings.Contains(got, "handler boom") {
		t.Errorf("log = %q, want the recovered panic site and value", got)
	}
}

// TestWithHub_HandlerPanicAfterWriteFailsClient verifies a panic part way through
// a chunked response does not end the response cleanly: without the abort,
// net/http writes the final chunk and the client reads a truncated body as whole.
func TestWithHub_HandlerPanicAfterWriteFailsClient(t *testing.T) {
	srv, finished := serveWithHub(t, func(_ *Hub, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic("handler boom")
	}, nil)

	resp, err := http.Get(srv.URL + "/app/test")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Errorf("read %q with no error, want the truncation to fail the read", body)
	}
	<-finished
}

// TestWithHub_HandlerPanicAfterHijackLeavesConn verifies a panic after a real
// WebSocket upgrade writes nothing to the hijacked connection and does not close
// it: the first frame the client reads is the one sent after the panic. It also
// proves gorilla's Hijacker assertion still succeeds through withHub.
func TestWithHub_HandlerPanicAfterHijackLeavesConn(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var ws *websocket.Conn
	const marker = "written-after-panic"
	srv, finished := serveWithHub(t, func(_ *Hub, w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade: %v", err)
			return
		}
		ws = c
		panic("handler boom")
	}, func() {
		if ws == nil {
			return
		}
		if err := ws.WriteMessage(websocket.TextMessage, []byte(marker)); err != nil {
			t.Errorf("write after panic: %v", err)
		}
		_ = ws.Close()
	})

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/app/ws", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, msg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if string(msg) != marker {
		t.Errorf("first frame = %q, want %q", msg, marker)
	}

	if v := <-finished; v != http.ErrAbortHandler {
		t.Errorf("withHub panicked with %v, want http.ErrAbortHandler", v)
	}
	if got := buf.String(); !strings.Contains(got, "recovered panic in http GET /app/ws") {
		t.Errorf("log = %q, want the recovered panic", got)
	}
}

// TestProviderDegraded_IsInert verifies an app provider whose Init failed
// (nil hub) is harmless in the active provider set: ConnectionManager never
// returns nil and its methods don't dereference the absent hub, and
// SetupAgentConnection declines instead of nil-dereferencing.
func TestProviderDegraded_IsInert(t *testing.T) {
	p := &appProvider{} // hub and connMgr nil, as after a recovered Init panic

	cm := p.ConnectionManager()
	if cm == nil {
		t.Fatal("ConnectionManager returned nil for degraded provider")
	}
	// Exercise every method the aggregator might call — none may panic.
	_ = cm.Primary("agent")
	_ = cm.AllForAgent("agent")
	_ = cm.ForSession("sk")
	_ = cm.ForSessionOrPrimary("sk", "agent")
	_, _ = cm.AcquireFacet("agent")
	_ = cm.HasFacet("agent")
	cm.StartAll(context.Background())
	cm.Wait()

	if r := p.SetupAgentConnection(platform.AgentConnectionParams{}); r != nil {
		t.Errorf("SetupAgentConnection = %v, want nil for degraded provider", r)
	}
}
