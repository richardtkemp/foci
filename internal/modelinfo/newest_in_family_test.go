package modelinfo

import "testing"

// TestFamilyVersion covers the parse rule that decides what counts as a family
// member at all. The registry's anthropic ids are messier than they look — two
// spellings of the same version ("4-6" and "4.6"), price variants ("-fast",
// "[1m]"), a moving pointer ("-latest"), and one model whose version sits
// BEFORE the family token ("claude-3-haiku") — so every exclusion below is a
// real id the registry holds today, not a hypothetical.
func TestFamilyVersion(t *testing.T) {
	for _, tc := range []struct {
		id, family string
		want       []int
		wantOK     bool
	}{
		{"claude-fable-5", "fable", []int{5}, true},
		{"claude-fable-5-1", "fable", []int{5, 1}, true},
		{"claude-opus-4-6", "opus", []int{4, 6}, true},
		{"claude-opus-4.6", "opus", []int{4, 6}, true}, // dot spelling parses the same
		{"claude-opus-5", "opus", []int{5}, true},
		{"claude-fable-latest", "fable", nil, false}, // moving pointer
		{"claude-opus-5-fast", "opus", nil, false},   // price variant
		{"claude-opus-4-6[1m]", "opus", nil, false},  // context variant
		{"claude-3-haiku", "haiku", nil, false},      // version precedes family
		{"claude-opus-5", "sonnet", nil, false},      // wrong family
		{"claude-opusx-5", "opus", nil, false},       // family must be a whole segment
	} {
		got, ok := familyVersion(tc.id, tc.family)
		if ok != tc.wantOK {
			t.Errorf("familyVersion(%q, %q) ok = %v, want %v", tc.id, tc.family, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("familyVersion(%q, %q) = %v, want %v", tc.id, tc.family, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("familyVersion(%q, %q) = %v, want %v", tc.id, tc.family, got, tc.want)
				break
			}
		}
	}
}

// TestVersionLess pins the ordering that makes 5 beat 4.6 — a plain lexical or
// float compare gets this wrong (4.6 > 4.1 lexically, but 4.10 would not).
func TestVersionLess(t *testing.T) {
	for _, tc := range []struct {
		a, b []int
		want bool
	}{
		{[]int{4, 6}, []int{5}, true},
		{[]int{5}, []int{5, 1}, true}, // a longer tuple wins the tie
		{[]int{5, 1}, []int{5}, false},
		{[]int{4, 1}, []int{4, 10}, true}, // numeric, not lexical
		{[]int{5}, []int{5}, false},       // equal is not less
	} {
		if got := versionLess(tc.a, tc.b); got != tc.want {
			t.Errorf("versionLess(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestNewestInFamily_UnknownInputs proves the caller's fallback path is
// reachable: an unregistered developer or family must report ok=false rather
// than returning an empty id that would be sent to an endpoint as a model name.
func TestNewestInFamily_UnknownInputs(t *testing.T) {
	for _, tc := range []struct{ dev, family string }{
		{"anthropic", "nosuchfamily"},
		{"nosuchdev", "opus"},
	} {
		if id, ok := NewestInFamily(tc.dev, tc.family); ok {
			t.Errorf("NewestInFamily(%q, %q) = %q, true; want ok=false", tc.dev, tc.family, id)
		}
	}
	if id, ok := NewestInFamily("ANTHROPIC", "Opus"); !ok || id != "claude-opus-5" {
		t.Errorf("NewestInFamily(\"ANTHROPIC\", \"Opus\") = %q, %v; want claude-opus-5, true "+
			"(inputs are case-insensitive)", id, ok)
	}
}
