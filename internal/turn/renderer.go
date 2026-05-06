package turn

import (
	"strings"

	"foci/internal/log"
	"foci/internal/platform"
)

// TurnBackend provides platform-specific message rendering operations.
type TurnBackend interface {
	// FormatResponse converts raw response text to platform format
	// (e.g. markdown to HTML for Telegram, passthrough for Discord).
	FormatResponse(text string) string

	// SendReply sends text as a response message (auto-formats and chunks).
	SendReply(text string)

	// SendChunked sends pre-formatted text, splitting into platform-appropriate chunks.
	SendChunked(formatted string)

	// EditMessage edits a message with pre-formatted text.
	EditMessage(msgID string, formatted string) error

	// SendWithThinkingButton sends formatted response with a thinking toggle
	// button on the last chunk. Stores thinking data internally.
	SendWithThinkingButton(formatted string, thinkingText string) error

	// EditWithThinkingButton edits a message with formatted text and a thinking
	// toggle button. Stores thinking data internally. Only stores if edit succeeds.
	EditWithThinkingButton(msgID string, formatted string, thinkingText string) error

	// BuildThinkingCombined builds a combined thinking + divider + response string
	// in platform-specific format.
	BuildThinkingCombined(responseFormatted string, thinkingText string) string

	// FormatStreamPreview formats a truncated preview string for a stream message
	// that was replaced by a full response below.
	FormatStreamPreview(preview string) string

	// SendTyping sends a typing indicator.
	SendTyping()

	// Logger returns the component logger.
	Logger() *log.ComponentLogger
}

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
type TurnRenderer struct {
	backend  TurnBackend
	tracker  ToolTracker
	display  TurnDisplay
	sw       *StreamWriter
	newSW    func() *StreamWriter
	thinking strings.Builder

	// thinkingPhase is true while thinking deltas are being streamed live
	// (compact mode + streaming). Reset when the first text delta arrives.
	thinkingPhase bool
	// streamedThinkingLive is true when thinking was written to the current
	// StreamWriter, so the Content() fallback knows to strip it.
	streamedThinkingLive bool
}

// NewTurnRenderer creates a TurnRenderer with the given backend, tracker, and display
// settings. The newSW factory creates fresh StreamWriters for each segment (OnReply
// creates a new writer after finishing the previous one).
func NewTurnRenderer(backend TurnBackend, tracker ToolTracker, display TurnDisplay, newSW func() *StreamWriter) *TurnRenderer {
	return &TurnRenderer{
		backend: backend,
		tracker: tracker,
		display: display,
		sw:      newSW(),
		newSW:   newSW,
	}
}

// Cleanup finishes the stream writer if it hasn't been finished yet.
// Safe to call from defer — Finish is idempotent.
func (r *TurnRenderer) Cleanup() {
	r.sw.Finish()
}

// OnReply handles intermediate text delivery invoked by the StreamingSink on
// TextBlock events. When streaming is active, the text was already delivered
// via the stream writer — finalize that message and clean up any tool call
// preview. Otherwise, overwrite the tool call preview with the reply text
// (preview mode) or send a new message.
//
// Silencing gate: silent text (sentinels, empty) skips delivery entirely.
// This is the authoritative gate for intermediate text — every downstream
// delivery method (editToolPreviewWithReply, SendReply, EditMessage on the
// stream message) is reachable only past this check, so no subsequent code
// needs to repeat it.
//
// Bug fix: previously, the non-streaming fallback was guarded by
// "else if !streamOutput", which dropped text when streaming was configured
// but no stream deltas arrived. Now always delivers when no stream message exists.
func (r *TurnRenderer) OnReply(text string) {
	if platform.IsSilent(text) {
		// Silent intermediate text — clean up state without delivering.
		// The streaming-prefix gate in StreamWriter.OnDelta keeps the sw
		// from creating a Telegram message in the first place; this branch
		// just stops the (already-empty) writer and clears any lingering
		// tool preview so there's no orphaned UI.
		r.sw.Finish()
		r.tracker.CleanupPreview()
		r.sw = r.newSW()
		r.thinkingPhase = false
		r.streamedThinkingLive = false
		return
	}
	msgID := r.sw.Finish()
	if msgID != "" {
		// Streaming: reply content is in the stream message. Finalize it
		// and delete any lingering tool call preview.
		content := r.streamTextContent()
		if strings.TrimSpace(content) != "" {
			formatted := r.backend.FormatResponse(content)
			_ = r.backend.EditMessage(msgID, formatted)
		} else if strings.TrimSpace(text) != "" {
			// Stream had no text content (e.g. only thinking deltas arrived,
			// not the reply text), but OnReply has the reply. Edit the
			// existing stream message with the reply — same as the happy path.
			r.backend.Logger().Debugf("OnReply: stream text empty, editing with OnReply text (%d chars)", len(text))
			formatted := r.backend.FormatResponse(text)
			_ = r.backend.EditMessage(msgID, formatted)
		}
		r.tracker.CleanupPreview()
	} else {
		// No stream message. Always deliver — this fixes the bug where text
		// was dropped when streaming was enabled but no deltas arrived.
		if !r.editToolPreviewWithReply(text) {
			r.backend.SendReply(text)
		}
		r.tracker.ResetMsgID()
	}
	// Fresh stream writer for the next segment.
	r.sw = r.newSW()
	r.thinkingPhase = false
	r.streamedThinkingLive = false
}

