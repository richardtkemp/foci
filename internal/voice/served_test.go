package voice

import (
	"errors"
	"testing"
)

// TestFallbackTTS_ReportsServedVoice (#2992): the chain names the voice that
// produced the audio — the fallback when the first failed — and names none
// when every voice failed.
func TestFallbackTTS_ReportsServedVoice(t *testing.T) {
	var served []string
	ctx := WithServedRecorder(t.Context(), func(id string) { served = append(served, id) })

	if _, err := chain(t, &countingTTS{err: errors.New("down")}, &countingTTS{out: "x"}).Synthesize(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 || served[0] != "b" {
		t.Fatalf("served = %v, want [b]: the second voice spoke", served)
	}

	served = nil
	if _, err := chain(t, &countingTTS{err: errors.New("down")}).Synthesize(ctx, "hi"); err == nil {
		t.Fatal("want an error from a chain whose only voice failed")
	}
	if len(served) != 0 {
		t.Errorf("served = %v after a failure, want none", served)
	}

	// No recorder on the context: nothing to call, nothing breaks.
	if _, err := chain(t, &countingTTS{out: "x"}).Synthesize(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}
}

// TestNamed_ReportsServedVoice (#2992): a chain of one names its voice too,
// and its failure passes through unchanged.
func TestNamed_ReportsServedVoice(t *testing.T) {
	var served []string
	ctx := WithServedRecorder(t.Context(), func(id string) { served = append(served, id) })
	if _, err := (&Named{ID: "groq", TTS: &countingTTS{out: "x"}}).Synthesize(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 || served[0] != "groq" {
		t.Fatalf("served = %v, want [groq]", served)
	}
	down := errors.New("down")
	if _, err := (&Named{ID: "groq", TTS: &countingTTS{err: down}}).Synthesize(ctx, "hi"); err != down {
		t.Errorf("err = %v, want the voice's own error unchanged", err)
	}
	if len(served) != 1 {
		t.Errorf("served = %v after a failure", served)
	}
}
