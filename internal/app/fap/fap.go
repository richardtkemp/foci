// Package fap implements the Foci App Protocol (FAP v1) wire layer on the
// server side — the Go mirror of the Android client's pure-Kotlin `:protocol`
// module (github.com/richardtkemp/foci-android).
//
// Every WebSocket text frame is exactly one JSON object: an Envelope wrapping a
// type-specific payload in `d`, selected by the envelope `t`. This file defines
// the envelope, the shared models, and every concrete frame payload for both
// directions (server->app, app->server). codec.go does encode/decode.
//
// The wire contract is authoritative in foci-android/docs/01-wire-protocol.md
// and MUST stay byte-compatible with the Kotlin types in that repo's
// protocol/ module. When you change a frame here, change it there too.
package fap

import (
	"crypto/rand"
	"encoding/json"
	"time"
)

// ProtocolVersion is the FAP major version this server speaks.
const ProtocolVersion = 1

// Subprotocol is the WebSocket subprotocol token negotiated on the /app/ws
// handshake (wire §1: Sec-WebSocket-Protocol: fap.v1).
const Subprotocol = "fap.v1"

// Frame type strings — the canonical `t` envelope values (wire-protocol §4/§5).
// One source of truth, mirroring Kotlin FrameType.
const (
	// server -> app
	TypeHello           = "hello"
	TypeTurnStart       = "turn.start"
	TypeTextDelta       = "text.delta"
	TypeTextEnd         = "text.end"
	TypeMessage         = "message"
	TypeNotification    = "notification"
	TypeActivity        = "activity"
	TypeCacheExpiry     = "cacheExpiry"
	TypeMedia           = "media"
	TypeInteractive     = "interactive"
	TypeInteractiveEdit = "interactive.edit"
	// InteractiveProgressEdit syncs a batched ask's accumulated answers to the
	// other attached clients (Done=true when resolved, so they close the form).
	TypeInteractiveProgressEdit = "interactive.progressEdit"
	TypeWizardStep      = "wizard.step"
	TypeWizardEnd       = "wizard.end"
	TypeSubagentStart   = "subagent.start"
	TypeSubagentText    = "subagent.text"
	TypeSubagentEnd     = "subagent.end"
	TypeMeta            = "meta"
	TypeError           = "error"
	TypePong            = "pong"
	TypeTranscript      = "transcript"
	TypeToolInvoke      = "tool.invoke"

	// app -> server
	TypeCommand                = "command"
	TypeInteractiveResponse    = "interactive.response"
	TypeInteractiveProgress    = "interactive.progress"
	TypeWizardResponse         = "wizard.response"
	TypeConversationOpen       = "conversation.open"
	TypeConversationList       = "conversation.list"
	TypeConversationRename     = "conversation.rename"
	TypeConversationSetDefault = "conversation.setDefault"
	TypeConversationArchive    = "conversation.archive"
	TypeConversationOpenSet    = "conversation.openSet"
	TypeRead                   = "read"
	TypePing                   = "ping"
	TypeToolResult             = "tool.result"
	TypeSettingPut             = "setting.put"
	TypeSettingsSnapshot       = "settings.snapshot"
	TypeReadSync               = "read.sync"
	// TypeTyping is the app->server "user is typing" signal (ClientTyping). It is
	// distinct from the server->app agent activity indicator, which is now the
	// unified Activity frame (TypeActivity) with an "typing" ActivityKind.
	TypeTyping = "typing"
)

// ActivityKind enumerates the server->app agent activity states carried by the
// unified Activity frame. The server resolves the concurrently-true inputs to a
// single kind by this precedence (highest first):
//
//	subagents > waiting > tool > thinking > warming > typing > idle
//
// The zero value is the empty string; treat it as ActivityKindIdle. Mirrors the
// Kotlin ActivityKind enum in the :protocol module.
type ActivityKind string

const (
	// ActivityKindIdle: no turn in flight and nothing session-scoped pending.
	ActivityKindIdle ActivityKind = "idle"
	// ActivityKindTyping: the model is streaming visible text.
	ActivityKindTyping ActivityKind = "typing"
	// ActivityKindThinking: the model is mid extended-thinking.
	ActivityKindThinking ActivityKind = "thinking"
	// ActivityKindWarming: turn started, no output token yet.
	ActivityKindWarming ActivityKind = "warming"
	// ActivityKindTool: a tool is running (detail carries the tool name).
	ActivityKindTool ActivityKind = "tool"
	// ActivityKindSubagents: one or more CC subagents (Agent-tool spawns) are
	// running (detail carries their descriptions).
	ActivityKindSubagents ActivityKind = "subagents"
	// ActivityKindWaiting: this conversation dispatched a send_to_session and is
	// waiting on another foci agent's reply (detail carries the target agent id).
	ActivityKindWaiting ActivityKind = "waiting"
)

