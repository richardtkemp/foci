package agent

import (
	"context"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
)

// fnSink is a closure-to-Sink adapter used by tests in this package to
// observe the event stream without defining one-off types per test. Lives
// in a *_test.go file (and therefore invisible to deadcode) so the
// production turnevent package stays free of test-only utilities; the
// production sinks are all concrete (StreamingSink, SessionSink, BufferSink),
// none of which expose per-event hooks tests need for assertions.
type fnSink func(context.Context, turnevent.Event)

// Emit implements turnevent.Sink.
func (f fnSink) Emit(ctx context.Context, ev turnevent.Event) { f(ctx, ev) }

// DeliversToPlatform implements turnevent.Sink. Tests that adapt closures
// through fnSink are observing the event stream, not delivering — but they
// also wrap an existing sink whose answer ought to surface. Returning true
// here keeps existing test wiring (which expects the in-flight tracking and
// gate logic to treat the test sink as delivering, matching the platform
// sinks it stands in for) behaviourally stable.
func (f fnSink) DeliversToPlatform() bool { return true }

// hmTest wraps HandleMessage with a fan-out sink that preserves any
// previously-attached sink (so tests that want to observe intermediate
// events keep working) and additionally captures TurnComplete.FinalText so
// tests can assert on the (string, error) shape they used pre-refactor.
func (a *Agent) hmTest(ctx context.Context, sessionKey, message string) (string, error) {
	return a.hmTestAttachments(ctx, sessionKey, []string{message}, nil)
}

// hmTestAttachments is the attachment-aware counterpart.
func (a *Agent) hmTestAttachments(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (string, error) {
	existing := turnevent.SinkFromContext(ctx)
	var finalText string
	ctx = turnevent.WithSink(ctx, fnSink(func(c context.Context, ev turnevent.Event) {
		existing.Emit(c, ev)
		if tc, ok := ev.(turnevent.TurnComplete); ok {
			finalText = tc.FinalText
		}
	}))
	err := a.HandleMessage(ctx, sessionKey, texts, attachments)
	return finalText, err
}
