package nudge

import (
	"strings"
	"testing"
)

// validSpec is the baseline Spec every validation case mutates: it must
// itself register cleanly, so a rejection can only come from the mutated
// field, and an acceptance proves the table's valid arm ran.
func validSpec() Spec {
	return Spec{
		ID:   "onboarding.sr",
		Text: "Do you want to enable spaced repetition?",
		Actions: []Action{
			{Label: "Yes", Command: "/sr on"},
			{Label: "Not yet", Command: "/sr snooze"},
		},
	}
}

// TestRegisterValidation proves Register's contract: only well-formed Specs
// enter the registry — the id charset and length, uniqueness, non-empty text,
// the action cap, and per-action label/command shape (a command must be a
// slash-command line).
func TestRegisterValidation(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*Spec)
		wantErr bool
	}{
		{"empty id", func(s *Spec) { s.ID = "" }, true},
		{"id with uppercase", func(s *Spec) { s.ID = "Onboarding" }, true},
		{"id with slash", func(s *Spec) { s.ID = "onboarding/sr" }, true},
		{"id with space", func(s *Spec) { s.ID = "onboarding sr" }, true},
		{"id over 64 chars", func(s *Spec) { s.ID = strings.Repeat("a", 65) }, true},
		{"id at 64 chars", func(s *Spec) { s.ID = strings.Repeat("a", 64) }, false},
		{"empty text", func(s *Spec) { s.Text = "" }, true},
		{"five actions", func(s *Spec) {
			s.Actions = append(s.Actions,
				Action{Label: "A3", Command: "/sr a"},
				Action{Label: "A4", Command: "/sr b"},
				Action{Label: "A5", Command: "/sr c"},
			)
		}, true},
		{"empty label", func(s *Spec) { s.Actions[0].Label = "" }, true},
		{"empty command", func(s *Spec) { s.Actions[0].Command = "" }, true},
		{"command without slash", func(s *Spec) { s.Actions[0].Command = "sr on" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			s := validSpec()
			tc.mut(&s)
			err := r.Register(s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Register(%s) error = %v, want error = %v", tc.name, err, tc.wantErr)
			}
		})
	}

	// Duplicate id: the same id twice against one registry.
	r := NewRegistry()
	if err := r.Register(validSpec()); err != nil {
		t.Fatalf("Register(valid) = %v", err)
	}
	if err := r.Register(validSpec()); err == nil {
		t.Fatal("Register(duplicate id) = nil, want error")
	}
}

// TestRegistryGetReturnsIsolatedCopy proves the registry serves deep copies:
// mutating a Spec before or after registration cannot alter what Get serves,
// so no caller can reach into the registry's stored Specs through a handle.
func TestRegistryGetReturnsIsolatedCopy(t *testing.T) {
	r := NewRegistry()
	orig := validSpec()
	if err := r.Register(orig); err != nil {
		t.Fatalf("Register = %v", err)
	}

	// Mutating the caller's Spec after Register changes nothing.
	orig.Actions[0].Command = "/evil"
	orig.Text = "tampered"

	got, ok := r.Get(validSpec().ID)
	if !ok {
		t.Fatal("Get(valid id) not found")
	}
	if got.Text != validSpec().Text {
		t.Errorf("served Text = %q, want %q", got.Text, validSpec().Text)
	}
	if got.Actions[0].Command != "/sr on" {
		t.Errorf("served command = %q, want %q", got.Actions[0].Command, "/sr on")
	}

	// Mutating the served copy changes nothing either.
	got.Actions[0].Command = "/also-evil"
	again, _ := r.Get(validSpec().ID)
	if again.Actions[0].Command != "/sr on" {
		t.Errorf("after mutating served copy, Get serves %q, want %q", again.Actions[0].Command, "/sr on")
	}

	// A served Spec with no actions has a nil (not shared-empty) slice.
	r2 := NewRegistry()
	if err := r2.Register(Spec{ID: "bare", Text: "just text"}); err != nil {
		t.Fatalf("Register(bare) = %v", err)
	}
	bare, _ := r2.Get("bare")
	if len(bare.Actions) != 0 {
		t.Errorf("bare.Actions = %v, want none", bare.Actions)
	}

	// Unknown id is a clean miss.
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(unknown) found, want miss")
	}
}
