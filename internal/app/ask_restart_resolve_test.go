package app

import (
	"testing"

	"foci/internal/app/fap"
)

// TestInteractiveResponse_BatchedAskAfterRestartLostRegistration reproduces #1473
// at the hub: a BATCHED ask is opened, a server RESTART drops the in-memory
// batchPrompts registration, then the app answers the still-displayed form. The
// answer arrives as InteractiveResponse{Answers, Data:""} with no matching
// registration.
//
// RED (before fix B): handleInteractiveResponse falls through to the single-prompt
// path, HandleInteractiveCallback fails on the empty Data, the prompt is deleted
// and the handler returns — emitting NO resolution frame (sibling never syncs) and
// delivering NO answer to the agent (which then re-asks).
//
// GREEN (with fix B): the batched Answers route to the ask layer (delivered) AND
// the Done progressEdit fans out to every attached client (sibling form/banner/chit
// resolve).
func TestInteractiveResponse_BatchedAskAfterRestartLostRegistration(t *testing.T) {
	h := newTestHub()

	// The agent's ask-layer route (fix B target). Captures what would be delivered
	// back into the session — the answer the agent was waiting on.
	var deliveredPrompt string
	var deliveredAnswers []string
	primary := &appConn{hub: h, agentID: "ag"}
	primary.routeBatchAnswer = func(promptID string, answers []string) {
		deliveredPrompt = promptID
		deliveredAnswers = answers
	}
	h.mu.Lock()
	h.agents["ag"] = primary
	// Durable conversation binding (survives a restart; both devices attached).
	b := &convBinding{convID: "c1", sessionKey: "ag/c7", agentID: "ag", chatID: 7}
	h.convs["c1"] = b
	h.bySession["ag/c7"] = b
	h.mu.Unlock()

	answerer := fakeClient()
	answerer.hub = h
	sibling := fakeClient()
	sibling.hub = h
	b.attach(answerer)
	b.attach(sibling)
	drain(t, answerer)
	drain(t, sibling)

	// NO registerBatchPrompt — the restart dropped the in-memory registration. The
	// app answers the pre-restart batched form (two questions).
	const promptID = "ask-ag-01ABCDEF-q0"
	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{
		ConversationID: "c1",
		PromptID:       promptID,
		Answers:        []string{"qa:0", "qa:3"},
	})

	// The answer must reach the ask layer (was silently lost before the fix).
	if deliveredAnswers == nil {
		t.Fatal("batched answer NOT delivered to the ask layer — #1473 drop (answer lost, agent re-asks)")
	}
	if deliveredPrompt != promptID {
		t.Errorf("delivered promptID = %q, want %q", deliveredPrompt, promptID)
	}
	if len(deliveredAnswers) != 2 || deliveredAnswers[0] != "qa:0" || deliveredAnswers[1] != "qa:3" {
		t.Errorf("delivered answers = %v, want [qa:0 qa:3]", deliveredAnswers)
	}

	// The terminal Done progressEdit must fan out to the SIBLING so its form closes,
	// its banner clears, and the complete chit renders.
	dd := drain(t, sibling)
	if len(dd) != 1 || dd[0].t != fap.TypeInteractiveProgressEdit || dd[0].d["done"] != true {
		t.Fatalf("sibling resolution frames = %v, want one Done progressEdit", types(dd))
	}
}
