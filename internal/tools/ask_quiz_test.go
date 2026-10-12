package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/question"
)

// Distinctive quiz secrets for the tool-level no-leak assertions — see
// question.quizOptionQuestion for why the correct LABEL cannot be one.
const (
	quizSecretAnswer = "M0DEL-ANSWER-skylos"
	quizSecretExpl   = "XPLAN-9-never-in-a-prompt"
)

// quizRevealRecorder is the fake AskRevealFn: it records every reveal's session
// key and text. Concurrent-safe because the reveal hook fires from platform
// callback goroutines in production shapes.
type quizRevealRecorder struct {
	mu      sync.Mutex
	session []string
	texts   []string
}

func (r *quizRevealRecorder) reveal(sessionKey, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = append(r.session, sessionKey)
	r.texts = append(r.texts, text)
}

func (r *quizRevealRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.texts)
}

func (r *quizRevealRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.texts) == 0 {
		return ""
	}
	return r.texts[len(r.texts)-1]
}

// TestAsk_QuizValidationRejectedAtCallTime pins requirement 1's rejection
// behaviour: invalid quiz input is a TOOL ERROR naming the question (number and
// text) and the problem, and the ask is never presented. On the pre-change tree
// the `quiz` key is silently ignored, the ask succeeds and these assertions
// fail — the red gate for this suite.
func TestAsk_QuizValidationRejectedAtCallTime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		raw      string
		wantErrs []string
	}{
		{
			"correct out of range",
			`{"questions":[{"question":"Capital?","options":[{"label":"Bergen"},{"label":"Oslo"}],"quiz":{"correct":[2],"explanation":"` + quizSecretExpl + `"}}]}`,
			[]string{"question 1", "Capital?", "0-based index 2 is out of range: question has 2 options"},
		},
		{
			"neither correct nor answer",
			`{"questions":[{"question":"Capital?","options":[{"label":"Oslo"}],"quiz":{}}]}`,
			[]string{"question 1", "needs"},
		},
		{
			"free recall with options",
			`{"questions":[{"question":"Capital?","options":[{"label":"Oslo"}],"quiz":{"answer":"Oslo"}}]}`,
			[]string{"question 1", "only for a free-recall question"},
		},
		{
			"free recall carrying correct",
			`{"questions":[{"question":"Translate.","quiz":{"correct":[0]}}]}`,
			[]string{"question 1", "only for a question with options"},
		},
		{
			"free recall with no answer",
			`{"questions":[{"question":"Translate.","quiz":{"explanation":"x"}}]}`,
			[]string{"question 1", "needs the correct option(s)"},
		},
		{
			"second question invalid names IT",
			`{"questions":[{"question":"Fine?","options":[{"label":"Yes"}],"quiz":{"correct":[0]}},{"question":"Bad?","options":[{"label":"No"}],"quiz":{"correct":[5]}}]}`,
			[]string{"question 2", "Bad?"},
		},
	}
	for _, tc := range cases {
		p := &fakePresenter{}
		d := &fakeDeliver{}
		tool, _ := NewAskTool(p.present, nil, d.deliver, nil, nil, "test")
		_, err := tool.Execute(askCtx(), json.RawMessage(tc.raw))
		if err == nil {
			t.Errorf("%s: expected a tool error, got success", tc.name)
			continue
		}
		for _, want := range tc.wantErrs {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q missing %q", tc.name, err.Error(), want)
			}
		}
		if p.presents != 0 {
			t.Errorf("%s: invalid quiz must reject BEFORE presenting (presents=%d)", tc.name, p.presents)
		}
		if _, ok := d.last(); ok {
			t.Errorf("%s: nothing may be delivered for a rejected ask", tc.name)
		}
	}

	// Well-formed quiz input is accepted and presented.
	p := &fakePresenter{}
	tool, _ := NewAskTool(p.present, nil, func(sk, req, msg string) {}, nil, nil, "test")
	if _, err := tool.Execute(askCtx(), json.RawMessage(
		`{"questions":[{"question":"Capital?","options":[{"label":"Oslo"},{"label":"Bergen"}],"quiz":{"correct":[0],"explanation":"x"}}]}`)); err != nil {
		t.Fatalf("valid quiz input rejected: %v", err)
	}
	if p.presents != 1 {
		t.Errorf("valid quiz question should present once, got %d", p.presents)
	}
}

