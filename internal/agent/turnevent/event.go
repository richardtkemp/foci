// Package turnevent defines the event stream produced by the agent during a
// turn and the Sink interface consumers implement to receive it.
//
// The agent emits a strictly ordered sequence of events per turn:
//
//	TurnStart → (any number of content events) → TurnComplete
//
// TurnStart opens the stream; TurnComplete is terminal and always fires (the
// agent emits it via defer so it is reliable on error paths). Between them,
// the agent emits TextDelta/TextBlock, ThinkingDelta/ThinkingBlock, ToolCall/
// ToolResult, RetryNotice/RetrySuccess, and Activity events as appropriate.
//
// Contract:
//   - Emit is sequential within a single turn (single-producer invariant);
//     sinks do not need internal locks for intra-turn state.
//   - Callers observe final sink state after the agent's HandleMessage returns
//     (happens-before via the agent's internal turn completion signal).
//   - All per-turn platform-facing state — including typing indicator lifecycle
//     — flows through the stream. There is no out-of-band wiring.
package turnevent

import (
	"context"
	"encoding/json"

	"foci/internal/provider"
)

// Event is the sum type of all events the agent emits during a turn.
// Implementations are closed to this package; external code switches on the
// concrete type.
type Event interface {
	turnEvent()
}

// Phase distinguishes intermediate text blocks emitted mid-turn from the
// final text block that settles at turn completion.
type Phase int

const (
	// PhaseIntermediate is a text block delivered before the turn completes
	// (e.g. a tool-loop reply between tool calls, or a mid-turn CC assistant
	// message in a delegated turn).
	PhaseIntermediate Phase = iota
	// PhaseFinal is the last text block of the turn. Rarely emitted directly;
	// the final text is normally carried by TurnComplete.FinalText.
	PhaseFinal
)

// TurnStart is the first event of every turn. Sinks that manage turn-scoped
// state (typing indicators, stream writers) should initialise in response.
type TurnStart struct{}

// TextDelta carries a streaming fragment of assistant text as it arrives from
// the model. Consumers that want edit-in-place streaming UI handle this;
// consumers that only want completed text can ignore it.
type TextDelta struct {
	Delta string
}

// TextBlock carries a complete, logical chunk of assistant text. Intermediate
// blocks arrive mid-turn (e.g. replies between tool calls); final blocks are
// rare — the canonical source of final text is TurnComplete.FinalText.
type TextBlock struct {
	Text  string
	Phase Phase
}

// SubagentText carries a complete text block produced by a subagent (a Task/
// Agent tool invocation). GroupKey identifies the originating subagent (its
// parent tool_use id) so platforms that support per-subagent message control
// — Telegram's rolling "Hide this" button — can group a subagent's messages,
// roll the control forward to the newest, and delete the set on demand. Text
// is the already-formatted (blockquoted) body. Platforms without such support
// render it as ordinary intermediate text.
type SubagentText struct {
	GroupKey string
	Text     string
}

// ThinkingDelta carries a streaming fragment of extended-thinking output.
type ThinkingDelta struct {
	Delta string
}

// ThinkingBlock carries a complete extended-thinking block.
type ThinkingBlock struct {
	Text string
}

// ToolCall announces that the agent is invoking a tool.
type ToolCall struct {
	Name string
	ID   string
	Args json.RawMessage
}

// ToolResult announces the outcome of a tool invocation. Name is included in
// addition to the tool-use ID so sinks can dispatch to platform observers
// without having to correlate with a prior ToolCall.
type ToolResult struct {
	Name    string
	ID      string
	Output  string
	IsError bool
}

// RetryNotice fires when the agent retries an upstream API call. Endpoint is
// a human-readable label (e.g. "Anthropic API").
type RetryNotice struct {
	Attempt  int
	Endpoint string
	Err      error
}

// RetrySuccess fires when a retry succeeds, so UI can clear any retry notice.
type RetrySuccess struct{}

// Activity is a heartbeat emitted during long silent stretches (e.g. an
// in-flight tool execution with no intervening text) so sinks can refresh
// liveness indicators without coupling to content events.
type Activity struct{}

// TurnComplete is the terminal event of every turn. It carries the final
// accumulated text and usage. The agent emits it via defer so it always
// fires, including on error paths (Err will be non-nil in that case).
type TurnComplete struct {
	FinalText string
	Usage     *provider.Usage
	Cost      float64
	Model     string
	Err       error
}

func (TurnStart) turnEvent()     {}
func (TextDelta) turnEvent()     {}
func (TextBlock) turnEvent()     {}
func (SubagentText) turnEvent()  {}
func (ThinkingDelta) turnEvent() {}
func (ThinkingBlock) turnEvent() {}
func (ToolCall) turnEvent()      {}
func (ToolResult) turnEvent()    {}
func (RetryNotice) turnEvent()   {}
func (RetrySuccess) turnEvent()  {}
func (Activity) turnEvent()      {}
func (TurnComplete) turnEvent()  {}

// Sink receives events during a turn. One sink per turn; sinks are constructed
// fresh in each caller and attached to the turn context via WithSink.
//
// DeliversToPlatform reports whether this sink ultimately routes output to a
// platform.Connection where a user can see it (Telegram, Discord, etc.).
// Returns false for sinks that discard everything (NopSink), buffer for an
// in-process caller (BufferSink), or otherwise lack a user-facing destination.
// Delegating sinks (router, logging, late-delivery wrappers) forward the
// answer from their inner sink.
//
// The inbox's session worker consults this — via a separate Agent-level query
// keyed on session base — to decide whether a freshly arrived Telegram message
// can fold into the currently in-flight turn or must wait for a fresh turn
// with its own delivering sink (see TODO #767 / inbox.go sessionWorker).
type Sink interface {
	Emit(ctx context.Context, ev Event)
	DeliversToPlatform() bool
}
