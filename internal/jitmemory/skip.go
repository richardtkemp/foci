package jitmemory

import (
	"strings"
	"time"
	"unicode/utf8"
)

// RecordSkip logs a message the caller decided not to run on, as a run row
// with only its skipped reason set (#3176): a gate that drops every message
// then shows in the run log instead of passing unseen. It does not add the
// message to the session's context (Observe does that). A no-op without a
// Store.
func (e *Engine) RecordSkip(sk string, role Role, text, reason string) {
	store := e.store()
	if store == nil {
		return
	}
	text = strings.TrimSpace(text)
	rec := &RunRecord{
		Time: time.Now(), AgentID: e.deps.AgentID, SessionKey: sk, Role: string(role),
		TextChars: utf8.RuneCountInString(text), TextHead: headRunes(text, 200), Skipped: reason,
	}
	if _, err := store.LogRun(rec); err != nil {
		jitLog.Warnf("session=%s record skipped %s message: %v", sk, role, err)
	}
}
