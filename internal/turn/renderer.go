package turn

import (
	"errors"
	"strings"

	"foci/internal/platform"
)

// ToolTracker exposes the subset of tool call tracker state needed by the renderer.
type ToolTracker interface {
	// LastMsgID returns the current tool-call message ID ("" if none).
	LastMsgID() string
	// ResetMsgID clears the tool-call message ID.
	ResetMsgID()
	// CleanupPreview deletes the tool call preview message if one exists.
	CleanupPreview()
}

// TurnRenderer encapsulates all per-turn rendering state: streaming, thinking
// accumulation, tool call tracking, and response finalization. It collapses
// the combinatorial explosion of finalization code paths into a single
// Finalize method. The delivered/skip-re-delivery flag lives on the
// StreamingSink that wraps the renderer (internal/turn/sink.go) — the renderer
// itself is stateless across OnReply → Finalize boundaries.
//
// The renderer is platform-agnostic: it never measures text against a char
// limit and never truncates. It hands the FULL response text to the platform
// via Payload; the platform owns all layout (chopping, message identity,
// streaming rollover, thinking-button placement). This is the #738 guarantee.
type TurnRenderer struct {
	platform Platform
	tracker  ToolTracker
	display  TurnDisplay
	sw       *StreamBuffer
	newSB    func() *StreamBuffer
	thinking strings.Builder

	// thinkingPhase is true while thinking deltas are being streamed live
	// (compact mode + streaming). Reset when the first text delta arrives.
	thinkingPhase bool
	// streamedThinkingLive is true when thinking was written to the current
	// StreamBuffer, so the Content() fallback knows to strip it.
	streamedThinkingLive bool
}

// NewTurnRenderer creates a TurnRenderer with the given platform, tracker, and
// display settings. The newSB factory creates fresh StreamBuffers for each
// segment (it calls platform.OpenStream() and captures the streaming interval).
// OnReply/Finalize recreate the buffer after each terminal delivery.
func NewTurnRenderer(p Platform, tracker ToolTracker, display TurnDisplay, newSB func() *StreamBuffer) *TurnRenderer {
	return &TurnRenderer{
		platform: p,
		tracker:  tracker,
		display:  display,
		sw:       newSB(),
		newSB:    newSB,
	}
}

// Cleanup finishes the stream buffer if it hasn't been finished yet.
// Safe to call from defer — Finish is idempotent.
func (r *TurnRenderer) Cleanup() {
	r.sw.Finish()
}

// OnReply handles intermediate text delivery invoked by the StreamingSink on
// TextBlock events. Intermediate replies never carry a thinking button
// (matching the historical OnReply behaviour).
//
// Silencing gate: silent text (sentinels, empty) skips delivery entirely.
// This is the authoritative gate for intermediate text.
func (r *TurnRenderer) OnReply(text string) {
	// Strip a leading spurious token (e.g. the "court" decoding artifact on
	// injected turns) before anything else, then strip trailing silencing
	// sentinel(s). An agent that appends "[[NO_RESPONSE]]" to a real reply
	// leaves real content; deliver the content without the marker. Text that is
	// *entirely* sentinel/junk strips to "" and takes the silent branch below.
	text = platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
	if text == "" {
		// Silent intermediate text — clean up state without delivering. The
		// silencing gate in StreamBuffer.OnDelta keeps the sink from surfacing
		// a message in the first place; this branch just stops the (already
		// non-surfacing) buffer and clears any lingering tool preview.
		r.platform.Logger().Debugf("OnReply: silent text, skipping delivery")
		r.sw.Finish()
		r.tracker.CleanupPreview()
		r.resetStream()
		return
	}

	sink, surfaced := r.sw.Finish()
	r.platform.Logger().Debugf("OnReply: non-silent text (len=%d), stream_surfaced=%v", len(text), surfaced)

	// Intermediate replies carry no thinking button — pass plain text. The full
	// (uncut) text goes to Deliver; the platform splits as needed (#738).
	p := Payload{Text: text}

	if surfaced {
		// The reply content already surfaced via the live stream. Deliver
		// terminally, reusing the stream's message sequence, and clean up any
		// lingering tool preview.
		if _, err := r.platform.Deliver(p, sink); err != nil {
			r.platform.Logger().Errorf("OnReply: deliver (stream) failed: %v", err)
		}
		r.tracker.CleanupPreview()
	} else {
		// No stream surfaced. Try to overwrite the tool-call preview in place;
		// otherwise deliver as a fresh message (the sink has no msgIDs, so
		// Deliver sends new).
		if !r.tryEditPreview(p) {
			if _, err := r.platform.Deliver(p, sink); err != nil {
				r.platform.Logger().Errorf("OnReply: deliver failed: %v", err)
			}
		}
		r.tracker.ResetMsgID()
	}

	r.resetStream()
}

