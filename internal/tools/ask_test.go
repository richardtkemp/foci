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
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, "test")
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
	tool, _ := NewAskTool(nil, nil, nil, nil, "test")
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