// TestAsk_QuizVerdictInDeliveredBatch pins requirement 4: the delivered answer
// batch carries a per-question correct/incorrect verdict for quiz OPTION
// questions (readable marker + "quiz_results", and "quiz_results_by_id" when
// ids are set), and no verdict at all for free-recall quiz questions or
// non-quiz questions. Existing keys ("answers", "answers_by_id") are
// unchanged. On the pre-change tree no "quiz_results" key exists, so the
// verdict assertions fail. The JSON payload is parsed rather than substring-
// matched because json.Marshal HTML-escapes the `<`/`>` in the question text.
func TestAsk_QuizVerdictInDeliveredBatch(t *testing.T) {
	t.Parallel()

	// One ask: a quiz option question (id "cap"), a free-recall quiz question
	// (id "tr") and a plain question. q0Choice drives the quiz answer ("qa:1"
	// = Oslo = correct, "qa:0" = Bergen = wrong).
	delivered := func(t *testing.T, q0Choice string) (string, map[string]any) {
		t.Helper()
		tool, _, p, d := newAskFixture()
		execAsk(t, tool, `{"questions":[
			{"question":"Capital of <Norway>?","id":"cap","header":"Geo","options":[{"label":"Bergen"},{"label":"Oslo"}],"quiz":{"correct":[1],"explanation":"`+quizSecretExpl+`"}},
			{"question":"Translate dog.","id":"tr","quiz":{"answer":"`+quizSecretAnswer+`"}},
			{"question":"Plain?","options":[{"label":"Yes"}]}]}`)
		p.answer(q0Choice) // Q0: quiz option question
		p.answer("a dog")  // Q1: free recall, typed
		p.answer("qa:0")   // Q2: plain
		msg, ok := d.last()
		if !ok {
			t.Fatal("answer batch not delivered")
		}
		idx := strings.Index(msg, `{"answers"`)
		if idx < 0 {
			t.Fatalf("delivered batch missing JSON payload:\n%s", msg)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(msg[idx:]), &payload); err != nil {
			t.Fatalf("unmarshal payload: %v\n%s", err, msg)
		}
		return msg, payload
	}

	msg, payload := delivered(t, "qa:1")
	if !strings.Contains(msg, "• Geo → Oslo ✅") {
		t.Errorf("readable line missing correct verdict:\n%s", msg)
	}
	if answers, _ := payload["answers"].(map[string]any); len(answers) != 3 || answers["Capital of <Norway>?"] != "Oslo" {
		t.Errorf("answers payload changed shape: %+v", payload["answers"])
	}
	if byID, _ := payload["answers_by_id"].(map[string]any); len(byID) != 2 || byID["cap"] != "Oslo" {
		t.Errorf("answers_by_id payload changed shape: %+v", payload["answers_by_id"])
	}
	results, ok := payload["quiz_results"].(map[string]any)
	if !ok || len(results) != 1 || results["Capital of <Norway>?"] != true {
		t.Errorf("quiz_results = %+v, want {Capital of <Norway>?: true} only (free recall and plain get none)", payload["quiz_results"])
	}
	if byID, ok := payload["quiz_results_by_id"].(map[string]any); !ok || len(byID) != 1 || byID["cap"] != true {
		t.Errorf("quiz_results_by_id = %+v, want {cap: true}", payload["quiz_results_by_id"])
	}

	msg, payload = delivered(t, "qa:0")
	if !strings.Contains(msg, "• Geo → Bergen ❌") {
		t.Errorf("readable line missing incorrect verdict:\n%s", msg)
	}
	if results, _ := payload["quiz_results"].(map[string]any); len(results) != 1 || results["Capital of <Norway>?"] != false {
		t.Errorf("quiz_results = %+v, want {Capital of <Norway>?: false}", payload["quiz_results"])
	}

	// A non-quiz ask: no quiz keys at all — requirement 1/4's "unchanged".
	tool, _, p, d := newAskFixture()
	execAsk(t, tool, `{"questions":[{"question":"Plain?","options":[{"label":"Yes"}]}]}`)
	p.answer("qa:0")
	msg, _ = d.last()
	if strings.Contains(msg, "quiz_results") {
		t.Errorf("non-quiz batch must not carry quiz_results:\n%s", msg)
	}
}

