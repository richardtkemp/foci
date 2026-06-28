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

// pendingPermission is the per-prompt store entry. Stub: fields get
// added in Step 9.2 when the real permission lifecycle is implemented.
type pendingPermission struct{}
