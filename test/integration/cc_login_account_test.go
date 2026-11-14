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

	"foci/internal/testharness"
)

// The #3104 phase-2 L2 tests: each Claude Code account has its own login.
// alpha is on the host folder; beta and beta2 on <tmp>/ccB. cc-stub keeps a
// login state per config folder (.ccstub-auth.json) and, run with no
// arguments on a terminal, emulates the /login screens, so the relogin pane
// (real tmux, a private server per gateway) signs ccB in when a code is
// typed.

// writeStubAuth sets the stub's login state for one config folder.
func writeStubAuth(t *testing.T, dir string, st map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, ".ccstub-auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sentTexts returns the text of every message the agent's bot sent or
// edited.
func sentTexts(h *testharness.Harness, agentID string) []string {
	var out []string
	for _, c := range h.TelegramStub().PeekSent(h.AgentBotToken(agentID)) {
		if c.Method != "sendMessage" && c.Method != "editMessageText" {
			continue
		}
		var body struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(c.Body, &body)
		out = append(out, body.Text)
	}
	return out
}

// countContaining counts the texts containing every one of must.
func countContaining(texts []string, must ...string) int {
	n := 0
	for _, s := range texts {
		ok := true
		for _, m := range must {
			ok = ok && strings.Contains(s, m)
		}
		if ok {
			n++
		}
	}
	return n
}

// waitSent waits until the agent's bot sent a message containing every one
// of must.
func waitSent(t *testing.T, h *testharness.Harness, agentID string, must ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * testharness.CorrectnessWaitFloor)
	for time.Now().Before(deadline) {
		if countContaining(sentTexts(h, agentID), must...) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never got a message with %q; sent %q\nstderr:\n%s", agentID, must, sentTexts(h, agentID), stderrTail(h.Stderr()))
}

// TestL2_CCLogin_OneGatePerAccount (H3, H4, H5, H6 end to end): account B's
// login expires. A message to beta meets CC's auth failure and one relogin
// starts for B: its link, naming B's email, goes to beta's chat, and the
// login pane runs with B's folder. While it runs, alpha (host account) answers
// normally and gets no link; a message to beta2 (also on B) is held and gets
// the same link repeated; no second link appears in beta's chat. The code
// typed in beta's chat signs B in, the completion names B, and beta2's held
// message then runs.
func TestL2_CCLogin_OneGatePerAccount(t *testing.T) {
	testharness.ParallelWait(t)
	const alphaUser, betaUser, beta2User = 31050, 31051, 31052
	ccB := filepath.Join(t.TempDir(), "ccB")
	writeStubAuth(t, ccB, map[string]any{"email": "b@example.com"})
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: alphaUser},
			{ID: "beta", UserID: betaUser, ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
			{ID: "beta2", UserID: beta2User, ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	alphaKey, betaKey := fmt.Sprintf("alpha/c%d", alphaUser), fmt.Sprintf("beta/c%d", betaUser)
	// One normal turn each, so every chat exists and is the default one.
	pushUserMessage(t, h, "alpha", alphaUser, "WARM_3104_A")
	pushUserMessage(t, h, "beta", betaUser, "WARM_3104_B")
	pushUserMessage(t, h, "beta2", beta2User, "WARM_3104_B2")
	waitForTurnDone(t, h, "alpha", alphaKey, "WARM_3104_A")
	waitForTurnDone(t, h, "beta", betaKey, "WARM_3104_B")
	waitForTurnDone(t, h, "beta2", fmt.Sprintf("beta2/c%d", beta2User), "WARM_3104_B2")

	// B's login expires.
	writeStubAuth(t, ccB, map[string]any{"loggedIn": false, "email": "b@example.com"})
	pushUserMessage(t, h, "beta", betaUser, "MARKER_3104_B1")
	waitSent(t, h, "beta", "Sign in to Claude Code", "(account: b@example.com)")

	// The login pane ran with B's folder.
	var pane recorderEntry
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "interactive_login" {
			pane = e
		}
	}
	if pane.ConfigDir != ccB {
		t.Errorf("login pane CLAUDE_CONFIG_DIR %q, want %q", pane.ConfigDir, ccB)
	}

	// alpha runs normally and never sees the link.
	pushUserMessage(t, h, "alpha", alphaUser, "MARKER_3104_A1")
	waitForTurnDone(t, h, "alpha", alphaKey, "MARKER_3104_A1")
	if n := countContaining(sentTexts(h, "alpha"), "Sign in"); n != 0 {
		t.Errorf("alpha (host account) got %d sign-in links", n)
	}

	// beta2 (account B) is held and gets the same link repeated.
	pushUserMessage(t, h, "beta2", beta2User, "MARKER_3104_B2")
	waitSent(t, h, "beta2", "Sign in to Claude Code", "(account: b@example.com)")
	if waitForUserMessage(t, h, "workspaces/beta2", "MARKER_3104_B2", time.Second) {
		t.Error("beta2's message ran during B's login")
	}
	if n := countContaining(sentTexts(h, "beta"), "Sign in to Claude Code"); n != 1 {
		t.Errorf("beta's chat has %d sign-in links after alpha's turn, want 1", n)
	}

	// The code in beta's chat signs B in.
	pushUserMessage(t, h, "beta", betaUser, "stub-code-3104")
	waitSent(t, h, "beta", "login completed", "(account: b@example.com)")
	if !waitForUserMessage(t, h, "workspaces/beta2", "MARKER_3104_B2", 20*time.Second) {
		t.Fatalf("beta2's held message never ran after B's login; stderr:\n%s", stderrTail(h.Stderr()))
	}
	b, err := os.ReadFile(filepath.Join(ccB, ".ccstub-auth.json"))
	if err != nil || !strings.Contains(string(b), `"loggedIn":true`) {
		t.Errorf("ccB not signed in after the code: %s, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(h.TempDir(), "workspaces", ".claude", ".ccstub-auth.json")); err == nil {
		t.Error("B's login was written to the host folder")
	}
}

// TestL2_CCLogin_StartupChecksEachAccount (H7, requirement 6): at startup the
// host folder is signed in and ccB is not. Exactly one relogin starts, for B,
// its link in beta's chat (the only agent on B); alpha gets none.
func TestL2_CCLogin_StartupChecksEachAccount(t *testing.T) {
	testharness.ParallelWait(t)
	const alphaUser, betaUser = 31053, 31054
	ccB := filepath.Join(t.TempDir(), "ccB")
	writeStubAuth(t, ccB, map[string]any{"loggedIn": false, "email": "b2@example.com"})
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: alphaUser},
			{ID: "beta", UserID: betaUser, ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	if !waitForStderr(h, "CC re-login starting", 30*time.Second) {
		t.Fatalf("no relogin started at startup; stderr:\n%s", stderrTail(h.Stderr()))
	}
	// alpha's account is fine: its message runs.
	pushUserMessage(t, h, "alpha", alphaUser, "MARKER_3104_S1")
	waitForTurnDone(t, h, "alpha", fmt.Sprintf("alpha/c%d", alphaUser), "MARKER_3104_S1")
	if n := strings.Count(h.Stderr(), "CC re-login starting"); n != 1 {
		t.Errorf("%d relogins started, want exactly one (for B)", n)
	}
	if !strings.Contains(h.Stderr(), "relogin/beta") || strings.Contains(h.Stderr(), "relogin/alpha] CC re-login starting") {
		t.Errorf("the relogin was not beta's; stderr:\n%s", stderrTail(h.Stderr()))
	}
	if n := countContaining(sentTexts(h, "alpha"), "Sign in"); n != 0 {
		t.Errorf("alpha got %d sign-in links", n)
	}
	waitSent(t, h, "beta", "Sign in to Claude Code", "(account: b2@example.com)")
}
