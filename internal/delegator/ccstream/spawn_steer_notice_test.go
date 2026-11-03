package ccstream

import (
	"strings"
	"testing"
)

// #2833: the paragraph a background spawn gets quotes the words CC opens a
// folded message with (CC 2.1.314). Haiku followed 8 of 8 steers with this
// wording and 1 of 8 without it, so a change needs a new live test.
func TestSpawnSteerNoticeQuotesCCWrapper(t *testing.T) {
	t.Parallel()
	const want = "'The user sent a new message while you were working'"
	if !strings.Contains(spec.SpawnSteerNotice, want) {
		t.Fatalf("SpawnSteerNotice = %q, want it to quote %s", spec.SpawnSteerNotice, want)
	}
}
