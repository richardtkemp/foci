package fap

import (
	"encoding/json"
	"strings"
	"testing"
)

// A server→app Interactive frame carries its batched Questions (each with its own
// choices) intact through Encode and back.
func TestEncode_InteractiveBatchQuestionsRoundTrip(t *testing.T) {
	orig := Interactive{
		ConversationID: "c1",
		PromptID:       "p1",
		Questions: []Question{
			{Text: "Color?", Choices: []Choice{{Label: "Red", Data: "qa:0"}, {Label: "Cancel", Data: "qa:cancel"}}},
			{Text: "Size?"}, // typed-answer-only: no choices
		},
	}
	wire, err := Encode(orig, 0, 0, "X", "ts")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		D Interactive `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	got := env.D
	if len(got.Questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(got.Questions))
	}
	if got.Questions[0].Text != "Color?" || len(got.Questions[0].Choices) != 2 || got.Questions[0].Choices[0].Data != "qa:0" {
		t.Errorf("q0 round-trip wrong: %+v", got.Questions[0])
	}
	if got.Questions[1].Text != "Size?" || len(got.Questions[1].Choices) != 0 {
		t.Errorf("q1 round-trip wrong: %+v", got.Questions[1])
	}
}

// A legacy single-question Interactive omits the `questions` field entirely, so
// uncapable clients never see it.
func TestEncode_InteractiveOmitsQuestionsWhenSingle(t *testing.T) {
	wire, err := Encode(Interactive{ConversationID: "c1", PromptID: "p1", Text: "Run bash?", Choices: []Choice{{Label: "Allow", Data: "allow"}}}, 0, 0, "X", "ts")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire, "questions") {
		t.Errorf("single-question Interactive should omit `questions`: %s", wire)
	}
}

// A batched question's quiz data (#1316) rides the Interactive frame intact —
// correct (0-based), model answer and explanation — while a question WITHOUT
// quiz data omits the `quiz` key entirely, so its frame is byte-compatible
// with pre-#1316 clients. Guard test (passes as soon as it compiles); mutation
// arm: rename a Quiz JSON tag (e.g. "answer" → "model") or drop the omitempty
// and the exact-wire-key assertions go red.
func TestEncode_InteractiveQuizRoundTrip(t *testing.T) {
	orig := Interactive{
		ConversationID: "c1",
		PromptID:       "p1",
		Questions: []Question{
			{
				Text:    "Capital of <Norway>?",
				Choices: []Choice{{Label: "Bergen", Data: "qa:0"}, {Label: "Oslo", Data: "qa:1"}},
				Quiz:    &Quiz{Correct: []int{1}, Explanation: "Oslo since 1299."},
			},
			{Text: "Translate dog.", Quiz: &Quiz{ModelAnswer: "σκύλος"}}, // free recall
			{Text: "Plain?"}, // non-quiz
		},
	}
	wire, err := Encode(orig, 0, 0, "X", "ts")
	if err != nil {
		t.Fatal(err)
	}

	// Exact wire shape: decode the raw payload as generic JSON so the KEY
	// names themselves are asserted, not just Go's view of them.
	var env struct {
		D struct {
			Questions []map[string]any `json:"questions"`
		} `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.D.Questions) != 3 {
		t.Fatalf("questions = %d, want 3", len(env.D.Questions))
	}
	q0 := env.D.Questions[0]["quiz"].(map[string]any)
	if _, has := q0["answer"]; has {
		t.Errorf("option question's quiz must not carry an answer key: %v", q0)
	}
	// json numbers decode as float64; 1 (the 0-based index of Oslo).
	if correct, _ := q0["correct"].([]any); len(correct) != 1 || correct[0] != float64(1) {
		t.Errorf("quiz.correct = %v, want [1] (0-based index into choices)", q0["correct"])
	}
	if q0["explanation"] != "Oslo since 1299." {
		t.Errorf("quiz.explanation = %v", q0["explanation"])
	}
	q1 := env.D.Questions[1]["quiz"].(map[string]any)
	if q1["answer"] != "σκύλος" {
		t.Errorf("quiz.answer = %v", q1["answer"])
	}
	if _, has := q1["correct"]; has {
		t.Errorf("free-recall quiz must not carry a correct key: %v", q1)
	}
	if _, has := env.D.Questions[2]["quiz"]; has {
		t.Errorf("non-quiz question must omit the quiz key entirely: %s", wire)
	}

	// Typed round-trip through the codec.
	var round struct {
		D Interactive `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &round); err != nil {
		t.Fatal(err)
	}
	got := round.D.Questions[0].Quiz
	if got == nil || len(got.Correct) != 1 || got.Correct[0] != 1 || got.Explanation != "Oslo since 1299." {
		t.Errorf("q0 quiz round-trip wrong: %+v", got)
	}
	if got := round.D.Questions[1].Quiz; got == nil || got.ModelAnswer != "σκύλος" {
		t.Errorf("q1 quiz round-trip wrong: %+v", got)
	}
	if got := round.D.Questions[2].Quiz; got != nil {
		t.Errorf("q2 (non-quiz) round-trip wrong: %+v", got)
	}
}

// A pre-#1316 client's Question decoder has no `quiz` field; a frame that
// carries one must still decode for it — the additive-omitempty versioning
// rule (unknown keys are ignored, the standing guarantee pinned for ClientHello
// in TestDecode_ClientHelloIgnoresLegacyResumeOpen). Mutation arm: set
// DisallowUnknownFields in Decode and this goes red.
func TestDecode_InteractiveQuizLegacyClient(t *testing.T) {
	var legacy struct {
		D struct {
			Questions []struct {
				Text    string   `json:"text"`
				Choices []Choice `json:"choices"`
			} `json:"questions"`
		} `json:"d"`
	}
	wire := `{"t":"interactive","id":"i1","d":{"conversationId":"c1","promptId":"p1","questions":[` +
		`{"text":"Q?","choices":[{"label":"A","data":"qa:0"}],"quiz":{"correct":[0],"explanation":"x"}}]}}`
	if err := json.Unmarshal([]byte(wire), &legacy); err != nil {
		t.Fatalf("legacy decoder must ignore the quiz key: %v", err)
	}
	if len(legacy.D.Questions) != 1 || legacy.D.Questions[0].Text != "Q?" || len(legacy.D.Questions[0].Choices) != 1 {
		t.Errorf("legacy decode wrong: %+v", legacy.D.Questions)
	}
}

// A batched InteractiveResponse decodes its Answers list; Data stays empty.
func TestDecode_InteractiveResponseBatchAnswers(t *testing.T) {
	wire := `{"t":"interactive.response","id":"r1","d":{"conversationId":"c1","promptId":"p1","answers":["qa:0","qa:cancel","typed text"]}}`
	in, err := Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := in.Frame.(InteractiveResponse)
	if !ok {
		t.Fatalf("frame type = %T, want InteractiveResponse", in.Frame)
	}
	if resp.Data != "" {
		t.Errorf("Data should be empty for a batched reply, got %q", resp.Data)
	}
	if len(resp.Answers) != 3 || resp.Answers[0] != "qa:0" || resp.Answers[2] != "typed text" {
		t.Errorf("answers wrong: %+v", resp.Answers)
	}
}

// ClientHello carries the advertised feature list (back-compat: absent ⇒ empty).
func TestDecode_ClientHelloFeatures(t *testing.T) {
	wire := `{"t":"hello","id":"h1","d":{"client":{"app":"foci","os":"android","version":"1","deviceId":"d"},"features":["interactiveBatch","voice"]}}`
	in, err := Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	hello, ok := in.Frame.(ClientHello)
	if !ok {
		t.Fatalf("frame type = %T, want ClientHello", in.Frame)
	}
	if len(hello.Features) != 2 || hello.Features[0] != "interactiveBatch" {
		t.Errorf("features wrong: %+v", hello.Features)
	}

	bare := `{"t":"hello","id":"h2","d":{"client":{"deviceId":"d2"}}}`
	in2, err := Decode(bare)
	if err != nil {
		t.Fatal(err)
	}
	if h := in2.Frame.(ClientHello); len(h.Features) != 0 {
		t.Errorf("absent features should decode empty, got %+v", h.Features)
	}
}
