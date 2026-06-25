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
