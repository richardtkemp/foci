package app

import "testing"

func TestPinSet_EncodeDecodeRoundTrip(t *testing.T) {
	// Sorted + de-duplicated, so an unchanged set round-trips to an identical
	// string and two equal sets never differ on the wire.
	if got := encodePinSet([]string{"m9", "m1", "m9"}); got != `["m1","m9"]` {
		t.Errorf("encodePinSet = %q, want [\"m1\",\"m9\"]", got)
	}
	if got := decodePinSet(`["m9","m1"]`); !equalIDs(got, []string{"m1", "m9"}) {
		t.Errorf("decodePinSet = %v, want [m1 m9]", got)
	}
}

// An empty set must encode as `[]`, never `null`: the Kotlin PinSync.messageIds
// is a non-nullable List and a null would fail to decode, silently dropping the
// replay that clears a stale pin.
func TestPinSet_EmptyIsArrayNotNull(t *testing.T) {
	if got := encodePinSet(nil); got != "[]" {
		t.Errorf("encodePinSet(nil) = %q, want []", got)
	}
	if got := decodePinSet(""); got == nil {
		t.Error("decodePinSet(\"\") returned nil; must be an empty non-nil slice so JSON is [] not null")
	}
}

func TestPinSet_UnparseableDegradesToEmpty(t *testing.T) {
	if got := decodePinSet(`not json`); len(got) != 0 || got == nil {
		t.Errorf("decodePinSet(garbage) = %v, want an empty non-nil slice", got)
	}
}

func TestApplyPin_Idempotent(t *testing.T) {
	base := []string{"m1"}
	if got := applyPin(base, "m1", true); !equalIDs(got, []string{"m1"}) {
		t.Errorf("re-pinning = %v, want [m1]", got)
	}
	if got := applyPin(base, "m2", false); !equalIDs(got, []string{"m1"}) {
		t.Errorf("unpinning an absent id = %v, want [m1]", got)
	}
	if got := applyPin(base, "m1", false); len(got) != 0 {
		t.Errorf("unpinning the only id = %v, want empty", got)
	}
	if got := applyPin(base, "m0", true); !equalIDs(got, []string{"m0", "m1"}) {
		t.Errorf("pinning a second id = %v, want [m0 m1]", got)
	}
	if !equalIDs(base, []string{"m1"}) {
		t.Errorf("applyPin mutated its input: %v", base)
	}
}
