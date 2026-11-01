package app

import (
	"strings"
	"unicode/utf8"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/telemetry"
	"foci/internal/turnevent"
)

// Tool-call transcript content on the app (#999): the app half of what
// Telegram's ToolCallTracker shows. One toolCallEmitter per turn, built by
// appConn.NewTurnSink only when the session's resolved show_tool_calls is
// preview or full; with off (the app default) the sink has none and sends
// exactly what it sent before #999 — the transient activity frame only.

// Size bounds for the content of one tool.call frame, in bytes of UTF-8.
//
// A card is a glance at what the call did, not its log: the full text stays in
// the session transcript. Every frame is kept in the durable replay store and
// replayed to each device that reconnects inside the store TTL, and a busy turn
// makes ~100 calls, so the bounds keep a turn's cards near 1 MiB at worst
// (100 × (4 KiB + 8 KiB)) — well under the 1 MiB-per-frame inbound cap the app
// itself uses (maxFrameSize) for any single frame. Args get the smaller bound:
// a call's arguments that matter (a command, a path, a URL) are short, and
// Telegram's preview shows only 450 characters of them. Results get twice as
// much, since the useful part of an output (an error, a first screen) runs
// longer.
const (
	toolCallArgsMax   = 4 << 10
	toolCallResultMax = 8 << 10
)

// toolCallEmitter turns one turn's ToolCall / ToolResult events into tool.call
// frames on the binding.
type toolCallEmitter struct {
	b    *convBinding
	mode string // "preview" | "full"
	// redact scrubs secrets from args and results. Never nil (newToolCallEmitter
	// installs the generic patterns at least).
	redact func(string) string

	// byID pairs a result with its call by the backend's tool-use id; byName
	// pairs ID-less events in call order per tool name (FIFO), so two
	// overlapping ID-less calls of different tools still pair correctly.
	byID   map[string]string   // backend id -> wire ToolCallID
	byName map[string][]string // tool name -> wire ToolCallIDs, oldest first
}

// toolCallMode maps a resolved show_tool_calls value to the wire mode, or ""
// when the gate is closed. sessionOverride (session_metadata) wins over the
// config value, as on Telegram (Bot.resolveDisplay).
func toolCallMode(configured, sessionOverride string) string {
	v := configured
	if sessionOverride != "" {
		v = sessionOverride
	}
	switch config.ToolCallDisplay(v) {
	case config.ToolCallPreview, config.ToolCallFull:
		return v
	}
	return ""
}

// liveToolCallConfig returns a reader of the app platform's resolved
// show_tool_calls from the live config snapshot (the per-agent-platform →
// agent → global-platform → global cascade). It reads the snapshot on every
// call, so an edit applied live takes effect from the next turn. nil live = nil
// reader (off).
func liveToolCallConfig(live *config.LiveValue[*config.ResolvedAgentConfig]) func() string {
	if live == nil {
		return nil
	}
	return func() string {
		r := live.Load()
		if r == nil {
			return ""
		}
		return r.PlatformDisplay("app").ShowToolCalls
	}
}

// newToolCallEmitter builds the emitter for one turn. agentRedact is the
// agent's own secret-value redaction (agent.Agent.Redact, the secrets store and
// Bitwarden values; nil = none configured). The generic credential patterns the
// trace exporter strips (telemetry.Redactor) run after it, so a token shape no
// store knows about is still scrubbed.
func newToolCallEmitter(b *convBinding, mode string, agentRedact func(string) string) *toolCallEmitter {
	generic := telemetry.NewRedactor(nil)
	return &toolCallEmitter{
		b:    b,
		mode: mode,
		redact: func(s string) string {
			if agentRedact != nil {
				s = agentRedact(s)
			}
			s, _ = generic.Redact(s)
			return s
		},
		byID:   make(map[string]string),
		byName: make(map[string][]string),
	}
}

// call sends the start of a tool call.
func (t *toolCallEmitter) call(e turnevent.ToolCall) {
	id := fap.NewULID()
	if e.ID != "" {
		t.byID[e.ID] = id
	} else {
		t.byName[e.Name] = append(t.byName[e.Name], id)
	}
	args, cut, n := boundUTF8(t.redact(string(e.Args)), toolCallArgsMax)
	f := fap.ToolCall{
		ConversationID: t.b.convID,
		ToolCallID:     id,
		Name:           e.Name,
		State:          fap.ToolCallRunning,
		Mode:           t.mode,
		Args:           args,
		ArgsTruncated:  cut,
	}
	if cut {
		f.ArgsBytes = n
	}
	t.b.send(f)
}

// result sends the completion of a tool call, paired with its start.
func (t *toolCallEmitter) result(e turnevent.ToolResult) {
	id := t.pair(e)
	out, cut, n := boundUTF8(t.redact(e.Output), toolCallResultMax)
	f := fap.ToolCall{
		ConversationID:  t.b.convID,
		ToolCallID:      id,
		Name:            e.Name,
		State:           fap.ToolCallDone,
		Mode:            t.mode,
		Result:          out,
		ResultTruncated: cut,
		IsError:         e.IsError,
	}
	if cut {
		f.ResultBytes = n
	}
	t.b.send(f)
}

// pair returns the wire id of the start e completes, consuming it, or a fresh
// id when there was no start (a lone completion card).
func (t *toolCallEmitter) pair(e turnevent.ToolResult) string {
	if e.ID != "" {
		if id, ok := t.byID[e.ID]; ok {
			delete(t.byID, e.ID)
			return id
		}
		return fap.NewULID()
	}
	if q := t.byName[e.Name]; len(q) > 0 {
		t.byName[e.Name] = q[1:]
		return q[0]
	}
	return fap.NewULID()
}

// boundUTF8 returns s cut to at most max bytes on a rune boundary, whether it
// was cut, and the uncut size. Invalid UTF-8 in s (a raw byte from a tool's
// output) is replaced with U+FFFD first, so the result is always valid UTF-8:
// the JSON encoder would otherwise make that replacement after the cut and
// could push the text back over max.
func boundUTF8(s string, max int) (string, bool, int) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	n := len(s)
	if n <= max {
		return s, false, n
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true, n
}