// WebSocket close codes (wire-protocol §7).
const (
	CloseNormal        = 1000
	CloseAuthRequired  = 4401
	CloseForbidden     = 4403
	CloseIdleTimeout   = 4408
	CloseReplaced      = 4409
	CloseRateLimited   = 4429
	CloseServerError   = 1011
	CloseServerRestart = 1012
)

// Envelope is the on-the-wire wrapper around every FAP frame (wire-protocol §1).
// Optional reliability fields are omitted when zero; the peer fills defaults.
type Envelope struct {
	T   string          `json:"t"`
	ID  string          `json:"id"`
	Seq int64           `json:"seq,omitempty"`
	Ack int64           `json:"ack,omitempty"`
	TS  string          `json:"ts,omitempty"`
	V   int             `json:"v,omitempty"`
	D   json.RawMessage `json:"d,omitempty"`
}

// --- Shared models (mirror Kotlin Models.kt) ---

// Caps is the server capability set advertised in `hello`.
type Caps struct {
	Versions []int    `json:"versions"`
	Push     []string `json:"push,omitempty"`
	Features []string `json:"features,omitempty"`
	Host     string   `json:"host,omitempty"` // public host the app reconnects to (§2.1/§6)
}

// CommandInfo is one slash command advertised to the app for its command
// palette. It mirrors the subset of command.Command the app needs to render a
// "/" autocomplete entry and invoke it: Name (echoed back verbatim in a
// Command frame), the one-line Description, and an optional grouping Category.
//
// Hidden commands are excluded server-side. Dynamic visibility (a command only
// meaningful in some states, e.g. /pause mid-ask) is NOT encoded — exactly like
// Telegram's static setMyCommands menu, the full non-hidden set is advertised
// and each command reports a no-op ("No active question.") when invoked out of
// context. The server is authoritative for these strings; the app must render
// what it receives rather than hardcoding its own copies.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
}

// AgentInfo is one agent the credential may talk to, with its roster.
type AgentInfo struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Avatar        string             `json:"avatar,omitempty"`    // emoji fallback (e.g. "🥔")
	AvatarURL     string             `json:"avatarUrl,omitempty"` // path to fetch the avatar image (e.g. "/app/avatar/clutch"); empty if none
	AvatarVer     string             `json:"avatarVer,omitempty"` // image fingerprint (mtime+size); changes when the file changes, drives client cache invalidation
	Conversations []ConversationInfo `json:"conversations,omitempty"`
	Commands      []CommandInfo      `json:"commands,omitempty"` // the agent's slash-command palette (non-hidden); app renders these rather than hardcoding
}

// ConversationInfo is a conversation (<-> a foci session key) within an agent.
type ConversationInfo struct {
	ID         string `json:"id"`
	SessionKey string `json:"sessionKey"`
	Title      string `json:"title,omitempty"`
	LastSeq    int64  `json:"lastSeq,omitempty"`
	Unread     int    `json:"unread,omitempty"`
	// IsDefault marks this conversation as the agent's default chat for the app
	// platform (used by keepalive/cron routing; rendered as the golden pin in the
	// app). Server-authoritative; set via ConversationSetDefault.
	IsDefault bool `json:"isDefault,omitempty"`
	// Archived marks this conversation as hidden from the roster. Reversible —
	// the server retains replay frames, the binding, and the session, so unarchive
	// restores full history. Server-authoritative; set via ConversationArchive.
	// Surfaced here so the roster reconcile is the app's source of truth and a
	// freshly-paired device learns the archived state without a local column to
	// seed from.
	Archived bool `json:"archived,omitempty"`
	// Activity is the roster snapshot of this conversation's resolved agent
	// activity kind (one of the ActivityKind values, as a string). It is the
	// snapshot half of the unified Activity frame, which carries live changes.
	// "idle" (or empty) means nothing is in flight.
	Activity string `json:"activity,omitempty"`
	// ActivityDetail carries the kind-specific detail for Activity: the tool name
	// (kind=tool), the running-subagent descriptions (kind=subagents), the target
	// agent id (kind=waiting), else empty.
	ActivityDetail string `json:"activityDetail,omitempty"`
	// LastActivityTs and LastPreview seed the roster row (last-active time + last
	// message preview) so a freshly-paired device renders them without opening each
	// chat to backfill. LastActivityTs is unix ms of the last visible frame.
	LastActivityTs int64  `json:"lastActivityTs,omitempty"`
	LastPreview    string `json:"lastPreview,omitempty"`
	// CacheExpiryMs is the roster snapshot of the CacheExpiry frame — the unix ms
	// after which the prompt cache is cold. 0 = unknown/cold (e.g. after a server
	// restart, which busts the cache). Reseeds a reconnecting client.
	CacheExpiryMs int64 `json:"cacheExpiryMs,omitempty"`
}

