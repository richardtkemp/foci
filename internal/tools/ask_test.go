package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/question"
)

// fakePresenter records presented questions and exposes the latest onResponse
// callback so a test can simulate the user answering.
type fakePresenter struct {
	mu          sync.Mutex
	presents    int
	lastMsgID   string
	lastText    string
	lastChoices []question.Choice
	onResponse  func(string)
}

func (f *fakePresenter) present(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presents++
	f.lastMsgID = msgID
	f.lastText = text
	f.lastChoices = choices
	f.onResponse = onResponse
}

func (f *fakePresenter) answer(data string) {
	f.mu.Lock()
	cb := f.onResponse
	f.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

// fakeDeliver records messages delivered back into the session.
type fakeDeliver struct {
	mu       sync.Mutex
	messages []string
	sessions []string
}

func (d *fakeDeliver) deliver(sessionKey, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions = append(d.sessions, sessionKey)
	d.messages = append(d.messages, message)
}

func (d *fakeDeliver) last() (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.messages) == 0 {
		return "", false
	}
	return d.messages[len(d.messages)-1], true
}

func newAskFixture() (*Tool, *AskRouter, *fakePresenter, *fakeDeliver) {
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, router := NewAskTool(p.present, d.deliver)
	return tool, router, p, d
}

const askSession = "clutch/c123/1000"

func askCtx() context.Context {
	return WithSessionKey(context.Background(), askSession)
}

