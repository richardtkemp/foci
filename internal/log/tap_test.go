package log

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// tapRecorder is a goroutine-safe AddTap subscriber.
type tapRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *tapRecorder) fn(level Level, component, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, level.String()+" ["+component+"] "+msg)
}

func (r *tapRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

// TestAddTap proves a tap (#2357) sees events at its OWN threshold without
// changing the event output or the global level: a DEBUG tap gets a DEBUG
// event the INFO-level event log drops, a WARN tap does not get INFO, the
// formatted message matches the event line's, and after remove no event
// reaches the tap.
func TestAddTap(t *testing.T) {
	buf := captureOutput(t) // the event output: the tap must not change what it gets
	setLevel(INFO)
	t.Cleanup(func() { setLevel(INFO) })

	var debugTap, warnTap tapRecorder
	removeDebug := AddTap(DEBUG, debugTap.fn)
	removeWarn := AddTap(WARN, warnTap.fn)

	Debugf("taptest", "debug %d", 1)
	Infof("taptest", "info line\n")
	Warnf("taptest", "warn %s", "w")

	if got, want := debugTap.got(), []string{"DEBUG [taptest] debug 1", "INFO [taptest] info line", "WARN [taptest] warn w"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("DEBUG tap got %q, want %q", got, want)
	}
	if got, want := warnTap.got(), []string{"WARN [taptest] warn w"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("WARN tap got %q, want %q", got, want)
	}
	out := buf.String()
	if strings.Contains(out, "debug 1") {
		t.Errorf("a DEBUG tap leaked a DEBUG event into the INFO-level event output:\n%s", out)
	}
	if !strings.Contains(out, "[taptest] info line") || !strings.Contains(out, "[taptest] warn w") {
		t.Errorf("event output lost lines while taps were registered:\n%s", out)
	}
	if lvl := getLevel(); lvl != INFO {
		t.Errorf("global level = %v after AddTap(DEBUG), want INFO unchanged", lvl)
	}

	removeDebug()
	removeDebug() // idempotent
	Debugf("taptest", "after remove")
	Warnf("taptest", "warn after debug removed")
	if got := debugTap.got(); len(got) != 3 {
		t.Errorf("removed tap still receives events: %q", got)
	}
	if got := warnTap.got(); len(got) != 2 {
		t.Errorf("WARN tap got %q, want the second warn too (removing one tap must not remove another)", got)
	}
	removeWarn()
	if m := tapMin.Load(); m != noTaps {
		t.Errorf("tapMin = %d with no taps registered, want noTaps", m)
	}

	var after bytes.Buffer
	setOutput(&after)
	Debugf("taptest", "dropped")
	if after.Len() != 0 {
		t.Errorf("DEBUG event reached INFO-level output with no taps: %q", after.String())
	}
}