// Tokens is the token accounting carried by `meta`.
type Tokens struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
	CR  int64 `json:"cR"`
	CW  int64 `json:"cW"`
}

// ClientInfo is the client identity sent in the client `hello`.
type ClientInfo struct {
	App      string `json:"app"`
	OS       string `json:"os"`
	Version  string `json:"version"`
	DeviceID string `json:"deviceId"`
}

// ResumePoint is a per-conversation resume high-water sent in client `hello`.
// Open seeds the server's per-connection open-set: true iff the app currently
// has this conversation open (its pager tabs), used to warm open chats.
type ResumePoint struct {
	ConversationID string `json:"conversationId"`
	Ack            int64  `json:"ack"`
	Open           bool   `json:"open,omitempty"`
}

// AttachmentRef references an already-uploaded blob on an outbound message.
type AttachmentRef struct {
	BlobID string `json:"blobId"`
	Kind   string `json:"kind"`
	MIME   string `json:"mime"`
	Name   string `json:"name,omitempty"`
}

// Media kinds: internal labels for blob storage and for selecting the Telegram
// send-method (sendPhoto/sendDocument/sendVoice/…). These are NOT part of the app
// wire protocol — the Media frame carries only `mime`, and app clients derive
// presentation from that. Kind is a delivery-sink concern, not a content type.
const (
	MediaPhoto     = "photo"
	MediaDocument  = "document"
	MediaVoice     = "voice"
	MediaAudio     = "audio"
	MediaVideo     = "video"
	MediaAnimation = "animation"
)

// Choice is one button on an interactive prompt (permission / ask / plan
// approval). Maps 1:1 onto platform.ButtonChoice. Data is opaque to the app —
// it echoes it back verbatim in InteractiveResponse.data; foci routes on it.
type Choice struct {
	Label string `json:"label"`
	Data  string `json:"data"`
	Row   int    `json:"row,omitempty"`
	// Description is an optional sub-label for an ask option (mirrors
	// AskUserQuestion's per-option description). Batched-app prompts only; the
	// app renders it under the choice. Empty for permission/plan-approval buttons.
	Description string `json:"description,omitempty"`
}

// Question is one question within a BATCHED interactive prompt (app only). When
// Interactive.Questions is non-empty the app renders every question as a single
// form and returns one answer per question, positionally, in
// InteractiveResponse.Answers. Text is the RAW question (the app renders its own
// layout — header, counter, option buttons — rather than receiving pre-rendered
// markdown). Header is an optional bold title. Choices carry "qa:<index>" data
// (positional answers, so no per-prompt routing token) and NO Cancel button (the
// app's full-screen form has its own Cancel). Empty Choices ⇒ typed-answer-only.
type Question struct {
	Text    string   `json:"text"`
	Header  string   `json:"header,omitempty"`
	Choices []Choice `json:"choices,omitempty"`
}

// --- Server -> App frame payloads (mirror Kotlin Frames.kt) ---

// ServerFrame is implemented by every server->app payload. Type returns the
// envelope `t` value used when encoding.
type ServerFrame interface {
	Type() string
}

// HelloServer is the server greeting: capabilities + the agent roster.
type HelloServer struct {
	Version int         `json:"version"`
	Caps    Caps        `json:"caps"`
	Agents  []AgentInfo `json:"agents,omitempty"`
}

func (HelloServer) Type() string { return TypeHello }

// TurnStart opens a streaming agent turn.
type TurnStart struct {
	ConversationID string `json:"conversationId"`
	TurnID         string `json:"turnId"`
}

