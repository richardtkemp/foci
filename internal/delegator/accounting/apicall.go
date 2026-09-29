package accounting

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/modelinfo"
)

// APIResponse is one direct-API response, as its caller has it. The direct
// API is the simplest adapter (#2111 §3.1): one response is one call, final
// on receipt, with no backend report of its own.
type APIResponse struct {
	// ID is the provider's response id, the call's key.
	ID string
	// Kind is KindCall for a conversation turn's own call, else KindCompaction,
	// KindSummary or KindSpawn.
	Kind                     string
	Provider, Model          string
	Session, AgentID, TurnID string
	Start                    time.Time
	Duration                 time.Duration
	// Tokens are the response's counts, its cache writes already classed by
	// the backend rule (provider.Usage.Tokens: always 1h, per Dick).
	Tokens      modelinfo.Tokens
	StopReason  string
	SessionFile string
	SessionLine int
	// PreMessages is a compaction's message count before it ran.
	PreMessages int
}

// NoSession stands in for the session of a direct-API call made outside any
// session (a summary requested with none in its context): every call names a
// session.
const NoSession = "(none)"

// Call is the response as a ledger call. It is billed at its start, as the
// pre-ledger rows were stamped. A response with no id — a provider or test
// double that sets none — is keyed by its session, kind and start instant,
// which is unique: one session sends one request of a kind at a time.
func (r APIResponse) Call() Call {
	session := r.Session
	if session == "" {
		session = NoSession
	}
	key := r.ID
	if key == "" {
		key = fmt.Sprintf("%s:%s@%d", r.Kind, session, r.Start.UnixNano())
	}
	detail := map[string]any{"duration_ms": r.Duration.Milliseconds()}
	if r.PreMessages > 0 {
		detail["pre_messages"] = r.PreMessages
	}
	provider := r.Provider
	if provider == "" {
		provider = inferProvider(r.Model)
	}
	return Call{
		Key: key, Backend: BackendAPI, Provider: provider, Model: r.Model,
		Session: session, AgentID: r.AgentID, TurnID: r.TurnID,
		Kind: r.Kind, Finality: FinalityCompleted, ClassMethod: ClassMethodBackendRule,
		BilledAt: r.Start, Tokens: r.Tokens, StopReason: r.StopReason,
		SessionFile: r.SessionFile, SessionLine: r.SessionLine, Detail: detail,
	}
}

// inferProvider names a call's provider from its model when its caller gave
// none (a spawn whose route named no format), as the pre-ledger writer did.
func inferProvider(model string) string {
	switch {
	case strings.HasPrefix(model, "gemini-"):
		return "gemini"
	case modelinfo.IsOpenAI(model):
		return "openai"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	}
	return ""
}

// OwnTurn is the turn a helper call with no conversation turn of its own is
// booked on — a compaction (source compaction), or a summary or spawn
// (source system) — ending when the call does. R3: only overhead has no turn.
func OwnTurn(turnID, session, agentID, source string, start, end time.Time) Turn {
	if session == "" {
		session = NoSession
	}
	return Turn{
		TurnID: turnID, Session: session, AgentID: agentID, Backend: BackendAPI, Source: source,
		StartedAt: start, EndedAt: end, ActivityClosedAt: end,
	}
}

// MintTurnID names a helper call's own turn: its session and start instant,
// as a conversation turn's id is (agent.TurnState.RowID), plus the kind, so
// it can never collide with one.
func MintTurnID(session, kind string, start time.Time) string {
	if session == "" {
		session = NoSession
	}
	return fmt.Sprintf("%s@%d:%s", session, start.UnixNano(), kind)
}
