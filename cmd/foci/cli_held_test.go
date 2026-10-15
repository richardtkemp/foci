package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPrintResponse_HeldShownOnStderrOnly pins the CLI half of requirement 6:
// a 202 receipt whose held field says the session is behind background work
// shows it on the stderr receipt line — stdout stays the bare status token
// (the #1787 data-channel contract).
func TestPrintResponse_HeldShownOnStderrOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"queued","session":"clutch/c123","resolved_via":"default","held":"background_work"}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	stdout, stderr := capture(t, func() {
		if err := printResponse(resp); err != nil {
			t.Errorf("printResponse: %v", err)
		}
	})

	if !strings.Contains(stderr, "held=background_work") {
		t.Errorf("stderr receipt omits the held reason — the sender cannot see why nothing happened yet: %q", stderr)
	}
	if !strings.Contains(stderr, "status=queued") {
		t.Errorf("stderr receipt omits the status: %q", stderr)
	}
	if got := strings.TrimRight(stdout, "\n"); got != "queued" {
		t.Errorf("stdout = %q, want the bare queued token — prefixing stdout breaks `foci send | jq` and the queued contract", got)
	}
}

// TestPrintResponse_NoHeldLineUnchanged is the characterisation pin: a
// receipt without a held field prints today's stderr line, unchanged.
func TestPrintResponse_NoHeldLineUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"queued","session":"clutch/c123","resolved_via":"default"}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	stdout, stderr := capture(t, func() {
		if err := printResponse(resp); err != nil {
			t.Errorf("printResponse: %v", err)
		}
	})

	if strings.Contains(stderr, "held=") {
		t.Errorf("stderr gained a held segment for an unheld receipt: %q", stderr)
	}
	if !isoPrefix.MatchString(stderr) || !strings.Contains(stderr, "status=queued") {
		t.Errorf("stderr receipt changed shape: %q", stderr)
	}
	if got := strings.TrimRight(stdout, "\n"); got != "queued" {
		t.Errorf("stdout = %q, want the bare queued token", got)
	}
}
