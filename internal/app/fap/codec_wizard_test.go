package fap

import (
	"encoding/json"
	"testing"
)

func TestEncode_WizardStepShape(t *testing.T) {
	frame := WizardStep{
		ConversationID: "c1",
		WizardID:       "w1",
		StepID:         "s1",
		Title:          "/agents new",
		Step: Question{
			Text:   "Execution mode — how should this agent run?",
			Header: "Backend",
			Choices: []Choice{
				{Label: "claude-code", Data: "qa:0", Description: "Delegated (recommended)"},
				{Label: "api", Data: "qa:1"},
			},
		},
		ExpiresAt: "2026-01-01T00:00:00Z",
	}
	wire, err := Encode(frame, 3, 0, "id1", "ts1")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		T string          `json:"t"`
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	if env.T != "wizard.step" {
		t.Errorf("t = %q, want wizard.step", env.T)
	}
	var d map[string]any
	if err := json.Unmarshal(env.D, &d); err != nil {
		t.Fatal(err)
	}
	if d["wizardId"] != "w1" || d["stepId"] != "s1" || d["title"] != "/agents new" {
		t.Errorf("payload ids/title wrong: %v", d)
	}
	step, ok := d["step"].(map[string]any)
	if !ok {
		t.Fatalf("step missing or not an object: %v", d["step"])
	}
	if step["header"] != "Backend" {
		t.Errorf("step.header = %v, want Backend", step["header"])
	}
	choices, _ := step["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("step.choices len = %d, want 2", len(choices))
	}
	first, _ := choices[0].(map[string]any)
	if first["data"] != "qa:0" || first["description"] != "Delegated (recommended)" {
		t.Errorf("first choice wrong: %v", first)
	}
}

func TestEncode_WizardStepFreeTextOmitsChoices(t *testing.T) {
	wire, err := Encode(WizardStep{ConversationID: "c1", WizardID: "w1", StepID: "s1", Step: Question{Text: "Name?"}}, 0, 0, "id", "ts")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		D map[string]json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	var step map[string]any
	if err := json.Unmarshal(env.D["step"], &step); err != nil {
		t.Fatal(err)
	}
	if _, present := step["choices"]; present {
		t.Errorf("free-text step should omit choices, got %v", step["choices"])
	}
	if _, present := env.D["title"]; present {
		t.Errorf("empty title should be omitted, got %s", env.D["title"])
	}
}

func TestEncode_WizardStepMedia(t *testing.T) {
	withMedia := WizardStep{
		ConversationID: "c1", WizardID: "w1", StepID: "s1",
		Step:  Question{Text: "Scan this QR"},
		Media: &WizardStepMedia{BlobID: "01BLOB", MIME: "image/png", Name: "qr.png"},
	}
	wire, err := Encode(withMedia, 0, 0, "id", "ts")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		D map[string]json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	var media map[string]any
	if err := json.Unmarshal(env.D["media"], &media); err != nil {
		t.Fatalf("media missing: %v", err)
	}
	if media["blobId"] != "01BLOB" || media["mime"] != "image/png" || media["name"] != "qr.png" {
		t.Errorf("media wrong: %v", media)
	}

	// Media-less steps omit the field entirely. (Fresh envelope var: Unmarshal
	// into a reused non-nil map MERGES keys, which would leak the first frame's
	// media into this check.)
	wire, err = Encode(WizardStep{ConversationID: "c1", WizardID: "w1", StepID: "s2", Step: Question{Text: "Next"}}, 0, 0, "id", "ts")
	if err != nil {
		t.Fatal(err)
	}
	var env2 struct {
		D map[string]json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env2); err != nil {
		t.Fatal(err)
	}
	if _, present := env2.D["media"]; present {
		t.Error("nil media must be omitted from the payload")
	}
}

func TestEncode_WizardEndShape(t *testing.T) {
	wire, err := Encode(WizardEnd{ConversationID: "c1", WizardID: "w1", Status: WizardCancelled, Text: "Wizard cancelled."}, 0, 0, "id", "ts")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		T string         `json:"t"`
		D map[string]any `json:"d"`
	}
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	if env.T != "wizard.end" {
		t.Errorf("t = %q, want wizard.end", env.T)
	}
	if env.D["status"] != "cancelled" || env.D["text"] != "Wizard cancelled." {
		t.Errorf("payload wrong: %v", env.D)
	}
}

func TestDecode_WizardResponse(t *testing.T) {
	wire := `{"t":"wizard.response","id":"r1","d":{"conversationId":"c1","wizardId":"w1","stepId":"s1","data":"qa:0"}}`
	in, err := Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := in.Frame.(WizardResponse)
	if !ok {
		t.Fatalf("frame type = %T, want WizardResponse", in.Frame)
	}
	if resp.ConversationID != "c1" || resp.WizardID != "w1" || resp.StepID != "s1" || resp.Data != "qa:0" {
		t.Errorf("decoded response wrong: %+v", resp)
	}
}
