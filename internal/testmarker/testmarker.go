// Package testmarker gives test doubles a way to make their fake values
// self-identifying wherever they end up rendered into a component log line.
//
// Some tests stub a func/field that production code calls on a real error
// path — e.g. a "kill the tmux server" function — and inject a canned
// literal like fmt.Errorf("permission denied"). Nothing was actually
// killed and nothing was actually denied, but the resulting WARN/ERROR log
// line (e.g. "kill-server failed: permission denied") is indistinguishable
// from a genuine failure when read later in a shared `make test` log. Use
// Err/ID to mark injected errors and identifiers so a reader (or a grep for
// Prefix) can tell at a glance that a line reflects test data, not a real
// event.
//
// This is NOT for errors a test provokes from real behaviour (e.g. chmod'ing
// a directory read-only and letting a real syscall fail) — those errors are
// genuine and should read that way.
package testmarker

import "fmt"

// Prefix marks a string as originating from a test double rather than a
// real system event. Grep a shared test log for Prefix to confirm a WARN/
// ERROR line reflects injected test data.
const Prefix = "FAKE-TEST"

// Err wraps msg as an error whose text carries Prefix, for stubbed
// functions that inject a canned failure a production code path may log at
// WARN/ERROR, e.g.:
//
//	m.killTmuxFn = func() ([]string, error) { return nil, testmarker.Err("permission denied") }
func Err(msg string) error {
	return fmt.Errorf("%s: %s", Prefix, msg)
}

// ID marks a fake identifier (process name, session name, host name, ...)
// that a stub injects and that may surface in a WARN/ERROR log line, e.g. a
// fake process comm name a test tells production code to "kill".
func ID(name string) string {
	return Prefix + "-" + name
}