func (TurnStart) Type() string { return TypeTurnStart }

// TextDelta is one incremental chunk of streamed agent text.
type TextDelta struct {
	ConversationID string `json:"conversationId"`
	TurnID         string `json:"turnId"`
	Text           string `json:"text"`
}

func (TextDelta) Type() string { return TypeTextDelta }

// TextEnd finalizes a streamed turn, naming the resulting message.
type TextEnd struct {
	ConversationID string  `json:"conversationId"`
	TurnID         string  `json:"turnId"`
	MessageID      string  `json:"messageId"`
	FinalText      *string `json:"finalText,omitempty"`
}

func (TextEnd) Type() string { return TypeTextEnd }

// ServerMessage is a complete (non-streamed) message row.
type ServerMessage struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Role           string `json:"role"`
	Text           string `json:"text"`
}

func (ServerMessage) Type() string { return TypeMessage }

// Notification is a system-styled notice (Direct bypasses turn buffering).
type Notification struct {
	ConversationID string `json:"conversationId"`
	// MessageID is the notification's stable application-level id. Re-sending a
	// notification with the same MessageID replaces it in place on the client
	// (the compaction ⏳→✅ edit), so it occupies one row, not two.
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"text"`
	Level     string `json:"level,omitempty"`
}

func (Notification) Type() string { return TypeNotification }

// Activity is the unified agent-activity indicator: the server resolves the
// per-turn state (warming/thinking/tool/typing) and the two session-scoped
// states (subagents running, waiting on another agent) to a single Kind by the
// ActivityKind precedence, and emits one Activity frame on every resolved
// change. Detail carries the kind-specific payload: the tool name (kind=tool),
// the running-subagent descriptions (kind=subagents), the target agent id
// (kind=waiting), else empty. This REPLACES the former Typing/Thinking/Warming/
// Tool frames. App-only.
type Activity struct {
	ConversationID string `json:"conversationId"`
	Kind           string `json:"kind"`
	Detail         string `json:"detail,omitempty"`
}

func (Activity) Type() string { return TypeActivity }

// CacheExpiry carries the wall-clock time (unix ms) at which this conversation's
// Anthropic prompt cache goes cold if untouched — the session's last cache-touch
// plus the model's cache TTL. Ephemeral and server-authoritative: emitted once
// per completed turn (a turn refreshes the cache), the client derives warm/cold
// by comparing ExpiryMs to now, so it needs no knowledge of the TTL. App-only.
type CacheExpiry struct {
	ConversationID string `json:"conversationId"`
	ExpiryMs       int64  `json:"expiryMs"`
}

func (CacheExpiry) Type() string { return TypeCacheExpiry }

// Media references an out-of-band blob the app fetches via GET /app/blob/<id>.
// The bytes never travel over the WebSocket — only this reference does.
type Media struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	BlobID         string `json:"blobId"`
	MIME           string `json:"mime"`
	Name           string `json:"name,omitempty"`
	Caption        string `json:"caption,omitempty"`
	Size           int64  `json:"size,omitempty"`
	W              *int   `json:"w,omitempty"`
	H              *int   `json:"h,omitempty"`
	DurationMs     *int64 `json:"durationMs,omitempty"`
}

func (Media) Type() string { return TypeMedia }

// Interactive presents a prompt with buttons (permission / ask / plan approval).
// The app renders choices, and on tap echoes the chosen Choice.Data back in an
// InteractiveResponse. ExpiresAt (RFC3339) is advisory for the app's UI; foci
// owns the authoritative 24h expiry.
type Interactive struct {
	ConversationID string   `json:"conversationId"`
	PromptID       string   `json:"promptId"`
	Text           string   `json:"text"`
	Choices        []Choice `json:"choices,omitempty"`
	ExpiresAt      string   `json:"expiresAt,omitempty"`
	// Questions, when non-empty, makes this a BATCHED prompt: the app renders all
	// questions as one form and replies with one InteractiveResponse carrying an
	// answer per question in Answers (positional). Only sent to clients that
	// advertised the "interactiveBatch" feature in their ClientHello; Text/Choices
	// stay empty in that case. Legacy/uncapable clients never receive this field.
	Questions []Question `json:"questions,omitempty"`
}

func (Interactive) Type() string { return TypeInteractive }

// InteractiveEdit replaces a live prompt's text and buttons — used to show the
// resolution ("✅ Approved"), advance a multi-question ask, or disable a
// cancelled/expired prompt (empty Choices = buttons removed).
type InteractiveEdit struct {
	ConversationID string   `json:"conversationId"`
	PromptID       string   `json:"promptId"`
	Text           string   `json:"text"`
	Choices        []Choice `json:"choices,omitempty"`
}

func (InteractiveEdit) Type() string { return TypeInteractiveEdit }

// InteractiveProgressEdit carries a batched ask's accumulated answers out to the
// OTHER attached clients so a form can be part-answered on one device and
// finished on another. Answers is positional (empty entry = still unanswered).
// Done=true means the ask resolved — clients close the form.
type InteractiveProgressEdit struct {
	ConversationID string   `json:"conversationId"`
	PromptID       string   `json:"promptId"`
	Answers        []string `json:"answers"`
	Done           bool     `json:"done,omitempty"`
}

func (InteractiveProgressEdit) Type() string { return TypeInteractiveProgressEdit }

// SubagentStart marks a subagent (Task/Agent tool) run beginning, so the app can
// create a collapsed "Agent started" entry that its text blocks attach to. GroupKey
// is the parent tool_use id; Label the agent's description. From the PreToolUse hook
// — the app creates a run's entry ONLY on this frame, so a broken hook (no start)
// means text with no group renders inline, never a "never finishes" run.
type SubagentStart struct {
	ConversationID string `json:"conversationId"`
	GroupKey       string `json:"groupKey"`
	Label          string `json:"label,omitempty"`
}

func (SubagentStart) Type() string { return TypeSubagentStart }

// SubagentText carries one progress block from a subagent run, attached to its
// SubagentStart by GroupKey. Text is raw (no blockquote — that's a tg/discord
// presentation choice applied in the renderer, not here).
type SubagentText struct {
	ConversationID string `json:"conversationId"`
	GroupKey       string `json:"groupKey"`
	Text           string `json:"text"`
}

func (SubagentText) Type() string { return TypeSubagentText }

// SubagentEnd marks a subagent run complete (its Agent tool_use resolved), so the
// app flips the run's "started" entry to "completed". Precise per-run signal from
// the PostToolUse hook's OnToolEnd for the Agent tool.
type SubagentEnd struct {
	ConversationID string `json:"conversationId"`
	GroupKey       string `json:"groupKey"`
}

func (SubagentEnd) Type() string { return TypeSubagentEnd }

// WizardStep presents the current step of an active command wizard (wire §12).
// Only sent to clients that advertised the "wizard" feature in their
// ClientHello. WizardID is stable across the wizard's life; StepID is minted
// per step and must be echoed back in WizardResponse (staleness guard). Each
// new step — including a validation re-ask — is a fresh WizardStep with the
// same WizardID; the app replaces the step in place. Step reuses the batched-
// ask Question shape: empty Choices ⇒ free-text step, and no Cancel choice is
// sent (the app's wizard screen has its own Cancel). ExpiresAt (RFC3339) is
// advisory for the app's UI.
type WizardStep struct {
	ConversationID string           `json:"conversationId"`
	WizardID       string           `json:"wizardId"`
	StepID         string           `json:"stepId"`
	Title          string           `json:"title,omitempty"`
	Step           Question         `json:"step"`
	Media          *WizardStepMedia `json:"media,omitempty"`
	ExpiresAt      string           `json:"expiresAt,omitempty"`
}

// WizardStepMedia references a blob accompanying a wizard step (e.g. the
// /android pairing QR from a WizardDocProvider). The app fetches the bytes via
// blob GET (§9) and renders them inline in the wizard screen, above the step.
type WizardStepMedia struct {
	BlobID string `json:"blobId"`
	MIME   string `json:"mime"`
	Name   string `json:"name,omitempty"`
}

func (WizardStep) Type() string { return TypeWizardStep }

// Wizard end statuses (WizardEnd.Status).
const (
	WizardDone      = "done"
	WizardCancelled = "cancelled"
	WizardExpired   = "expired"
)

// WizardEnd terminates a wizard on the app: Status is "done", "cancelled" or
// "expired"; Text is the terminal summary (may be empty for expired).
type WizardEnd struct {
	ConversationID string `json:"conversationId"`
	WizardID       string `json:"wizardId"`
	Status         string `json:"status"`
	Text           string `json:"text,omitempty"`
}

func (WizardEnd) Type() string { return TypeWizardEnd }

// Transcript returns a voice note's STT text to the app for editing before send
// (the TranscribeOnly path) rather than routing it to the agent.
type Transcript struct {
	ConversationID string `json:"conversationId"`
	Text           string `json:"text"`
}

func (Transcript) Type() string { return TypeTranscript }

// Meta carries the user-facing status chips (model, cost, tokens, gap).
type Meta struct {
	ConversationID string   `json:"conversationId"`
	Model          string   `json:"model,omitempty"`
	PrevCostUsd    *float64 `json:"prevCostUsd,omitempty"`
	Tokens         *Tokens  `json:"tokens,omitempty"`
	Gap            string   `json:"gap,omitempty"`
}

func (Meta) Type() string { return TypeMeta }

// ErrorFrame reports a server-side error.
type ErrorFrame struct {
	ConversationID string `json:"conversationId,omitempty"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	Retryable      bool   `json:"retryable,omitempty"`
}

