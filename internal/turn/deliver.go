package turn

import "foci/internal/log"

// ChunkWriter is the per-platform layout surface the shared delivery loop drives.
// Telegram and Discord backends implement it; DeliverChunks/EditChunksInPlace own
// the control flow that was previously duplicated in each backend's Deliver and
// EditInPlace methods. The platform owns formatting (HTML vs Markdown), the char
// limit / split rule, and message-ID representation; the loop here owns the
// edit-existing / send-new / delete-orphan sequencing.
type ChunkWriter interface {
	// ComposeBody builds the message body for the payload per thinking mode and
	// reports whether the last chunk carries a "Show thinking" button (compact
	// mode) plus the raw thinking text to store with it.
	ComposeBody(p Payload) (body string, hasButton bool, thinkingText string)
	// Split chops the body at the platform's char limit, preserving the
	// platform's formatting (HTML tags / code fences). Never returns nil.
	Split(body string) []string
	// SendChunk sends one already-split chunk as a new message; ok=false on a
	// (logged) send failure.
	SendChunk(chunk string) (msgID string, ok bool)
	// SendChunkWithButton sends a chunk carrying the "Show thinking" button and
	// stores the thinking entry keyed on the sent message ID.
	SendChunkWithButton(chunk, thinkingText string) (msgID string, err error)
	// EditChunk edits an existing message's body in place.
	EditChunk(msgID, chunk string) error
	// EditChunkWithButton edits an existing message and (re)attaches the
	// "Show thinking" button, storing the thinking entry.
	EditChunkWithButton(msgID, chunk, thinkingText string) error
	// DeleteMsg deletes a leftover live-stream message (best-effort, self-logs).
	DeleteMsg(msgID string)
	// Logger returns the component logger.
	Logger() *log.ComponentLogger
}

// DeliverChunks performs a terminal delivery: it lays the payload's chunks over
// the stream's existing message sequence (editing in place, appending beyond it,
// or — when nothing surfaced — sending fresh), then deletes any leftover live
// messages. The last chunk carries the thinking button in compact mode.
func DeliverChunks(w ChunkWriter, p Payload, stream StreamSink) (DeliveryResult, error) {
	body, hasButton, thinkingText := w.ComposeBody(p)
	chunks := w.Split(body)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	var ids []string
	if stream != nil {
		ids = stream.MsgIDs()
	}

	var used []string
	for i, chunk := range chunks {
		withButton := i == len(chunks)-1 && hasButton
		if i < len(ids) {
			// Edit the existing message at this position.
			if withButton {
				if err := w.EditChunkWithButton(ids[i], chunk, thinkingText); err != nil {
					return DeliveryResult{MsgIDs: used}, err
				}
			} else if err := w.EditChunk(ids[i], chunk); err != nil {
				w.Logger().Debugf("deliver edit: %v", err)
			}
			used = append(used, ids[i])
			continue
		}
		// Send a new message: a fresh send (no live sequence) or an append
		// beyond the existing one.
		if withButton {
			id, err := w.SendChunkWithButton(chunk, thinkingText)
			if err != nil {
				return DeliveryResult{MsgIDs: used}, err
			}
			used = append(used, id)
		} else if id, ok := w.SendChunk(chunk); ok {
			used = append(used, id)
		}
	}

	// Delete any leftover messages from the live sequence (final shorter than the
	// live stream). When the final needed more chunks than the stream had
	// messages there are no leftovers — min() keeps the slice in bounds.
	for _, orphan := range ids[min(len(chunks), len(ids)):] {
		w.DeleteMsg(orphan)
	}

	return DeliveryResult{MsgIDs: used}, nil
}

// EditChunksInPlace replaces one existing message (a tool-call preview) in place,
// or returns ErrTooLongForEdit when the composed body would need to split across
// more than one message.
func EditChunksInPlace(w ChunkWriter, msgID string, p Payload) error {
	body, hasButton, thinkingText := w.ComposeBody(p)
	chunks := w.Split(body)
	if len(chunks) > 1 {
		return ErrTooLongForEdit
	}
	chunk := body
	if len(chunks) == 1 {
		chunk = chunks[0]
	}
	if hasButton {
		return w.EditChunkWithButton(msgID, chunk, thinkingText)
	}
	return w.EditChunk(msgID, chunk)
}
