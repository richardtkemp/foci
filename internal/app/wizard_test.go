package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/question"
)

// scriptedWizard is a WizardHandler whose replies are scripted per Handle
// call. steps[i] is the structured PendingStep AFTER i Handle calls (steps[0]
// = the activation step); a nil entry (or missing slice) means free-text.
type scriptedWizard struct {
	responses []string
	doneAt    int // 1-based Handle call index that returns done=true
	steps     []*question.Question
	docPath   string // returned once by PendingDoc after the next Handle

	calls  int
	inputs []string
}

func (w *scriptedWizard) Handle(text string) (string, bool) {
	w.inputs = append(w.inputs, text)
	w.calls++
	return w.responses[w.calls-1], w.calls >= w.doneAt
}

func (w *scriptedWizard) PendingStep() *question.Question {
	if w.calls < len(w.steps) {
		return w.steps[w.calls]
	}
	return nil
}

type docWizard struct {
	scriptedWizard
}

func (w *docWizard) PendingDoc() string {
	p := w.docPath
	w.docPath = ""
	return p
}

// wizardTestBed wires a hub + capable binding + registry for the wizard flow
// tests. The returned start func runs SetWizard + maybeStartWizard the way
// dispatchCommand would, returning the emitted wizard.step payload.
func wizardTestBed(t *testing.T, capable bool) (*Hub, *appConn, *convBinding, *wsClient) {
	t.Helper()
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1", clients: map[*wsClient]struct{}{c: {}}}
	if capable {
		b.features = map[string]struct{}{featureWizard: {}}
	}
	h.convs[b.convID] = b
	conn := &appConn{hub: h, agentID: "ag", commands: command.NewRegistry()}
	return h, conn, b, c
}

func startWizard(t *testing.T, h *Hub, conn *appConn, b *convBinding, w command.WizardHandler, firstPrompt string) bool {
	t.Helper()
	genBefore := conn.commands.WizardGen(b.sessionKey)
	conn.commands.SetWizard(b.sessionKey, w)
	return h.maybeStartWizard(conn, b, command.Response{Text: firstPrompt}, "/fake", b.sessionKey, genBefore)
}

// one drained frame of type want, or fatal.
func single(t *testing.T, c *wsClient, want string) map[string]any {
	t.Helper()
	got := drain(t, c)
	if len(got) != 1 || got[0].t != want {
		t.Fatalf("frames = %v, want one %s", types(got), want)
	}
	return got[0].d
}

func TestWizard_StartEmitsFreeTextStep(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	w := &scriptedWizard{responses: []string{"done"}, doneAt: 1}

	if !startWizard(t, h, conn, b, w, "Name?") {
		t.Fatal("maybeStartWizard = false, want true (capable client, fresh wizard)")
	}
	d := single(t, c, fap.TypeWizardStep)
	if d["title"] != "/fake" || d["conversationId"] != "c1" {
		t.Errorf("step envelope wrong: %v", d)
	}
	step := d["step"].(map[string]any)
	if step["text"] != "Name?" {
		t.Errorf("step text = %v, want the plain prompt (free-text fallback)", step["text"])
	}
	if _, hasChoices := step["choices"]; hasChoices {
		t.Errorf("free-text step must carry no choices: %v", step)
	}
}

func TestWizard_UncapableClientKeepsTextPath(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, false)
	w := &scriptedWizard{responses: []string{"done"}, doneAt: 1}

	if startWizard(t, h, conn, b, w, "Name?") {
		t.Fatal("maybeStartWizard = true for uncapable client, want false")
	}
	if got := drain(t, c); len(got) != 0 {
		t.Errorf("frames = %v, want none (caller renders plain text)", types(got))
	}
}

func TestWizard_NoActivationNoFrame(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	// A command that did NOT set a wizard: generation unchanged.
	if h.maybeStartWizard(conn, b, command.Response{Text: "plain reply"}, "/status", b.sessionKey, conn.commands.WizardGen(b.sessionKey)) {
		t.Fatal("maybeStartWizard = true with no activation")
	}
	if got := drain(t, c); len(got) != 0 {
		t.Errorf("frames = %v, want none", types(got))
	}
}

