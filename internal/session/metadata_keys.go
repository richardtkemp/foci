package session

// The ONE registry of session_metadata keys (#1271).
//
// Every key read, written or deleted through the session_metadata API
// (SetSessionMetadata / GetSessionMetadata / DeleteSessionMetadata /
// AllSessionMetadata / SessionKeysWithMetadata, index.go) is a MetaKey*
// constant listed in SessionMetadataKeys below. Writers, readers and the
// /sessions info display list all share this registry — never reintroduce a
// key string literal or a second list at a call site.
//
// The constant values are storage format: each equals the key string
// persisted today, byte for byte. Renaming one (cc_resume_id in particular)
// is a data change and requires a migration.

const (
	// MetaKeyModel is the per-session model override (agent/session_meta.go).
	MetaKeyModel = "model"
	// MetaKeyModelEndpoint is the per-session endpoint override (agent/session_meta.go).
	MetaKeyModelEndpoint = "model_endpoint"
	// MetaKeyModelFormat is the per-session wire-format override (agent/session_meta.go).
	MetaKeyModelFormat = "model_format"
	// MetaKeyEffort is the per-session effort override (agent/session_meta.go).
	MetaKeyEffort = "effort"
	// MetaKeyThinking is the per-session thinking-mode override (agent/session_meta.go).
	MetaKeyThinking = "thinking"
	// MetaKeySpeed is the per-session speed override (agent/session_meta.go).
	MetaKeySpeed = "speed"
	// MetaKeyShowToolCalls is the per-session show_tool_calls display override.
	MetaKeyShowToolCalls = "show_tool_calls"
	// MetaKeyDisplayShowThinking is the per-session display show_thinking override.
	MetaKeyDisplayShowThinking = "display_show_thinking"
	// MetaKeyStreamOutput is the per-session stream_output display override.
	MetaKeyStreamOutput = "stream_output"
	// MetaKeyDisplayWidth is the per-session display width override.
	MetaKeyDisplayWidth = "display_width"
	// MetaKeyPermissionMode is the per-session CC permission-mode override.
	MetaKeyPermissionMode = "permission_mode"
	// MetaKeyNoCompact is the per-session sticky no-compaction flag.
	MetaKeyNoCompact = "no_compact"
	// MetaKeyCCResumeID holds delegated-backend resume ids, written by
	// DelegatedManager.saveResumeID (agent/delegated_manager.go).
	MetaKeyCCResumeID = "cc_resume_id"
	// MetaKeyCCUndelivered holds a session's not-yet-consumed inputs as a
	// JSON array of delegator.PendingInput in write order, written ahead of
	// every tracked backend write and trimmed as the backend confirms each
	// one (#2050, agent/delivery_store.go).
	MetaKeyCCUndelivered = "cc_undelivered"
	// MetaKeyQuietCompactedAt holds the RFC3339Nano time of the session's
	// last quiet-hours compaction ATTEMPT that passed the guards, written by
	// the periodic runner (#2218) — the anchor for the once-per-window and
	// human-interacted-since rules.
	MetaKeyQuietCompactedAt = "quiet_compacted_at"
	// MetaKeyOrientationConsumed marks a branch session's orientation text as
	// consumed (branch.go ConsumeOrientation; branch sessions only).
	MetaKeyOrientationConsumed = "orientation_consumed"
	// MetaKeyLastActivity is a legacy key no longer written through the API;
	// rows may persist in existing databases, so /sessions info keeps
	// rendering it.
	MetaKeyLastActivity = "last_activity"
)

// SessionMetadataKey describes one session_metadata key: the storage key
// plus the display data /sessions info needs for it.
type SessionMetadataKey struct {
	// Key is the storage key — one of the MetaKey* constants above.
	Key string
	// Unset is how /sessions info renders the key when it has no value.
	Unset string
	// BranchOnly marks a key only meaningful for branch sessions;
	// /sessions info skips it when unset on a root (non-branch) session
	// (#1297).
	BranchOnly bool
	// Shown reports whether /sessions info lists the key as a known key.
	// False means it surfaces only as a sorted present-but-unknown extra
	// when set, exactly like any unlisted key.
	Shown bool
}

// SessionMetadataKeys lists every session_metadata key with its display
// data. It is an ordered slice, not a map: the Shown entries come first, in
// the exact order /sessions info renders them (display order is not
// derivable by sorting), followed by the keys /sessions info does not list.
// metadata_keys_test.go fails if a MetaKey* constant is missing from this
// slice or a key is duplicated.
var SessionMetadataKeys = []SessionMetadataKey{
	{Key: MetaKeyModel, Unset: "null", Shown: true},
	{Key: MetaKeyModelEndpoint, Unset: "null", Shown: true},
	{Key: MetaKeyModelFormat, Unset: "null", Shown: true},
	{Key: MetaKeyEffort, Unset: "null", Shown: true},
	{Key: MetaKeyPermissionMode, Unset: "null", Shown: true},
	{Key: MetaKeyCCResumeID, Unset: "null", Shown: true},
	{Key: MetaKeyLastActivity, Unset: "null", Shown: true},
	{Key: MetaKeyNoCompact, Unset: "false", Shown: true},
	{Key: MetaKeyDisplayShowThinking, Unset: "false", Shown: true},
	{Key: MetaKeyOrientationConsumed, Unset: "false", BranchOnly: true, Shown: true},

	{Key: MetaKeyThinking},
	{Key: MetaKeySpeed},
	{Key: MetaKeyShowToolCalls},
	{Key: MetaKeyStreamOutput},
	{Key: MetaKeyDisplayWidth},
	{Key: MetaKeyCCUndelivered},
	{Key: MetaKeyQuietCompactedAt},
}
