package app

import (
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/question"
	"foci/internal/tools"
)

// lateAskHub is a hub with one agent whose ask layer has FORGOTTEN every ask
// (routeBatchAnswer reports no match) and records what the late-answer route is
// handed. Two devices are attached, so the removal must reach the sibling too.
func lateAskHub(t *testing.T) (h *Hub, b *convBinding, answerer, sibling *wsClient, late *[]tools.LateAnswer) {
	t.Helper()
	h = newTestHub()
	var got []tools.LateAnswer
	conn := &appConn{hub: h, agentID: "ag"}
	conn.routeBatchAnswer = func(string, []string) bool { return false }
	conn.routeLateAnswer = func(la tools.LateAnswer) bool {
		got = append(got, la)
		return true
	}
	h.agents["ag"] = conn
	b = &convBinding{convID: "c1", sessionKey: "ag/c7", agentID: "ag", chatID: 7}
	h.convs["c1"] = b
	h.bySession["ag/c7"] = b
	answerer, sibling = fakeClientFor(h), fakeClientFor(h)
	b.attach(answerer)
	b.attach(sibling)
	drain(t, answerer)
	drain(t, sibling)
	return h, b, answerer, sibling, &got
}

const lateBatchPrompt = "ask-ag-01K5ZQ3V9N8X7M6B5C4D3E2F1G-q0"

func sendBatchForm(b *convBinding) {
	b.send(fap.Interactive{ConversationID: b.convID, PromptID: lateBatchPrompt, Questions: []fap.Question{
		{Text: "Deploy tonight?", Header: "Deploy", Choices: []fap.Choice{{Label: "Yes", Data: "qa:0"}, {Label: "No", Data: "qa:1"}}},
		{Text: "Which host?", Choices: []fap.Choice{{Label: "nuc", Data: "qa:0"}, {Label: "mac", Data: "qa:1"}}},
	}})
}

func onlyRemove(t *testing.T, who string, c *wsClient, promptID string) {
	t.Helper()
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractiveRemove || ds[0].d["promptId"] != promptID {
		t.Fatalf("%s frames = %v, want exactly one interactive.remove for %s", who, types(ds), promptID)
	}
}

// #1868/#2080: a batched answer for an ask nobody holds any more (expired,
// dropped across a restart) is delivered to the agent as a late answer carrying
// the question TEXT, when it was asked and the raw choices — and the form is
// REMOVED from every device. Before the fix the ask layer logged "(dropped)" and
// returned, and the app was told the ask was answered (a Done edit).
func TestLateAnswer_UnregisteredBatchedAsk_DeliveredAndRemoved(t *testing.T) {
	h, b, answerer, sibling, late := lateAskHub(t)
	sendBatchForm(b)
	drain(t, answerer)
	drain(t, sibling)

	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{
		ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:0", "qa:1"},
	})

	if len(*late) != 1 {
		t.Fatalf("late answers routed = %d, want 1 — the answer was dropped", len(*late))
	}
	la := (*late)[0]
	if la.PromptID != lateBatchPrompt || la.SessionKey != "ag/c7" {
		t.Errorf("late answer prompt/session = %q/%q, want %q/ag/c7", la.PromptID, la.SessionKey, lateBatchPrompt)
	}
	if len(la.Questions) != 2 || la.Questions[0].Question != "Deploy tonight?" || la.Questions[1].Options[1].Label != "mac" {
		t.Errorf("late answer questions = %+v, want the two asked questions with their options", la.Questions)
	}
	if la.AskedAt.IsZero() || time.Since(la.AskedAt) > time.Minute {
		t.Errorf("late answer AskedAt = %v, want the form's send time", la.AskedAt)
	}
	if len(la.Answers) != 2 || la.Answers[0] != "qa:0" || la.Answers[1] != "qa:1" {
		t.Errorf("late answer raw answers = %v, want [qa:0 qa:1]", la.Answers)
	}
	onlyRemove(t, "answerer", answerer, lateBatchPrompt)
	onlyRemove(t, "sibling", sibling, lateBatchPrompt)
}

