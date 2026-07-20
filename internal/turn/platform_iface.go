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

// SubagentDeliverer is an optional interface a Platform may implement to take
// over delivery of subagent (Task tool) progress messages, enabling per-subagent
// UI such as Telegram's rolling "Hide this" button. Platforms that don't
// implement it receive subagent text as ordinary intermediate replies via the
// renderer's OnReply fallback.
type SubagentDeliverer interface {
	// DeliverSubagentStart signals a subagent run (groupKey) began; label is the
	// agent's description. runIndex is 1 for the initial spawn, 2+ for a
	// SendMessage reactivation of the same subagent; prompt is the main agent's
	// instruction for this run (#1355). Lets a platform open a collapsed entry the
	// run's text attaches to.
	DeliverSubagentStart(groupKey, label string, runIndex int, prompt string)
	// DeliverSubagentText delivers one subagent progress block for groupKey. The
	// renderer applies blockquote unless SubagentTextRaw reports true, so the
	// platform receives text already in its preferred presentation. runIndex is
	// the run the block belongs to (#1355), for platforms that split runs.
	DeliverSubagentText(groupKey, text string, runIndex int)
	// DeliverSubagentEnd signals the subagent run (groupKey, runIndex) completed,
	// letting the platform finalize that run's UI (e.g. flip a collapsed entry to
	// "completed").
	DeliverSubagentEnd(groupKey string, runIndex int)
	// DeliverSubagentPrompt delivers a SendMessage follow-up sent to a subagent
	// that is STILL RUNNING (#1419) — attaches to the ALREADY-OPEN run (groupKey,
	// runIndex), not a new one. Platforms without a per-run "asked" concept can
	// no-op this (e.g. render it via DeliverSubagentText instead).
	DeliverSubagentPrompt(groupKey, prompt string, runIndex int)
	// SubagentTextRaw reports whether this platform wants subagent text raw (the
	// app, which renders traces in an expandable view) rather than blockquoted
	// (telegram, whose inline messages read as blockquotes).
	SubagentTextRaw() bool
}

// SessionSubagentDeliverer is the session-keyed counterpart of SubagentDeliverer,
// used by the late-delivery SessionSink (which holds no renderer/backend, only a
// bare connection). A connection implementing it keeps subagent framing on the
// out-of-turn path — text arriving after the spawning turn cleared still reaches
// the per-subagent chit rather than flattening into a plain chat message.
// Connections that don't implement it fall back to SendToSession as before.
type SessionSubagentDeliverer interface {
	DeliverSubagentStartToSession(sessionKey, groupKey, label string, runIndex int, prompt string)
	DeliverSubagentTextToSession(sessionKey, groupKey, text string, runIndex int)
	DeliverSubagentEndToSession(sessionKey, groupKey string, runIndex int)
	DeliverSubagentPromptToSession(sessionKey, groupKey, prompt string, runIndex int)
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
