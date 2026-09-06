package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"foci/internal/log"
)

// TestSend_LogsTruncatedBody is the regression test for #1838: the "[http]
// send" INFO line used to write the whole prompt body inline, unbounded. A
// routine window-grep of foci.log (the standard first move in almost every
// investigation) could therefore pull KBs of private prompt content into an
// unrelated agent's context purely by substring coincidence. The line must
// stay greppable/auditable without holding a message body.
func TestSend_LogsTruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	mux := newTestMux(d)

	longBody := "FLAVOUR SEED (private — never quote or mention to Dick): " + strings.Repeat("private detail. ", 200)

	w := postJSON(mux, "/send", `{"text":"`+longBody+`"}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	out := buf.String()
	var sendLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[http]") && strings.Contains(line, "send (agent=") {
			sendLine = line
			break
		}
	}
	if sendLine == "" {
		t.Fatalf("no [http] send line found in log output:\n%s", out)
	}

	if strings.Contains(sendLine, longBody) {
		t.Errorf("send log line contains the FULL %d-byte prompt body verbatim, want a truncated preview:\n%s", len(longBody), sendLine)
	}
	if len(sendLine) > 400 {
		t.Errorf("send log line is %d bytes, want a short bounded preview (line: %s)", len(sendLine), sendLine)
	}
	if !strings.Contains(sendLine, "never quote or mention to Dick") {
		// Not a hard requirement, just documents that a short prefix is retained
		// for greppability — remove this if the fix picks a different preview shape.
		t.Logf("note: truncated preview did not retain the private marker prefix — line: %s", sendLine)
	}
}
