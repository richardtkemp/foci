package agent

import "testing"

// batchTrigger is what RunTurn uses to label a turn's trigger for the [meta]
// via= header. A transcribed-voice turn must read trigger="voice" regardless
// of the originating platform; a normal typed message keeps the platform's
// own trigger (#1436).
func TestBatchTrigger_VoiceEnvelopeOverridesPlatform(t *testing.T) {
	batch := []Envelope{{Text: "hello", Voice: true}}
	if got := batchTrigger("app", batch); got != "voice" {
		t.Errorf("batchTrigger = %q, want %q", got, "voice")
	}
}

func TestBatchTrigger_TypedEnvelopeKeepsPlatform(t *testing.T) {
	batch := []Envelope{{Text: "hello"}}
	if got := batchTrigger("app", batch); got != "app" {
		t.Errorf("batchTrigger = %q, want %q", got, "app")
	}
}

func TestBatchTrigger_MixedVoiceAndTypedCountsAsVoice(t *testing.T) {
	// A batch can accumulate more than one envelope (e.g. a follow-up message
	// queued behind an in-flight turn) — if ANY of them carries transcribed
	// voice content, the whole turn is tagged voice.
	batch := []Envelope{{Text: "typed"}, {Text: "spoken", Voice: true}}
	if got := batchTrigger("telegram", batch); got != "voice" {
		t.Errorf("batchTrigger = %q, want %q", got, "voice")
	}
}

func TestBatchTrigger_EmptyBatchKeepsPlatform(t *testing.T) {
	if got := batchTrigger("discord", nil); got != "discord" {
		t.Errorf("batchTrigger = %q, want %q", got, "discord")
	}
}