func (ErrorFrame) Type() string { return TypeError }

// Pong is the payload-less keepalive reply.
type Pong struct{}

func (Pong) Type() string { return TypePong }

// ToolInvoke asks the connected device to run a tool it hosts (e.g. the
// `app/android` tool backed by Tasker). NOT conversation-scoped — encoded with
// seq=0/ack=0 and bypasses the reliability layer entirely. The server re-issues
// on reconnect if it still needs a result.
//
// The device replies with one or more ToolResult frames carrying the same
// InvocationID: an immediate status="pending" if the work doesn't finish in
// the server's sync window, then a follow-up status="completed" (or "error")
// when it actually does. The server correlates by InvocationID.
type ToolInvoke struct {
	InvocationID string          `json:"invocationId"`
	Tool         string          `json:"tool"`           // device-side handler name (v1: "android")
	Action       string          `json:"action"`         // handler verb (e.g. "list", "perform")
	Args         json.RawMessage `json:"args,omitempty"` // handler-specific JSON; omitted → empty object
}

func (ToolInvoke) Type() string { return TypeToolInvoke }

// --- App -> Server frame payloads (mirror Kotlin ClientFrame) ---

// ClientHello is the app greeting: identity, resume points, push token.
type ClientHello struct {
	Client    ClientInfo    `json:"client"`
	Resume    []ResumePoint `json:"resume,omitempty"`
	PushToken string        `json:"pushToken,omitempty"`
	// Features are the optional client capabilities the app supports, mirroring
	// the server's Caps.Features. "interactiveBatch" ⇒ the app can render a
	// batched multi-question prompt and return all answers at once; absent ⇒ the
	// server keeps presenting questions one at a time (back-compat). Any unknown
	// feature is ignored.
	Features []string `json:"features,omitempty"`
}

