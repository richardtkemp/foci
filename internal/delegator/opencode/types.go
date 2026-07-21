// types.go — internal types shared across the opencode backend package.

package opencode

import "encoding/json"

// ResultMessage carries the subtype/result of a completed turn, used by
// WaitForTurn to signal turn completion.
type ResultMessage struct {
	Subtype string `json:"subtype"`
	Result  string `json:"result,omitempty"`
}

// TokenUsage mirrors opencode's per-message token breakdown.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`

	// CostUSD is opencode's own provider-reported cost for this message
	// (Message.Cost), captured alongside the tokens it was computed from. nil
	// only if no Tokens update has been seen yet (foci_todo #1407).
	CostUSD *float64
}

// pendingPermission stores a pending opencode permission request.
// The permType distinguishes regular tool permissions ("bash", "edit",
// etc.) from the built-in question tool ("question"), which foci
// handles via QuestionResponder rather than the binary Allow/Deny path.
type pendingPermission struct {
	id       string
	permType string // "bash"|"edit"|"question"|...
	title    string
	patterns []string        // command source texts (bash) or paths (read/edit) — from permission.asked
	metadata json.RawMessage // question schema for type=="question"
	// replyNext selects the reply transport: true for permission.asked
	// (opencode 1.2.x — POST /permission/{id}/reply {reply:once|always|reject});
	// false for the legacy permission.updated (POST /session/:id/permissions/:id
	// {response:allow|deny}). Set per-event so foci handles BOTH opencode
	// permission models (#arnix-perm).
	replyNext bool
	// aliasOf, when non-empty, marks this permission as a UI-duplicate of the
	// primary with this ID. opencode can raise multiple distinct permission
	// objects for the SAME target (one per tool call) near-simultaneously; we
	// surface only the primary's prompt and fan the user's single answer out to
	// the whole group. An alias is still Registered in `outstanding` (opencode
	// genuinely blocks on it), it just doesn't get its own prompt.
	aliasOf string
}