// TestAsk_QuizRevealPostedOnceSequential pins requirement 3 on the sequential
// path: exactly ONE reveal per ANSWERED quiz question (never before answering,
// never for a non-quiz question), with the exact FormatReveal content, ordered
// AFTER the answered question closes and BEFORE the next one is presented, so
// the user reads answer → reveal → next question. On the pre-change tree the
// reveal hook does not exist, so the reveal assertions fail.
func TestAsk_QuizRevealPostedOnceSequential(t *testing.T) {
	t.Parallel()

	// One shared event log so present/close/reveal/deliver ordering is exact.
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	present := func(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(string)) string {
		record("present")
		return ""
	}
	closeMsg := func(msgID, finalText string) { record("close") }
	rec := &quizRevealRecorder{}

	tool, router := NewAskTool(present, nil, func(sk, req, msg string) { record("deliver") }, closeMsg, nil, "test",
		WithReveal(func(sk, text string) { rec.reveal(sk, text); record("reveal") }))

	execAsk(t, tool, `{"questions":[
		{"question":"Capital of <Norway>?","options":[{"label":"Bergen"},{"label":"Oslo"}],"quiz":{"correct":[1],"explanation":"Oslo since 1299."}},
		{"question":"Plain?","options":[{"label":"Yes"}]}]}`)

	mu.Lock()
	presented := len(events)
	mu.Unlock()
	if presented != 1 || rec.count() != 0 {
		t.Fatalf("after asking: %d presents, %d reveals; want 1 present, 0 reveals", presented, rec.count())
	}

	// Answer Q0 (quiz) by typing via the router; Q1 (non-quiz) by button.
	reqID := router.PendingForSession(askSession)
	router.HandleResponse(reqID, "Oslo")
	if rec.count() != 1 {
		t.Fatalf("quiz answer must post exactly one reveal, got %d", rec.count())
	}
	if got := rec.session[0]; got != askSession {
		t.Errorf("reveal session = %q, want %q", got, askSession)
	}
	if want := strings.Join([]string{
		"Capital of <Norway>?",
		"Answer: 2. Oslo",
		"✅ Correct.",
		"Explanation: Oslo since 1299.",
	}, "\n"); rec.last() != want {
		t.Errorf("reveal text:\ngot:  %q\nwant: %q", rec.last(), want)
	}

	router.HandleResponse(reqID, "qa:0") // Q1: non-quiz → no second reveal
	if rec.count() != 1 {
		t.Errorf("non-quiz answer must not reveal (count=%d)", rec.count())
	}

	// Exact order: present, close, reveal, present, close, deliver — the
	// reveal sits between the answered question closing and the next appearing.
	mu.Lock()
	defer mu.Unlock()
	wantSeq := []string{"present", "close", "reveal", "present", "close", "deliver"}
	if len(events) != len(wantSeq) {
		t.Fatalf("event sequence = %v, want %v", events, wantSeq)
	}
	for i, want := range wantSeq {
		if events[i] != want {
			t.Errorf("event[%d] = %q, want %q (full: %v)", i, events[i], want, events)
		}
	}
}

