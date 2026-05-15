//go:build integration

package integration

import (
	"testing"
	"time"

	"foci/internal/testharness"
)

// TestL2_Files_DocumentSaved_PathInjectedToAgent proves that a non-image,
// non-PDF, non-convertible document arriving over Telegram travels
// through bot_receive.handleMediaMessage, lands on disk in the agent's
// received_files_dir, and the resulting "[Document saved to: <path>]"
// tag is what cc-stub sees as its user_message text. Wire under test:
// Document update → downloadAndSaveMedia → saveMedia → injected path
// tag → Agent.HandleMessage → cc-stub recorder user_message entry.
func TestL2_Files_DocumentSaved_PathInjectedToAgent(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_PhotoAttachment_ReachesAgent proves photo updates take
// the content-block attachment path (not save-to-disk). A PushUpdate
// with Photo[] should cause foci to download the largest size, attach
// it to the QueuedMessage, and forward it to the agent — cc-stub
// receives an attachment alongside any caption text. Wire under test:
// Photo update → downloadAttachment → platform.Attachment → user
// envelope content blocks.
func TestL2_Files_PhotoAttachment_ReachesAgent(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_PDFUnderLimit_GoesViaAttachment proves that a PDF
// document smaller than the 32MB content-block cap takes the
// attachment path (downloadAttachment → content block), NOT the
// save-to-disk path. Distinguishes the small-PDF branch in
// bot_receive.go from the over-size branch.
func TestL2_Files_PDFUnderLimit_GoesViaAttachment(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_PDFOverLimit_FallsBackToDiskSave proves a PDF over the
// 32MB content-block cap takes the save-to-disk path with a
// "[PDF saved to: <path>]" tag, not the attachment path. Asserts the
// branching threshold in bot_receive.handlePDF for file_size > 32MB.
func TestL2_Files_PDFOverLimit_FallsBackToDiskSave(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_VideoAttachment_SavedAndPathInjected proves Video
// messages route through handleMediaMessage (save-to-disk + path tag)
// rather than the attachment path. Wire under test: Video update →
// downloadAndSaveMedia with extForVideo → "[Video saved to: <path>]"
// prepended to caption, both forwarded to agent.
func TestL2_Files_VideoAttachment_SavedAndPathInjected(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_VideoNoteAttachment_SavedAndPathInjected proves the
// VideoNote branch (circular video) wires through handleMediaMessage
// with the "videonote" mediaType label and .mp4 extension, distinct
// from the Video branch. Asserts the saved-path tag uses "Video" as
// the human label.
func TestL2_Files_VideoNoteAttachment_SavedAndPathInjected(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_ConvertibleDoc_NormalizedMIMEReachesAgent proves that a
// document whose MIME type is in platform.IsConvertibleDocMIME (docx,
// xlsx, html, csv, text/plain etc.) takes the attachment path with a
// normalized MIME — the agent layer sees the canonical type, not the
// raw Telegram-reported MIME. Wire under test: Document update with
// convertible MIME → NormalizeMIME → downloadAttachment → attachment
// reaching the agent with canonical mime_type.
func TestL2_Files_ConvertibleDoc_NormalizedMIMEReachesAgent(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatFile_RecordsSendDocument proves the egress
// half of the file pipeline: cc-stub scripted to emit a Bash tool_use
// running `foci_send_to_chat --description ... --file ...`, exec
// bridge dispatches send_to_chat, which calls SendDocumentToChat on
// the platform — Telegram stub records a sendDocument multipart
// request. Wire under test: scripted Bash tool_use → execbridge
// dispatch → send_to_chat tool → bot.SendDocument → sendDocument API
// call recorded.
func TestL2_Files_SendToChatFile_RecordsSendDocument(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatVideo_RoutesToSendVideo proves the send_as
// switch in tools/message.go: send_as=video must route through
// SendVideoToChat → sendVideo API call, NOT SendDocumentToChat. Wire
// under test: scripted send_to_chat with --send-as video → sendVideo
// (not sendDocument) recorded on the Telegram stub.
func TestL2_Files_SendToChatVideo_RoutesToSendVideo(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatPhoto_RoutesToSendPhoto proves the send_as=photo
// branch lands on the sendPhoto API, distinct from sendDocument. Counter-
// part of the video routing test for the photo branch of the switch.
func TestL2_Files_SendToChatPhoto_RoutesToSendPhoto(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatAnimation_RoutesToSendAnimation proves that
// send_as=animation routes through SendAnimationToChat → sendAnimation
// API call, distinct from sendDocument. Closes the GIF branch of the
// send_as switch.
func TestL2_Files_SendToChatAnimation_RoutesToSendAnimation(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatAudio_RoutesToSendAudio proves the send_as=audio
// branch lands on sendAudio, distinct from both sendDocument and
// sendVoice. Audio and voice are separate Telegram surfaces (sendVoice
// only for OGG-Opus voice notes; sendAudio for general music files).
func TestL2_Files_SendToChatAudio_RoutesToSendAudio(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatFile_WithCaption_CaptionIncluded proves that
// short text (<=MaxCaptionLen=1024) accompanying a file rides along as
// the document's caption in a SINGLE sendDocument call — no separate
// sendMessage. Counterpart of the long-text fallback test below.
func TestL2_Files_SendToChatFile_WithCaption_CaptionIncluded(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatFile_LongTextFallsBackToTwoMessages proves
// that text longer than platform.MaxCaptionLen (1024) cannot ride as a
// caption — message.go must split into a separate sendMessage AND an
// uncaptioned sendDocument. Verifies both calls land on the Telegram
// stub in the expected order.
func TestL2_Files_SendToChatFile_LongTextFallsBackToTwoMessages(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatFile_CustomFilename_DisplaysAsRequested
// proves the symlink-rename trick in tools/message.go: when --filename
// is supplied, the tool symlinks the source into a temp dir under the
// requested basename, and openMediaFile sends that basename — the
// Telegram multipart body should reference the custom filename, not
// the source path's basename.
func TestL2_Files_SendToChatFile_CustomFilename_DisplaysAsRequested(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatMarkdownCaption_RegressionFor771 is the
// regression net for TODO #771 — markdown in a caption must reach
// Telegram correctly when send_to_chat is invoked with text+file
// under MaxCaptionLen. Today the markdown caption path goes plain
// (no parse_mode), so bold/italic markup arrives unformatted. Wire
// under test: scripted send_to_chat with markdown caption → recorded
// sendDocument multipart body must contain markdown formatting AND a
// parse_mode hint (once the bug is fixed).
func TestL2_Files_SendToChatMarkdownCaption_RegressionFor771(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_CaptionOnPhotoUpdate_BecomesAgentText proves a
// Telegram message whose Text is empty but Caption is set (the normal
// shape for media uploads) still produces a non-empty user message
// for the agent — bot_receive falls back to msg.Caption when
// msg.Text is empty. cc-stub recorder's text_prefix should contain
// the caption.
func TestL2_Files_CaptionOnPhotoUpdate_BecomesAgentText(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_ReplyToMessageWithFile_QuoteContextPreserved proves
// that when a user replies-to a previous message AND attaches a file,
// the reply context "[Replying to: <quoted>]" is prepended to the
// text BEFORE the saved-path tag, and the whole thing reaches the
// agent in one user message. Asserts the ordering: quote header,
// saved-path tag, original caption.
func TestL2_Files_ReplyToMessageWithFile_QuoteContextPreserved(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_Photo_DownloadFails_AgentStillSeesText is a negative
// path: when downloadAttachment errors (e.g. getFile returns 5xx or
// the file body 404s), the attachment is dropped but the user's
// caption text still reaches the agent as a normal user message. No
// crash, no silent message drop. Wire under test: failure in
// downloadFile → att{} skipped → text-only message enqueued.
func TestL2_Files_Photo_DownloadFails_AgentStillSeesText(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_Document_TooLarge_SizeWarningPrepended is a negative
// path: a Document update whose FileSize exceeds the 20MB Bot API
// limit must trigger fileTooLargeError BEFORE attempting download —
// agent sees "[Document too large to download (NN MB)]" prepended to
// the caption. No file written to received_files_dir.
func TestL2_Files_Document_TooLarge_SizeWarningPrepended(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_VideoTooLarge_AnnotatedNotSaved is the Video-branch
// twin of the document-too-large test: oversized Video → annotated
// "[Video too large to download]" text, no file in received_files_dir,
// agent still receives the user message.
func TestL2_Files_VideoTooLarge_AnnotatedNotSaved(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_VoiceWithoutTranscriber_UserGetsErrorMessage is a
// negative path that doesn't require a real STT provider: a Voice
// update with no transcriber configured must cause foci to sendReply
// "Voice notes require an STT provider..." and silently drop the
// message — recorder should show no user_message entry for the
// voice. Wire under test: msg.Voice != nil && b.transcriber == nil
// → sendReply + return false from buildReceivedMessage.
func TestL2_Files_VoiceWithoutTranscriber_UserGetsErrorMessage(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatFile_MissingFile_ReturnsError is a negative
// path: agent invokes send_to_chat --file pointing at a path that
// doesn't exist. The tool's openMediaFile call returns an error; the
// exec bridge surfaces it as a non-zero exit / error result. No
// sendDocument call should reach the Telegram stub.
func TestL2_Files_SendToChatFile_MissingFile_ReturnsError(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChat_FilenameWithoutFile_ReturnsError is a
// negative path covering the validation rule in tools/message.go:
// "filename requires file". A send_to_chat call with --filename but
// no --file must error before any platform call.
func TestL2_Files_SendToChat_FilenameWithoutFile_ReturnsError(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChat_EmptyTextAndFile_ReturnsError is a negative
// path covering the "at least one of text or file is required"
// validation. A send_to_chat call with neither text nor file must
// error; no sendMessage or sendDocument lands on the Telegram stub.
func TestL2_Files_SendToChat_EmptyTextAndFile_ReturnsError(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_UnauthorizedUserSendsDocument_NoFileSaved is a negative
// path covering the auth gate: a Document update from a user_id NOT
// in allowed_users must be silently dropped — no file in
// received_files_dir, no user_message in the cc-stub recorder, no
// sendMessage outbound. Wire under test: allowedUsers check in
// buildReceivedMessage returning false BEFORE downloadAttachment runs.
func TestL2_Files_UnauthorizedUserSendsDocument_NoFileSaved(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Files_SendToChatVoice_WithTextSynthesizesTTS proves the TTS
// branch in tools/message.go: send_to_chat with send_as=voice and
// text (no file) calls tts.Synthesize and then SendVoiceDataToChat —
// the Telegram stub records a sendVoice multipart upload, NOT a
// sendMessage with the text. Requires extending testharness with a
// stub TTS implementation that emits canned audio bytes (no real
// OpenAI/ElevenLabs round-trip).
func TestL2_Files_SendToChatVoice_WithTextSynthesizesTTS(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}
