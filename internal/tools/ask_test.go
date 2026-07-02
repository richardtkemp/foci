package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/question"
)

// fakePresenter records presented questions and exposes the latest onResponse
// callback so a test can simulate the user answering.
type fakePresenter struct {
	mu            sync.Mutex
	presents      int
	lastMsgID     string
	lastText      string
	lastChoices   []question.Choice
	onResponse    func(string)
	platformMsgID string // returned from present (simulates the platform-side message id)
}

func (f *fakePresenter) present(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presents++
	f.lastMsgID = msgID
	f.lastText = text
	f.lastChoices = choices
	f.onResponse = onResponse
	return f.platformMsgID
}

func (f *fakePresenter) answer(data string) {
	f.mu.Lock()
	cb := f.onResponse
	f.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

// fakeCloser records (msgID, finalText) pairs passed to the AskCloseFn so a test
// can assert that an answered question's on-screen message was closed.
type fakeCloser struct {
	mu     sync.Mutex
	msgIDs []string
	texts  []string
}

func (c *fakeCloser) close(msgID, finalText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgIDs = append(c.msgIDs, msgID)
	c.texts = append(c.texts, finalText)
}

func (c *fakeCloser) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgIDs)
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
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, nil, "test")
	return tool, router, p, d
}

// newAskFixtureWithCloser is newAskFixture plus a fakeCloser wired as the
// AskCloseFn, for tests that assert answered questions are closed on screen.
func newAskFixtureWithCloser() (*Tool, *AskRouter, *fakePresenter, *fakeDeliver, *fakeCloser) {
	p := &fakePresenter{}
	d := &fakeDeliver{}
	c := &fakeCloser{}
	tool, router := NewAskTool(p.present, nil, d.deliver, c.close, nil, "test")
	return tool, router, p, d, c
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

// TestAsk_OnResolveFiresWhenAnswered proves the ask-resolve hook (#984): when a
// session's pending ask clears, WithOnResolve's callback fires with that session
// key so deferred injections can be redelivered.
func TestAsk_OnResolveFiresWhenAnswered(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{}
	d := &fakeDeliver{}
	resolved := make(chan string, 1)
	tool, _ := NewAskTool(p.present, nil, d.deliver, nil, nil, "test",
		WithOnResolve(func(sk string) { resolved <- sk }))

	execAsk(t, tool, `{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]}]}`)

	select {
	case <-resolved:
		t.Fatal("onResolve fired before the ask was answered")
	default:
	}

	p.answer("qa:1")

	select {
	case got := <-resolved:
		if got != askSession {
			t.Fatalf("onResolve fired with %q, want %q", got, askSession)
		}
	case <-time.After(time.Second):
		t.Fatal("onResolve did not fire within 1s of the ask being answered")
	}
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

// TestAsk_TypedAnswerClosesMessage proves that answering a question by TYPING
// closes its on-screen message (edits it shut, dropping the stale buttons) — the
// gap the button path covers itself but the typed path previously left open,
// most visibly for option-less questions whose only button is Cancel.
func TestAsk_TypedAnswerClosesMessage(t *testing.T) {
	t.Parallel()
	tool, router, _, _, c := newAskFixtureWithCloser()
	execAsk(t, tool, `{"questions":[{"question":"What name?"}]}`) // option-less

	reqID := router.PendingForSession(askSession)
	if reqID == "" {
		t.Fatal("router should report a pending ask")
	}
	router.HandleResponse(reqID, "Aristotle")

	if c.calls() != 1 {
		t.Fatalf("close called %d times, want 1", c.calls())
	}
	if want := questionMsgID(reqID, 0); c.msgIDs[0] != want {
		t.Errorf("closed msgID = %q, want %q", c.msgIDs[0], want)
	}
	if !strings.HasPrefix(c.texts[0], "✅ ") || !strings.Contains(c.texts[0], "Aristotle") {
		t.Errorf("closed text = %q, want a ✅ confirmation echoing the answer", c.texts[0])
	}
}

// TestAsk_EachAnsweredQuestionCloses proves every question in a sequence closes
// as it is answered, each addressed by its own per-index message id.
func TestAsk_EachAnsweredQuestionCloses(t *testing.T) {
	t.Parallel()
	tool, _, p, _, c := newAskFixtureWithCloser()
	execAsk(t, tool, `{"questions":[
		{"question":"Q1?","options":[{"label":"A1"}]},
		{"question":"Q2?"}
	]}`)

	p.answer("qa:0") // Q1 via button
	p.answer("typed reply") // Q2 via typing

	if c.calls() != 2 {
		t.Fatalf("close called %d times, want 2 (one per question)", c.calls())
	}
	// The two closes must address distinct, index-derived message ids.
	if c.msgIDs[0] == c.msgIDs[1] {
		t.Errorf("both closes hit the same msgID %q; want per-question ids", c.msgIDs[0])
	}
}

// TestAsk_PauseResumeRouting verifies the pause flag toggles via the router and
// that pause/resume are no-ops (return false) when no ask is pending.
func TestAsk_PauseResumeRouting(t *testing.T) {
	t.Parallel()
	tool, router, _, _ := newAskFixture()

	// No ask yet: every pause-path call is a no-op.
	if router.IsPaused(askSession) {
		t.Error("IsPaused should be false with no pending ask")
	}
	if router.PauseSession(askSession) {
		t.Error("PauseSession with no pending ask should return false")
	}
	if router.ResumeSession(askSession) {
		t.Error("ResumeSession with no pending ask should return false")
	}

	execAsk(t, tool, `{"questions":[{"question":"What name?","options":[{"label":"Default"}]}]}`)

	if router.IsPaused(askSession) {
		t.Error("a fresh ask should not be paused")
	}
	if !router.PauseSession(askSession) {
		t.Fatal("PauseSession should return true for a pending ask")
	}
	if !router.IsPaused(askSession) {
		t.Error("IsPaused should be true after PauseSession")
	}
	if !router.ResumeSession(askSession) {
		t.Fatal("ResumeSession should return true for a pending ask")
	}
	if router.IsPaused(askSession) {
		t.Error("IsPaused should be false after ResumeSession")
	}
}

// TestAsk_CompletePartial proves /complete delivers only the answered questions
// (with a partial preamble), drops the unanswered ones, clears the pending ask,
// and closes the still-displayed current question.
func TestAsk_CompletePartial(t *testing.T) {
	t.Parallel()
	tool, router, p, d, c := newAskFixtureWithCloser()
	execAsk(t, tool, `{"questions":[
		{"question":"Q1?","header":"H1","options":[{"label":"A1"},{"label":"B1"}]},
		{"question":"Q2?","header":"H2","options":[{"label":"A2"},{"label":"B2"}]},
		{"question":"Q3?","header":"H3","options":[{"label":"A3"},{"label":"B3"}]}
	]}`)
	p.answer("qa:0") // answer Q1 → A1; now positioned on Q2

	answered, total, ok := router.CompleteSession(askSession)
	if !ok || answered != 1 || total != 3 {
		t.Fatalf("CompleteSession = (%d,%d,%v), want (1,3,true)", answered, total, ok)
	}
	msg, got := d.last()
	if !got {
		t.Fatal("a partial batch should be delivered")
	}
	// Includes the answered question, excludes the unanswered ones, and signals partiality.
	if !strings.Contains(msg, `"Q1?":"A1"`) {
		t.Errorf("partial batch missing answered Q1:\n%s", msg)
	}
	if strings.Contains(msg, "Q2?") || strings.Contains(msg, "Q3?") {
		t.Errorf("partial batch must not mention unanswered questions:\n%s", msg)
	}
	if !strings.Contains(msg, "early") || !strings.Contains(msg, "1 of 3") {
		t.Errorf("partial batch should flag it as an early completion (1 of 3):\n%s", msg)
	}
	// Pending ask cleared.
	if router.PendingForSession(askSession) != "" {
		t.Error("pending ask should be cleared after /complete")
	}
	// The current (still-displayed) Q2 message is closed; plus the close for Q1's
	// answered message = 2 total.
	if c.calls() < 1 {
		t.Errorf("the current question should be closed on /complete (closes=%d)", c.calls())
	}
}

// TestAsk_CompleteZeroAnswered proves /complete before any answer is a no-op:
// nothing is delivered and the ask stays pending.
func TestAsk_CompleteZeroAnswered(t *testing.T) {
	t.Parallel()
	tool, router, _, d := newAskFixture()
	execAsk(t, tool, `{"questions":[
		{"question":"Q1?","options":[{"label":"A1"}]},
		{"question":"Q2?","options":[{"label":"A2"}]}
	]}`)

	answered, total, ok := router.CompleteSession(askSession)
	if ok || answered != 0 || total != 2 {
		t.Fatalf("CompleteSession with nothing answered = (%d,%d,%v), want (0,2,false)", answered, total, ok)
	}
	if _, got := d.last(); got {
		t.Error("nothing should be delivered when no question is answered")
	}
	if router.PendingForSession(askSession) == "" {
		t.Error("ask should remain pending after a no-op /complete")
	}
}

// TestAsk_CompleteNoPending proves /complete with no pending ask reports the
// no-active-question no-op (total==0).
func TestAsk_CompleteNoPending(t *testing.T) {
	t.Parallel()
	_, router, _, d := newAskFixture()
	answered, total, ok := router.CompleteSession(askSession)
	if ok || answered != 0 || total != 0 {
		t.Fatalf("CompleteSession with no ask = (%d,%d,%v), want (0,0,false)", answered, total, ok)
	}
	if _, got := d.last(); got {
		t.Error("nothing should be delivered with no pending ask")
	}
}

// TestAsk_CompleteRunsGraderOnPartial proves a configured grader still runs when
// the ask is completed early, and is told the set is partial (partial:true plus
// the answered/total counts in its stdin payload).
func TestAsk_CompleteRunsGraderOnPartial(t *testing.T) {
	t.Parallel()
	tool, router, p, d := newAskFixture()
	// Grader echoes whether it saw partial:true and the answered/total counts.
	grader := writeGrader(t, "#!/bin/sh\ninput=\"$(cat)\"\ncase \"$input\" in\n  *'\"partial\":true'*) echo \"graded-partial $1\" ;;\n  *) echo graded-full ;;\nesac\n")
	raw := `{"questions":[
		{"question":"Q1?","options":[{"label":"A1"},{"label":"B1"}]},
		{"question":"Q2?","options":[{"label":"A2"},{"label":"B2"}]}
	],"grader":"` + grader + `"}`
	execAsk(t, tool, raw)
	p.answer("qa:0") // answer Q1 only

	answered, total, ok := router.CompleteSession(askSession)
	if !ok || answered != 1 || total != 2 {
		t.Fatalf("CompleteSession = (%d,%d,%v), want (1,2,true)", answered, total, ok)
	}
	msg := waitDeliver(t, d)
	if !strings.Contains(msg, "graded-partial ask-") {
		t.Errorf("grader should run on the partial set and see partial:true, got: %q", msg)
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
		`not json`,
	}
	for _, c := range cases {
		if _, err := tool.Execute(askCtx(), json.RawMessage(c)); err == nil {
			t.Errorf("expected error for input %q", c)
		}
	}
}

// TestAsk_OptionlessQuestionTypedAnswer proves a question with no options is
// accepted (typed-answer-only) and a typed reply routed via the AskRouter
// completes and delivers it.
func TestAsk_OptionlessQuestionTypedAnswer(t *testing.T) {
	t.Parallel()
	tool, router, p, d := newAskFixture()

	// No options at all — must NOT error, and must present a question.
	execAsk(t, tool, `{"questions":[{"question":"What should I name it?"}]}`)
	if p.presents != 1 {
		t.Fatalf("option-less question should be presented (presents=%d)", p.presents)
	}
	// The only button offered is Cancel (no option buttons).
	if len(p.lastChoices) != 1 || p.lastChoices[0].Data != question.CancelData {
		t.Errorf("option-less question should offer only a Cancel button, got %+v", p.lastChoices)
	}

	reqID := router.PendingForSession(askSession)
	if reqID == "" {
		t.Fatal("option-less ask should be pending for the session")
	}
	router.HandleResponse(reqID, "Aristotle")

	msg, ok := d.last()
	if !ok {
		t.Fatal("typed answer to an option-less question should deliver the batch")
	}
	if !strings.Contains(msg, "Aristotle") {
		t.Errorf("delivered batch should contain the typed answer:\n%s", msg)
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

// writeGrader writes an executable grader script and returns its absolute path.
func writeGrader(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grader.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write grader: %v", err)
	}
	return path
}

// waitDeliver polls for a delivered message (the grader path delivers from a
// goroutine, so delivery is not synchronous with the final answer).
func waitDeliver(t *testing.T, d *fakeDeliver) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msg, ok := d.last(); ok {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no message delivered within timeout")
	return ""
}

func TestAsk_GraderTransformsAnswers(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	// Grader echoes a verdict derived from the answer (proves stdin plumbing)
	// and the request id from argv[1].
	grader := writeGrader(t, "#!/bin/sh\ninput=\"$(cat)\"\ncase \"$input\" in\n  *skylos*) echo \"correct ($1)\" ;;\n  *) echo wrong ;;\nesac\n")
	raw := `{"questions":[{"question":"dog?","options":[{"label":"skylos"},{"label":"gata"}]}],"grader":"` + grader + `"}`
	execAsk(t, tool, raw)
	p.answer("qa:0") // selects "skylos"

	msg := waitDeliver(t, d)
	if !strings.Contains(msg, "correct (ask-") {
		t.Errorf("expected grader stdout with request id, got: %q", msg)
	}
	// Success path replaces the raw batch — no answer list / JSON tail.
	if strings.Contains(msg, `"answers"`) || strings.Contains(msg, "act on them") {
		t.Errorf("graded success must not include the raw answer batch, got: %q", msg)
	}
}

func TestAsk_GraderFailureFallsBackToRawAnswers(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	grader := writeGrader(t, "#!/bin/sh\necho boom >&2\nexit 3\n")
	raw := `{"questions":[{"question":"color?","header":"Color","options":[{"label":"Red"}]}],"grader":"` + grader + `"}`
	execAsk(t, tool, raw)
	p.answer("qa:0")

	msg := waitDeliver(t, d)
	if !strings.Contains(msg, "Red") {
		t.Errorf("fallback must include the user's raw answer, got: %q", msg)
	}
	if !strings.Contains(msg, "grader could not run") {
		t.Errorf("fallback must note the grader failure, got: %q", msg)
	}
}

func TestAsk_GraderOnErrorReport(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	grader := writeGrader(t, "#!/bin/sh\nexit 1\n")
	raw := `{"questions":[{"question":"color?","header":"Color","options":[{"label":"Red"}]}],"grader":"` + grader + `","grader_on_error":"report"}`
	execAsk(t, tool, raw)
	p.answer("qa:0")

	msg := waitDeliver(t, d)
	if !strings.Contains(msg, "grader FAILED") {
		t.Errorf("report mode must lead with the failure, got: %q", msg)
	}
	if !strings.Contains(msg, "Red") {
		t.Errorf("report mode must still include raw answers, got: %q", msg)
	}
}

func TestAsk_GraderInvalidPathRejectedAtCallTime(t *testing.T) {
	t.Parallel()
	tool, _, _, _ := newAskFixture()
	for _, bad := range []string{
		`"grader":"/nonexistent/grader-xyz"`, // missing
		`"grader":"relative/path"`,           // not absolute
	} {
		raw := `{"questions":[{"question":"q?","options":[{"label":"A"}]}],` + bad + `}`
		if _, err := tool.Execute(askCtx(), json.RawMessage(raw)); err == nil {
			t.Errorf("expected call-time error for %s", bad)
		}
	}
}

func TestAsk_GraderOnErrorInvalidValueRejected(t *testing.T) {
	t.Parallel()
	tool, _, _, _ := newAskFixture()
	grader := writeGrader(t, "#!/bin/sh\necho ok\n")
	raw := `{"questions":[{"question":"q?","options":[{"label":"A"}]}],"grader":"` + grader + `","grader_on_error":"explode"}`
	if _, err := tool.Execute(askCtx(), json.RawMessage(raw)); err == nil {
		t.Error("expected error for invalid grader_on_error value")
	}
}

func TestAsk_QuestionIDPreservedInOutput(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	raw := `{"questions":[{"id":"q_color","question":"color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]}]}`
	execAsk(t, tool, raw)
	p.answer("qa:1") // Blue

	msg, ok := d.last() // non-grader path delivers synchronously
	if !ok {
		t.Fatal("no delivery")
	}
	// The id must be NOT shown in the question text but preserved in the output.
	if !strings.Contains(msg, `"answers_by_id"`) {
		t.Errorf("expected answers_by_id in output, got: %q", msg)
	}
	if !strings.Contains(msg, `"q_color":"Blue"`) {
		t.Errorf("expected id-keyed answer q_color→Blue, got: %q", msg)
	}
	if strings.Contains(p.lastText, "q_color") {
		t.Errorf("question id must not appear in presented text: %q", p.lastText)
	}
}

func TestAsk_NoIDMeansNoAnswersByID(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	raw := `{"questions":[{"question":"color?","header":"Color","options":[{"label":"Red"}]}]}`
	execAsk(t, tool, raw)
	p.answer("qa:0")
	msg, _ := d.last()
	if strings.Contains(msg, "answers_by_id") {
		t.Errorf("no id supplied → no answers_by_id, got: %q", msg)
	}
}

func TestAsk_GraderArgsPassedAsArgv(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	// Grader echoes argv: $1 is request_id, $2 is the first grader_arg. This
	// proves the contract argv = [request_id, ...grader_args] — the filename
	// lands at argv[2].
	grader := writeGrader(t, "#!/bin/sh\ncat >/dev/null\necho \"req=$1 file=$2\"\n")
	raw := `{"questions":[{"question":"q?","options":[{"label":"A"}]}],"grader":"` + grader + `","grader_args":["/data/quiz_01.json"]}`
	execAsk(t, tool, raw)
	p.answer("qa:0")

	msg := waitDeliver(t, d)
	if !strings.Contains(msg, "req=ask-") {
		t.Errorf("request_id must stay at argv[1], got: %q", msg)
	}
	if !strings.Contains(msg, "file=/data/quiz_01.json") {
		t.Errorf("grader_args[0] must land at argv[2], got: %q", msg)
	}
}

func TestAsk_GraderArgsWithoutGraderRejected(t *testing.T) {
	t.Parallel()
	tool, _, _, _ := newAskFixture()
	raw := `{"questions":[{"question":"q?","options":[{"label":"A"}]}],"grader_args":["x"]}`
	if _, err := tool.Execute(askCtx(), json.RawMessage(raw)); err == nil {
		t.Error("expected error when grader_args set without a grader")
	}
}

func TestAsk_ShellFuncGeneration(t *testing.T) {
	// The hand-rolled `ask` shell func must pass schema-parity validation
	// (questions is positional, so skipped) and offer JSON-only input.
	t.Parallel()
	tool, _ := NewAskTool(nil, nil, nil, nil, nil, "test")
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
