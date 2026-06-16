package platform

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
)

// ButtonCallback is called when an interactive message button is pressed.
// Receives the chosen button. Returns the text to replace the message with
// (empty string = no edit).
type ButtonCallback func(choice ButtonChoice) string

// expiredInteractiveText replaces an interactive message that auto-expired
// before the user responded.
const expiredInteractiveText = "⌛ This request expired."

// ConnResolver lazily resolves the live Connection for a prompt at fire time.
// Storing a resolver — rather than a Connection captured at registration — keeps
// an interactive callback correct when the connection comes and goes: a platform
// reconnect, or a restart where the callback is re-registered (from persisted
// state) before the platform connection is back up. It is re-invoked on each
// proactive edit, so a prompt that lives up to its 24h expiry never edits through
// a stale handle. A resolver may legitimately return nil (no connection yet); the
// caller treats that as "skip the proactive edit", never as a fatal error.
type ConnResolver func() Connection

// interactiveMsg stores the state for an active interactive message.
type interactiveMsg struct {
	resolve  ConnResolver // lazily resolves the connection for later proactive edits; nil = none
	msgID    string       // platform-side message ID, used by CancelInteractiveMessage / expiry
	buttons  []ButtonChoice
	callback ButtonCallback
	onExpire func() // resolves the upstream waiter on expiry (e.g. deny to CC); nil = no-op
	created  time.Time
}

// buttonSender resolves the current connection and returns it as a ButtonSender,
// or nil when there is no live connection or it can't render buttons. Proactive
// edits (cancel/expiry) call this at fire time so they act on the connection that
// is live now, not one captured when the prompt was first registered.
func (m *interactiveMsg) buttonSender() ButtonSender {
	if m.resolve == nil {
		return nil
	}
	if c := m.resolve(); c != nil {
		if bs, ok := c.(ButtonSender); ok {
			return bs
		}
	}
	return nil
}

var (
	imMu    sync.Mutex
	imStore = make(map[string]*interactiveMsg) // promptID → msg
)

// SendInteractiveMessageWithID sends a message with buttons via the connection
// resolve returns, keyed by the caller-supplied id. When a button is pressed, cb
// is called and the message is edited with the return value. Falls back to plain
// text if the connection doesn't support ButtonSender. Callback is auto-expired
// after 24h. Returns the platform-side message id of the posted message (empty on
// the plain-text fallback), so the caller can persist it and address the message
// for later cancel/expiry edits — including after a restart.
//
// resolve is stored, not its result: later proactive edits re-invoke it so they
// act on the connection that is live then (see ConnResolver). It is called once
// here for the initial send; if it returns nil there is no connection to post
// through and an error is returned.
//
// The caller is responsible for uniqueness of id — typically a CC requestID (a
// UUID), which both ensures uniqueness and lets later CancelInteractiveMessage
// calls find the message without maintaining an extra reqID→promptID map.
//
// If id collides with an existing entry in the store, the older entry is
// overwritten silently.
//
// onExpire (may be nil) is invoked if the prompt auto-expires unanswered (see
// CleanupExpiredInteractive) — callers use it to resolve the upstream waiter,
// e.g. send a denial to CC, so an unanswered prompt doesn't orphan the turn.
func SendInteractiveMessageWithID(resolve ConnResolver, id string, text string, buttons []ButtonChoice, cb ButtonCallback, onExpire func()) (string, error) {
	conn := resolve()
	if conn == nil {
		return "", fmt.Errorf("no connection to present interactive message %q", id)
	}
	bs, ok := conn.(ButtonSender)
	if !ok {
		// Fallback: plain text with numbered choices.
		var lines []string
		for i, b := range buttons {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, b.Label))
		}
		return "", conn.SendText(text + "\n\n" + strings.Join(lines, "\n") + "\n\nReply with your choice.")
	}

	// Reserve the slot before sending so the callback router can find it
	// even if the response races with the send returning. msgID is
	// backfilled below once SendTextWithButtons returns it.
	imMu.Lock()
	imStore[id] = &interactiveMsg{
		resolve:  resolve,
		buttons:  buttons,
		callback: cb,
		onExpire: onExpire,
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
		return "", err
	}

	// Backfill msgID for later edits (e.g. CancelInteractiveMessage).
	imMu.Lock()
	if m, ok := imStore[id]; ok {
		m.msgID = msgID
	}
	imMu.Unlock()
	return msgID, nil
}

// RestoreInteractiveCallback re-registers an interactive message's callback after
// a restart, binding it to buttons the platform already displays (the message and
// its inline keyboard survive on the platform's servers across a foci restart;
// only foci's in-memory routing entry was lost). id must equal the promptID the
// buttons were originally created with so their "im:<id>:<idx>" callback data
// routes back here, and buttons must be the same slice in the same order so the
// index carried by each button still resolves to the right choice.
//
// msgID is the platform-side message id, used ONLY for proactive edits
// (CancelInteractiveMessage / expiry); pass "" if unknown — click-driven edits use
// the message reference carried by the incoming callback, so routing and the
// "✅ <label>" edit both work without it. When the original msgID is persisted and
// passed here, a restored prompt is fully first-class: cancel and expiry can edit
// it too. resolve lazily provides the connection for those proactive edits and may
// be nil (edits are then skipped) or return nil at restore time (the connection
// isn't up yet) — it is re-invoked at fire time, by then the connection is live.
// created carries the original post time so the existing expiry sweep measures the
// 24h lifetime from the real start, not from the restart.
//
// If id collides with an existing entry it is overwritten (matching the
// send-path's last-writer-wins semantics).
func RestoreInteractiveCallback(id, msgID string, resolve ConnResolver, buttons []ButtonChoice, cb ButtonCallback, onExpire func(), created time.Time) {
	imMu.Lock()
	imStore[id] = &interactiveMsg{
		resolve:  resolve,
		msgID:    msgID,
		buttons:  buttons,
		callback: cb,
		onExpire: onExpire,
		created:  created,
	}
	imMu.Unlock()
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
	bs := msg.buttonSender()
	if bs == nil || msg.msgID == "" {
		return nil
	}
	return bs.EditMessageText(msg.msgID, finalText)
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

// CleanupExpiredInteractive removes interactive message callbacks older than
// maxAge. For each expired prompt it resolves the upstream waiter via onExpire
// (e.g. a denial to CC, so a turn blocked in WaitForPermission doesn't orphan)
// and edits the message to show it expired. Called periodically from a
// background goroutine.
func CleanupExpiredInteractive(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	imMu.Lock()
	var expired []*interactiveMsg
	for id, msg := range imStore {
		if msg.created.Before(cutoff) {
			expired = append(expired, msg)
			delete(imStore, id)
		}
	}
	imMu.Unlock()

	// Resolve and edit outside the lock: onExpire and EditMessageText may call
	// back into the platform/agent layers, and we must not hold imMu across
	// those (CancelInteractiveMessage and the callback router both take it).
	for _, msg := range expired {
		if msg.onExpire != nil {
			msg.onExpire()
		}
		if bs := msg.buttonSender(); bs != nil && msg.msgID != "" {
			if err := bs.EditMessageText(msg.msgID, expiredInteractiveText); err != nil {
				log.Warnf("interactive", "edit expired message %s: %v", msg.msgID, err)
			}
		}
	}
}
