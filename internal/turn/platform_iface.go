package turn

import (
	"errors"

	"foci/internal/log"
)

// Payload is the platform-neutral description of a terminal delivery. The
// renderer builds it; the platform owns all layout (formatting, chopping at
// its own char limit, message identity, thinking-button placement).
type Payload struct {
	Text         string // raw response text (markdown); sentinel already stripped
	ThinkingText string // raw thinking; "" if none
	ThinkingMode string // "off" | "compact" | "full" (already resolved by turn)
}

// DeliveryResult reports the message IDs Deliver created/used, in order.
type DeliveryResult struct{ MsgIDs []string }

// ErrTooLongForEdit is returned by EditInPlace when the payload would need to
// split across >1 message and so cannot replace one existing message in place.
var ErrTooLongForEdit = errors.New("turn: payload too long to edit in place")

// Platform is the single interface the renderer uses to reach a chat platform.
// The platform owns ALL layout: formatting, chopping at its own char limit,
// message identity, streaming rollover, and thinking-button placement.
type Platform interface {
	// OpenStream begins a live streaming surface.
	OpenStream() StreamSink
	// Deliver performs a terminal delivery; it reuses the stream's message
	// sequence if that stream surfaced. The full text is passed uncut — the
	// platform splits as needed; turn never truncates.
	Deliver(p Payload, stream StreamSink) (DeliveryResult, error)
	// EditInPlace replaces an existing message (a tool-call preview) in place.
	// Returns ErrTooLongForEdit if the payload would need to chop across >1
	// message.
	EditInPlace(msgID string, p Payload) error
	// SendTyping sends a typing indicator.
	SendTyping()
	// Logger returns the component logger.
	Logger() *log.ComponentLogger
}

// StreamSink is the live streaming handle returned by OpenStream. It is
// platform-side and owns the live message sequence internally. Update is
// called ONLY by the turn-side pump goroutine; Close is called by
// StreamBuffer.Finish AFTER the pump has stopped; MsgIDs is read by the
// renderer AFTER Finish. So Update/Close/MsgIDs are never concurrent
// (happens-before via the pump's done channel) — but implementations should
// keep a mutex anyway for -race safety / defensive correctness.
type StreamSink interface {
	// Update is given the full accumulated text; the platform chops/caps/rolls
	// over as needed. Idempotent.
	Update(fullText string)
	// Close stops accepting updates and reports whether any message surfaced.
	Close() (surfaced bool)
	// MsgIDs returns the live sequence IDs, in order; empty if nothing surfaced.
	MsgIDs() []string
}
