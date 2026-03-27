package platform

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ButtonCallback is called when an interactive message button is pressed.
// Receives the chosen button. Returns the text to replace the message with
// (empty string = no edit).
type ButtonCallback func(choice ButtonChoice) string

// interactiveMsg stores the state for an active interactive message.
type interactiveMsg struct {
	buttons  []ButtonChoice
	callback ButtonCallback
	created  time.Time
}

var (
	imMu      sync.Mutex
	imStore   = make(map[string]*interactiveMsg) // promptID → msg
	imCounter uint64
)

// nextPromptID generates a unique prompt ID.
func nextPromptID() string {
	n := atomic.AddUint64(&imCounter, 1)
	return strconv.FormatUint(n, 36)
}

// SendInteractiveMessage sends a message with buttons via the connection.
// When a button is pressed, cb is called and the message is edited with
// the return value. Falls back to plain text if the connection doesn't
// support ButtonSender. Callback is auto-expired after 24h.
func SendInteractiveMessage(conn Connection, text string, buttons []ButtonChoice, cb ButtonCallback) error {
	bs, ok := conn.(ButtonSender)
	if !ok {
		// Fallback: plain text with numbered choices.
		var lines []string
		for i, b := range buttons {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, b.Label))
		}
		return SendText(conn, text+"\n\n"+strings.Join(lines, "\n")+"\n\nReply with your choice.")
	}

	promptID := nextPromptID()

	// Store the callback before sending so it's available immediately.
	imMu.Lock()
	imStore[promptID] = &interactiveMsg{
		buttons:  buttons,
		callback: cb,
		created:  time.Now(),
	}
	imMu.Unlock()

	// Callback data: "im:<promptID>:<buttonIndex>"
	var imButtons []ButtonChoice
	for i, b := range buttons {
		imButtons = append(imButtons, ButtonChoice{
			Label: b.Label,
			Data:  promptID + ":" + strconv.Itoa(i),
		})
	}

	_, err := bs.SendTextWithButtons(text, imButtons, "im:")
	if err != nil {
		// Clean up on failure.
		imMu.Lock()
		delete(imStore, promptID)
		imMu.Unlock()
		return err
	}
	return nil
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
