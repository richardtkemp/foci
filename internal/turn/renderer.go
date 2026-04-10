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
// accumulation, tool call tracking, and response finalization. It collapses the
// combinatorial explosion of finalization code paths into a single Finalize method.
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
	// replyDelivered is true when OnReply delivered content to the user
	// during the turn (via the watcher's replyFunc for delegated agents).
	// Finalize skips re-delivery when this is set. Stream deltas are NOT
	// counted — they need Finalize to edit-in-place with final formatting.
	replyDelivered bool
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

// OnReply handles intermediate text delivery (ReplyFunc callback).
// When streaming is active, the text was already delivered via the stream
// writer — finalize that message and clean up any tool call preview. Otherwise,
// overwrite the tool call preview with the reply text (preview mode) or send
// a new message.
//
// Bug fix: previously, the non-streaming fallback was guarded by
// "else if !streamOutput", which dropped text when streaming was configured
// but no stream deltas arrived. Now always delivers when no stream message exists.
func (r *TurnRenderer) OnReply(text string) {
	msgID := r.sw.Finish()
	if msgID != "" {
		// Streaming: reply content is in the stream message. Finalize it
		// and delete any lingering tool call preview.
		content := r.streamTextContent()
		if strings.TrimSpace(content) != "" {
			formatted := r.backend.FormatResponse(content)
			_ = r.backend.EditMessage(msgID, formatted)
		}
		r.tracker.CleanupPreview()
		r.replyDelivered = true
	} else {
		// No stream message. Always deliver — this fixes the bug where text
		// was dropped when streaming was enabled but no deltas arrived.
		if !r.editToolPreviewWithReply(text) {
			r.backend.SendReply(text)
		}
		r.tracker.ResetMsgID()
		r.replyDelivered = true
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

// OnThinking accumulates thinking blocks (gated by showThinking config).
// In compact mode with live streaming, thinking is also fed into the
// StreamWriter so users can read it while the model thinks. At finalization
// the message is edited to show only the response with a thinking button.
func (r *TurnRenderer) OnThinking(thinking string) {
	mode := r.display.ShowThinking
	if mode == "off" || mode == "" {
		return
	}
	if r.thinking.Len() > 0 {
		r.thinking.WriteString("\n")
	}
	r.thinking.WriteString(thinking)

	// Stream thinking live so there's visible progress while the model
	// thinks. For compact mode, the message is collapsed to a button at
	// finalization. For true mode, it's reformatted with proper styling.
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
// streaming/non-streaming, thinking modes, response length, and tool call previews.
//
// When the renderer already delivered content during the turn (via OnReply or
// streaming), Finalize only does cleanup — it won't send the response again.
// This prevents double delivery when HandleMessage returns FinalText for
// delegated agents whose watcher already streamed the response.
func (r *TurnRenderer) Finalize(response string) {
	// Finish the stream writer and get the message ID it created (if any).
	//
	// During a turn, model text is delivered two ways simultaneously:
	//   1. TextDeltaObserver -> stream writer (real-time edits)
	//   2. ReplyFunc (agent loop splits a turn -- nudges, deferred replies)
	// Without streaming, only #2 exists. With streaming, both fire for the
	// same text; we suppress #2 (see OnReply) and rely on the stream writer.
	//
	// The agent loop's return value only contains text from the *last* API
	// call. When response is empty but the stream has content, use the
	// stream's buffer so the message gets properly finalized.
	streamMsgID := r.sw.Finish()
	if textContent := r.streamTextContent(); strings.TrimSpace(response) == "" && strings.TrimSpace(textContent) != "" {
		response = textContent
	}

	// Content was already delivered via OnReply during the turn (delegated
	// agents deliver via watcher's replyFunc). Clean up tool previews but
	// don't re-send the response.
	if r.replyDelivered {
		r.tracker.CleanupPreview()
		return
	}

	// Guard against empty and silent responses (e.g. [[NO_RESPONSE]] sentinel).
	if platform.IsSilent(response) {
		r.backend.Logger().Debugf("silent response, not sending")
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