// ClientMessage is a user message from the app.
type ClientMessage struct {
	ConversationID string `json:"conversationId"`
	// AgentID names the agent that owns ConversationID. The turn is self-
	// describing: the server binds + routes by it rather than any socket-wide
	// "current agent" (one socket multiplexes every agent's conversations). For a
	// warm conversation the binding stays authoritative; AgentID seeds a cold one.
	AgentID     string          `json:"agentId,omitempty"`
	Text        string          `json:"text"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	ReplyTo     string          `json:"replyTo,omitempty"`
	// Steer is the sender's per-message steer/queue choice: "steer" folds the
	// message into an in-flight turn even when the agent's steer_mode is off;
	// "queue" waits for the in-flight turn to complete and runs a fresh turn
	// (and is never consumed as plan feedback or an ask answer). Empty means
	// "use the agent's steer_mode config". Unknown values are treated as empty.
	Steer string `json:"steer,omitempty"`
	// TranscribeOnly requests that a voice attachment be transcribed and the
	// text returned to the app (as a Transcript frame) for the user to edit
	// before sending, instead of being routed to the agent as a turn. Ignored
	// when there is no voice attachment.
	TranscribeOnly bool `json:"transcribeOnly,omitempty"`
}

// Command is a slash-command invocation from the app. AgentID names the owning
// agent (as on ClientMessage) so command dispatch needs no socket-wide focus.
type Command struct {
	ConversationID string `json:"conversationId"`
	AgentID        string `json:"agentId,omitempty"`
	Name           string `json:"name"`
	Args           string `json:"args,omitempty"`
}

// InteractiveResponse is a button tap / typed answer to a prompt. For a single
// question the answer is in Data. For a BATCHED prompt (server sent
// Interactive.Questions) the app instead fills Answers with one entry per
// question, in the same order — each the chosen Choice.Data ("qa:<index>") or a
// typed string. Data stays empty for batched replies; Answers stays empty for
// single ones, so the server routes on which is set.
type InteractiveResponse struct {
	ConversationID string   `json:"conversationId"`
	PromptID       string   `json:"promptId"`
	Data           string   `json:"data,omitempty"`
	Answers        []string `json:"answers,omitempty"`
}

// InteractiveProgress reports one answered question of a batched ask as the user
// fills it in, so the server accumulates the answer set and mirrors it to the
// other clients. Index is the question's position; Answer the chosen
// Choice.Data ("qa:<index>") or typed string.
type InteractiveProgress struct {
	ConversationID string `json:"conversationId"`
	PromptID       string `json:"promptId"`
	Index          int    `json:"index"`
	Answer         string `json:"answer"`
}

func (InteractiveProgress) Type() string { return TypeInteractiveProgress }

// WizardResponse answers the current step of an active wizard. Data is the
// chosen Choice.Data ("qa:<index>"), the "qa:cancel" sentinel, or free typed
// text (verbatim). StepID must match the last-emitted WizardStep or the
// response is dropped as stale.
type WizardResponse struct {
	ConversationID string `json:"conversationId"`
	WizardID       string `json:"wizardId"`
	StepID         string `json:"stepId"`
	Data           string `json:"data,omitempty"`
}

// ConversationOpen creates/opens a conversation for an agent.
type ConversationOpen struct {
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey,omitempty"`
	// ConversationID, when set, is a client-assigned id the server adopts instead
	// of minting its own — lets the app create + open a conversation locally and
	// instantly. Idempotent: reopening an id that already has a binding reuses it.
	ConversationID string `json:"conversationId,omitempty"`
}

