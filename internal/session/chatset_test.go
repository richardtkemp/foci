package session

import "testing"

func TestChatSetEncodeSortsAndDeduplicates(t *testing.T) {
	// Encoding must produce the canonical set rendering — sorted, duplicates
	// collapsed, empty ids dropped — so two equal sets never differ on the wire.
	if got := EncodeChatSet([]string{"m9", "m1", "m9", ""}); got != `["m1","m9"]` {
		t.Errorf("EncodeChatSet = %q, want [\"m1\",\"m9\"]", got)
	}
}

// F2: the decode side must normalise too. A value that reached storage
// unsorted (hand-edited, or written by an older version) decodes sorted with
// duplicates collapsed, so it cannot leak an uncanonical form back into
// storage on the next write.
func TestChatSetDecodeNormalises(t *testing.T) {
	if got := DecodeChatSet(`["m9","m1","m9"]`); !equalChatSets(got, []string{"m1", "m9"}) {
		t.Errorf("DecodeChatSet = %v, want [m1 m9]", got)
	}
}

// An empty set must encode as `[]`, never `null`: the app's PinSync field is
// a non-nullable List and a null would fail to decode, silently dropping the
// replay that clears a stale pin. An absent or empty stored value must decode
// to an empty NON-NIL slice for the same reason.
func TestChatSetEmptyIsArrayNotNull(t *testing.T) {
	if got := EncodeChatSet(nil); got != "[]" {
		t.Errorf("EncodeChatSet(nil) = %q, want []", got)
	}
	for _, in := range []string{"", "[]"} {
		if got := DecodeChatSet(in); got == nil {
			t.Errorf("DecodeChatSet(%q) returned nil; must be an empty non-nil slice so JSON is [] not null", in)
		}
	}
}

// Unparseable stored input degrades to an empty non-nil set: the value is
// convenience state and is overwritten (recovered) by the next write.
func TestChatSetUnparseableDegradesToEmpty(t *testing.T) {
	if got := DecodeChatSet(`not json`); len(got) != 0 || got == nil {
		t.Errorf("DecodeChatSet(garbage) = %v, want an empty non-nil slice", got)
	}
}

func equalChatSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
