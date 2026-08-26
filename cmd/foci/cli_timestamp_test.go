package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ISO-8601 with an offset, as time.RFC3339 emits: 2026-08-26T22:40:00+01:00
var isoPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2}) `)

// capture runs fn with stdout and stderr replaced by pipes, returning both.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr
	done := make(chan [2]string, 1)
	go func() {
		o, _ := io.ReadAll(rOut)
		e, _ := io.ReadAll(rErr)
		done <- [2]string{string(o), string(e)}
	}()
	fn()
	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	got := <-done
	return got[0], got[1]
}

// Every agent's crontab appends stdout AND stderr to one shared cron.log. An
// undated line there cannot be attributed to a job or a day — 4,591 "skipped"
// and 229 "HTTP 412" lines were indistinguishable, which is how a three-day
// delivery gap went unnoticed (#1787).
//
// The fix must not be bought with stdout: that is a data channel (`foci send |
// jq`, and the single-token "queued" contract). So this asserts BOTH halves —
// stderr gains a stamp, stdout keeps none.
func TestPrintResponse_StampsStderrAndLeavesStdoutClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"skipped: session recently active","session":"clutch/c123","resolved_via":"default"}`))
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

	if !isoPrefix.MatchString(stderr) {
		t.Errorf("stderr line has no ISO timestamp, so it is still unattributable in cron.log: %q", stderr)
	}
	if !strings.Contains(stderr, "status=skipped: session recently active") {
		t.Errorf("stderr receipt omits the status, leaving the undated stdout token as the only record: %q", stderr)
	}
	if !strings.Contains(stderr, "clutch/c123") {
		t.Errorf("stderr receipt omits the session, so the line names no job: %q", stderr)
	}

	// stdout must be byte-identical to the old contract.
	if got := strings.TrimRight(stdout, "\n"); got != "skipped: session recently active" {
		t.Errorf("stdout = %q, want the bare status token — prefixing stdout breaks `foci send | jq` and the queued contract", got)
	}
	if isoPrefix.MatchString(stdout) {
		t.Errorf("stdout was timestamped; it is a data channel and must stay clean: %q", stdout)
	}
}

// A response carrying real content must still print that content, unprefixed.
func TestPrintResponse_ResponseBodyStaysUnprefixed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":"hello world"}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	stdout, _ := capture(t, func() { _ = printResponse(resp) })
	if got := strings.TrimRight(stdout, "\n"); got != "hello world" {
		t.Errorf("stdout = %q, want %q verbatim", got, "hello world")
	}
}
