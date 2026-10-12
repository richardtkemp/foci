package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"foci/internal/log"
)

// logCapture records event log lines through a log tap (log.AddTap, #2357).
// A tap is additive: it neither swaps the process-wide event output, level
// or warn hook, nor stops other tests' taps, so a test using it can run in
// parallel. The tap sees EVERY test's events, so a positive assertion must
// match on something only this run logs — use runToken for a session name,
// agent id or marker. A negative assertion (a secret marker never logged)
// needs no token: it must hold for every line.
type logCapture struct {
	mu    sync.Mutex
	lines []capturedLine
}

// capturedLine is one tapped event.
type capturedLine struct {
	Level     log.Level
	Component string
	Msg       string
}

// String renders the line like the event log does, minus the timestamp:
// "WARN  [component] message".
func (l capturedLine) String() string {
	return fmt.Sprintf("%-5s [%s] %s", l.Level.String(), l.Component, l.Msg)
}

// captureLog taps every event at or above min until the test ends. min is
// the tap's own threshold: a DEBUG capture sees DEBUG lines while the event
// log itself stays at its level.
func captureLog(t *testing.T, min log.Level) *logCapture {
	t.Helper()
	c := &logCapture{}
	remove := log.AddTap(min, func(level log.Level, component, msg string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, capturedLine{Level: level, Component: component, Msg: msg})
	})
	t.Cleanup(remove)
	return c
}

// Lines returns the captured lines whose String() contains every substring.
func (c *logCapture) Lines(substrs ...string) []capturedLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedLine
	for _, l := range c.lines {
		s := l.String()
		ok := true
		for _, sub := range substrs {
			if !strings.Contains(s, sub) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, l)
		}
	}
	return out
}

// String returns every captured line, one per row, for failure messages and
// whole-log negative checks.
func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, l := range c.lines {
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	return b.String()
}

var runTokenSeq atomic.Int64

// runToken returns a string unique to this call within the test binary
// (prefix-r<6-digit N>), for naming a session, agent or marker that only the
// calling test run logs. The fixed width keeps one token from being a prefix
// of another, so a substring match cannot pick up a sibling run's line. Safe
// in session names (no separators).
func runToken(prefix string) string {
	return fmt.Sprintf("%s-r%06d", prefix, runTokenSeq.Add(1))
}

// The package's tests install no warn hook (they read WARNs through taps), so
// switch the log package from "buffer WARNs for the first hook" to dropping
// them, once, before any test runs: otherwise every WARN the test binary logs
// is buffered for a replay that never comes. Before #2357 the first test that
// cleared its own hook did this as a side effect.
func init() { log.SetWarnHook(nil) }
