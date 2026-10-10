package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/defersend"
	"foci/internal/log"
	"foci/internal/timeutil"
)

// captureEventLog redirects the event log into a buffer for the duration of
// the test and raises the level to DEBUG, so a leaked text is caught no
// matter which level it was logged at. Restores stderr and INFO on cleanup.
func captureEventLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetLevel(log.DEBUG)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetLevel(log.INFO)
	})
	return &buf
}

// logLineWith returns the first captured line containing every given
// substring, failing the test when no line matches.
func logLineWith(t *testing.T, out string, substrs ...string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		matches := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				matches = false
				break
			}
		}
		if matches {
			return line
		}
	}
	t.Fatalf("no log line contains all of %q:\n%s", substrs, out)
	return ""
}

// enqueueCommand queues one deferred command record that is deliverable on
// the very first sweep (wait_cold holds immediately on a never-touched
// session) and returns the store-assigned id for log-line assertions.
func enqueueCommand(t *testing.T, store *defersend.Store, text string) int64 {
	t.Helper()
	now := timeutil.Now()
	id, err := store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: text, WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSweep_DeliveredCommandLogsMetadataOnly is the red test for #2259: a
// deferred command's result text must never reach the log at any level — a
// command's output can carry secrets (/pair mints a single-use pairing key
// into its text). The delivery line records metadata only: id, agent,
// session, kind and the length of the text.
func TestSweep_DeliveredCommandLogsMetadataOnly(t *testing.T) {
	const marker = "DEFERRED-RESULT-MARKER-2259"
	resultText := "deferred echo result " + marker
	var runs atomic.Int32
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{{
		Name: "echo2259",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			runs.Add(1)
			return command.Response{Text: resultText}, nil
		},
	}}})
	buf := captureEventLog(t)
	store := withDeferStore(t, &d)
	id := enqueueCommand(t, store, "/echo2259")

	sweepFor(d, store).sweep()

	if n := runs.Load(); n != 1 {
		t.Errorf("command executed %d time(s), want 1", n)
	}
	out := buf.String()
	if strings.Contains(out, marker) {
		t.Errorf("log contains the command result text %s — a deferred command's output must never be logged:\n%s", marker, out)
	}
	line := logLineWith(t, out, "[defersend]", "INFO", fmt.Sprintf("deferred command %d delivered", id))
	for _, want := range []string{
		"agent=" + testAgentID + " ",
		"session=" + testSessionKey + " ",
		"kind=command ",
		fmt.Sprintf("text_len=%d)", len(resultText)),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("delivery line missing %q: %s", want, line)
		}
	}
}

// TestSweep_DeferredPairKeyNotLogged pins #2259 on the real command: /pair
// returns a freshly minted single-use pairing key in its result text, on the
// promise that the key is never persisted or logged. The deferred delivery
// path used to break that promise by writing the text to the log at INFO.
// The stub mints an obviously fake marker, never a real key.
func TestSweep_DeferredPairKeyNotLogged(t *testing.T) {
	const fakeKey = "FAKE-PAIRKEY-MARKER-2259"
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{command.PairKeyCommand()}})
	d.agents[testAgentID].cc.AndroidDeps = &command.AndroidDeps{
		MintPairKey: func(ttl time.Duration) (string, time.Time, error) {
			return fakeKey, time.Now().Add(ttl), nil
		},
	}
	buf := captureEventLog(t)
	store := withDeferStore(t, &d)
	// No host argument: with one the command also emits a foci://pair string
	// and a QR file; neither is needed to prove the key stays out of the log.
	id := enqueueCommand(t, store, "/pair")

	sweepFor(d, store).sweep()

	out := buf.String()
	if strings.Contains(out, fakeKey) {
		t.Errorf("log contains the minted pairing key marker %s — a pairing key must never be logged:\n%s", fakeKey, out)
	}
	logLineWith(t, out, "[defersend]", "INFO", fmt.Sprintf("deferred command %d delivered", id))
}

// TestSweep_NotExecutableCommandLogsNameOnly is the red test for the #2259
// Clutch addition: a deferred command that turns out not executable (a
// REGISTERED command with no Execute function) used to log the full command
// line — arguments included — at WARN. /pass echoes its arguments in its
// result text, so deferred arguments leaked the same way as result texts.
// The WARN must carry the command name only, never its arguments.
func TestSweep_NotExecutableCommandLogsNameOnly(t *testing.T) {
	const argMarker = "NOEXEC-ARG-MARKER-2259"
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{
		{Name: "noexec2259"}, // registered, but no Execute: reaches the !ok branch
	}})
	buf := captureEventLog(t)
	store := withDeferStore(t, &d)
	id := enqueueCommand(t, store, "/noexec2259 "+argMarker)

	sweepFor(d, store).sweep()

	out := buf.String()
	if strings.Contains(out, argMarker) {
		t.Errorf("log contains the command argument marker %s — the not-executable warning must log the command name only:\n%s", argMarker, out)
	}
	line := logLineWith(t, out, "[defersend]", "WARN", fmt.Sprintf("deferred command %d not executable", id))
	if !strings.Contains(line, "/noexec2259") {
		t.Errorf("not-executable warning lost the command name /noexec2259: %s", line)
	}
}
