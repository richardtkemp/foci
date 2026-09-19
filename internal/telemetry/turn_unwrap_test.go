package telemetry

import (
	"testing"

	"foci/internal/agent/turnevent"
)

// The agent's session router relies on this to see through the tracing
// decoration when deciding whether a ctx sink already is the router (#1944).
func TestTurnSink_UnwrapReturnsInner(t *testing.T) {
	setupTest(t, Options{})
	inner := turnevent.NopSink{}
	sink, turn := NewTurnSink(inner)
	if turn == nil {
		t.Fatal("tracing should be enabled by setupTest")
	}
	if got := turnevent.Unwrap(sink); got != turnevent.Sink(inner) {
		t.Errorf("Unwrap(turnSink) = %T, want the inner sink", got)
	}
}
