//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"

	"foci/internal/testharness"
)

// The #3186 phase-2 L2 test: ONE agent with two Claude Code accounts (two
// real config folders on cc-stub). Each new chat takes the account with the
// most usage left and keeps it across a restart; a hold of one account holds
// only the chat on it, and its notice goes only to the chat on it.

// sentTextsToChat is every message the agent's bot sent to chatID.
func sentTextsToChat(h *testharness.Harness, agentID string, chatID int64) []string {
	var out []string
	for _, c := range h.TelegramStub().PeekSent(h.AgentBotToken(agentID)) {
		if c.Method != "sendMessage" && c.Method != "editMessageText" {
			continue
		}
		var body struct {
			ChatID json.Number `json:"chat_id"`
			Text   string      `json:"text"`
		}
		_ = json.Unmarshal(c.Body, &body)
		if body.ChatID.String() == fmt.Sprint(chatID) {
			out = append(out, body.Text)
		}
	}
	return out
}

// scriptUsageEvent makes the agent's next turn containing match report the
// five-hour window at util (0..1).
func scriptUsageEvent(t *testing.T, h *testharness.Harness, agentID, match string, util float64) {
	t.Helper()
	scriptFiveHourEvent(t, h, agentID, match, util)
}

// TestL2_MultiAccount_PickHoldAndRestart (#3186 F2, F3, F6, F7):
//  1. chat 1 is new with no figure on either account: it takes the first,
//     A, and reports A's five-hour window at 90% (resetting in 2 h);
//  2. chat 2 is new: it takes B (no figure = unused), and its CC runs with
//     B's folder, where its transcript is;
//  3. A's window reaches 99%: A holds; chat 1 is told and its next message
//     waits; chat 2 is not told and its message runs;
//  4. after a restart both chats are still on their accounts, keep their
//     rows, and chat 2 resumes its CC session in B's folder.
func TestL2_MultiAccount_PickHoldAndRestart(t *testing.T) {
	testharness.ParallelWait(t)
	const user = 31090
	const chat1, chat2 = int64(user), int64(user + 1)
	root := t.TempDir()
	ccA, ccB := filepath.Join(root, "ccA"), filepath.Join(root, "ccB")
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID: "multi", UserID: user, Accounts: []string{ccA, ccB},
			ExtraEnv: map[string]string{"CCSTUB_WRITE_TRANSCRIPT": "1"},
		}},
		ReadyTimeout: 30 * time.Second,
	})
	key1, key2 := fmt.Sprintf("multi/c%d", chat1), fmt.Sprintf("multi/c%d", chat2)
	token := h.AgentBotToken("multi")
	send := func(chatID int64, text string) {
		h.TelegramStub().PushUpdate(token, gotgbot.Update{Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: chatID, Type: "private"},
			From: &gotgbot.User{Id: user, FirstName: "Tester"},
			Text: text,
		}})
	}
	accountOf := func(key string) string { return readChatState(t, h, key).meta["account"] }

	// 1. Chat 1 takes A and reports A at 90%, resetting in 2 h. Under the
	// T-profile pick (#3247) an account's score is its tighter window's
	// headroom per hour to reset, the 5h window weighted by the default
	// 5h/7d cap ratio (0.1). With no 7d figure both accounts score
	// 100%/168 h on the 7d window, so the 5h window must bind for A:
	// 10% x 0.1 / 2 h = 0.5 < 0.595, so chat 2 takes B. (At 50% the 7d
	// window binds both, they tie, and the tie goes to A.) A second turn
	// names the transcript in the index row.
	scriptUsageEvent(t, h, "multi", "WARM_3186_A", 0.9)
	send(chat1, "WARM_3186_A")
	waitForTurnDone(t, h, "multi", key1, "WARM_3186_A")
	send(chat1, "WARM_3186_A2")
	waitForTurnDone(t, h, "multi", key1, "WARM_3186_A2")
	if acct := accountOf(key1); !strings.Contains(acct, ccA) {
		t.Fatalf("chat 1 on account %q, want A (%s)", acct, ccA)
	}

	// 2. Chat 2 takes B.
	for _, m := range []string{"WARM_3186_B", "WARM_3186_B2"} {
		send(chat2, m)
		waitForTurnDone(t, h, "multi", key2, m)
	}
	if acct := accountOf(key2); !strings.Contains(acct, ccB) {
		t.Fatalf("chat 2 on account %q, want B (%s): A is 90%% used, resetting in 2 h, B has no figure", acct, ccB)
	}
	st2 := waitForIndexedFile(t, h, key2)
	ws := agentWorkspace(h, "multi")
	want2 := filepath.Join(ccB, "projects", ccSlug(ws), st2.meta["cc_resume_id"]+".jsonl")
	if st2.filePath != want2 {
		t.Errorf("chat 2's row names %s, want B's folder's %s", st2.filePath, want2)
	}
	if _, err := os.Stat(want2); err != nil {
		t.Errorf("chat 2's transcript is not in B's folder: %v", err)
	}
	st1 := waitForIndexedFile(t, h, key1)
	if !strings.HasPrefix(st1.filePath, ccA) {
		t.Errorf("chat 1's row names %s, want a file in A's folder %s", st1.filePath, ccA)
	}

	// 3. A holds: chat 1 is told and waits, chat 2 runs and is not told.
	scriptUsageEvent(t, h, "multi", "HOLD_3186_A", 0.99)
	send(chat1, "HOLD_3186_A")
	deadline := time.Now().Add(2 * testharness.CorrectnessWaitFloor)
	for countContaining(sentTextsToChat(h, "multi", chat1), "Usage hold", "(account: "+ccA+")") == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if countContaining(sentTextsToChat(h, "multi", chat1), "Usage hold", "(account: "+ccA+")") == 0 {
		t.Fatalf("chat 1 (on A) never got A's hold notice; sent %q\nstderr:\n%s", sentTexts(h, "multi"), stderrTail(h.Stderr()))
	}
	send(chat1, "MARKER_3186_A_HELD")
	send(chat2, "MARKER_3186_B_RUNS")
	waitForTurnDone(t, h, "multi", key2, "MARKER_3186_B_RUNS")
	if ranUserMessage(t, h, "workspaces/multi", "MARKER_3186_A_HELD") {
		t.Error("chat 1's message ran during A's hold")
	}
	if n := countContaining(sentTextsToChat(h, "multi", chat2), "Usage hold"); n != 0 {
		t.Errorf("chat 2 (on B) got %d hold notices of A", n)
	}

	// 4. A restart keeps both chats on their accounts.
	restartAndSweep(t, h)
	for key, want := range map[string]string{key1: ccA, key2: ccB} {
		got := readChatState(t, h, key)
		if !got.present || !strings.Contains(got.meta["account"], want) {
			t.Errorf("after the restart %s: present=%v account %q, want on %s", key, got.present, got.meta["account"], want)
		}
	}
	send(chat2, "MARKER_3186_B_AFTER")
	waitForTurnDone(t, h, "multi", key2, "MARKER_3186_B_AFTER")
	resumed := false
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		resumed = resumed || (e.ResumeID != "" && e.ResumeID == st2.meta["cc_resume_id"])
	}
	if !resumed {
		t.Errorf("chat 2 never resumed its CC session %s after the restart", st2.meta["cc_resume_id"])
	}
	if ranUserMessage(t, h, "workspaces/multi", "MARKER_3186_A_HELD") {
		t.Error("chat 1's held message ran after the restart while A still holds")
	}
}
