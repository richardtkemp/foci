package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// TestWithHub_RecoversHandlerPanic verifies the HTTP chokepoint recovers a
// panic in any app handler, so one bad request can't crash the gateway. The
// recovery writes nothing, so the client gets net/http's implicit 200 with an
// empty body; the log assert proves that 200 came from a recovered panic and
// not from a path that never reached the handler.
func TestWithHub_RecoversHandlerPanic(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	setActiveHub(newTestHub())
	t.Cleanup(func() { setActiveHub(nil) })

	h := withHub(func(*Hub, http.ResponseWriter, *http.Request) {
		panic("handler boom")
	})

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/app/test", nil)
	h(rw, req) // must return without panicking

	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want the implicit %d (recovery writes nothing)", rw.Code, http.StatusOK)
	}
	if rw.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rw.Body.String())
	}
	if got := buf.String(); !strings.Contains(got, "recovered panic in http GET /app/test") {
		t.Errorf("log = %q, want the recovered handler panic", got)
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
