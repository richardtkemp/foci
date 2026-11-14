//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"

	"foci/internal/testharness"
)

// TestL2_ReplyNotRedeliveredWhenMessageLandsBeforeIdle reproduces #3116
// deterministically. A user message arrives after the first turn's reply was
// sent but before CC's idle: foci still sees the turn in flight and writes the
// message as a steer, while CC (cc-stub with CCSTUB_DELAY_IDLE) has ended the
// run, goes idle with the input unacked and answers it as the next run. The
// first reply must reach the chat exactly once.
func TestL2_ReplyNotRedeliveredWhenMessageLandsBeforeIdle(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 8031
	logDir := t.TempDir()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{ID: "alpha", UserID: userID,
			ExtraEnv: map[string]string{"CCSTUB_DELAY_IDLE": "2s"}}},
		ReadyTimeout: 30 * time.Second,
		ExtraConfigTOML: scopedLoggingTOML(logDir, logDir+"/foci.log", false) +
			"level = \"DEBUG\"\n[debug]\nextra_ccstream_logging = true\nextra_inbox_logging = true\n",
	})
	token := h.AgentBotToken("alpha")

	filePath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(filePath, []byte("report payload"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	const firstText = "first message"
	push := func(text string) {
		h.TelegramStub().PushUpdate(token, gotgbot.Update{
			Message: &gotgbot.Message{
				Chat: gotgbot.Chat{Id: userID, Type: "private"},
				From: &gotgbot.User{Id: userID, FirstName: "Tester"},
				Text: text,
			},
		})
	}
	firstReplies := func() int {
		n := 0
		for _, c := range h.TelegramStub().PeekSent(token) {
			if c.Method == "sendMessage" && strings.Contains(string(c.Body), "stub-reply") && strings.Contains(string(c.Body), firstText) {
				n++
			}
		}
		return n
	}

	push(firstText)
	deadline := time.Now().Add(20 * time.Second)
	for firstReplies() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if firstReplies() == 0 {
		t.Fatalf("FAIL A1: the first reply never reached the chat\n%s\nstderr:\n%s", sentCallSummary(h, token), stderrTail(h.Stderr()))
	}
	t.Logf("PASS A1: the first reply reached the chat")

	// The second message lands inside the 2s CCSTUB_DELAY_IDLE window: the
	// reply was sent at the run's assistant event, the idle is still owed.
	writeSendToChatScript(t, h, "alpha",
		fmt.Sprintf(`foci_send_to_chat --description %q --file %q`, "short caption", filePath),
		"[[NO_RESPONSE]]")
	push("ship it")

	if _, ok := waitForSentMethod(h, token, "sendDocument", 20*time.Second); !ok {
		t.Fatalf("FAIL A2: the second turn's sendDocument never landed\n%s\nstderr:\n%s", sentCallSummary(h, token), stderrTail(h.Stderr()))
	}
	t.Logf("PASS A2: the second turn ran (sendDocument)")

	// Degenerate-pass guard: the message must really have been written
	// before CC's idle (foci logs this at the idle when an input is unacked).
	if !strings.Contains(h.Stderr(), "idle with 1 unacked input") {
		t.Fatalf("FAIL A3 (degenerate run): the second message did not land between the reply and CC's idle; the gap was not exercised\nstderr:\n%s", stderrTail(h.Stderr()))
	}
	t.Logf("PASS A3: the second message landed before CC's idle")

	// Let any late copy arrive (second run's own idle delay + delivery).
	time.Sleep(4 * time.Second)
	if n := firstReplies(); n != 1 {
		t.Errorf("FAIL A4: the first reply reached the chat %d times, want 1\n--- sent ---\n%s", n, sentCallSummary(h, token))
	} else {
		t.Logf("PASS A4: the first reply reached the chat exactly once")
	}
}
