// protocol.go — typed Go structs for every OpenCode wire message foci cares
// about. Source-of-truth: packages/sdk/js/src/gen/types.gen.ts in the
// opencode repo. Only fields foci reads are modelled; the rest are omitted
// (Go's JSON unmarshaling silently ignores unknown fields).
//
// The wire format uses JSON. Discriminated unions (Part, Event, Message,
// MessageError) are modelled as a single struct carrying a `Type` field plus
// optional fields; the dispatcher switches on Type and reads only the
// relevant fields. This matches ccstream's protocol.go pattern.

package opencode

import (
	"encoding/json"
)

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session is an opencode session (one per foci session). Created via
// POST /session; the returned ID is what every subsequent /session/:id
// endpoint takes.
type Session struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Time  struct {
		Created int64 `json:"created"` // unix ms
		Updated int64 `json:"updated"` // unix ms
	} `json:"time"`
}

// ---------------------------------------------------------------------------
// Messages — UserMessage | AssistantMessage
// ---------------------------------------------------------------------------

// Message is the wire shape returned by GET /session/:id/message and in
// message.updated SSE events. User and Assistant share id/sessionID/role;
// assistant-only fields are populated when role == "assistant".
type Message struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Role       string         `json:"role"` // "user" | "assistant"
	ParentID   string         `json:"parentID,omitempty"`
	ModelID    string         `json:"modelID,omitempty"`
	ProviderID string         `json:"providerID,omitempty"`
	Finish     string         `json:"finish,omitempty"`
	Cost       float64        `json:"cost,omitempty"`
	Tokens     *MessageTokens `json:"tokens,omitempty"`
	Error      *MessageError  `json:"error,omitempty"`
	Time       MessageTime    `json:"time"`
}

// MessageTime carries the timestamps for a Message. Created is always set;
// Completed is set only on assistant messages once the model is done.
// Both are unix ms (matching the wire format).
type MessageTime struct {
	Created   int64 `json:"created"`             // unix ms
	Completed int64 `json:"completed,omitempty"` // unix ms
}

// MessageTokens is the per-message token accounting embedded in an
// AssistantMessage. Cache is split into read/write.
type MessageTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// MessageError is the error payload on an AssistantMessage or in a
// session.error SSE event. Discriminated by Name.
type MessageError struct {
	Name string          `json:"name"` // "ProviderAuthError" | "MessageAbortedError" | "ApiError" | ...
	Data json.RawMessage `json:"data"`
}

// ErrorName constants — values of MessageError.Name that foci dispatches on.
const (
	ErrProviderAuth     = "ProviderAuthError"
	ErrMessageAborted   = "MessageAbortedError"
	ErrMessageOutLength = "MessageOutputLengthError"
	ErrAPI              = "APIError"
	ErrUnknown          = "UnknownError"
)

// ProviderAuthErrorData is the typed payload for MessageError.Name ==
// ErrProviderAuth. Parse MessageError.Data into this when dispatching.
type ProviderAuthErrorData struct {
	ProviderID string `json:"providerID"`
	Message    string `json:"message"`
}

// ApiErrorData is the typed payload for MessageError.Name == ErrAPI.
type ApiErrorData struct {
	Message      string            `json:"message"`
	StatusCode   int               `json:"statusCode,omitempty"`
	IsRetryable  bool              `json:"isRetryable"`
	ResponseBody string            `json:"responseBody,omitempty"`
}