// resetStream recreates the stream buffer for the next segment and clears the
// per-segment thinking-phase flags.
func (r *TurnRenderer) resetStream() {
	r.sw = r.newSB()
	r.thinkingPhase = false
	r.streamedThinkingLive = false
}

// streamTextContent returns the text-only portion of the stream buffer.
// When thinking was streamed live, it strips the thinking + divider prefix.
// Safe to call after Finish (Content() still works post-Finish).
func (r *TurnRenderer) streamTextContent() string {
	content := r.sw.Content()
	if !r.streamedThinkingLive {
		return content
	}
	// Thinking was streamed into the buffer — extract text after divider.
	if idx := strings.Index(content, "\n\n---\n\n"); idx >= 0 {
		return content[idx+len("\n\n---\n\n"):]
	}
	// No divider means only thinking arrived, no text.
	return ""
}

// tryEditPreview attempts to overwrite the tool-call preview message in place
// with the payload. Returns true when the edit succeeded. Falls back to false
// when there's no preview, the mode isn't "preview", or the payload would chop
// across messages (ErrTooLongForEdit) — in which case the preview is cleaned up
// so the fresh split-send path doesn't leave an orphan. A genuine edit error
// also returns false (caller falls through to a fresh send).
//
// Finalize additionally guards this behind !hasThinking, since previews can't
// carry thinking buttons.
func (r *TurnRenderer) tryEditPreview(p Payload) bool {
	editID := r.tracker.LastMsgID()
	if editID == "" || r.display.ShowToolCalls != "preview" {
		return false
	}
	err := r.platform.EditInPlace(editID, p)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrTooLongForEdit):
		r.tracker.CleanupPreview()
		return false
	default:
		r.platform.Logger().Debugf("edit tool preview: %v", err)
		return false
	}
}

// OnThinkingDelta streams a thinking fragment to the current stream buffer
// for live per-token progress. Does not accumulate into the builder — the
// terminal ThinkingBlock (or OnThinking for non-streaming turns) is the
// source of truth for the finalization button text.
//
// No-op when thinking display is off, when live streaming is disabled for
// the display mode, or when the StreamOutput config is false.
func (r *TurnRenderer) OnThinkingDelta(delta string) {
	mode := r.display.ShowThinking
	if mode == "off" || mode == "" {
		return
	}
	if (mode != "compact" && mode != "true") || !r.display.StreamOutput {
		return
	}
	if delta == "" {
		return
	}
	r.sw.OnDelta(delta)
	r.thinkingPhase = true
	r.streamedThinkingLive = true
	r.platform.SendTyping()
}

// OnThinking accumulates a complete thinking block for finalization and,
// when no per-token streaming has already written to the stream buffer,
// also streams the full block in one chunk (legacy behaviour for callers
// that don't emit ThinkingDelta events).
func (r *TurnRenderer) OnThinking(thinking string) {
	mode := r.display.ShowThinking
	if mode == "off" || mode == "" {
		return
	}
	if r.thinking.Len() > 0 {
		r.thinking.WriteString("\n")
	}
	r.thinking.WriteString(thinking)

	// Stream the full block if per-token deltas didn't already write to the
	// stream buffer. When OnThinkingDelta has already fired for this block,
	// streamedThinkingLive is set and we skip to avoid duplicating the text.
	if r.streamedThinkingLive {
		return
	}
	if (mode == "compact" || mode == "true") && r.display.StreamOutput {
		r.sw.OnDelta(thinking)
		r.thinkingPhase = true
		r.streamedThinkingLive = true
		r.platform.SendTyping()
	}
}

