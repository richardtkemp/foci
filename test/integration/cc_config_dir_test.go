//go:build integration

package integration

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"foci/internal/sqlite"
	"foci/internal/testharness"
)

// The #3104 phase-1 tests: an agent whose backend_config.env sets its own
// CLAUDE_CONFIG_DIR ("beta") next to one with no override ("alpha"). cc-stub
// writes every consumed message to the transcript under its CLAUDE_CONFIG_DIR
// (CCSTUB_WRITE_TRANSCRIPT), as real CC does, so the session index rows name
// files that exist exactly when foci looks in the folder CC uses.

// ccSlug is CC's transcript folder name for a workdir (fociroot.CCProjectSlug).
func ccSlug(path string) string {
	return regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(path, "-")
}

// chatState is what foci keeps for one chat session in state.db.
type chatState struct {
	present  bool
	filePath string
	meta     map[string]string
	history  int // backend_resume_history rows
}

// readChatState reads a session key's index row, session metadata and resume
// history from the gateway's state.db.
func readChatState(t *testing.T, h *testharness.Harness, sessionKey string) chatState {
	t.Helper()
	db, err := sqlite.OpenReadOnly(filepath.Join(h.DataDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state.db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var st chatState
	err = db.QueryRow(`SELECT file_path FROM session_index WHERE session_key = ?`, sessionKey).Scan(&st.filePath)
	switch {
	case err == nil:
		st.present = true
	case !errors.Is(err, sql.ErrNoRows):
		t.Fatalf("read session_index %s: %v", sessionKey, err)
	}
	st.meta = map[string]string{}
	rows, err := db.Query(`SELECT key, value FROM session_metadata WHERE session_key = ?`, sessionKey)
	if err != nil {
		t.Fatalf("read session_metadata %s: %v", sessionKey, err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		st.meta[k] = v
	}
	_ = rows.Close()
	if err := db.QueryRow(`SELECT COUNT(*) FROM backend_resume_history WHERE session_key = ?`, sessionKey).Scan(&st.history); err != nil {
		t.Fatalf("read backend_resume_history %s: %v", sessionKey, err)
	}
	return st
}

// waitForIndexedFile waits until the chat's index row names a transcript file
// and its resume id is saved. A turn records the path at its entry, from the
// running backend, so a chat's row names its file from its second turn on. Whether that
// file exists is the tests' to assert: a row naming a missing file is what
// the restart sweep purges.
func waitForIndexedFile(t *testing.T, h *testharness.Harness, sessionKey string) chatState {
	t.Helper()
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	var st chatState
	for time.Now().Before(deadline) {
		st = readChatState(t, h, sessionKey)
		if st.filePath != "" && st.meta["cc_resume_id"] != "" && st.history > 0 {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: index row never named a transcript with a saved resume id (last %+v)\nstderr:\n%s",
		sessionKey, st, stderrTail(h.Stderr()))
	return st
}

// waitForTurnDone waits until marker's turn has reached cc-stub and the
// session's backend is idle again, so the next message starts a new turn
// rather than folding into this one.
func waitForTurnDone(t *testing.T, h *testharness.Harness, agentID, sessionKey, marker string) {
	t.Helper()
	if !waitForUserMessage(t, h, "workspaces/"+agentID, marker, 20*time.Second) {
		t.Fatalf("turn %s never reached cc-stub; recorder:\n%s\nstderr:\n%s",
			marker, recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	var status string
	for time.Now().Before(deadline) {
		var err error
		if status, err = h.BackendStatus(agentID, sessionKey); err != nil {
			t.Fatalf("backend_status %s: %v", sessionKey, err)
		}
		if strings.HasPrefix(status, "idle") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s's backend never went idle after %s (last status %q)", sessionKey, marker, status)
}

// restartAndSweep restarts the gateway and waits for startup's orphan sweep
// (SessionIndex.PruneOrphans) to finish, so the rows read next are the ones
// the sweep left.
func restartAndSweep(t *testing.T, h *testharness.Harness) {
	t.Helper()
	if err := h.Restart(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitForSweep(t, h)
}

func waitForSweep(t *testing.T, h *testharness.Harness) {
	t.Helper()
	if !waitForStderr(h, "session index: pruned ", 30*time.Second) {
		t.Fatalf("startup orphan sweep never logged; stderr:\n%s", stderrTail(h.Stderr()))
	}
}

// lastAgentInvocation returns the last non-batch cc-stub spawn in the agent's
// workdir.
func lastAgentInvocation(t *testing.T, h *testharness.Harness, agentID string) (recorderEntry, bool) {
	t.Helper()
	invs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/"+agentID)
	for i := len(invs) - 1; i >= 0; i-- {
		if !isBatchInvocation(invs[i]) {
			return invs[i], true
		}
	}
	return recorderEntry{}, false
}

// TestL2_CCConfigDir_RowsSurviveRestarts (#3104 H1, H4): after two turns (the
// second records the transcript path in the index row), a per-folder agent's
// chat keeps its index row, session metadata and resume history across two
// restarts, and its next message resumes the same CC session. On main the row
// named a transcript in the host folder, where CC never writes for that
// agent, so every restart's orphan sweep purged it (F1). Its transcript is in
// its own folder and nothing of it is under the host folder. A row whose
// transcript is really deleted is still pruned, for both agents.
func TestL2_CCConfigDir_RowsSurviveRestarts(t *testing.T) {
	testharness.ParallelWait(t)
	const alphaUser, betaUser = 31040, 31041
	ccB := filepath.Join(t.TempDir(), "ccB")
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: alphaUser, ExtraEnv: map[string]string{"CCSTUB_WRITE_TRANSCRIPT": "1"}},
			{ID: "beta", UserID: betaUser, ExtraEnv: map[string]string{"CCSTUB_WRITE_TRANSCRIPT": "1", "CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	alphaKey, betaKey := fmt.Sprintf("alpha/c%d", alphaUser), fmt.Sprintf("beta/c%d", betaUser)

	for _, m := range []string{"MARKER_3104_1", "MARKER_3104_2"} {
		pushUserMessage(t, h, "alpha", alphaUser, m+"_A")
		pushUserMessage(t, h, "beta", betaUser, m+"_B")
		waitForTurnDone(t, h, "alpha", alphaKey, m+"_A")
		waitForTurnDone(t, h, "beta", betaKey, m+"_B")
	}
	alphaBefore := waitForIndexedFile(t, h, alphaKey)
	betaBefore := waitForIndexedFile(t, h, betaKey)

	for i := range 2 {
		restartAndSweep(t, h)
		for key, before := range map[string]chatState{alphaKey: alphaBefore, betaKey: betaBefore} {
			got := readChatState(t, h, key)
			if !got.present || got.meta["cc_resume_id"] != before.meta["cc_resume_id"] || got.history < before.history {
				t.Fatalf("after restart %d, %s lost its session data: before %+v, now %+v\nstderr:\n%s",
					i+1, key, before, got, stderrTail(h.Stderr()))
			}
		}
	}

	betaWS := agentWorkspace(h, "beta")
	resumeID := betaBefore.meta["cc_resume_id"]
	want := filepath.Join(ccB, "projects", ccSlug(betaWS), resumeID+".jsonl")
	if betaBefore.filePath != want {
		t.Errorf("beta's index row names %s, want its own folder's %s", betaBefore.filePath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("beta's transcript is not in its own folder: %v", err)
	}
	hostProjects := filepath.Join(h.TempDir(), "workspaces", ".claude", "projects")
	if _, err := os.Stat(filepath.Join(hostProjects, ccSlug(betaWS))); !os.IsNotExist(err) {
		t.Errorf("something for beta was written under the host folder %s (stat err %v)", hostProjects, err)
	}

	pushUserMessage(t, h, "beta", betaUser, "MARKER_3104_B3")
	if !waitForUserMessage(t, h, "workspaces/beta", "MARKER_3104_B3", 20*time.Second) {
		t.Fatalf("beta's turn after the restarts never reached cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if inv, ok := lastAgentInvocation(t, h, "beta"); !ok || inv.ResumeID != resumeID {
		t.Errorf("beta's CC after the restarts started with --resume %q, want the same session %q", inv.ResumeID, resumeID)
	}

	// H4: a transcript that is really gone still prunes its row.
	if err := h.Shutdown(15 * time.Second); err != nil {
		t.Logf("shutdown: %v", err)
	}
	alphaNow, betaNow := readChatState(t, h, alphaKey), readChatState(t, h, betaKey)
	for _, p := range []string{alphaNow.filePath, betaNow.filePath} {
		if err := os.Remove(p); err != nil {
			t.Fatalf("delete transcript %q: %v", p, err)
		}
	}
	restartAndSweep(t, h)
	for _, key := range []string{alphaKey, betaKey} {
		if got := readChatState(t, h, key); got.present {
			t.Errorf("%s's transcript was deleted but its row survived the sweep: %+v", key, got)
		}
	}
}

// TestL2_CCConfigDir_FoldedSteerNotResentAfterRestart (#3104 H2, restart
// path): a per-folder agent's CC folds a steer into its transcript and is
// still working, unacknowledged, when foci shuts down. The next foci process
// must find the steer in that agent's transcript (consumedBeforeRestart) and
// not send it again. On main it looked in the host folder, read the
// transcript as unreadable and redelivered (F9).
func TestL2_CCConfigDir_FoldedSteerNotResentAfterRestart(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 31044
	const primary = "MARKER_3104R_PRIMARY"
	const steer = "MARKER_3104R_STEER"
	const followUp = "MARKER_3104R_FOLLOWUP"
	ccB := filepath.Join(t.TempDir(), "ccB")
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "beta", UserID: userID, ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	script, _ := json.Marshal(map[string]any{
		"match":                primary,
		"text":                 "working on it",
		"hold_after_consuming": steer,
	})
	h.WriteCCStubScript(t, "beta", script)

	pushUserMessage(t, h, "beta", userID, primary)
	if !waitForUserMessage(t, h, "workspaces/beta", primary, 20*time.Second) {
		t.Fatalf("primary turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	pushUserMessage(t, h, "beta", userID, steer)
	if !waitForUserMessage(t, h, "workspaces/beta", steer, 20*time.Second) {
		t.Fatalf("steer never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	if err := h.Restart(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// The new process decides the persisted steer's fate at startup: wait for
	// its verdict before the follow-up, so a redelivery is queued first.
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	for time.Now().Before(deadline) {
		s := h.Stderr()
		if strings.Contains(s, "was folded in before the restart") || strings.Contains(s, "redelivering (at-least-once)") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	pushUserMessage(t, h, "beta", userID, followUp)
	if !waitForUserMessage(t, h, "workspaces/beta", followUp, 30*time.Second) {
		t.Fatalf("follow-up never reached the resumed cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if n := countUserMessagesContaining(t, h, "beta", steer); n != 1 {
		t.Errorf("steer delivered %d times, want exactly 1 (CC folded it into beta's transcript before the restart)", n)
	}
	if !strings.Contains(h.Stderr(), "was folded in before the restart") {
		t.Errorf("the restarted gateway never found the steer in beta's transcript (consumedBeforeRestart)")
	}
	if t.Failed() {
		t.Logf("recorder:\n%s\nstderr:\n%s", recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

// TestL2_CCConfigDir_FoldedSteerNotResentAfterDeath (#3104 H2, in-process
// path): TestL2_Steer_FoldedBeforeDeathNotRedelivered for a per-folder agent.
// CC folds a steer into the transcript in its own folder and dies before the
// ack; the hand-back must find it there and not redeliver it.
func TestL2_CCConfigDir_FoldedSteerNotResentAfterDeath(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 31045
	const primary = "MARKER_3104D_PRIMARY"
	const steer = "MARKER_3104D_STEER"
	const followUp = "MARKER_3104D_FOLLOWUP"
	ccB := filepath.Join(t.TempDir(), "ccB")
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "beta", UserID: userID, ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	script, _ := json.Marshal(map[string]any{
		"match":                 primary,
		"text":                  "working on it",
		"crash_after_consuming": steer,
	})
	h.WriteCCStubScript(t, "beta", script)

	pushUserMessage(t, h, "beta", userID, primary)
	if !waitForUserMessage(t, h, "workspaces/beta", primary, 20*time.Second) {
		t.Fatalf("primary turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	pushUserMessage(t, h, "beta", userID, steer)
	if !waitForUserMessage(t, h, "workspaces/beta", steer, 20*time.Second) {
		t.Fatalf("steer never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	waitForStubExitObserved(t, h, "beta", userID, "workspaces/beta")
	waitForHandBackDone(t, h)

	pushUserMessage(t, h, "beta", userID, followUp)
	if !waitForUserMessage(t, h, "workspaces/beta", followUp, 20*time.Second) {
		t.Fatalf("follow-up never reached the respawned cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if n := countUserMessagesContaining(t, h, "beta", steer); n != 1 {
		t.Errorf("steer delivered %d times, want exactly 1 (CC folded it into beta's transcript before dying)", n)
	}
	if t.Failed() {
		t.Logf("recorder:\n%s\nstderr:\n%s", recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

// TestL2_CCConfigDir_FolderChangeKeepsSessionData (#3104 H3, Dick's 00:23
// addition): an agent with two chats (two turns each, so each row names its
// transcript) moves to a new CLAUDE_CONFIG_DIR. One
// chat is used after the move: its resume cannot find the transcript (the
// stub fails --resume, as real CC does) and it starts fresh. Over two more
// restarts neither chat's index row, session metadata nor resume history is
// deleted, and foci copies or moves nothing between the folders (Q18).
func TestL2_CCConfigDir_FolderChangeKeepsSessionData(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 31046
	const chat2 = userID + 1000
	tmp := t.TempDir()
	ccB, ccB2 := filepath.Join(tmp, "ccB"), filepath.Join(tmp, "ccB2")
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "beta", UserID: userID, ExtraEnv: map[string]string{"CCSTUB_WRITE_TRANSCRIPT": "1", "CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("beta")
	key1, key2 := fmt.Sprintf("beta/c%d", userID), fmt.Sprintf("beta/c%d", chat2)

	for _, turn := range []string{"T1", "T2"} {
		sendText(h, token, userID, userID, "MARKER_3104M_CHAT1_"+turn)
		sendText(h, token, chat2, userID, "MARKER_3104M_CHAT2_"+turn)
		waitForTurnDone(t, h, "beta", key1, "MARKER_3104M_CHAT1_"+turn)
		waitForTurnDone(t, h, "beta", key2, "MARKER_3104M_CHAT2_"+turn)
	}
	before := map[string]chatState{key1: waitForIndexedFile(t, h, key1), key2: waitForIndexedFile(t, h, key2)}

	// Move beta to a new folder. Its CC there finds no transcript to resume.
	if err := h.Shutdown(15 * time.Second); err != nil {
		t.Logf("shutdown: %v", err)
	}
	cfg, err := os.ReadFile(h.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	oldEnv := fmt.Sprintf("CLAUDE_CONFIG_DIR = %q", ccB)
	if !strings.Contains(string(cfg), oldEnv) {
		t.Fatalf("config has no %s:\n%s", oldEnv, cfg)
	}
	moved := strings.Replace(string(cfg), oldEnv, fmt.Sprintf("CCSTUB_FAIL_ON_RESUME = \"1\", CLAUDE_CONFIG_DIR = %q", ccB2), 1)
	if err := os.WriteFile(h.ConfigPath(), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	restartAndSweep(t, h)
	// cut_turn marks a turn in flight (#2999) and is cleared when the turn
	// ends, so it is not data the change may keep or lose.
	assertKept := func(when string) {
		t.Helper()
		for key, b := range before {
			got := readChatState(t, h, key)
			lost := !got.present || got.history < b.history
			for k := range b.meta {
				if _, ok := got.meta[k]; !ok && k != "cut_turn" {
					lost = true
				}
			}
			if lost {
				t.Fatalf("%s, %s lost session data: before %+v, now %+v\nstderr:\n%s",
					when, key, b, got, stderrTail(h.Stderr()))
			}
		}
	}
	assertKept("at the restart that changed the folder")

	sendText(h, token, userID, userID, "MARKER_3104M_AFTER")
	if !waitForUserMessage(t, h, "workspaces/beta", "MARKER_3104M_AFTER", 30*time.Second) {
		t.Fatalf("chat 1's turn after the move never ran; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	// Wait for the fresh session's id to be saved in place of the old one.
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	for time.Now().Before(deadline) {
		if id := readChatState(t, h, key1).meta["cc_resume_id"]; id != "" && id != before[key1].meta["cc_resume_id"] {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	for i := range 2 {
		restartAndSweep(t, h)
		assertKept(fmt.Sprintf("after restart %d following chat 1's fresh start", i+1))
	}

	// Nothing was copied or moved between the folders (Q18).
	slug := ccSlug(agentWorkspace(h, "beta"))
	for key, b := range before {
		id := b.meta["cc_resume_id"]
		if _, err := os.Stat(filepath.Join(ccB, "projects", slug, id+".jsonl")); err != nil {
			t.Errorf("%s's old transcript left the old folder: %v", key, err)
		}
		if _, err := os.Stat(filepath.Join(ccB2, "projects", slug, id+".jsonl")); !os.IsNotExist(err) {
			t.Errorf("%s's old transcript appeared in the new folder (stat err %v): foci must copy nothing", key, err)
		}
	}
}
