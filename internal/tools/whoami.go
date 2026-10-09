package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"foci/internal/session"
)

// Missing-value markers for the whoami report: a fact foci does not have is
// stated explicitly, never guessed and never substituted.
const (
	whoamiNone    = "none"    // no session key on the context / no chat id
	whoamiUnknown = "unknown" // platform or model could not be resolved
)

// WhoamiDeps carries the identity facts NewWhoamiTool reports. Values are
// build-time config; funcs are per-call lookups (a session's model can change
// between calls). All injected so the tool is unit-testable without a gateway.
type WhoamiDeps struct {
	AgentID   string // configured agent id (config.AgentConfig.ID)
	Backend   string // configured backend name ("" = api)
	Delegated bool   // config.AgentConfig.IsDelegated(), captured at build

	// PlatformForChat resolves the platform owning an (agent, chat) pair —
	// the same lookup set_session_alias uses. Nil or "" = unknown.
	PlatformForChat func(agentID string, chatID int64) string

	// SessionModel returns the model the session's running backend last
	// reported (the resolved, provider-qualified id), else the session's
	// effective model (per-session override, else the agent default), which
	// may still be the configured alias before the first turn completes.
	// Nil or "" = unknown.
	SessionModel func(sessionKey string) string
}

// whoamiReport is the per-call identity snapshot: one resolution path feeding
// the text and JSON renderers. Field order is the report order.
type whoamiReport struct {
	agentID    string
	sessionKey string // "" = none on the context
	chatID     int64  // 0 = no chat (independent session)
	platform   string // "" = unknown
	backend    string // never empty ("" configured = "api")
	transport  string // "api" | "delegated"
	model      string // "" = unknown
}

// resolveWhoami gathers the calling session's identity from the context and
// the injected lookups. It never fails and never guesses: anything it cannot
// resolve stays zero and the renderers report it as none/unknown.
func resolveWhoami(ctx context.Context, d WhoamiDeps) whoamiReport {
	backend := d.Backend
	if backend == "" {
		backend = "api"
	}
	transport := "api"
	if d.Delegated {
		transport = "delegated"
	}
	rep := whoamiReport{agentID: d.AgentID, backend: backend, transport: transport}

	// agent_id is the configured agent serving the call; the session key's own
	// agent (used for the platform lookup below) is the same agent in
	// production wiring — the distinction only exists for odd test fixtures.
	rep.sessionKey = SessionKeyFromContext(ctx)
	if key, err := session.ParseSessionKey(rep.sessionKey); err == nil {
		// ChatID returns the root chat id for branch/spawn children of a chat
		// (c<chat>/b*, c<chat>/i*) and 0 for independent sessions, so branch
		// sessions report their parent chat — exactly requirement-level truth.
		if chatID := key.ChatID(); chatID != 0 {
			rep.chatID = chatID
			if d.PlatformForChat != nil {
				rep.platform = d.PlatformForChat(key.AgentID, chatID)
			}
		}
	}
	if rep.sessionKey != "" && d.SessionModel != nil {
		rep.model = d.SessionModel(rep.sessionKey)
	}
	return rep
}

// text renders one "name: value" line per field, in report order. Missing
// values are explicit: none for an absent session key or chat id, unknown for
// a platform or model that could not be resolved.
func (r whoamiReport) text() string {
	sessionKey := whoamiMarker(r.sessionKey, whoamiNone)
	chatID := whoamiNone
	if r.chatID != 0 {
		chatID = strconv.FormatInt(r.chatID, 10)
	}
	return fmt.Sprintf("agent_id: %s\nsession_key: %s\nchat_id: %s\nplatform: %s\nbackend: %s\ntransport: %s\nmodel: %s\n",
		r.agentID, sessionKey, chatID, whoamiMarker(r.platform, whoamiUnknown), r.backend, r.transport, whoamiMarker(r.model, whoamiUnknown))
}

// whoamiJSON is the --json shape: exactly the seven text fields, with null for
// the values the text form marks none/unknown.
type whoamiJSON struct {
	AgentID    string  `json:"agent_id"`
	SessionKey *string `json:"session_key"`
	ChatID     *int64  `json:"chat_id"`
	Platform   *string `json:"platform"`
	Backend    string  `json:"backend"`
	Transport  string  `json:"transport"`
	Model      *string `json:"model"`
}

func (r whoamiReport) jsonValue() whoamiJSON {
	var chatID *int64
	if r.chatID != 0 {
		id := r.chatID
		chatID = &id
	}
	return whoamiJSON{
		AgentID:    r.agentID,
		SessionKey: whoamiNilIfEmpty(r.sessionKey),
		ChatID:     chatID,
		Platform:   whoamiNilIfEmpty(r.platform),
		Backend:    r.backend,
		Transport:  r.transport,
		Model:      whoamiNilIfEmpty(r.model),
	}
}

// whoamiMarker returns s, or marker when s is empty.
func whoamiMarker(s, marker string) string {
	if s == "" {
		return marker
	}
	return s
}

// whoamiNilIfEmpty returns a pointer to s, or nil when s is empty.
func whoamiNilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NewWhoamiTool creates a read-only tool reporting the calling session's foci
// identity: agent id, session key, chat id, platform, backend, transport and
// model (#1135). It writes nothing and exposes nothing beyond those fields —
// no config values, tokens or paths — so agents can stop guessing facts like
// the chat id send_to_session addressing needs.
func NewWhoamiTool(d WhoamiDeps) *Tool {
	return &Tool{
		Name:        "whoami",
		Description: "Read-only report of the calling session's foci identity — agent id, session key, chat id, platform, backend, transport (`api` or `delegated`) and model; missing values are reported as `none`/`unknown`, never guessed.",
		ExecExport:  true,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		JSONOutput:  `{"agent_id": str, "session_key": str|null, "chat_id": number|null, "platform": str|null, "backend": str, "transport": "api"|"delegated", "model": str|null} — null marks a value the text form reports as none/unknown.`,
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			rep := resolveWhoami(ctx, d)
			if WantsJSON(ctx) {
				return JSONResult(rep.jsonValue())
			}
			return TextResult(rep.text()), nil
		},
	}
}