// The full happy path: free-text answer → structured step (buttons) → button
// pick resolves to the option label → wizard done → wizard.end(done).
func TestWizard_StepFlowToDone(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	backendQ := &question.Question{
		Header:   "Backend",
		Question: "How should this agent run?",
		Options:  []question.Option{{Label: "claude-code", Description: "Delegated"}, {Label: "api"}},
	}
	w := &scriptedWizard{
		responses: []string{"Backend?", "✅ created"},
		doneAt:    2,
		steps:     []*question.Question{nil, backendQ},
	}
	startWizard(t, h, conn, b, w, "Name?")
	first := single(t, c, fap.TypeWizardStep)
	wizID, stepID := first["wizardId"].(string), first["stepId"].(string)

	// Typed free-text answer advances to the structured backend step.
	h.handleWizardResponse(fap.WizardResponse{ConversationID: "c1", WizardID: wizID, StepID: stepID, Data: "George"})
	d := single(t, c, fap.TypeWizardStep)
	if d["wizardId"] != wizID {
		t.Errorf("wizardId changed across steps: %v", d["wizardId"])
	}
	if d["stepId"] == stepID {
		t.Error("stepId must be re-minted per step")
	}
	step := d["step"].(map[string]any)
	if step["header"] != "Backend" {
		t.Errorf("structured step header = %v", step["header"])
	}
	choices := step["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices = %v, want 2", choices)
	}
	if c0 := choices[0].(map[string]any); c0["data"] != "qa:0" || c0["label"] != "claude-code" || c0["description"] != "Delegated" {
		t.Errorf("choice 0 wrong: %v", c0)
	}

	// Button pick: qa:0 resolves to the label, wizard finishes.
	h.handleWizardResponse(fap.WizardResponse{ConversationID: "c1", WizardID: wizID, StepID: d["stepId"].(string), Data: "qa:0"})
	end := single(t, c, fap.TypeWizardEnd)
	if end["status"] != fap.WizardDone || end["text"] != "✅ created" {
		t.Errorf("end frame wrong: %v", end)
	}
	if len(w.inputs) != 2 || w.inputs[0] != "George" || w.inputs[1] != "claude-code" {
		t.Errorf("wizard received inputs %v, want [George claude-code]", w.inputs)
	}
	if conn.commands.WizardActive(b.sessionKey) {
		t.Error("registry wizard should be cleared after done")
	}
	h.wizardMu.Lock()
	defer h.wizardMu.Unlock()
	if len(h.wizards) != 0 || len(h.wizardByScope) != 0 {
		t.Error("wizard session leaked after end")
	}
}

func TestWizard_CancelSentinel(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	w := &scriptedWizard{responses: []string{"never"}, doneAt: 99}
	startWizard(t, h, conn, b, w, "Name?")
	first := single(t, c, fap.TypeWizardStep)

	h.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1",
		WizardID:       first["wizardId"].(string),
		StepID:         first["stepId"].(string),
		Data:           question.CancelData,
	})
	end := single(t, c, fap.TypeWizardEnd)
	if end["status"] != fap.WizardCancelled {
		t.Errorf("status = %v, want cancelled", end["status"])
	}
	if len(w.inputs) != 0 {
		t.Errorf("cancel must be consumed by HandleMessage, not the wizard; got inputs %v", w.inputs)
	}
	if conn.commands.WizardActive(b.sessionKey) {
		t.Error("registry wizard should be cleared by /cancel")
	}
}

func TestWizard_UnknownIDExpires(t *testing.T) {
	h, _, _, c := wizardTestBed(t, true)
	h.handleWizardResponse(fap.WizardResponse{ConversationID: "c1", WizardID: "stale", StepID: "s", Data: "x"})
	end := single(t, c, fap.TypeWizardEnd)
	if end["status"] != fap.WizardExpired || end["wizardId"] != "stale" {
		t.Errorf("end frame wrong: %v (self-heal for post-restart rows)", end)
	}
}

func TestWizard_StaleStepDropped(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	w := &scriptedWizard{responses: []string{"next"}, doneAt: 99}
	startWizard(t, h, conn, b, w, "Name?")
	first := single(t, c, fap.TypeWizardStep)

	h.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1",
		WizardID:       first["wizardId"].(string),
		StepID:         "old-step",
		Data:           "x",
	})
	if got := drain(t, c); len(got) != 0 {
		t.Errorf("frames = %v, want none (stale stepId dropped)", types(got))
	}
	if len(w.inputs) != 0 {
		t.Errorf("stale response must not reach the wizard; got %v", w.inputs)
	}
	if !conn.commands.WizardActive(b.sessionKey) {
		t.Error("wizard must stay active after a dropped stale response")
	}
}

