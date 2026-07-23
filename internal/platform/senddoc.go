package platform

import "os"

// SendDocAndRemove delivers the temp file at path via s — to chatID if
// nonzero, otherwise s's default chat — and then removes path, regardless of
// whether the send succeeded or s is nil. A failed (or skipped) send must not
// leak the temp file any more than a successful one keeps it around: every
// transport hand-rolled this "send then remove" pair, and the one that
// dropped the `os.Remove` line leaked hundreds of files over two weeks before
// anyone noticed (#1511). path == "" is a no-op (mirrors the `if X.DocPath !=
// ""` guard every prior call site had).
//
// Returns the send error (nil on success, no-op, or nil s) so callers that
// want to log a failed send still can; most existing call sites discard it,
// preserving prior behaviour.
func SendDocAndRemove(s Sender, chatID int64, path, caption string) error {
	if path == "" {
		return nil
	}
	defer func() { _ = os.Remove(path) }()
	if s == nil {
		return nil
	}
	if chatID != 0 {
		return s.SendDocumentToChat(chatID, path, caption)
	}
	return s.SendDocument(path, caption)
}