// TestAsk_QuizRevealNotOnCancelExpiryOrBatch pins the negative reveal cases:
// cancel and expiry post NO reveal (nothing was answered), and the BATCHED
// (app) path posts none either — the app's form carries the quiz data and
// reveals locally (#1316 ruling). The batched path also resolves a typed
// number through the same rule (review note: handleBatchResponse goes through
// ResolveAnswer).
func TestAsk_QuizRevealNotOnCancelExpiryOrBatch(t *testing.T) {
	t.Parallel()

	// Cancel.
	rec := &quizRevealRecorder{}
	tool, _, p, _ := newAskFixtureWithReveal(rec)
	execAsk(t, tool, `{"questions":[{"question":"Q?","options":[{"label":"A"}],"quiz":{"correct":[0]}}]}`)
	p.answer(question.CancelData)
	if rec.count() != 0 {
		t.Errorf("cancel must not reveal, got %d", rec.count())
	}

	// Expiry.
	tool, _, p, _ = newAskFixtureWithReveal(rec)
	execAsk(t, tool, `{"questions":[{"question":"Q?","options":[{"label":"A"}],"quiz":{"correct":[0]}}]}`)
	p.answer(question.ExpiredData)
	if rec.count() != 0 {
		t.Errorf("expiry must not reveal, got %d", rec.count())
	}

	// Batched (app) path: answered quiz question, still no server reveal.
	rec = &quizRevealRecorder{}
	tool, _, seq, bp, d := newAskBatchFixtureWithReveal(rec)
	execAsk(t, tool, `{"questions":[{"question":"Q?","options":[{"label":"A"},{"label":"B"}],"quiz":{"correct":[1],"explanation":"`+quizSecretExpl+`"}}]}`)
	if seq.presents != 0 {
		t.Fatal("batch-capable client must not take the sequential path")
	}
	bp.answer([]string{"2"})
	if rec.count() != 0 {
		t.Errorf("batched path must not reveal server-side, got %d", rec.count())
	}
	msg, ok := d.last()
	if !ok {
		t.Fatal("batched answers not delivered")
	}
	if !strings.Contains(msg, `"Q?":"B"`) {
		t.Errorf("typed number 2 in a batch must resolve to option B:\n%s", msg)
	}
	if !strings.Contains(msg, `"quiz_results":{"Q?":true}`) {
		t.Errorf("batched quiz verdict missing:\n%s", msg)
	}
}

