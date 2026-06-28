// types.go — Step 1.4 TDD-red-bar stubs for ccstream-shaped types that
// some KEPT tests in opencode_test.go construct directly. These are
// NOT the final opencode wire types (those land in Step 2.4's
// protocol.go and replace these stubs entirely).
//
// The reason these stubs exist: a handful of KEPT interface-level tests
// (TestWaitForTurn_SignalledByResult, TestBeginTurnResetsState)
// construct values to push at the Backend's internal channels / fields.
// The shape they use mirrors ccstream's wire types. When the real
// opencode types land in Step 2/Step 5, those tests will be rewritten
// against the new types and this file deleted.

package opencode

import "encoding/json"

// ResultMessage is a ccstream-shaped stub used by WaitForTurn tests.
// Step 7 replaces turnResultCh with a private *turnResult struct
// assembled from session.idle events; this type goes away.
type ResultMessage struct {
	Subtype string `json:"subtype"`
	Result  string `json:"result,omitempty"`
}

// TokenUsage is a ccstream-shaped stub used by TestBeginTurnResetsState.
// Step 2.4 replaces it with an opencode-shaped TokenUsage (or removes
// it in favour of delegator.TurnUsage directly).
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// pendingPermission stores a pending opencode permission request.
// The permType distinguishes regular tool permissions ("bash", "edit",
// etc.) from the built-in question tool ("question"), which foci
// handles via QuestionResponder rather than the binary Allow/Deny path.
type pendingPermission struct {
	id       string
	permType string          // "bash"|"edit"|"question"|...
	title    string
	metadata json.RawMessage // question schema for type=="question"
}
