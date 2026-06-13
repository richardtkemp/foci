package turn

import (
	"strconv"
	"sync"

	"foci/internal/log"
	"foci/internal/tooldetail"
)

// ToolResultEntry is the cached display state for one tool-call message. It is
// used to re-render the message when the user toggles its inline expand/collapse
// button after the message has already been sent.
type ToolResultEntry struct {
	CompactText string // compact one-line summary (collapsed state)
	FullInput   string // full formatted tool call with params
	Result      string // raw tool result text (empty while the tool is running)
	Expanded    bool   // true if the message is currently expanded
}

// ToolResultStore is the shared in-memory cache of tool-call display state,
// keyed by platform message ID (as a string), with optional write-through to a
// tooldetail SQLite store. It satisfies TrackerStore (so the tool-call tracker
// records entries through it) and also backs the platform button-callback
// handlers (which Load/Update entries on expand/collapse). Safe for concurrent
// use; the zero value is ready to use.
//
// Telegram and Discord previously each carried a byte-identical copy of this
// store plus a per-platform toolResultEntry whose only difference was a
// chat/channel locator field that was written but never read. Folding both into
// one type here removes that duplication.
type ToolResultStore struct {
	entries sync.Map // msgID (string) -> ToolResultEntry
	detail  *tooldetail.Store
}

// StoreEntry records (or overwrites) the entry for a message. Implements
// TrackerStore.
func (s *ToolResultStore) StoreEntry(msgID, compact, full, result string, expanded bool) {
	s.entries.Store(msgID, ToolResultEntry{
		CompactText: compact,
		FullInput:   full,
		Result:      result,
		Expanded:    expanded,
	})
}

// IsExpanded reports whether the message is currently in expanded state, or
// false for an unknown message. Implements TrackerStore.
func (s *ToolResultStore) IsExpanded(msgID string) bool {
	if v, ok := s.entries.Load(msgID); ok {
		return v.(ToolResultEntry).Expanded
	}
	return false
}

// Persist write-throughs the entry to durable storage. No-op when no detail
// store is configured or the message ID is not numeric. Implements TrackerStore.
func (s *ToolResultStore) Persist(msgID, compact, full, result string) {
	if s.detail == nil {
		return
	}
	id, err := strconv.ParseInt(msgID, 10, 64)
	if err != nil {
		return
	}
	s.detail.Store(id, compact, full, result)
}

// Load returns the cached entry for a message, if present. Used by the platform
// button-callback handlers to re-render on expand/collapse.
func (s *ToolResultStore) Load(msgID string) (ToolResultEntry, bool) {
	if v, ok := s.entries.Load(msgID); ok {
		return v.(ToolResultEntry), true
	}
	return ToolResultEntry{}, false
}

// Update replaces the cached entry for a message. Used by the platform
// button-callback handlers to record the new expand/collapse state.
func (s *ToolResultStore) Update(msgID string, e ToolResultEntry) {
	s.entries.Store(msgID, e)
}

// SetDetailStore wires durable persistence and warms the in-memory cache from
// entries (<48h old) already on disk. A nil store disables persistence. The
// logger, if non-nil, reports the restore count or a load failure.
func (s *ToolResultStore) SetDetailStore(store *tooldetail.Store, logger *log.ComponentLogger) {
	s.detail = store
	if store == nil {
		return
	}
	entries, err := store.LoadAll()
	if err != nil {
		if logger != nil {
			logger.Warnf("load tool details: %v", err)
		}
		return
	}
	for id, e := range entries {
		s.entries.Store(strconv.FormatInt(id, 10), ToolResultEntry{
			CompactText: e.CompactText,
			FullInput:   e.FullInput,
			Result:      e.Result,
		})
	}
	if len(entries) > 0 && logger != nil {
		logger.Infof("restored %d tool call details from disk", len(entries))
	}
}
