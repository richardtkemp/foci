//go:build integration

package integration

// L2 coverage for POST /webhook/{agent}/{hookid} (handleWebhook in
// cmd/foci-gw/http_handlers.go) against a real config-loaded agent: the hook
// is looked up in the agent's LIVE resolved config (inst.LiveConfig().Webhooks,
// merged from [system.webhooks]), the prompt file is resolved from the agent
// workspace's prompts/ dir, and the combined prompt+payload turn runs on
// cc-stub. The L1 tests in cmd/foci-gw/http_webhook_test.go mock the agent
// and session layer; these prove the wiring end to end (foci_todo #1229).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"foci/internal/testharness"
)

// webhookClient returns a unix-socket client for the gateway whose timeout
// covers a ?sync=true webhook: that request blocks until the agent's turn
// (a lazy cc-stub spawn included) completes, which can exceed
// gwUnixClient's 10s under host load.
func webhookClient(h *testharness.Harness) *http.Client {
	c := gwUnixClient(h.SocketPath())
	c.Timeout = testharness.CorrectnessWaitFloor
	return c
}

// postWebhook POSTs payload to /webhook/<agent>/<hook><query> and returns the
// status code and response body.
func postWebhook(t *testing.T, c *http.Client, agentID, hookID, query, payload string) (int, string) {
	t.Helper()
	resp, err := c.Post("http://foci-gw/webhook/"+agentID+"/"+hookID+query, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /webhook/%s/%s%s: %v", agentID, hookID, query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// decodeJSONMap decodes a flat string-valued JSON object response body.
func decodeJSONMap(t *testing.T, body string) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return m
}

// startWebhookGateway starts a one-agent gateway ("alpha") with the given
// extra config and workspace files, and waits for its unix socket.
func startWebhookGateway(t *testing.T, userID int64, extraTOML string, files map[string]string) *testharness.Harness {
	t.Helper()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:          []testharness.AgentSpec{{ID: "alpha", UserID: userID, PreStartFiles: files}},
		ExtraConfigTOML: extraTOML,
	})
	waitForSocket(t, h.SocketPath(), 0)
	return h
}

// TestL2_Webhook_AsyncQueuesTurnInHookSession proves the default (async)
// mode: a configured hook answers 202 "queued" at once, receipted to the
// per-hook independent session, and the queued turn then reaches cc-stub
// carrying the prompt file text followed by the request payload.
func TestL2_Webhook_AsyncQueuesTurnInHookSession(t *testing.T) {
	testharness.ParallelWait(t)
	const promptMarker = "WEBHOOK-ASYNC-PROMPT-1229"
	const payloadMarker = "webhook-async-payload-1229"

	h := startWebhookGateway(t, 12291,
		"\n[system.webhooks]\ndeploy = \"deploy.md\"\n",
		map[string]string{"prompts/deploy.md": promptMarker + ": handle this deploy."})

	code, body := postWebhook(t, webhookClient(h), "alpha", "deploy", "", `{"event":"`+payloadMarker+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("async webhook: status %d, want 202; body %q\nstderr:\n%s", code, body, stderrTail(h.Stderr()))
	}
	rcpt := decodeJSONMap(t, body)
	if rcpt["status"] != "queued" {
		t.Errorf("async receipt status = %q, want \"queued\" (receipt %v)", rcpt["status"], rcpt)
	}
	if rcpt["session"] != "alpha/ideploy" {
		t.Errorf("async receipt session = %q, want the per-hook independent session \"alpha/ideploy\" (receipt %v)", rcpt["session"], rcpt)
	}

	e, ok := waitForUserMessageContaining(t, h, "alpha", 0, promptMarker, "## Webhook Payload", payloadMarker)
	if !ok {
		t.Fatalf("queued webhook turn never reached cc-stub with prompt + payload\nrecorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if strings.Index(e.TextPrefix, promptMarker) > strings.Index(e.TextPrefix, payloadMarker) {
		t.Errorf("payload precedes prompt in the turn text: %q", e.TextPrefix)
	}
}

// TestL2_Webhook_SyncReturnsAgentReply proves ?sync=true: the request blocks
// for the turn and answers 200 with the agent's reply. cc-stub's default
// reply echoes its input, so the reply carrying the prompt and payload
// markers proves the response is THIS turn's, not a canned one.
func TestL2_Webhook_SyncReturnsAgentReply(t *testing.T) {
	testharness.ParallelWait(t)
	const promptMarker = "WEBHOOK-SYNC-PROMPT-1229"
	const payloadMarker = "webhook-sync-payload-1229"

	h := startWebhookGateway(t, 12292,
		"\n[system.webhooks]\nbuild = \"build.md\"\n",
		map[string]string{"prompts/build.md": promptMarker + ": handle this build."})

	code, body := postWebhook(t, webhookClient(h), "alpha", "build", "?sync=true", payloadMarker)
	if code != http.StatusOK {
		t.Fatalf("sync webhook: status %d, want 200; body %q\nstderr:\n%s", code, body, stderrTail(h.Stderr()))
	}
	rcpt := decodeJSONMap(t, body)
	for _, want := range []string{"stub-reply:", promptMarker, payloadMarker} {
		if !strings.Contains(rcpt["response"], want) {
			t.Errorf("sync response missing %q: %q", want, rcpt["response"])
		}
	}
	if rcpt["session"] != "alpha/ibuild" {
		t.Errorf("sync receipt session = %q, want \"alpha/ibuild\" (receipt %v)", rcpt["session"], rcpt)
	}
}

// TestL2_Webhook_UnknownHookIs404 proves a hookid absent from the agent's
// webhooks map answers 404 and dispatches nothing, even when the agent has
// other hooks configured (so the miss is the lookup, not an empty map).
// The follow-up sync call to a known hook is the synchronous observable for
// the non-event: once its turn has completed, the rejected payload must
// still be absent from cc-stub's recorder.
func TestL2_Webhook_UnknownHookIs404(t *testing.T) {
	testharness.ParallelWait(t)
	const rejectedMarker = "webhook-rejected-payload-1229"
	const knownMarker = "WEBHOOK-KNOWN-PROMPT-1229"

	h := startWebhookGateway(t, 12293,
		"\n[system.webhooks]\nknown = \"known.md\"\n",
		map[string]string{"prompts/known.md": knownMarker})
	c := webhookClient(h)

	for _, query := range []string{"", "?sync=true"} {
		code, body := postWebhook(t, c, "alpha", "nosuchhook", query, rejectedMarker)
		if code != http.StatusNotFound {
			t.Fatalf("unknown hook (query %q): status %d, want 404; body %q", query, code, body)
		}
		if !strings.Contains(body, `unknown webhook: "nosuchhook"`) {
			t.Errorf("unknown hook (query %q): body %q does not name the unknown hook", query, body)
		}
	}

	if code, body := postWebhook(t, c, "alpha", "known", "?sync=true", ""); code != http.StatusOK {
		t.Fatalf("known hook: status %d, want 200; body %q\nstderr:\n%s", code, body, stderrTail(h.Stderr()))
	}
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if strings.Contains(e.TextPrefix, rejectedMarker) {
			t.Fatalf("a 404'd webhook dispatched a turn: %+v", e)
		}
	}
}