// OnTextDelta handles streaming delta callbacks: updates the stream buffer
// and refreshes the typing indicator. When transitioning from a live thinking
// phase (compact mode), inserts a divider so thinking and response are
// visually separated during streaming.
func (r *TurnRenderer) OnTextDelta(delta string) {
	if r.thinkingPhase {
		r.sw.OnDelta("\n\n---\n\n")
		r.thinkingPhase = false
	}
	r.sw.OnDelta(delta)
	r.platform.SendTyping()
}

// OnActivity refreshes the typing indicator when tools complete.
func (r *TurnRenderer) OnActivity() {
	r.platform.SendTyping()
}

// Finalize renders the final agent response. It handles all combinations of
// streaming/non-streaming, thinking modes, and tool call previews by building
// a single Payload and handing it to the platform, which owns layout.
//
// Finalize is invoked exclusively on the "not-yet-delivered" path — the
// StreamingSink owns the delivered flag and calls Cleanup()+tracker.CleanupPreview()
// directly when intermediate delivery already happened.
//
// Silencing gate: silent responses (sentinels, empty) skip delivery entirely.
// The check is applied AFTER the stream-content fallback so an empty FinalText
// that the buffer fills in with real content is not mistaken for silence.
func (r *TurnRenderer) Finalize(response string) {
	// Finish the stream buffer. The agent's tool-loop accumulator only exposes
	// text from the *last* API call via FinalText — when response is empty but
	// the stream has content, fall back to the stream buffer so the message is
	// finalised.
	sink, surfaced := r.sw.Finish()
	r.platform.Logger().Debugf("Finalize: response_len=%d stream_surfaced=%v", len(response), surfaced)
	if textContent := r.streamTextContent(); strings.TrimSpace(response) == "" && strings.TrimSpace(textContent) != "" {
		r.platform.Logger().Debugf("Finalize: response was empty, falling back to stream buffer (len=%d)", len(textContent))
		response = textContent
	}

	// Strip a leading spurious token then trailing silencing sentinel(s) before
	// delivery — covers both the FinalText path and the stream-buffer fallback
	// above. A response that is entirely sentinel/junk strips to "" and takes
	// the silent branch below.
	response = platform.StripSilencingSuffix(platform.StripSpuriousPrefix(response))

	if response == "" {
		r.platform.Logger().Debugf("Finalize: silent response, skipping delivery")
		r.tracker.CleanupPreview()
		return
	}
	r.platform.Logger().Debugf("Finalize: delivering non-silent response (len=%d)", len(response))

	thinkingText := r.thinking.String()
	mode := r.display.ShowThinking
	hasThinking := thinkingText != "" && mode != "off" && mode != ""

	// Resolve the thinking mode into the platform-neutral Payload vocabulary.
	// Current semantics: "true" → full-combined body, "compact" → button on
	// last chunk, everything else → plain.
	p := Payload{Text: response}
	if hasThinking {
		p.ThinkingText = thinkingText
		switch mode {
		case "true":
			p.ThinkingMode = "full"
		case "compact":
			p.ThinkingMode = "compact"
		default:
			p.ThinkingMode = "off"
		}
	} else {
		p.ThinkingMode = "off"
	}

	// Stream surfaced: deliver terminally, reusing the stream's message
	// sequence. The platform handles thinking-button placement and rollover.
	if surfaced {
		if _, err := r.platform.Deliver(p, sink); err != nil {
			r.platform.Logger().Errorf("Finalize: deliver (stream) failed: %v", err)
		}
		r.tracker.CleanupPreview()
		return
	}

	// No stream surfaced. Try the tool-preview edit, but only when there's no
	// thinking (previews can't carry thinking buttons — matching the historical
	// guard).
	if !hasThinking && r.tryEditPreview(p) {
		return
	}

	// Fresh terminal delivery. CleanupPreview first so no orphan preview
	// lingers next to the new message(s).
	r.tracker.CleanupPreview()
	if _, err := r.platform.Deliver(p, sink); err != nil {
		r.platform.Logger().Errorf("Finalize: deliver failed: %v", err)
	}
}
