package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
	_ "foci/internal/delegator/ccstream" // registers the claude-code spec
	"foci/internal/turnevent"
)

// #3070: a turn whose backend ends mid-turn is not a success. These run a real
// claude-code backend (ccstream) over a stub binary, so the turn ends through
// the transport's own EOF path (finalizeExit), not a fake that returns an
// error. The agent turn must end with an error matching
// delegator.ErrTurnInterrupted; a background spawn reads that error as a cut
// run and, during a shutdown, keeps the spawn for resume
// (TestSpawnCutByShutdownLeftForNextProcess covers that half).

// interruptedTurnAgent is a delegated agent whose claude-code backend runs
// body as its binary.
func interruptedTurnAgent(t *testing.T, body string) (*Agent, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	spec, ok := delegator.SpecFor("claude-code")
	if !ok {
		t.Fatal("claude-code spec not registered")
	}
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return spec.New(map[string]any{"binary": script}) },
		Spec:       spec,
		StartOpts:  delegator.StartOptions{WorkDir: dir, AgentID: "interrupted"},
	}
	t.Cleanup(mgr.Close)
	return &Agent{Model: "test-model", DelegatedManager: mgr}, dir
}

// stubInit opens a stub CC: the system/init line that makes the backend
// ready (WaitReady), then the stub reads the turn's input from stdin.
const stubInit = "#!/bin/sh\n" +
	`echo '{"type":"system","subtype":"init","session_id":"S1","model":"stub-model","tools":[]}'` + "\n"

// runInterruptedTurn runs one turn and returns HandleMessage's error and what
// the turn's sink saw.
func runInterruptedTurn(t *testing.T, a *Agent) (error, *turnevent.BufferSink) {
	t.Helper()
	buf := turnevent.NewBufferSink()
	done := make(chan error, 1)
	go func() {
		done <- a.HandleMessage(turnevent.WithSink(context.Background(), buf), "test/s", []string{"hello"}, nil)
	}()
	select {
	case err := <-done:
		return err, buf
	case <-time.After(30 * time.Second):
		t.Fatal("the turn never ended after its backend died")
		return nil, nil
	}
}

func assertInterrupted(t *testing.T, err error, buf *turnevent.BufferSink) {
	t.Helper()
	if err == nil {
		t.Fatalf("HandleMessage returned nil for a turn whose backend died mid-turn (final text %q): reported as a success", buf.FinalText())
	}
	if !errors.Is(err, delegator.ErrTurnInterrupted) {
		t.Fatalf("HandleMessage error = %v, want one matching delegator.ErrTurnInterrupted", err)
	}
	if !buf.Done() || !errors.Is(buf.Err(), delegator.ErrTurnInterrupted) {
		t.Fatalf("the turn's sink saw done=%v err=%v, want a TurnComplete carrying the interruption", buf.Done(), buf.Err())
	}
}

// The CC process exits on its own once it has the turn's input: stdout EOF.
func TestDelegatedTurn_BackendExitMidTurnIsAnError(t *testing.T) {
	a, _ := interruptedTurnAgent(t, stubInit+
		"while read -r line; do case \"$line\" in *'\"type\":\"user\"'*) exit 3;; esac; done\n")
	err, buf := runInterruptedTurn(t, a)
	assertInterrupted(t, err, buf)
}

// The backend is closed under the running turn, as foci's shutdown closes
// every backend after the drain deadline (cmd/foci-gw/shutdown.go).
func TestDelegatedTurn_BackendClosedMidTurnIsAnError(t *testing.T) {
	dirMarker := filepath.Join(t.TempDir(), "has-input")
	a, _ := interruptedTurnAgent(t, stubInit+
		"while read -r line; do case \"$line\" in *'\"type\":\"user\"'*) : > '"+dirMarker+"'; exec sleep 60;; esac; done\n")
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			if _, err := os.Stat(dirMarker); err == nil {
				a.DelegatedManager.Close()
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	err, buf := runInterruptedTurn(t, a)
	assertInterrupted(t, err, buf)
}