// ClientTyping signals the user is typing.
type ClientTyping struct {
	ConversationID string `json:"conversationId"`
	On             bool   `json:"on"`
}

// Read marks a message read (drives unread counts / ack).
type Read struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
}

// ReadSync mirrors a conversation's read watermark to a user's other devices
// (server->client). Sent to the other clients when one device reads (a Read
// frame), and replayed per-conversation after a hello so a device that was
// offline during the read catches up. The client advances its read line
// monotonically, so a stale/backward watermark is a safe no-op.
type ReadSync struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
}

func (ReadSync) Type() string { return TypeReadSync }

// ConversationRename sets (or clears, when Title is empty) a user-friendly alias
// for a conversation. Persisted server-side; echoed back in ConversationInfo.Title.
type ConversationRename struct {
	ConversationID string `json:"conversationId"`
	Title          string `json:"title"`
}

// ConversationSetDefault sets (IsDefault=true) or clears (IsDefault=false) this
// conversation as the agent's default chat for the app platform. Persisted
// server-side via SessionIndex.SetDefaultChat/ClearDefaultChat; the updated
// roster (with ConversationInfo.IsDefault) is echoed back. Setting a new default
// clears any previous one for the platform (enforced by SetDefaultChat).
type ConversationSetDefault struct {
	ConversationID string `json:"conversationId"`
	IsDefault      bool   `json:"isDefault"`
}

// ConversationArchive sets or clears the archived flag on a conversation.
// Reversible — the server does NOT purge replay frames, drop the binding, or
// flip session status; it only persists the flag (keyed by agent+platform+chatID
// in chat_metadata, alongside is_default) and pushes back an updated roster so
// every device reconciles. Archived conversations stay live: inbound frames
// still flow, history is retained, and unarchive is a real server action
// (Archived=false) rather than a local re-show. See docs/WIRING.md → app
// binding restore.
type ConversationArchive struct {
	ConversationID string `json:"conversationId"`
	// Archived is true to archive (hide from roster), false to unarchive.
	Archived bool `json:"archived"`
}

// ConversationOpenSet carries the app's full current open-set (the conversations
// it has open in its pager). Idempotent replace of the server's per-connection
// open-set; sent whenever the app's open chats change.
type ConversationOpenSet struct {
	ConversationIDs []string `json:"conversationIds"`
}

