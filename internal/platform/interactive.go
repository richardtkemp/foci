package platform

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ButtonCallback is called when an interactive message button is pressed.
// Receives the chosen button. Returns the text to replace the message with
// (empty string = no edit).
type ButtonCallback func(choice ButtonChoice) string

// interactiveMsg stores the state for an active interactive message.
type interactiveMsg struct {
	bs       ButtonSender    // who to call to edit the message later (e.g. for cancellation)
	msgID    string          // platform-side message ID, used by CancelInteractiveMessage
	buttons  []ButtonChoice
	callback ButtonCallback
	created  time.Time
}

var (
	imMu    sync.Mutex
	imStore = make(map[string]*interactiveMsg) // promptID → msg
)

// SendInteractiveMessageWithID sends a message with buttons via the connection,
// keyed by the caller-supplied id. When a button is pressed, cb is called and
// the message is edited with the return value. Falls back to plain text if the
// connection doesn't support ButtonSender. Callback is auto-expired after 24h.
//
// The caller is responsible for uniqueness of id — typically a CC requestID (a
// UUID), which both ensures uniqueness and lets later CancelInteractiveMessage
// calls find the message without maintaining an extra reqID→promptID map.
//
// If id collides with an existing entry in the store, the older entry is
// overwritten silently.
func SendInteractiveMessageWithID(conn Connection, id string, text string, buttons []ButtonChoice, cb ButtonCallback) error {
	bs, ok := conn.(ButtonSender)
	if !ok {
		// Fallback: plain text with numbered choices.
		var lines []string
		for i, b := range buttons {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, b.Label))
		}
		return conn.SendText(text + "\n\n" + strings.Join(lines, "\n") + "\n\nReply with your choice.")
	}

	// Reserve the slot before sending so the callback router can find it
	// even if the response races with the send returning. msgID is
	// backfilled below once SendTextWithButtons returns it.
	imMu.Lock()
	imStore[id] = &interactiveMsg{
		bs:       bs,
		buttons:  buttons,
		callback: cb,
		created:  time.Now(),
	}
	imMu.Unlock()

	// Callback data: "im:<id>:<buttonIndex>"
	var imButtons []ButtonChoice
	for i, b := range buttons {
		imButtons = append(imButtons, ButtonChoice{
			Label: b.Label,
			Data:  id + ":" + strconv.Itoa(i),
		})
	}

	msgID, err := bs.SendTextWithButtons(text, imButtons, "im:")
	if err != nil {
		// Clean up on failure.
		imMu.Lock()
		delete(imStore, id)
		imMu.Unlock()
		return err
	}

	// Backfill msgID for later edits (e.g. CancelInteractiveMessage).
	imMu.Lock()
	if m, ok := imStore[id]; ok {
		m.msgID = msgID
	}
	imMu.Unlock()
	return nil
}

// CancelInteractiveMessage edits the message identified by id to finalText
// (with no buttons) and removes its callback so subsequent clicks become
// no-ops. Idempotent: returns nil if id is unknown (already responded to,
// already cancelled, or never existed).
//
// Used to disable inline keyboards when an upstream event makes the prompt
// moot — for example, when CC cancels a permission request after a
// PriorityNow steer aborted the in-flight tool execution.
func CancelInteractiveMessage(id string, finalText string) error {
	imMu.Lock()
	msg, ok := imStore[id]
	if ok {
		delete(imStore, id)
	}
	imMu.Unlock()
	if !ok {
		return nil
	}
	// Racy edge: cancel arrived between reserve and msgID backfill.
	// The store entry is gone (so future clicks are no-ops), but we
	// can't edit the message because we don't have its ID yet. The
	// orphan keyboard window is bounded by SendTextWithButtons latency.
	if msg.bs == nil || msg.msgID == "" {
		return nil
	}
	return msg.bs.EditMessageText(msg.msgID, finalText)
}

// HandleInteractiveCallback processes a button press for an interactive message.
// callbackData is the full callback data string (with "im:" prefix already stripped).
// Returns the edit text, the chosen button's Data, and whether the callback was found.
func HandleInteractiveCallback(callbackData string) (editText, choiceData string, ok bool) {
	// Format: "<promptID>:<buttonIndex>"
	parts := strings.SplitN(callbackData, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	promptID := parts[0]
	btnIdx, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", "", false
	}

	imMu.Lock()
	msg, found := imStore[promptID]
	if found {
		delete(imStore, promptID) // one-shot: remove after handling
	}
	imMu.Unlock()

	if !found || btnIdx < 0 || btnIdx >= len(msg.buttons) {
		return "", "", false
	}

	choice := msg.buttons[btnIdx]
	edit := ""
	if msg.callback != nil {
		edit = msg.callback(choice)
	}
	return edit, choice.Data, true
}

// CleanupExpiredInteractive removes interactive message callbacks older than 24h.
// Called periodically (e.g. from a background goroutine).
func CleanupExpiredInteractive() {
	cutoff := time.Now().Add(-24 * time.Hour)
	imMu.Lock()
	defer imMu.Unlock()
	for id, msg := range imStore {
		if msg.created.Before(cutoff) {
			delete(imStore, id)
		}
	}
}