// TestL2_Webhook_ConfigSetAddsHookLive proves the live-config path: a hook
// added at runtime via `/config set system.webhooks.<id>=<prompt>` (routed
// through matchMapField + liveApply's map-section applier) is served by the
// running gateway without a restart. The same hook 404s before the set, so
// the 200 after it can only come from the live edit.
func TestL2_Webhook_ConfigSetAddsHookLive(t *testing.T) {
	testharness.ParallelWait(t)
	const promptMarker = "WEBHOOK-LIVE-PROMPT-1229"

	h := startWebhookGateway(t, 12294,
		"\n[system.webhooks]\nexisting = \"existing.md\"\n",
		map[string]string{
			"prompts/existing.md": "existing hook prompt",
			"prompts/late.md":     promptMarker + ": a hook added live.",
		})
	c := webhookClient(h)

	if code, body := postWebhook(t, c, "alpha", "late", "?sync=true", ""); code != http.StatusNotFound {
		t.Fatalf("before /config set: status %d, want 404; body %q", code, body)
	}

	cmdBody, _ := json.Marshal(map[string]string{"agent": "alpha", "command": "/config set system.webhooks.late=late.md"})
	resp, err := c.Post("http://foci-gw/command", "application/json", bytes.NewReader(cmdBody))
	if err != nil {
		t.Fatalf("POST /command /config set: %v", err)
	}
	var cmdOut struct {
		Response string `json:"response"`
	}
	err = json.NewDecoder(resp.Body).Decode(&cmdOut)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/config set: status %d, decode err %v", resp.StatusCode, err)
	}
	if !strings.Contains(cmdOut.Response, "Applied live") {
		t.Fatalf("/config set did not apply live: %q\nstderr:\n%s", cmdOut.Response, stderrTail(h.Stderr()))
	}

	code, body := postWebhook(t, c, "alpha", "late", "?sync=true", "")
	if code != http.StatusOK {
		t.Fatalf("after /config set: status %d, want 200; body %q\nstderr:\n%s", code, body, stderrTail(h.Stderr()))
	}
	if got := decodeJSONMap(t, body)["response"]; !strings.Contains(got, promptMarker) {
		t.Errorf("live-added hook's reply does not carry its prompt %q: %q", promptMarker, got)
	}

	// The live edit must extend the map, not replace it.
	if code, body := postWebhook(t, c, "alpha", "existing", "?sync=true", ""); code != http.StatusOK {
		t.Errorf("pre-existing hook after /config set: status %d, want 200; body %q", code, body)
	}
}