func execAsk(t *testing.T, tool *Tool, raw string) ToolResult {
	t.Helper()
	res, err := tool.Execute(askCtx(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestAsk_ReturnsImmediatelyAndPresentsFirst(t *testing.T) {
	t.Parallel()
	tool, _, p, _ := newAskFixture()
	res := execAsk(t, tool, `{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]}]}`)

	// The result is the async ack, NOT the answers.
	var ack struct {
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
		Questions int    `json:"questions"`
	}
	if err := json.Unmarshal([]byte(res.Text), &ack); err != nil {
		t.Fatalf("unmarshal ack %q: %v", res.Text, err)
	}
	if ack.Status != "asked" || ack.RequestID == "" || ack.Questions != 1 {
		t.Errorf("unexpected ack: %+v", ack)
	}
	if strings.Contains(ack.RequestID, ":") {
		t.Errorf("request_id %q must be colon-free (breaks button routing otherwise)", ack.RequestID)
	}
	if p.presents != 1 {
		t.Fatalf("present called %d times, want 1", p.presents)
	}
	// Choices = options + Cancel.
	if len(p.lastChoices) != 3 || p.lastChoices[2].Data != question.CancelData {
		t.Errorf("choices = %+v, want 2 options + Cancel", p.lastChoices)
	}
}

func TestAsk_SingleButtonAnswerDelivers(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	execAsk(t, tool, `{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]}]}`)

	if _, ok := d.last(); ok {
		t.Fatal("nothing should be delivered before the user answers")
	}
	p.answer("qa:1") // Blue

	msg, ok := d.last()
	if !ok {
		t.Fatal("answer batch should have been delivered")
	}
	if !strings.Contains(msg, "Blue") {
		t.Errorf("delivered message missing answer 'Blue':\n%s", msg)
	}
	// The compact JSON tail must carry the answer keyed by question text.
	if !strings.Contains(msg, `"Which color?":"Blue"`) {
		t.Errorf("delivered message missing JSON answers:\n%s", msg)
	}
}

func TestAsk_SequentialMultiQuestion(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	execAsk(t, tool, `{"questions":[
		{"question":"Q1?","header":"H1","options":[{"label":"A1"},{"label":"B1"}]},
		{"question":"Q2?","header":"H2","options":[{"label":"A2"},{"label":"B2"}]}
	]}`)

	if p.presents != 1 {
		t.Fatalf("after Execute: present=%d, want 1", p.presents)
	}
	p.answer("qa:0") // A1

	if p.presents != 2 {
		t.Fatalf("after Q1 answer: present=%d, want 2 (Q2 presented)", p.presents)
	}
	if _, ok := d.last(); ok {
		t.Fatal("must not deliver until all questions answered")
	}
	p.answer("qa:1") // B2

	msg, ok := d.last()
	if !ok {
		t.Fatal("answer batch should be delivered after final answer")
	}
	if !strings.Contains(msg, `"Q1?":"A1"`) || !strings.Contains(msg, `"Q2?":"B2"`) {
		t.Errorf("delivered batch missing answers:\n%s", msg)
	}
}

func TestAsk_Cancel(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	execAsk(t, tool, `{"questions":[{"question":"Q?","options":[{"label":"A"}]}]}`)
	p.answer(question.CancelData)

	msg, ok := d.last()
	if !ok {
		t.Fatal("a cancel notice should be delivered so the agent knows")
	}
	if !strings.Contains(strings.ToUpper(msg), "CANCEL") {
		t.Errorf("cancel message should mention cancellation:\n%s", msg)
	}
}

func TestAsk_TypedAnswerViaRouter(t *testing.T) {
	t.Parallel()
	tool, router, _, d := newAskFixture()
	execAsk(t, tool, `{"questions":[{"question":"What name?","options":[{"label":"Default"}]}]}`)

	reqID := router.PendingForSession(askSession)
	if reqID == "" {
		t.Fatal("router should report a pending ask for the session")
	}
	// User types a freeform ("Other") answer instead of clicking.
	router.HandleResponse(reqID, "Aristotle")

	msg, ok := d.last()
	if !ok {
		t.Fatal("typed answer should complete and deliver the batch")
	}
	if !strings.Contains(msg, "Aristotle") {
		t.Errorf("delivered batch should contain the typed answer:\n%s", msg)
	}
	if router.PendingForSession(askSession) != "" {
		t.Error("pending ask should be cleared after completion")
	}
}

func TestAsk_NoSessionErrors(t *testing.T) {
	t.Parallel()
	tool, _, _, _ := newAskFixture()
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"questions":[{"question":"Q?","options":[{"label":"A"}]}]}`)); err == nil {
		t.Error("Execute without a session in context should error")
	}
}

func TestAsk_Validation(t *testing.T) {
	t.Parallel()
	tool, _, _, _ := newAskFixture()
	cases := []string{
		`{"questions":[]}`,
		`{"questions":[{"question":"","options":[{"label":"A"}]}]}`,
		`{"questions":[{"question":"Q?","options":[]}]}`,
		`not json`,
	}
	for _, c := range cases {
		if _, err := tool.Execute(askCtx(), json.RawMessage(c)); err == nil {
			t.Errorf("expected error for input %q", c)
		}
	}
}

func TestAsk_ManyQuestionsNoCap(t *testing.T) {
	// The reason the tool exists: more than 4 questions must work.
	t.Parallel()
	tool, _, p, d := newAskFixture()
	var sb strings.Builder
	sb.WriteString(`{"questions":[`)
	for i := 0; i < 7; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"question":"Q","options":[{"label":"A"},{"label":"B"}]}`)
	}
	sb.WriteString(`]}`)
	execAsk(t, tool, sb.String())

	for i := 0; i < 7; i++ {
		if _, ok := d.last(); ok {
			t.Fatalf("delivered early at answer %d", i)
		}
		p.answer("qa:0")
	}
	if _, ok := d.last(); !ok {
		t.Error("batch should deliver after all 7 questions answered")
	}
}

func TestAsk_ShellFuncGeneration(t *testing.T) {
	// The hand-rolled `ask` shell func must pass schema-parity validation
	// (questions is positional, so skipped) and offer JSON-only input.
	t.Parallel()
	tool, _ := NewAskTool(nil, nil)
	if err := validateShellFuncSchemaParity(tool); err != nil {
		t.Fatalf("ask shell func failed parity: %v", err)
	}
	body := generateShellFunc(tool)
	for _, want := range []string{"foci_ask()", "--json", "foci-call"} {
		if !strings.Contains(body, want) {
			t.Errorf("generated shell func missing %q:\n%s", want, body)
		}
	}
	// JSON-only: no per-field flags like --question / --options.
	if strings.Contains(body, "--question)") || strings.Contains(body, "--options)") {
		t.Errorf("ask shell func should not expose flat per-field flags:\n%s", body)
	}
}
