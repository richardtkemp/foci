package voice

import (
	"context"
	"testing"
)

// TestApplyReplacements verifies that word replacement is case-insensitive,
// whole-word only, and preserves the case pattern of the original text.
func TestApplyReplacements(t *testing.T) {
	repls := map[string]string{
		"foci": "foki",
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"lowercase", "hello foci", "hello foki"},
		{"uppercase", "FOCI IS GREAT", "FOKI IS GREAT"},
		{"title case", "Foci works", "Foki works"},
		{"mixed sentence", "the foci project, FOCI, and Foci", "the foki project, FOKI, and Foki"},
		{"no match", "focus on this", "focus on this"},
		{"empty", "", ""},
		{"word boundary punctuation", "foci. foci, foci!", "foki. foki, foki!"},
		{"embedded no match", "refocied", "refocied"},
		{"adjacent to digits", "foci123 test", "foki123 test"},
		{"only word", "foci", "foki"},
		{"with newlines", "line1 foci\nline2 FOCI", "line1 foki\nline2 FOKI"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyReplacements(tt.input, repls)
			if got != tt.want {
				t.Errorf("ApplyReplacements(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestApplyReplacementsMultipleWords verifies that multiple replacement
// pairs work simultaneously and later entries in the map don't interfere.
func TestApplyReplacementsMultipleWords(t *testing.T) {
	repls := map[string]string{
		"foci":  "foki",
		"llm":   "L L M",
		"api":   "A P I",
	}

	input := "foci uses an LLM via the API"
	want := "foki uses an L L M via the A P I"
	got := ApplyReplacements(input, repls)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestApplyReplacementsNilMap verifies that nil/empty maps are no-ops.
func TestApplyReplacementsNilMap(t *testing.T) {
	input := "foci test"
	if got := ApplyReplacements(input, nil); got != input {
		t.Errorf("nil map: got %q, want %q", got, input)
	}
	if got := ApplyReplacements(input, map[string]string{}); got != input {
		t.Errorf("empty map: got %q, want %q", got, input)
	}
}

// TestMatchCase verifies the case-matching logic used for replacements.
func TestMatchCase(t *testing.T) {
	tests := []struct {
		original, replacement, want string
	}{
		{"foci", "foki", "foki"},
		{"FOCI", "foki", "FOKI"},
		{"Foci", "foki", "Foki"},
		{"FOci", "foki", "Foki"}, // mixed — first letter upper → title case
		{"", "foki", "foki"},
		{"foci", "", ""},
	}
	for _, tt := range tests {
		got := matchCase(tt.original, tt.replacement)
		if got != tt.want {
			t.Errorf("matchCase(%q, %q) = %q, want %q", tt.original, tt.replacement, got, tt.want)
		}
	}
}

// TestReplacingTTSWrapper verifies that ReplacingTTS applies word replacements
// to the text before passing it to the inner TTS provider.
func TestReplacingTTSWrapper(t *testing.T) {
	inner := &fakeTTS{}
	wrapped := &ReplacingTTS{
		Inner:        inner,
		Replacements: map[string]string{"foci": "foki"},
	}

	_, err := wrapped.Synthesize(context.Background(), "hello foci")
	if err != nil {
		t.Fatal(err)
	}
	if inner.lastText != "hello foki" {
		t.Errorf("inner TTS got %q, want %q", inner.lastText, "hello foki")
	}
}

// TestReplacingSTTWrapper verifies that ReplacingSTT applies word replacements
// to the transcribed text returned by the inner STT provider.
func TestReplacingSTTWrapper(t *testing.T) {
	inner := &fakeSTT{result: "hello foki"}
	wrapped := &ReplacingSTT{
		Inner:        inner,
		Replacements: map[string]string{"foki": "foci"},
	}

	text, err := wrapped.Transcribe(context.Background(), nil, "test.wav")
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello foci" {
		t.Errorf("got %q, want %q", text, "hello foci")
	}
}

// TestMergeReplacements verifies that later maps override earlier ones and
// nil/empty inputs produce nil output.
func TestMergeReplacements(t *testing.T) {
	a := map[string]string{"foci": "foki", "api": "A P I"}
	b := map[string]string{"foci": "fokee"} // override

	merged := MergeReplacements(a, b)
	if merged["foci"] != "fokee" {
		t.Errorf("foci: got %q, want %q", merged["foci"], "fokee")
	}
	if merged["api"] != "A P I" {
		t.Errorf("api: got %q, want %q", merged["api"], "A P I")
	}

	if MergeReplacements(nil, nil) != nil {
		t.Error("expected nil for all-nil inputs")
	}
	if MergeReplacements(map[string]string{}) != nil {
		t.Error("expected nil for empty inputs")
	}
}

// TestWrapTTSNoop verifies WrapTTS returns the original when no replacements.
func TestWrapTTSNoop(t *testing.T) {
	inner := &fakeTTS{}
	if WrapTTS(inner, nil) != inner {
		t.Error("expected same provider for nil replacements")
	}
	if WrapTTS(inner, map[string]string{}) != inner {
		t.Error("expected same provider for empty replacements")
	}
	if WrapTTS(nil, map[string]string{"a": "b"}) != nil {
		t.Error("expected nil for nil provider")
	}
}

// TestWrapSTTNoop verifies WrapSTT returns the original when no replacements.
func TestWrapSTTNoop(t *testing.T) {
	inner := &fakeSTT{}
	if WrapSTT(inner, nil) != inner {
		t.Error("expected same provider for nil replacements")
	}
	if WrapSTT(nil, map[string]string{"a": "b"}) != nil {
		t.Error("expected nil for nil provider")
	}
}

// --- fakes ---

type fakeTTS struct {
	lastText string
}

func (f *fakeTTS) Synthesize(_ context.Context, text string) ([]byte, error) {
	f.lastText = text
	return []byte("audio"), nil
}

type fakeSTT struct {
	result string
}

func (f *fakeSTT) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return f.result, nil
}
