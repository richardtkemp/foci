package app

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"foci/internal/fap"
	flog "foci/internal/log"
	"foci/internal/platform"
)

// captureOpenSetLog runs fn with appLog captured at INFO and returns the output.
func captureOpenSetLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.INFO)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })
	fn()
	return buf.String()
}

// openSetLines returns the captured lines that mention the open-set.
func openSetLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "open-set") {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestOpenSetLog_NamesDeviceAndDelta proves an accepted open-set that changes the
// shared set logs ONE INFO line naming the sending device (id and paired label)
// and the conversation ids it added and removed, so "which device closed my
// chat" is answerable from the log alone (#2576).
func TestOpenSetLog_NamesDeviceAndDelta(t *testing.T) {
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	h.devices.pair("phone-1", "Pixel 8")
	h.storeOpenChats([]string{"keep", "gone"})

	sender := fakeClient()
	sender.deviceID = "phone-1"

	out := captureOpenSetLog(t, func() {
		h.handleConversationOpenSet(sender, fap.ConversationOpenSet{ConversationIDs: []string{"keep", "new"}})
	})

	lines := openSetLines(out)
	if len(lines) != 1 {
		t.Fatalf("want exactly one open-set line, got %d:\n%s", len(lines), out)
	}
	l := lines[0]
	for _, want := range []string{"INFO", "device=phone-1", `label="Pixel 8"`, "open=2", "added=[new]", "removed=[gone]"} {
		if !strings.Contains(l, want) {
			t.Errorf("open-set line lacks %q:\n%s", want, l)
		}
	}
}

// TestOpenSetLog_UnchangedIsNotInfo proves a re-send of the same set in another
// order (a no-op: it opens and closes nothing) adds no INFO line, so idempotent
// re-sends do not flood the log.
func TestOpenSetLog_UnchangedIsNotInfo(t *testing.T) {
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	h.storeOpenChats([]string{"a", "b"})

	sender := fakeClient()
	sender.deviceID = "mac-1"

	out := captureOpenSetLog(t, func() {
		h.handleConversationOpenSet(sender, fap.ConversationOpenSet{ConversationIDs: []string{"b", "a"}})
	})
	if lines := openSetLines(out); len(lines) != 0 {
		t.Errorf("unchanged open-set logged at INFO:\n%s", out)
	}
}
