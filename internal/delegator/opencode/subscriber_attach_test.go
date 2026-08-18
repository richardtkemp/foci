package opencode

// #1722: health readiness and SSE readiness are NOT the same event. On
// 2026-08-16 GET /global/health returned 200 eight seconds before GET /event
// did; the prompt POSTed in that window ran to completion with no subscriber
// attached, so foci never saw the turn end and the session worker wedged
// permanently — every later message to that agent queued behind a turn that
// had already finished.
//
// These tests pin the gap itself rather than the symptom: that health can pass
// while the stream is still refusing, and that the new wait blocks across
// exactly that interval.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// attachStub serves an opencode-shaped server whose /event endpoint refuses
// until the returned gate is closed, while /global/health is 200 from the
// start. Closing the gate is what makes the stream establishable.
//
// The negative assertions below are load-INDEPENDENT because of this: the
// subscriber cannot attach while the handler is still answering 503, no matter
// how fast or slow the machine is. Nothing here asserts on elapsed time.
func attachStub(t *testing.T) (*httptest.Server, func(), *int32) {
	t.Helper()
	var open int32      // 0 = /event refuses, 1 = /event streams
	var eventHits int32 // how many times /event was actually requested
	gate := make(chan struct{})
	done := make(chan struct{})

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			// Shape matters: healthProbe requires healthy=true, not merely
			// a 200. A wrong body here makes the probe spin forever and the
			// test hang rather than fail.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"healthy":true,"version":"test"}`))
		case "/event":
			atomic.AddInt32(&eventHits, 1)
			if atomic.LoadInt32(&open) == 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// Established: headers out, then hold the stream open. A stream
			// that closed immediately would send runSubscriber down the
			// OnSubscriberStopped path and test teardown instead of steady
			// state.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-done:
			case <-r.Context().Done():
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// Ordering is load-bearing: t.Cleanup is LIFO, so releasing the held
	// stream must be registered AFTER hs.Close to run BEFORE it. Registered
	// the other way round, httptest's Close waits on the in-flight /event
	// handler which is itself waiting on done — a clean deadlock that hangs
	// the package rather than failing it.
	t.Cleanup(hs.Close)
	t.Cleanup(func() { close(done) })

	openUp := func() {
		atomic.StoreInt32(&open, 1)
		close(gate)
	}
	return hs, openUp, &eventHits
}

func attachTestServer(t *testing.T, hs *httptest.Server) *Server {
	t.Helper()
	s := newServer("attach-agent", serverConfig{workDir: t.TempDir()})
	s.baseURL = hs.URL
	s.http = hs.Client()
	return s
}

// The core of the fix: while /event is refusing, the health probe already
// succeeds — which is precisely the window in which the old code returned from
// Start and let a prompt through. waitForSubscriber must report NOT attached
// there, and attached once the stream is up.
func TestStartSequence_HealthPassesBeforeSubscriberAttaches(t *testing.T) {
	hs, openUp, eventHits := attachStub(t)
	s := attachTestServer(t, hs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runSubscriber(ctx)

	// Health is ready immediately — the old Start would return here.
	if err := s.healthProbe(ctx); err != nil {
		t.Fatalf("healthProbe: %v", err)
	}

	// ...but the stream is not, so a prompt sent now would be unobserved.
	if s.waitForSubscriber(ctx, 100*time.Millisecond) {
		t.Fatal("reported subscribed while /event was still refusing — " +
			"this is the #1722 window, prompts sent here complete unobserved")
	}
	if got := atomic.LoadInt32(eventHits); got == 0 {
		t.Fatal("subscriber never attempted GET /event — the connect loop is not running, " +
			"so the negative assertion above proves nothing")
	}

	openUp()

	if !s.waitForSubscriber(ctx, 10*time.Second) {
		t.Fatal("never reported subscribed after /event began serving 200")
	}
}

// markSubscribed must fire on the stream establishing, not on a decoded event:
// gating readiness on a frame would make it depend on the server choosing to
// speak, and opencode sends server.connected only after some delay of its own.
func TestRunSubscriber_MarksSubscribedOnStreamNotOnFirstEvent(t *testing.T) {
	hs, openUp, _ := attachStub(t)
	s := attachTestServer(t, hs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runSubscriber(ctx)
	openUp()

	if !s.waitForSubscriber(ctx, 10*time.Second) {
		t.Fatal("not subscribed after the stream established")
	}
	// The stub sends NO events at all — only headers. Subscribed must still
	// be true, which is the distinction this test exists to hold.
	select {
	case <-s.subscribedCh():
	default:
		t.Fatal("subscribed channel not closed despite an established stream")
	}
}

// A subprocess that dies during the wait must release Start rather than hold
// it for the full bound — the server is gone, waiting cannot help.
func TestWaitForSubscriber_ReleasesOnSubprocessDeath(t *testing.T) {
	hs, _, _ := attachStub(t)
	s := attachTestServer(t, hs)
	s.done = make(chan struct{})

	go func() { close(s.done) }()

	if s.waitForSubscriber(context.Background(), time.Minute) {
		t.Fatal("reported subscribed after the subprocess died")
	}
}

// The bound is the backstop: Start proceeds with a WARN rather than blocking
// forever when the stream never attaches.
func TestWaitForSubscriber_ReleasesWhenBoundElapses(t *testing.T) {
	hs, _, _ := attachStub(t)
	s := attachTestServer(t, hs)

	if s.waitForSubscriber(context.Background(), time.Millisecond) {
		t.Fatal("reported subscribed without the stream ever establishing")
	}
}

// A cancelled context (Close tearing the Server down mid-Start) must also
// release the wait.
func TestWaitForSubscriber_ReleasesOnContextCancel(t *testing.T) {
	hs, _, _ := attachStub(t)
	s := attachTestServer(t, hs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.waitForSubscriber(ctx, time.Minute) {
		t.Fatal("reported subscribed after ctx cancellation")
	}
}
