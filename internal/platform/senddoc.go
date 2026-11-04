package platform

import (
	"errors"
	"os"
)

// ErrNoSender is SendDocAndRemove's error when there is no connection to send
// through: the document was dropped, which must not read as a delivery.
var ErrNoSender = errors.New("no live connection")

// SendDocAndRemove delivers the temp file at path via s — to chatID if
// nonzero, otherwise s's default chat — and then removes path, regardless of
// whether the send succeeded or s is nil. A failed (or skipped) send must not
// leak the temp file any more than a successful one keeps it around: every
// transport hand-rolled this "send then remove" pair, and the one that
// dropped the `os.Remove` line leaked hundreds of files over two weeks before
// anyone noticed (#1511). path == "" is a no-op (mirrors the `if X.DocPath !=
// ""` guard every prior call site had).
//
// A document that did not go out is lost once this returns, so the failure is
// logged here, where no caller can forget it (#2760). The error is also
// returned (ErrNoSender for a nil s) for a caller that can tell the user;
// a caller with nobody to tell discards it.
func SendDocAndRemove(s Sender, chatID int64, path, caption string) error {
	if path == "" {
		return nil
	}
	defer func() { _ = os.Remove(path) }()
	err := sendDoc(s, chatID, path, caption)
	if err != nil {
		platformLog.Warnf("document not delivered, removing it (chat %d): %v", chatID, err)
	}
	return err
}

func sendDoc(s Sender, chatID int64, path, caption string) error {
	if s == nil {
		return ErrNoSender
	}
	if chatID != 0 {
		return s.SendDocumentToChat(chatID, path, caption)
	}
	return s.SendDocument(path, caption)
}
