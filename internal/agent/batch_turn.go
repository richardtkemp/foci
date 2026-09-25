package agent

import (
	"context"

	"foci/internal/turnevent"
)

// RunBatchTurn runs one ordinary turn on a batch session (see
// DelegatedManager.RunBatch, its only caller, wired as
// DelegatedManager.RunBatchTurn) and returns the final text to the caller
// instead of delivering it.
//
// It is HandleMessage — the turn every platform message, injection and branch
// goes through, so the api.db row and the trace are written by the usual turn
// code — with three things set up around it:
//
//   - the turn is marked as a batch for purpose (recorded on the row and the
//     trace; ComposePrompt sends the prompt verbatim) and triggered as it;
//   - a BufferSink captures the result. It is registered on the session's
//     router BEFORE dispatch: the orchestrator only registers a system turn's
//     sink after the begin-turn inject returns, and on a batch key anything
//     emitted in that gap would fall through to late delivery, which resolves
//     to the owner's chat. A fresh batch key has no concurrent autonomous run
//     whose delivery this could clobber (the reason the orchestrator waits);
//   - after the turn, a NopSink stays registered, so nothing the backend emits
//     before RunBatch closes it can reach a chat either.
func (a *Agent) RunBatchTurn(ctx context.Context, sessionKey, prompt, purpose string) (string, error) {
	buf := turnevent.NewBufferSink()
	ctx = WithTrigger(withBatchPurpose(ctx, purpose), purpose)
	ctx = turnevent.WithSink(ctx, buf)

	router := a.sessionRouter(sessionKey)
	router.Register(buf)
	defer router.Register(turnevent.NopSink{})

	if err := a.HandleMessage(ctx, sessionKey, []string{prompt}, nil); err != nil {
		return "", err
	}
	if err := buf.Err(); err != nil {
		return "", err
	}
	return buf.FinalText(), nil
}
