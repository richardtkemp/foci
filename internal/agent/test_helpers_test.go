package agent

import (
	"context"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
)

// hmTest wraps HandleMessage with a BufferSink so tests that want the old
// (string, error) return shape can keep their existing assertions without
// having to build a sink + read FinalText() inline at every call site.
//
// If ctx already carries a sink (e.g. a RecordingSink installed by the test
// to assert on intermediate events), both receive every event via a TeeSink
// — the BufferSink captures FinalText, and the caller's sink sees the full
// event stream.
func (a *Agent) hmTest(ctx context.Context, sessionKey, message string) (string, error) {
	buf := turnevent.NewBufferSink()
	existing := turnevent.SinkFromContext(ctx)
	ctx = turnevent.WithSink(ctx, turnevent.NewTeeSink(existing, buf))
	err := a.HandleMessage(ctx, sessionKey, []string{message}, nil)
	return buf.FinalText(), err
}

// hmTestAttachments is the attachment-aware counterpart.
func (a *Agent) hmTestAttachments(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (string, error) {
	buf := turnevent.NewBufferSink()
	existing := turnevent.SinkFromContext(ctx)
	ctx = turnevent.WithSink(ctx, turnevent.NewTeeSink(existing, buf))
	err := a.HandleMessage(ctx, sessionKey, texts, attachments)
	return buf.FinalText(), err
}
