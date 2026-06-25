package main

import (
	"testing"

	"foci/internal/question"
)

// TestBatchQuestionsFor verifies the batched-app payload mapping: structured
// fields (raw text, header, per-option label+description) and NO appended Cancel
// button (the app's full-screen form supplies its own; only the chat/sequential
// path via question.Choices carries a per-question Cancel).
func TestBatchQuestionsFor(t *testing.T) {
	qs := []question.Question{
		{
			Question: "Pick a colour",
			Header:   "Colour",
			Options: []question.Option{
				{Label: "Red", Description: "warm"},
				{Label: "Blue", Description: "cool"},
			},
		},
		{Question: "Anything else?"}, // option-less ⇒ typed-answer-only
	}

	got := batchQuestionsFor(qs)
	if len(got) != 2 {
		t.Fatalf("got %d questions, want 2", len(got))
	}

	// Q0: raw text + header preserved; option-only choices with descriptions; no Cancel.
	q0 := got[0]
	if q0.Text != "Pick a colour" {
		t.Errorf("q0.Text = %q, want raw question (no markdown)", q0.Text)
	}
	if q0.Header != "Colour" {
		t.Errorf("q0.Header = %q, want %q", q0.Header, "Colour")
	}
	if len(q0.Choices) != 2 {
		t.Fatalf("q0 has %d choices, want 2 (no Cancel appended)", len(q0.Choices))
	}
	for _, c := range q0.Choices {
		if c.Data == question.CancelData {
			t.Errorf("q0 must not carry a Cancel choice; found %q", c.Data)
		}
	}
	if q0.Choices[0].Label != "Red" || q0.Choices[0].Data != "qa:0" || q0.Choices[0].Description != "warm" {
		t.Errorf("q0.Choices[0] = %+v, want {Red qa:0 warm}", q0.Choices[0])
	}
	if q0.Choices[1].Data != "qa:1" || q0.Choices[1].Description != "cool" {
		t.Errorf("q0.Choices[1] = %+v, want data qa:1 desc cool", q0.Choices[1])
	}

	// Q1: option-less ⇒ no choices at all (typed-answer-only), raw text.
	q1 := got[1]
	if q1.Text != "Anything else?" {
		t.Errorf("q1.Text = %q, want raw question", q1.Text)
	}
	if len(q1.Choices) != 0 {
		t.Errorf("q1 has %d choices, want 0 (typed-only, no Cancel)", len(q1.Choices))
	}
}