// MessageAbortedErrorData is the typed payload for MessageError.Name ==
// ErrMessageAborted. Expected on /reset hard.
type MessageAbortedErrorData struct {
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Part — discriminated union by Type
// ---------------------------------------------------------------------------

// PartType constants — values of Part.Type that foci dispatches on.
const (
	PartText       = "text"
	PartReasoning  = "reasoning"
	PartTool       = "tool"
	PartFile       = "file"
	PartSubtask    = "subtask"
	PartCompaction = "compaction"
	PartStepStart  = "step-start"
	PartStepFinish = "step-finish"
	PartSnapshot   = "snapshot"
	PartPatch      = "patch"
	PartAgent      = "agent"
	PartRetry      = "retry"
)

// Part is the wire shape in a message.part.updated SSE event's `part`
// field. Discriminated by Type. Only the fields relevant to the active
// Type are populated; the rest carry zero values.
type Part struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	MessageID  string          `json:"messageID"`
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`        // text, reasoning
	Synthetic  bool            `json:"synthetic,omitempty"`   // text
	Ignored    bool            `json:"ignored,omitempty"`     // text
	CallID     string          `json:"callID,omitempty"`      // tool
	Tool       string          `json:"tool,omitempty"`        // tool
	State      *ToolState      `json:"state,omitempty"`       // tool
	Mime       string          `json:"mime,omitempty"`        // file
	Filename   string          `json:"filename,omitempty"`    // file
	URL        string          `json:"url,omitempty"`         // file
	Prompt     string          `json:"prompt,omitempty"`      // subtask
	Description string         `json:"description,omitempty"` // subtask
	Agent      string          `json:"agent,omitempty"`       // subtask, agent
	Auto       bool            `json:"auto,omitempty"`        // compaction
	Snapshot   string          `json:"snapshot,omitempty"`    // snapshot, step-start, step-finish
	Reason     string          `json:"reason,omitempty"`      // step-finish
	Cost       float64         `json:"cost,omitempty"`        // step-finish
	Tokens     *MessageTokens  `json:"tokens,omitempty"`      // step-finish
	Time       *PartTime       `json:"time,omitempty"`        // text, tool, step-finish
	Metadata   json.RawMessage `json:"metadata,omitempty"`    // generic
}

// PartTime is the optional timestamp block on a Part. Start is always set
// when Time is present; End is set when the part is "complete".
// Both are unix ms (matching the wire format).
type PartTime struct {
	Start int64 `json:"start"`           // unix ms
	End   int64 `json:"end,omitempty"`   // unix ms
}

// ToolState models Part.State for tool parts. Discriminated by Status.
type ToolState struct {
	Status   string          `json:"status"` // "pending" | "running" | "completed" | "error"
	Input    json.RawMessage `json:"input,omitempty"`
	Output   string          `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
	Title    string          `json:"title,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Time     *PartTime       `json:"time,omitempty"`
}

// ToolStateStatus constants.
const (
	ToolStatePending   = "pending"
	ToolStateRunning   = "running"
	ToolStateCompleted = "completed"
	ToolStateError     = "error"
)

// ---------------------------------------------------------------------------
// Permission
// ---------------------------------------------------------------------------

// Permission is the wire shape carried by permission.updated SSE events.
// opencode surfaces tool approvals AND the built-in `question` tool's
// prompts through this single type, discriminated by Type. Metadata is
// kind-specific raw JSON — Step 9 decodes per Type.
type Permission struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"` // "bash"|"edit"|"question"|"webfetch"|"external_directory"|"doom_loop"|...
	Pattern    json.RawMessage `json:"pattern,omitempty"` // string | []string
	SessionID  string          `json:"sessionID"`
	MessageID  string          `json:"messageID"`
	CallID     string          `json:"callID,omitempty"`
	Title      string          `json:"title"`
	Metadata   json.RawMessage `json:"metadata"`
	Time       struct {
		Created int64 `json:"created"` // unix ms
	} `json:"time"`
}

// PermissionType constants — the kinds foci dispatches on. The full list
// is at https://opencode.ai/docs/permissions/#available-permissions.
const (
	PermBash              = "bash"
	PermEdit              = "edit"
	PermRead              = "read"
	PermGlob              = "glob"
	PermGrep              = "grep"
	PermTask              = "task"
	PermSkill             = "skill"
	PermLSP               = "lsp"
	PermQuestion          = "question"
	PermWebfetch          = "webfetch"
	PermWebsearch         = "websearch"
	PermExternalDirectory = "external_directory"
	PermDoomLoop          = "doom_loop"
)