// streamTextContent returns the text-only portion of the stream buffer.
// When thinking was streamed live, it strips the thinking + divider prefix.
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

// editToolPreviewWithReply edits the tool call preview message with intermediate
// reply text, replacing the tool call indicator with the actual reply content.
// Returns true if the edit succeeded. Falls back to false when there's no
// preview, the mode isn't "preview", or the text is too long to edit in-place.
func (r *TurnRenderer) editToolPreviewWithReply(text string) bool {
	editID := r.tracker.LastMsgID()
	if editID == "" || r.display.ShowToolCalls != "preview" {
		return false
	}
	if strings.TrimSpace(text) == "" || len(text) > r.display.MaxChars {
		r.tracker.CleanupPreview()
		return false
	}
	formatted := r.backend.FormatResponse(text)
	err := r.backend.EditMessage(editID, formatted)
	if err != nil {
		r.backend.Logger().Debugf("edit tool preview with reply: %v", err)
		return false
	}
	return true
}

// OnThinkingDelta streams a thinking fragment to the current stream writer
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
	r.backend.SendTyping()
}

// OnThinking accumulates a complete thinking block for finalization and,
// when no per-token streaming has already written to the stream writer,
// also streams the full block in one chunk (legacy behaviour for callers
// that don't emit ThinkingDelta events).
//
// Kept under the same name so existing tests and non-streaming call sites
// keep working — the split between block and delta concerns is a pure
// extension, not a rename.
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
	// stream writer. When OnThinkingDelta has already fired for this block,
	// streamedThinkingLive is set and we skip to avoid duplicating the text.
	if r.streamedThinkingLive {
		return
	}
	if (mode == "compact" || mode == "true") && r.display.StreamOutput {
		r.sw.OnDelta(thinking)
		r.thinkingPhase = true
		r.streamedThinkingLive = true
		r.backend.SendTyping()
	}
}

// OnTextDelta handles streaming delta callbacks: updates the stream writer
// and refreshes the typing indicator. When transitioning from a live thinking
// phase (compact mode), inserts a divider so thinking and response are
// visually separated during streaming.
func (r *TurnRenderer) OnTextDelta(delta string) {
	if r.thinkingPhase {
		r.sw.OnDelta("\n\n---\n\n")
		r.thinkingPhase = false
	}
	r.sw.OnDelta(delta)
	r.backend.SendTyping()
}

// OnActivity refreshes the typing indicator when tools complete.
func (r *TurnRenderer) OnActivity() {
	r.backend.SendTyping()
}