func TestWizard_ReplacedWizardExpiresSession(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	startWizard(t, h, conn, b, &scriptedWizard{responses: []string{"next"}, doneAt: 99}, "Name?")
	first := single(t, c, fap.TypeWizardStep)

	// The Registry's wizard is replaced behind the session's back (e.g. a
	// second wizard started from Telegram — no app hook runs).
	conn.commands.SetWizard(b.sessionKey, &scriptedWizard{responses: []string{"other"}, doneAt: 99})

	h.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1",
		WizardID:       first["wizardId"].(string),
		StepID:         first["stepId"].(string),
		Data:           "answer for the wrong wizard",
	})
	end := single(t, c, fap.TypeWizardEnd)
	if end["status"] != fap.WizardExpired {
		t.Errorf("status = %v, want expired (gen mismatch)", end["status"])
	}
}

func TestWizard_SupersededSessionEndsCancelled(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	startWizard(t, h, conn, b, &scriptedWizard{responses: []string{"next"}, doneAt: 99}, "Name?")
	_ = single(t, c, fap.TypeWizardStep)

	// A second wizard-starting command on the same agent (app path this time):
	// the old session must end cancelled before the new step goes out.
	if !startWizard(t, h, conn, b, &scriptedWizard{responses: []string{"other"}, doneAt: 99}, "Other?") {
		t.Fatal("second wizard should start")
	}
	got := drain(t, c)
	if len(got) != 2 || got[0].t != fap.TypeWizardEnd || got[1].t != fap.TypeWizardStep {
		t.Fatalf("frames = %v, want [wizard.end wizard.step]", types(got))
	}
	if got[0].d["status"] != fap.WizardCancelled {
		t.Errorf("old session end status = %v, want cancelled", got[0].d["status"])
	}
}

// The restart story end-to-end: a real /agents new wizard is mid-flow when the
// "process" dies. A fresh registry restores the wizard from the session index,
// a fresh hub re-links the persisted app session to it, and the phone's
// wizard.response — carrying the pre-restart wizardId/stepId — routes into the
// restored wizard and advances it, instead of dying with expired.
func TestWizard_SessionSurvivesRestart(t *testing.T) {
	idx := newTestIndex(t)
	ccFor := func(reg *command.Registry) command.CommandContext {
		return command.CommandContext{AgentNewDeps: &command.AgentNewDeps{
			Registry: reg,
			ListFn:   func() []command.AgentInfo { return nil },
		}}
	}

	// --- process 1: start /agents new, answer the name step ---
	h1, conn1, b1, c1 := wizardTestBed(t, true)
	h1.deps.SessionIndex = idx
	conn1.commands.EnableWizardPersistence(idx, "ag")
	conn1.cmdCtx = ccFor(conn1.commands)
	conn1.commands.Register(command.AgentsCommand())

	genBefore := conn1.commands.WizardGen(b1.sessionKey)
	resp, handled, err := conn1.commands.Dispatch(context.Background(),
		command.Request{Name: "agents", Args: "new", SessionKey: b1.sessionKey}, conn1.cmdCtx)
	if err != nil || !handled {
		t.Fatalf("dispatch /agents new: handled=%v err=%v", handled, err)
	}
	if !h1.maybeStartWizard(conn1, b1, resp, "/agents new", b1.sessionKey, genBefore) {
		t.Fatal("wizard should start out-of-band")
	}
	first := single(t, c1, fap.TypeWizardStep)
	wizID := first["wizardId"].(string)

	h1.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1", WizardID: wizID,
		StepID: first["stepId"].(string), Data: "Greek Tutor",
	})
	second := single(t, c1, fap.TypeWizardStep)
	stepID2 := second["stepId"].(string)
	if second["step"].(map[string]any)["header"] != "Backend" {
		t.Fatalf("expected the structured backend step pre-restart, got %v", second["step"])
	}

	// --- process 2: fresh registry + hub restore from the same index ---
	reg2 := command.NewRegistry()
	reg2.EnableWizardPersistence(idx, "ag")
	reg2.RestoreWizards(ccFor(reg2))
	if !reg2.WizardActive(b1.sessionKey) {
		t.Fatal("registry should restore the wizard")
	}
	h2 := newTestHub()
	h2.deps.SessionIndex = idx
	conn2 := &appConn{hub: h2, agentID: "ag", commands: reg2}
	h2.restoreWizardSessions(conn2, "ag")

	// Attach a socket to the restored binding so we can observe frames.
	b2 := h2.ensureBinding(nil, "ag", "c1")
	b2.features = map[string]struct{}{featureWizard: {}}
	c2 := fakeClientForHub(h2)
	b2.attach(c2)

	// The phone answers the backend step with a button pick from BEFORE the
	// restart — same wizardId + stepId. It must advance, not expire.
	h2.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1", WizardID: wizID, StepID: stepID2, Data: "qa:0",
	})
	next := single(t, c2, fap.TypeWizardStep)
	if next["wizardId"] != wizID {
		t.Errorf("wizardId = %v, want %v (same wizard across restart)", next["wizardId"], wizID)
	}
	if txt := next["step"].(map[string]any)["text"].(string); !strings.Contains(txt, "Model") {
		t.Errorf("restored wizard should advance to the model step, got %q", txt)
	}
}