// TestAsk_QuizTypedNumberAnswer pins requirement 5 end-to-end through the
// router: on a quiz question a typed reply that is just a number selects that
// numbered option (and earns the matching verdict). On the pre-change tree the
// answer is recorded verbatim ("2"), so both assertions fail. The JSON payload
// is parsed because json.Marshal HTML-escapes the question's angle brackets.
func TestAsk_QuizTypedNumberAnswer(t *testing.T) {
	t.Parallel()
	tool, router, _, d := newAskFixture()
	execAsk(t, tool, `{"questions":[{"question":"Capital of <Norway>?","options":[{"label":"Bergen"},{"label":"Oslo"}],"quiz":{"correct":[1]}}]}`)

	reqID := router.PendingForSession(askSession)
	router.HandleResponse(reqID, "2") // the displayed number of Oslo

	msg, ok := d.last()
	if !ok {
		t.Fatal("typed number should complete and deliver the batch")
	}
	if !strings.Contains(msg, "→ Oslo") {
		t.Errorf("typed \"2\" must select option 2 (Oslo):\n%s", msg)
	}
	idx := strings.Index(msg, `{"answers"`)
	if idx < 0 {
		t.Fatalf("delivered batch missing JSON payload:\n%s", msg)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(msg[idx:]), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if answers, _ := payload["answers"].(map[string]any); answers["Capital of <Norway>?"] != "Oslo" {
		t.Errorf("answers[question] = %+v, want Oslo (the resolved option)", payload["answers"])
	}
	if results, _ := payload["quiz_results"].(map[string]any); results["Capital of <Norway>?"] != true {
		t.Errorf("quiz_results = %+v, want true for the selected-by-number option", payload["quiz_results"])
	}
}

// TestAsk_QuizPromptNoLeak is the tool-level no-leak guard (review note): the
// text, summary and choices handed to the presenter — the single source every
// sequential prompt and the plain-text fallback render from — carry none of the
// quiz secrets, and WHICH option is correct does not change them.
func TestAsk_QuizPromptNoLeak(t *testing.T) {
	t.Parallel()
	present := func(correct int) (string, string, []question.Choice) {
		pr := &fakePresenter{}
		tool, _ := NewAskTool(pr.present, nil, func(sk, req, msg string) {}, nil, nil, "test")
		execAsk(t, tool, fmt.Sprintf(`{"questions":[{"question":"Q?","header":"Geo","options":[{"label":"Bergen"},{"label":"Oslo"}],"quiz":{"correct":[%d],"explanation":"%s"}}]}`,
			correct, quizSecretExpl))
		return pr.lastText, "Geo", pr.lastChoices
	}

	text, summary, choices := present(1)
	for _, secret := range []string{quizSecretExpl, quizSecretAnswer, `"correct"`, "quiz"} {
		if strings.Contains(text, secret) {
			t.Errorf("prompt text leaks %q:\n%s", secret, text)
		}
		if strings.Contains(summary, secret) {
			t.Errorf("prompt summary leaks %q: %q", secret, summary)
		}
		for _, c := range choices {
			if strings.Contains(c.Label+c.Data, secret) {
				t.Errorf("choice %q leaks %q", c.Label, secret)
			}
		}
	}

	// The same prompt no matter which option is marked correct.
	t1, _, c1 := present(0)
	if t1 != text {
		t.Errorf("prompt text differs by correct index — it reveals the answer:\n%s\n%s", text, t1)
	}
	if len(c1) != len(choices) {
		t.Fatalf("choices count differs by correct index: %d vs %d", len(c1), len(choices))
	}
	for i := range c1 {
		if c1[i] != choices[i] {
			t.Errorf("choice %d differs by correct index: %+v vs %+v", i, c1[i], choices[i])
		}
	}
}

// TestAsk_DescriptionDocumentsQuizOutput pins that the tool's self-description
// (what agents actually see) documents the quiz input and the quiz_results
// output, including the 0-based correct indexing (review note: the footgun
// must be stated in the schema itself).
func TestAsk_DescriptionDocumentsQuizOutput(t *testing.T) {
	t.Parallel()
	tool, _, _, _ := newAskFixture()
	schema := tool.Description + " " + string(tool.Parameters)
	for _, want := range []string{"quiz", "quiz_results", "0-BASED", "reveal", "quiz_results_by_id", "free-recall"} {
		if !strings.Contains(schema, want) {
			t.Errorf("tool schema missing %q", want)
		}
	}
}

// --- fixtures -------------------------------------------------------------

// newAskFixtureWithReveal wires a quizRevealRecorder as the reveal hook.
func newAskFixtureWithReveal(rec *quizRevealRecorder) (*Tool, *AskRouter, *fakePresenter, *fakeDeliver) {
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, nil, "test", WithReveal(rec.reveal))
	return tool, router, p, d
}

// newAskBatchFixtureWithReveal is newAskBatchFixture plus a reveal recorder.
func newAskBatchFixtureWithReveal(rec *quizRevealRecorder) (*Tool, *AskRouter, *fakePresenter, *fakeBatchPresenter, *fakeDeliver) {
	seq := &fakePresenter{}
	bp := &fakeBatchPresenter{batched: true}
	d := &fakeDeliver{}
	tool, router := NewAskTool(seq.present, nil, d.deliver, nil, nil, "test",
		WithBatchPresent(bp.present), WithReveal(rec.reveal))
	return tool, router, seq, bp, d
}
