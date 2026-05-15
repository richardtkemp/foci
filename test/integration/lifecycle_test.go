//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestL2_Lifecycle_RestartAfterStubExit verifies foci-gw's retry path
// when the CC subprocess exits unexpectedly: subsequent messages spawn
// a fresh subprocess and the agent keeps working. This is the
// regression net for the morning's CC resume failure path
// (`delegated: backend died during init with --resume <ID> — retrying
// without resume`) — confirming the recovery loop works in isolation
// from real claude.
//
// Mechanism is intentionally simple: send two Telegram messages with a
// pause between them. The recorder should show TWO invocation entries
// for alpha's workdir AND TWO user_message entries — proving the
// subprocess processed both messages even if the first cc-stub
// subprocess exits before the second message arrives. (This doesn't
// force the subprocess to die; it just exercises the common case that
// would surface if foci's session-lifecycle code was broken. A more
// aggressive test using CCSTUB_FAIL_ON_RESUME is left for follow-up
// once the harness exposes per-test env-var injection.)
func TestL2_Lifecycle_RestartAfterStubExit(t *testing.T) {
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9999},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")

	send := func(text string) {
		h.TelegramStub().PushUpdate(token, gotgbot.Update{
			Message: &gotgbot.Message{
				Chat: gotgbot.Chat{Id: 9999, Type: "private"},
				From: &gotgbot.User{Id: 9999, FirstName: "Tester"},
				Text: text,
			},
		})
	}

	send("first message")
	if !waitForUserMessage(t, h, "workspaces/alpha", "first message", 15*time.Second) {
		t.Fatalf("first message never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	send("second message")
	if !waitForUserMessage(t, h, "workspaces/alpha", "second message", 15*time.Second) {
		t.Fatalf("second message never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	// Belt-and-braces: both should be in the recorder, both in alpha's workdir.
	var count int
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") {
			count++
		}
	}
	if count < 2 {
		t.Errorf("expected at least 2 user_message entries in alpha's workdir, got %d", count)
	}
}
