//go:build integration

package integration

// #2787: /agents rename renames an agent inside the running gateway. The test
// runs a two-agent gateway, renames one agent from the other's chat, and
// checks the observable result: one gateway process throughout, the other
// agent never interrupted, the renamed agent answering on the same bot and
// over HTTP under its new id, and the id rewritten on disk.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestL2_AgentsRename_LiveOnlyTheRenamedAgentStops(t *testing.T) {
	testharness.ParallelWait(t)
	const (
		alphaUser = 27871
		renUser   = 27872
	)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{ID: "alpha", UserID: alphaUser}, {ID: "l2ren", UserID: renUser}},
		// The rename reads and may install the crontab: never the real one.
		FakeCrontab: true,
	})
	waitForSocket(t, h.SocketPath(), 0)
	// A job naming the agent, which the rename must rewrite.
	if err := os.WriteFile(h.FakeCrontabPath(), []byte("0 9 * * * foci send -a l2ren hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alphaToken, renToken := h.AgentBotToken("alpha"), h.AgentBotToken("l2ren")

	// History before the rename.
	pushTelegramText(t, h, "l2ren", renUser, "before the rename")
	if got := waitForSendMessageText(t, h, renToken, 0, "before the rename"); got == "" {
		t.Fatalf("l2ren never answered before the rename\nstderr:\n%s", stderrTail(h.Stderr()))
	}
	oldSessions := filepath.Join(h.SessionsDir(), "l2ren")
	entries, _ := os.ReadDir(h.SessionsDir())
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("sessions dir before the rename: %v", names)

	// Without --yes: the plan, and nothing stops.
	pushTelegramText(t, h, "alpha", alphaUser, "/agents rename l2ren l2renamed")
	// A long plan reaches Telegram in several messages: the plan, and the
	// --yes instruction at its end.
	if got := waitForSendMessageText(t, h, alphaToken, 0, "/agents rename l2ren l2renamed --yes"); got == "" || waitForSendMessageText(t, h, alphaToken, 0, "Plan:") == "" {
		t.Fatalf("no plan reply; alpha's sends: %q\nstderr:\n%s", peekSendMessageTexts(h, alphaToken), stderrTail(h.Stderr()))
	}
	if strings.Contains(h.Stderr(), `agent "l2ren" stopped live`) {
		t.Fatal("the plan-only command stopped the agent")
	}

	pushTelegramText(t, h, "alpha", alphaUser, "/agents rename l2ren l2renamed --yes")
	// "🏆 Renamed": the bare word is also in this test's temp paths.
	if got := waitForSendMessageText(t, h, alphaToken, 0, "🏆 Renamed", "l2renamed"); got == "" {
		t.Fatalf("no rename result in alpha's chat; alpha's sends: %q\nstderr:\n%s", peekSendMessageTexts(h, alphaToken), stderrTail(h.Stderr()))
	}
	for _, want := range []string{`agent "l2ren" stopped live`, `agent "l2renamed" started live`} {
		if !strings.Contains(h.Stderr(), want) {
			t.Errorf("stderr lacks %q", want)
		}
	}

	// alpha was never interrupted and still answers.
	pushTelegramText(t, h, "alpha", alphaUser, "alpha after the rename")
	if got := waitForSendMessageText(t, h, alphaToken, 0, "alpha after the rename"); got == "" {
		t.Errorf("alpha did not answer after the rename\nstderr:\n%s", stderrTail(h.Stderr()))
	}
	if strings.Contains(h.Stderr(), `agent "alpha" stopped live`) {
		t.Error("alpha was stopped")
	}

	// The renamed agent answers on the same bot (its token, pinned by the rename).
	h.TelegramStub().PushUpdate(renToken, gotgbot.Update{Message: &gotgbot.Message{
		Chat: gotgbot.Chat{Id: renUser, Type: "private"},
		From: &gotgbot.User{Id: renUser, FirstName: "Tester"},
		Text: "after the rename on telegram",
	}})
	if got := waitForSendMessageText(t, h, renToken, 0, "after the rename on telegram"); got == "" {
		t.Errorf("the renamed agent's bot never answered; its sends: %q\nstderr:\n%s", peekSendMessageTexts(h, renToken), stderrTail(h.Stderr()))
	}

	// One poller per token: the old agent's bot ended before the new one
	// polled with the same token (real Telegram would refuse the second
	// poller with 409 Conflict).
	if o := h.TelegramStub().CrossConnectPollOverlaps(renToken); len(o) > 0 {
		t.Errorf("the old and the new bot polled the renamed agent's token at once: %q", o)
	}

	// HTTP /send reaches it under the new id.
	c := webhookClient(h)
	body, _ := json.Marshal(map[string]any{"agent": "l2renamed", "text": "after the rename over http", "wait_none": true})
	resp, err := c.Post("http://foci-gw/send", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /send: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "after the rename over http") {
		t.Errorf("/send to l2renamed: status %d: %s", resp.StatusCode, out)
	}

	// On disk: foci.toml, the sessions tree, the crontab.
	cfg, err := os.ReadFile(h.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `id = "l2renamed"`) || strings.Contains(string(cfg), `id = "l2ren"`) {
		t.Errorf("foci.toml not renamed:\n%s", cfg)
	}
	if _, err := os.Stat(oldSessions); !os.IsNotExist(err) {
		t.Errorf("the old sessions tree is still there: %v", err)
	}
	if cron, _ := os.ReadFile(h.FakeCrontabPath()); !strings.Contains(string(cron), "-a l2renamed hello") {
		t.Errorf("crontab not renamed: %q", cron)
	}

	// One gateway process all along.
	if n := strings.Count(h.Stderr(), "startup complete"); n != 1 {
		t.Errorf("gateway started %d times, want 1", n)
	}
}

// Over the HTTP API (`foci command`, an agent) the rename never runs on its
// own: the owner gets an Approve/Deny prompt in chat. Deny changes nothing;
// Approve runs it.
func TestL2_AgentsRename_OverHTTPNeedsTheOwnersApproval(t *testing.T) {
	testharness.ParallelWait(t)
	const alphaUser = 27873
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:      []testharness.AgentSpec{{ID: "alpha", UserID: alphaUser}, {ID: "l2apr", UserID: 27874}},
		FakeCrontab: true,
	})
	waitForSocket(t, h.SocketPath(), 0)
	alphaToken := h.AgentBotToken("alpha")
	c := webhookClient(h)
	request := func() string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"command": "/agents rename l2apr l2aprnew", "agent": "alpha"})
		resp, err := c.Post("http://foci-gw/command", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /command: %v", err)
		}
		var out struct {
			Response string `json:"response"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		return out.Response
	}
	click := func(label string) {
		t.Helper()
		call, ok := waitForPermissionPrompt(t, h.TelegramStub(), alphaToken, "Approve renaming agent l2apr", 20*time.Second)
		if !ok {
			var methods []string
			for _, c := range h.TelegramStub().PeekSent(alphaToken) {
				methods = append(methods, c.Method+" "+string(c.Body))
			}
			t.Fatalf("no approval prompt in alpha's chat; alpha's calls: %q\nstderr:\n%s", methods, stderrTail(h.Stderr()))
		}
		h.TelegramStub().DrainSent(alphaToken) // the next prompt must be a new one
		h.TelegramStub().PushCallbackQuery(alphaToken, askButtonData(t, decodeInlineKeyboard(call.Body), label), alphaUser, alphaUser, 0)
	}

	if got := request(); !strings.Contains(got, "sent to the user for approval") {
		t.Fatalf("/command reply = %q", got)
	}
	click("Deny")
	if got := waitForEditOrSendText(t, h, alphaToken, "Denied"); got == "" {
		t.Errorf("no Denied edit; alpha's calls: %q", peekSendMessageTexts(h, alphaToken))
	}
	if strings.Contains(h.Stderr(), `agent "l2apr" stopped live`) {
		t.Fatal("a denied rename stopped the agent")
	}

	if got := request(); !strings.Contains(got, "sent to the user for approval") {
		t.Fatalf("second /command reply = %q", got)
	}
	click("Approve")
	if got := waitForSendMessageText(t, h, alphaToken, 0, "🏆 Renamed", "l2aprnew"); got == "" {
		t.Fatalf("no rename result after Approve; alpha's sends: %q\nstderr:\n%s", peekSendMessageTexts(h, alphaToken), stderrTail(h.Stderr()))
	}
	if cfg, _ := os.ReadFile(h.ConfigPath()); !strings.Contains(string(cfg), `id = "l2aprnew"`) {
		t.Errorf("foci.toml not renamed after Approve")
	}
}

// waitForEditOrSendText waits for a sendMessage or editMessageText to token
// whose text contains want.
func waitForEditOrSendText(t *testing.T, h *testharness.Harness, token, want string) string {
	t.Helper()
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" && call.Method != "editMessageText" {
				continue
			}
			var body map[string]any
			if json.Unmarshal(call.Body, &body) == nil {
				if text, _ := body["text"].(string); strings.Contains(text, want) {
					return text
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}
