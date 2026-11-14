package agent

import (
	"fmt"
	"strings"
)

// The model block (#3106). Claude Code refuses a turn on a model the account
// may use only with usage credits it does not have: "Fable 5 requires usage
// credits. Switch to another model, or manage usage credits at <url>, to
// continue." This is not a time limit (no reset time; the rate-limit gate
// stays open) and it is per model, so it is kept per session. Dick's ruling
// (2026-11-14, option 1): post one notice that says to switch model, and send
// that session no more turns on that model until the model changes.
//
// A block is recorded when the backend reports the refusal
// (HostHooks.OnModelBlocked → BlockSessionModel) and lifted by any change of
// the session's model (SetSessionModel: /model, a model override set or
// cleared). While it holds, the turn orchestrator refuses every turn on the
// session before the backend sees it (modelBlockRefusesTurn): a user turn gets
// the refusal as its error reply, a background turn is dropped. The block is
// process-scoped: after a restart the first turn reaches the backend again,
// which re-blocks and re-notifies.

// ModelBlockedError is the error of a turn the model block refused.
type ModelBlockedError struct {
	// Detail is the backend's own refusal text.
	Detail string
}

func (e *ModelBlockedError) Error() string {
	return fmt.Sprintf("this session's model needs usage credits (%s). foci sends it no turns until you switch model with /model.",
		strings.TrimSuffix(firstLineOf(e.Detail), "."))
}

// ModelBlockNotice is the one chat notice posted when a session's model is
// blocked.
func ModelBlockNotice(detail string) string {
	return "🛑 Claude Code: " + firstLineOf(detail) +
		"\nfoci sends this session no more turns on this model. Switch model with /model to continue."
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// BlockSessionModel records that sessionKey's model needs usage credits.
// It reports whether the block is new, so the caller posts its notice once:
// the backend repeats the refusal on every turn it is sent, and a turn already
// in flight when the block engages can bring a second frame.
func (a *Agent) BlockSessionModel(sessionKey, detail string) (fresh bool) {
	_, loaded := a.modelBlocks.Swap(sessionKey, detail)
	if loaded {
		a.logger().Debugf("session=%s model block repeated: %s", sessionKey, firstLineOf(detail))
		return false
	}
	a.logger().Infof("session=%s model blocked until the model changes: %s", sessionKey, firstLineOf(detail))
	return true
}

// SessionModelBlocked reports whether sessionKey's model is blocked, with the
// backend's refusal text.
func (a *Agent) SessionModelBlocked(sessionKey string) (string, bool) {
	v, ok := a.modelBlocks.Load(sessionKey)
	if !ok {
		return "", false
	}
	return v.(string), true
}

// liftModelBlock ends sessionKey's model block, if any: its model changed.
func (a *Agent) liftModelBlock(sessionKey string) {
	if _, ok := a.modelBlocks.LoadAndDelete(sessionKey); ok {
		a.logger().Infof("session=%s model changed: model block lifted", sessionKey)
	}
}

// modelBlockRefusesTurn is the orchestrator's check: a turn on a session whose
// model is blocked does not start.
func (a *Agent) modelBlockRefusesTurn(ts *TurnState) error {
	detail, ok := a.SessionModelBlocked(ts.SessionKey)
	if !ok {
		return nil
	}
	return &ModelBlockedError{Detail: detail}
}