// The question survives in the DURABLE frame store after a restart has emptied
// the in-memory buffer, which is the real #2080 case (answers days later).
func TestLateAnswer_QuestionRecoveredFromDurableStore(t *testing.T) {
	h, b, answerer, _, late := lateAskHub(t)
	b.store = tempFrameStore(t)
	h.frames = b.store
	asked := time.Now().Add(-72 * time.Hour)
	wire, err := fap.Encode(fap.Interactive{ConversationID: "c1", PromptID: lateBatchPrompt, Questions: []fap.Question{
		{Text: "Deploy tonight?", Choices: []fap.Choice{{Label: "Yes", Data: "qa:0"}}},
	}}, 5, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b.store.insert(frameWrite{convID: "c1", agentID: "ag", seq: 5, wire: wire, sentMs: asked.UnixMilli()})
	b.seq = 5

	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:0"}})

	if len(*late) != 1 || len((*late)[0].Questions) != 1 || (*late)[0].Questions[0].Question != "Deploy tonight?" {
		t.Fatalf("late answers = %+v, want one carrying the stored question", *late)
	}
	if got := (*late)[0].AskedAt.UnixMilli(); got != asked.UnixMilli() {
		t.Errorf("AskedAt = %d, want the stored send time %d", got, asked.UnixMilli())
	}
	onlyRemove(t, "answerer", answerer, lateBatchPrompt)
}

// A second submit of a form that was already ANSWERED (another device, a double
// tap) is a duplicate: no late answer, and the answered marker is not removed.
func TestLateAnswer_AlreadyAnsweredIsDuplicateNotLate(t *testing.T) {
	h, b, answerer, sibling, late := lateAskHub(t)
	sendBatchForm(b)
	b.send(fap.InteractiveProgressEdit{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:0", "qa:0"}, Done: true})
	drain(t, answerer)
	drain(t, sibling)

	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:1", "qa:1"}})

	if len(*late) != 0 {
		t.Fatalf("duplicate submit routed as a late answer: %+v", *late)
	}
	if ds := drain(t, sibling); len(ds) != 0 {
		t.Fatalf("duplicate submit emitted %v, want nothing (keep the answered marker)", types(ds))
	}
}

// The per-question (single prompt) path had the same silent no-match return:
// a click whose callback is gone is now a late answer, and the prompt goes.
func TestLateAnswer_UnregisteredSingleClick_DeliveredAndRemoved(t *testing.T) {
	h, b, answerer, sibling, late := lateAskHub(t)
	const pid = "ask-ag-01K5ZQ3V9N8X7M6B5C4D3E2F1G-q1"
	b.send(fap.Interactive{ConversationID: "c1", PromptID: pid, Text: "Which host?", Header: "Host", Choices: []fap.Choice{
		{Label: "nuc", Data: pid + ":0"}, {Label: "mac", Data: pid + ":1"}, {Label: "Cancel", Data: pid + ":2"},
	}})
	drain(t, answerer)
	drain(t, sibling)

	// Nothing registered in platform's imStore for pid → HandleInteractiveCallback misses.
	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{ConversationID: "c1", PromptID: pid, Data: pid + ":1"})

	if len(*late) != 1 {
		t.Fatalf("late answers routed = %d, want 1", len(*late))
	}
	la := (*late)[0]
	if len(la.Answers) != 1 || la.Answers[0] != question.OptionData(1) {
		t.Errorf("raw answer = %v, want [%s]", la.Answers, question.OptionData(1))
	}
	if len(la.Questions) != 1 || la.Questions[0].Question != "Which host?" || len(la.Questions[0].Options) != 2 {
		t.Errorf("question = %+v, want the prompt text with its 2 options (Cancel dropped)", la.Questions)
	}
	onlyRemove(t, "answerer", answerer, pid)
	onlyRemove(t, "sibling", sibling, pid)
}

// The server's own expiry of a single prompt reaches the app as a removal, not an
// edit to "⌛ This request expired." (ruling 2: no expired state). The platform
// sweep picks RemoveInteractive over EditMessageText for any InteractiveRemover
// (pinned in internal/platform); this pins the app's half — the frame it emits.
func TestRemoveInteractive_EmitsRemoveFrame(t *testing.T) {
	h, c, b, conn := boundConn(t)
	const id = "ask-ag-expiry-q0"
	h.registerPrompt(id, b)

	if err := conn.RemoveInteractive(id); err != nil {
		t.Fatal(err)
	}
	onlyRemove(t, "client", c, id)
	if h.bindingForPrompt(id) != nil {
		t.Error("removed prompt still registered")
	}
	// Idempotent: a second removal of an unknown prompt emits nothing.
	_ = conn.RemoveInteractive(id)
	if ds := drain(t, c); len(ds) != 0 {
		t.Errorf("second removal emitted %v, want nothing", types(ds))
	}
}

// The core #2080 sequence in-process: the sweep expires a form (removing it from
// the app), then the user answers it anyway from a device that had not yet
// applied the removal. The answer is delivered late; no second removal is sent.
func TestLateAnswer_AfterExpirySweep(t *testing.T) {
	h, b, answerer, sibling, late := lateAskHub(t)
	var cancelled bool
	h.registerBatchPrompt(lateBatchPrompt, b, 2, func(a []string) { cancelled = len(a) == 1 && a[0] == question.CancelData })
	sendBatchForm(b)
	bp, _ := h.batchPromptByID(lateBatchPrompt)
	bp.created = time.Now().Add(-2 * time.Hour)
	h.expireBatchPrompts(time.Now().Add(-time.Hour))
	if !cancelled {
		t.Fatal("expiry did not resolve the ask layer's waiter")
	}
	drain(t, sibling)
	drain(t, answerer)

	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:1", "qa:0"}})

	if len(*late) != 1 || (*late)[0].Questions[0].Question != "Deploy tonight?" {
		t.Fatalf("late answers = %+v, want the answer to the expired form delivered", *late)
	}
	if ds := drain(t, sibling); len(ds) != 0 {
		t.Errorf("sibling got %v; the sweep already removed the form, want nothing more", types(ds))
	}
}

// Two devices submit the same live form at once: the one that loses the claim
// race is a duplicate of the winner's answer, not a late answer — even when it
// runs before the winner's Done frame exists. The claim record covers exactly that
// window: it is dropped once the Done frame is recorded (no timer), after which
// the frame history classifies the loser.
func TestLateAnswer_SecondDeviceAfterAnswerIsDuplicate(t *testing.T) {
	h, b, answerer, sibling, late := lateAskHub(t)
	var delivered int
	h.registerBatchPrompt(lateBatchPrompt, b, 2, func([]string) { delivered++ })
	sendBatchForm(b)
	drain(t, answerer)
	drain(t, sibling)

	// The winner has claimed but not yet sent its Done frame.
	bp, final, ok := h.claimBatchPrompt(lateBatchPrompt, []string{"qa:0", "qa:0"})
	if !ok {
		t.Fatal("claim failed")
	}
	h.handleInteractiveResponse(sibling, fap.InteractiveResponse{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:1", "qa:1"}})
	if len(*late) != 0 {
		t.Fatalf("losing submit (before Done) delivered as a late answer: %+v", *late)
	}
	if ds := drain(t, answerer); len(ds) != 0 {
		t.Fatalf("losing submit emitted %v, want nothing", types(ds))
	}

	// The winner resolves: once its Done frame is recorded the record is gone…
	h.resolveBatchedAsk(bp.b, lateBatchPrompt, final, bp.onResp)
	if _, held := h.batchClosed(lateBatchPrompt); held {
		t.Error("claim record outlived the Done frame's recording")
	}
	drain(t, answerer)
	drain(t, sibling)
	// …and a later loser is classified from the frame history instead.
	h.handleInteractiveResponse(sibling, fap.InteractiveResponse{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:1", "qa:1"}})
	if delivered != 1 || len(*late) != 0 {
		t.Fatalf("delivered=%d late=%+v, want exactly the winner's answer", delivered, *late)
	}
	if ds := drain(t, answerer); len(ds) != 0 {
		t.Fatalf("late loser emitted %v, want nothing (keep the answered marker)", types(ds))
	}
}

// With a durable store the claim record lives until the store has WRITTEN the
// closing frame — the in-memory buffer can be ack-trimmed before that — and is
// then dropped: bounded by the event, never by a timer.
func TestClosedBatch_ReleasedWhenClosingFrameStored(t *testing.T) {
	h, b, answerer, _, _ := lateAskHub(t)
	b.store = tempFrameStore(t)
	h.frames = b.store
	h.registerBatchPrompt(lateBatchPrompt, b, 1, func([]string) {})
	h.registerBatchPrompt("ask-ag-sweep-q0", b, 1, func([]string) {})
	bp, _ := h.batchPromptByID("ask-ag-sweep-q0")
	bp.created = time.Now().Add(-2 * time.Hour)

	h.handleInteractiveResponse(answerer, fap.InteractiveResponse{ConversationID: "c1", PromptID: lateBatchPrompt, Answers: []string{"qa:0"}})
	h.expireBatchPrompts(time.Now().Add(-time.Hour))

	for _, id := range []string{lateBatchPrompt, "ask-ag-sweep-q0"} {
		// The release is async (the store writer runs it after the insert); wait for
		// it, then check the frame really is stored — the record may only go once
		// the history can answer for it.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, held := h.batchClosed(id); !held {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("claim record for %s never released", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
		ph := h.lastResolution(&convBinding{convID: "c1"}, id) // store only: empty buffer
		if !ph.answered && !ph.removed {
			t.Errorf("%s: record released before its closing frame was stored", id)
		}
	}
}