// ---------------------------------------------------------------------------
// Session status
// ---------------------------------------------------------------------------

// SessionStatus is the wire shape in session.status SSE events. Type is
// "idle" | "busy" | "retry"; retry carries attempt/message/next.
type SessionStatus struct {
	Type     string `json:"type"` // "idle" | "busy" | "retry"
	Attempt  int    `json:"attempt,omitempty"`
	Message  string `json:"message,omitempty"`
	Next     int64  `json:"next,omitempty"` // unix ms
}

// SessionStatusType constants.
const (
	StatusIdle  = "idle"
	StatusBusy  = "busy"
	StatusRetry = "retry"
)

// ---------------------------------------------------------------------------
// Events — SSE wire envelope
// ---------------------------------------------------------------------------

// EventType constants — values of rawEvent.Type. Listed at
// https://opencode.ai/docs/server/#events.
const (
	EventServerConnected     = "server.connected"
	EventSessionCreated      = "session.created"
	EventSessionUpdated      = "session.updated"
	EventSessionDeleted      = "session.deleted"
	EventSessionStatus       = "session.status"
	EventSessionIdle         = "session.idle"
	EventSessionCompacted    = "session.compacted"
	EventSessionError        = "session.error"
	EventSessionDiff         = "session.diff"
	EventMessageUpdated      = "message.updated"
	EventMessageRemoved      = "message.removed"
	EventMessagePartUpdated  = "message.part.updated"
	EventMessagePartRemoved  = "message.part.removed"
	EventPermissionUpdated   = "permission.updated"
	EventPermissionReplied   = "permission.replied"
	EventFileEdited          = "file.edited"
	EventFileWatcherUpdated  = "file.watcher.updated"
	EventTodoUpdated         = "todo.updated"
	EventCommandExecuted     = "command.executed"
	EventVcsBranchUpdated    = "vcs.branch.updated"
	EventTuiPromptAppend     = "tui.prompt.append"
	EventTuiCommandExecute   = "tui.command.execute"
	EventTuiToastShow        = "tui.toast.show"
)

// rawEvent is the SSE wire envelope. The SSE subscriber decodes each
// `data:` payload into this shape, switches on Type, and unmarshals
// Properties into the matching typed payload below.
//
// A sessionID() helper (extracting properties.sessionID for the per-
// session router) is intentionally absent here — Step 4 adds it when
// the SSE subscriber actually calls it, so deadcode sees the production
// caller.
type rawEvent struct { //nolint:unused // decoded by Step 4 SSE subscriber
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// Typed event payloads — one per Event* constant the dispatcher cares
// about. Each is decoded from rawEvent.Properties by the SSE subscriber.

type eventMessageUpdated struct { //nolint:unused // decoded by Step 4 SSE subscriber
	Info Message `json:"info"`
}

type eventMessageRemoved struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
}

type eventMessagePartUpdated struct { //nolint:unused // decoded by Step 4 SSE subscriber
	Part  Part   `json:"part"`
	Delta string `json:"delta,omitempty"`
}

type eventMessagePartRemoved struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	PartID    string `json:"partID"`
}

type eventPermissionUpdated struct { //nolint:unused // decoded by Step 4 SSE subscriber
	Permission Permission `json:"permission"`
}

type eventPermissionReplied struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID     string `json:"sessionID"`
	PermissionID  string `json:"permissionID"`
	Response      string `json:"response"`
}

type eventSessionIdle struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID string `json:"sessionID"`
}

type eventSessionStatus struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID string         `json:"sessionID"`
	Status    SessionStatus  `json:"status"`
}

type eventSessionCompacted struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID string `json:"sessionID"`
}

type eventSessionCreatedUpdated struct { //nolint:unused // decoded by Step 4 SSE subscriber
	Info Session `json:"info"`
}

type eventSessionError struct { //nolint:unused // decoded by Step 4 SSE subscriber
	SessionID string         `json:"sessionID,omitempty"`
	Error     *MessageError  `json:"error,omitempty"`
}