// Finalize renders the final agent response. It handles all combinations of
// streaming/non-streaming, thinking modes, response length, and tool call
// previews.
//
// Finalize is invoked exclusively on the "not-yet-delivered" path — the
// StreamingSink owns the delivered flag and calls Cleanup()+tracker.CleanupPreview()
// directly when intermediate delivery already happened. This keeps the
// renderer stateless across delivery boundaries.
//
// Silencing gate: silent responses (sentinels, empty) skip delivery entirely.
// This is the authoritative gate for final-text delivery; every downstream
// path inside Finalize is reachable only past this check. The check is
// applied AFTER the stream-content fallback so an empty FinalText that
// the buffer fills in with real content is not mistaken for silence.
func (r *TurnRenderer) Finalize(response string) {
	// Finish the stream writer and get the message ID it created (if any).
	// The agent's tool-loop accumulator only exposes text from the *last*
	// API call via FinalText — when response is empty but the stream has
	// content, fall back to the stream buffer so the message is finalised.
	streamMsgID := r.sw.Finish()
	if textContent := r.streamTextContent(); strings.TrimSpace(response) == "" && strings.TrimSpace(textContent) != "" {
		response = textContent
	}

	if platform.IsSilent(response) {
		// Silent final response — nothing to deliver. The streaming-prefix
		// gate keeps the sw from having created a Telegram message when the
		// content was sentinel-only; clean up any lingering tool preview.
		r.tracker.CleanupPreview()
		return
	}

	thinkingText := r.thinking.String()
	showThinkMode := r.display.ShowThinking
	hasThinking := thinkingText != "" && showThinkMode != "off" && showThinkMode != ""

	// Stream finalization: edit the stream message in-place when possible.
	if streamMsgID != "" && len(response) <= r.display.MaxChars {
		r.finalizeStreamShort(streamMsgID, response, thinkingText, showThinkMode, hasThinking)
		r.tracker.CleanupPreview()
		return
	}

	// Stream message exists but response is too long — send as new message(s)
	// and convert the stream message to a truncated preview.
	if streamMsgID != "" {
		r.sendWithThinkingMode(response, thinkingText, showThinkMode, hasThinking)
		r.editStreamPreview(streamMsgID, response)
		r.tracker.CleanupPreview()
		return
	}

	// No streaming — try editing the tool call preview in-place.
	if r.tryEditToolPreview(response, hasThinking) {
		return
	}

	// Response sent as a new message — clean up any lingering tool call preview.
	r.tracker.CleanupPreview()
	r.sendWithThinkingMode(response, thinkingText, showThinkMode, hasThinking)
}

// finalizeStreamShort edits the stream message in-place with the final response
// (which fits within MaxChars).
func (r *TurnRenderer) finalizeStreamShort(streamMsgID, response, thinkingText, showThinkMode string, hasThinking bool) {
	formatted := r.backend.FormatResponse(response)
	switch {
	case hasThinking && showThinkMode == "compact":
		err := r.backend.EditWithThinkingButton(streamMsgID, formatted, thinkingText)
		if err != nil {
			r.backend.Logger().Errorf("edit stream with thinking button: %v", err)
		}
	case hasThinking && showThinkMode == "true":
		combined := r.backend.BuildThinkingCombined(formatted, thinkingText)
		err := r.backend.EditMessage(streamMsgID, combined)
		if err != nil {
			r.backend.Logger().Errorf("edit stream with full thinking: %v", err)
		}
	default:
		err := r.backend.EditMessage(streamMsgID, formatted)
		if err != nil {
			r.backend.Logger().Debugf("edit stream final: %v (stream already has content)", err)
		}
	}
}

// sendWithThinkingMode sends a response as a new message, applying the
// appropriate thinking display mode.
func (r *TurnRenderer) sendWithThinkingMode(response, thinkingText, showThinkMode string, hasThinking bool) {
	switch {
	case hasThinking && showThinkMode == "true":
		formatted := r.backend.FormatResponse(response)
		combined := r.backend.BuildThinkingCombined(formatted, thinkingText)
		r.backend.SendChunked(combined)
	case hasThinking && showThinkMode == "compact":
		formatted := r.backend.FormatResponse(response)
		err := r.backend.SendWithThinkingButton(formatted, thinkingText)
		if err != nil {
			r.backend.Logger().Errorf("send reply with thinking button: %v", err)
		}
	default:
		r.backend.SendReply(response)
	}
}

// tryEditToolPreview attempts to edit the tool call preview message with the
// final response. Returns true if successful.
func (r *TurnRenderer) tryEditToolPreview(response string, hasThinking bool) bool {
	editID := r.tracker.LastMsgID()
	if editID == "" || r.display.ShowToolCalls != "preview" || hasThinking || len(response) > r.display.MaxChars {
		return false
	}
	formatted := r.backend.FormatResponse(response)
	err := r.backend.EditMessage(editID, formatted)
	if err != nil {
		r.backend.Logger().Debugf("edit final response failed, falling back: %v", err)
		return false
	}
	return true
}

// editStreamPreview edits the stream message to a truncated preview when the
// final response was sent as a separate message (too long, has thinking, etc.).
func (r *TurnRenderer) editStreamPreview(streamMsgID, response string) {
	if streamMsgID == "" {
		return
	}
	preview := truncate(response, 200)
	formatted := r.backend.FormatStreamPreview(preview)
	_ = r.backend.EditMessage(streamMsgID, formatted)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