// A WizardDocProvider's file (the /android QR) is staged as a blob and
// referenced INLINE from the next wizard.step's media, then removed
// (consume-once) — no separate in-chat Media frame.
func TestWizard_DocInlineOnNextStep(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	// A blob store rooted in the test's temp dir (newTestHub's default points at
	// the shared /tmp, unwritable on some hosts).
	h.blobs = &blobStore{dir: t.TempDir(), maxBytes: maxBlobBytes, ttl: blobTTL, blobs: make(map[string]*blobMeta)}
	doc := filepath.Join(t.TempDir(), "qr.png")
	if err := os.WriteFile(doc, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &docWizard{scriptedWizard: scriptedWizard{responses: []string{"scan this"}, doneAt: 99}}
	startWizard(t, h, conn, b, w, "Start?")
	first := single(t, c, fap.TypeWizardStep)
	w.docPath = doc

	h.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1",
		WizardID:       first["wizardId"].(string),
		StepID:         first["stepId"].(string),
		Data:           "yes",
	})
	if _, err := os.Stat(doc); !os.IsNotExist(err) {
		t.Errorf("doc file should be removed after staging (consume-once), stat err = %v", err)
	}
	next := single(t, c, fap.TypeWizardStep)
	media, ok := next["media"].(map[string]any)
	if !ok {
		t.Fatalf("next step should carry inline media, got %v", next)
	}
	blobID, _ := media["blobId"].(string)
	if blobID == "" || media["mime"] != "image/png" {
		t.Errorf("media wrong: %v", media)
	}
	// The referenced blob is actually fetchable.
	if meta, ok := h.blobs.get(blobID); !ok || meta.size == 0 {
		t.Errorf("staged blob %q should exist in the store", blobID)
	}
}

// A doc produced by the wizard's FINAL step has no next step to ride on — it
// falls back to the in-chat path (consume-once still holds).
func TestWizard_DocOnFinalStepFallsBackInChat(t *testing.T) {
	h, conn, b, c := wizardTestBed(t, true)
	doc := filepath.Join(t.TempDir(), "qr.png")
	if err := os.WriteFile(doc, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &docWizard{scriptedWizard: scriptedWizard{responses: []string{"all done"}, doneAt: 1}}
	startWizard(t, h, conn, b, w, "Start?")
	first := single(t, c, fap.TypeWizardStep)
	w.docPath = doc

	h.handleWizardResponse(fap.WizardResponse{
		ConversationID: "c1",
		WizardID:       first["wizardId"].(string),
		StepID:         first["stepId"].(string),
		Data:           "yes",
	})
	if _, err := os.Stat(doc); !os.IsNotExist(err) {
		t.Errorf("doc file should be removed (consume-once), stat err = %v", err)
	}
	end := single(t, c, fap.TypeWizardEnd)
	if end["status"] != fap.WizardDone {
		t.Errorf("end status = %v", end["status"])
	}
}