// SettingPut mirrors one synced app-preference to the server (client->server).
// The server is a dumb store: it persists Key=Value under the global app-settings
// bag and rebroadcasts the full SettingsSnapshot to every settings-capable client.
// The client owns which keys are synced (its whitelist); the server never
// interprets a value.
type SettingPut struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// SettingsSnapshot is the full synced-preferences bag (server->client), pushed
// after the hello and re-broadcast on every change — the settings analogue of the
// roster push. Last-write-wins across devices.
type SettingsSnapshot struct {
	Settings map[string]string `json:"settings"`
}

func (SettingsSnapshot) Type() string { return TypeSettingsSnapshot }

// ConversationList re-requests the roster (payload-less).
type ConversationList struct{}

// Ping is the payload-less keepalive (payload-less).
type Ping struct{}

// ToolResult is the device's reply to a ToolInvoke. Carries the matching
// InvocationID so the server can correlate. Status is one of:
//   - "completed": the work finished; Output holds the JSON payload.
//   - "pending":   the work is still running after the server's sync window;
//     a later "completed"/"error" frame with the same id will follow.
//   - "error":     the work failed; Error is a short human-readable message.
//
// Fire-and-forget on the wire — NOT conversation-scoped, NOT retried. If the
// socket drops between invoke and result, the server-side tool call simply
// times out (the Tasker task may still finish on-device; its result is dropped).
type ToolResult struct {
	InvocationID string          `json:"invocationId"`
	Status       string          `json:"status"`
	Output       json.RawMessage `json:"output,omitempty"`
	Error        string          `json:"error,omitempty"`
}

// ToolResult.Status values. "pending" is a NON-terminal keepalive: the device
// emits it when a task overruns its own sync window, and a later "completed"/
// "error" frame with the same InvocationID follows. "completed"/"error" are
// terminal — the InvokeTool waiter returns on those.
const (
	ToolStatusCompleted = "completed"
	ToolStatusPending   = "pending"
	ToolStatusError     = "error"
)

// --- ULID (mirror Kotlin Ulid.kt) ---

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID generates a Crockford-base32 ULID: 48-bit millisecond timestamp +
// 80 bits of randomness, 26 chars, lexicographically sortable. Unique per
// sender stream, used for dedup and cross-frame references. The byte→symbol
// mapping is the canonical oklog/ulid layout (timestamp: 10 chars from the
// 48-bit ms; entropy: 16 chars from 10 random bytes), reproduced inline so the
// package stays dependency-free.
func NewULID() string {
	ms := uint64(time.Now().UnixMilli()) //nolint:gosec // unix-ms is always non-negative
	var r [10]byte
	_, _ = rand.Read(r[:])

	var out [26]byte
	// Timestamp: 48 bits → out[0:10] (top char carries only 3 significant bits).
	out[0] = crockford[(ms>>45)&0x1f]
	out[1] = crockford[(ms>>40)&0x1f]
	out[2] = crockford[(ms>>35)&0x1f]
	out[3] = crockford[(ms>>30)&0x1f]
	out[4] = crockford[(ms>>25)&0x1f]
	out[5] = crockford[(ms>>20)&0x1f]
	out[6] = crockford[(ms>>15)&0x1f]
	out[7] = crockford[(ms>>10)&0x1f]
	out[8] = crockford[(ms>>5)&0x1f]
	out[9] = crockford[ms&0x1f]
	// Entropy: 80 bits (r[0:10]) → out[10:26].
	out[10] = crockford[(r[0]&248)>>3]
	out[11] = crockford[((r[0]&7)<<2)|((r[1]&192)>>6)]
	out[12] = crockford[(r[1]&62)>>1]
	out[13] = crockford[((r[1]&1)<<4)|((r[2]&240)>>4)]
	out[14] = crockford[((r[2]&15)<<1)|((r[3]&128)>>7)]
	out[15] = crockford[(r[3]&124)>>2]
	out[16] = crockford[((r[3]&3)<<3)|((r[4]&224)>>5)]
	out[17] = crockford[r[4]&31]
	out[18] = crockford[(r[5]&248)>>3]
	out[19] = crockford[((r[5]&7)<<2)|((r[6]&192)>>6)]
	out[20] = crockford[(r[6]&62)>>1]
	out[21] = crockford[((r[6]&1)<<4)|((r[7]&240)>>4)]
	out[22] = crockford[((r[7]&15)<<1)|((r[8]&128)>>7)]
	out[23] = crockford[(r[8]&124)>>2]
	out[24] = crockford[((r[8]&3)<<3)|((r[9]&224)>>5)]
	out[25] = crockford[r[9]&31]
	return string(out[:])
}
