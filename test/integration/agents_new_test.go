//go:build integration

package integration

// #963: `/agents new` starts the new agent in the running gateway. The test
// runs the wizard through the Telegram stub on a one-agent gateway, then
// reaches the new agent on Telegram (its own bot) and over HTTP /send — with
// no restart in between.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestL2_AgentsNew_StartsAgentWithoutRestart(t *testing.T) {
	testharness.ParallelWait(t)
	const (
		userID     = 19631
		newBotUser = 19632
		newToken   = "963963:NEWBIE-TOKEN"
	)

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		// The new agent's bot token, as `/secrets set telegram.newbie` would
		// store it (appended inside the [telegram] secrets table).
		ExtraSecretsTOML: "newbie = \"" + newToken + "\"\n",
		// No onboarding turn for the new agent either: it would only add a
		// startup CC call.
		SeedAgentMetadata: map[string]map[string]string{"newbie": {"first_run_completed": "true"}},
		BeforeSpawn: func(stub *testharness.TelegramStub) {
			stub.RegisterBot(newToken, gotgbot.User{Id: newBotUser, IsBot: true, FirstName: "NewbieBot", Username: "newbie_bot"})
		},
	})
	waitForSocket(t, h.SocketPath(), 0)

	// The wizard installs crontab entries when the shared dir beside the
	// workspaces holds crontab.template, and boot seeded it there (HOME is
	// the test's workspaces dir). The harness's fake `crontab` takes those
	// writes (#2778); the check after the wizard proves the wizard did call
	// `crontab`, i.e. that without the fake it would edit the real crontab.
	tmpl := filepath.Join(filepath.Dir(h.AgentWorkspace("alpha")), "shared", "crontab.template")
	if _, err := os.Stat(tmpl); err != nil {
		t.Fatalf("boot did not seed %s (%v): the crontab check below would prove nothing", tmpl, err)
	}

	alphaToken := h.AgentBotToken("alpha")
	steps := []struct{ send, wantReply string }{
		{"/agents new", "Agent name"},
		{"Newbie", "Backend"},
		{"claude-code", "Model"},
		{"sonnet", "Character files"},
		{"blank", "is running"},
	}
	for _, s := range steps {
		pushTelegramText(t, h, "alpha", userID, s.send)
		if got := waitForSendMessageText(t, h, alphaToken, 0, s.wantReply); got == "" {
			t.Fatalf("after %q: no reply containing %q; replies: %q\nstderr:\n%s",
				s.send, s.wantReply, peekSendMessageTexts(h, alphaToken), stderrTail(h.Stderr()))
		}
	}
	if final := waitForSendMessageText(t, h, alphaToken, 0, "is running"); strings.Contains(final, "/restart") {
		t.Errorf("the wizard's final reply still asks for a restart:\n%s", final)
	}
	if cron, err := os.ReadFile(h.FakeCrontabPath()); err != nil || !strings.Contains(string(cron), "-a newbie") {
		t.Errorf("the wizard did not install the new agent's jobs through the fake crontab (err %v):\n%s", err, cron)
	}
	if !strings.Contains(h.Stderr(), `agent "newbie" started live`) {
		t.Errorf("no live-start log line for the new agent\nstderr:\n%s", stderrTail(h.Stderr()))
	}

	// Its own Telegram bot answers.
	h.TelegramStub().PushUpdate(newToken, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: userID, Type: "private"},
			From: &gotgbot.User{Id: userID, FirstName: "Tester"},
			Text: "hello newbie on telegram",
		},
	})
	if got := waitForSendMessageText(t, h, newToken, 0, "hello newbie on telegram"); got == "" {
		t.Fatalf("the new agent's bot never answered; its sends: %q\nstderr:\n%s",
			peekSendMessageTexts(h, newToken), stderrTail(h.Stderr()))
	}

	// HTTP /send reaches it too.
	c := webhookClient(h)
	body, _ := json.Marshal(map[string]any{"agent": "newbie", "text": "hello newbie over http", "wait_none": true})
	resp, err := c.Post("http://foci-gw/send", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /send: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/send to the new agent: status %d: %s\nstderr:\n%s", resp.StatusCode, out, stderrTail(h.Stderr()))
	}
	if !strings.Contains(string(out), "hello newbie over http") {
		t.Errorf("/send reply does not carry the agent's answer: %s", out)
	}

	// No restart happened: one gateway process all along.
	if n := strings.Count(h.Stderr(), "startup complete"); n != 1 {
		t.Errorf("gateway started %d times, want 1", n)
	}
}
