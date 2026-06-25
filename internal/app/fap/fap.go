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
	TypeTyping          = "typing"
	TypeMedia           = "media"
	TypeInteractive     = "interactive"
	TypeInteractiveEdit = "interactive.edit"
	TypeMeta            = "meta"
	TypeSessionUpdate   = "session.update"
	TypeError           = "error"
	TypePong            = "pong"

	// app -> server
	TypeCommand             = "command"
	TypeInteractiveResponse = "interactive.response"
	TypeConversationOpen    = "conversation.open"
	TypeConversationList    = "conversation.list"
	TypeConversationRename  = "conversation.rename"
	TypeRead                = "read"
	TypePing                = "ping"
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
type ResumePoint struct {
	ConversationID string `json:"conversationId"`
	Ack            int64  `json:"ack"`
}

// AttachmentRef references an already-uploaded blob on an outbound message.
type AttachmentRef struct {
	BlobID string `json:"blobId"`
	Kind   string `json:"kind"`
	MIME   string `json:"mime"`
	Name   string `json:"name,omitempty"`
}

// Media kinds (wire-protocol §9), mirroring Kotlin MediaKind.
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
	Text           string `json:"text"`
	Level          string `json:"level,omitempty"`
}

func (Notification) Type() string { return TypeNotification }

// Typing toggles the agent typing indicator.
type Typing struct {
	ConversationID string `json:"conversationId"`
	On             bool   `json:"on"`
}

func (Typing) Type() string { return TypeTyping }

// Media references an out-of-band blob the app fetches via GET /app/blob/<id>.
// The bytes never travel over the WebSocket — only this reference does.
type Media struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	BlobID         string `json:"blobId"`
	Kind           string `json:"kind"`
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

// Meta carries the user-facing status chips (model, mana, cost, tokens).
type Meta struct {
	ConversationID string   `json:"conversationId"`
	Model          string   `json:"model,omitempty"`
	ManaPct        *int     `json:"manaPct,omitempty"`
	ManaState      string   `json:"manaState,omitempty"`
	PrevCostUsd    *float64 `json:"prevCostUsd,omitempty"`
	Tokens         *Tokens  `json:"tokens,omitempty"`
	Gap            string   `json:"gap,omitempty"`
}

func (Meta) Type() string { return TypeMeta }

// SessionUpdate tells the app a conversation's session key rotated.
type SessionUpdate struct {
	ConversationID string `json:"conversationId"`
	SessionKey     string `json:"sessionKey"`
	Reason         string `json:"reason,omitempty"`
}

func (SessionUpdate) Type() string { return TypeSessionUpdate }

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

// --- App -> Server frame payloads (mirror Kotlin ClientFrame) ---

// ClientHello is the app greeting: identity, resume points, push token.
type ClientHello struct {
	Client    ClientInfo    `json:"client"`
	Resume    []ResumePoint `json:"resume,omitempty"`
	PushToken string        `json:"pushToken,omitempty"`
}

// ClientMessage is a user message from the app.
type ClientMessage struct {
	ConversationID string          `json:"conversationId"`
	Text           string          `json:"text"`
	Attachments    []AttachmentRef `json:"attachments,omitempty"`
	ReplyTo        string          `json:"replyTo,omitempty"`
}

// Command is a slash-command invocation from the app.
type Command struct {
	ConversationID string `json:"conversationId"`
	Name           string `json:"name"`
	Args           string `json:"args,omitempty"`
}

// InteractiveResponse is a button tap / typed answer to a prompt.
type InteractiveResponse struct {
	ConversationID string `json:"conversationId"`
	PromptID       string `json:"promptId"`
	Data           string `json:"data"`
}

// ConversationOpen creates/opens a conversation for an agent.
type ConversationOpen struct {
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey,omitempty"`
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

// ConversationRename sets (or clears, when Title is empty) a user-friendly alias
// for a conversation. Persisted server-side; echoed back in ConversationInfo.Title.
type ConversationRename struct {
	ConversationID string `json:"conversationId"`
	Title          string `json:"title"`
}

// ConversationList re-requests the roster (payload-less).
type ConversationList struct{}

// Ping is the payload-less keepalive (payload-less).
type Ping struct{}

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
