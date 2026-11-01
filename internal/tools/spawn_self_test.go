package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/provider"
)

// TestAPICharacterSpawnOwnIdentity proves an API agent's character spawn calls
// whoami and send_to_session as itself, not as its spawner (#2676; whoami was
// blacklisted for exactly that, #2226). Its tool calls run on the spawner's
// context (clutch/c42), so without its own identity whoami reports the
// spawner's session key and model, and send_to_session sends from the
// spawner's session with the reply routed back into it.
//
// Required, as a delegated character batch (session <root>/b<ts>, indexed as
// a background task) does it:
//   - whoami: a key of its own under the spawner's root, the spawner's chat,
//     and the model the spawn runs on (not the spawner session's model);
//   - send_to_session: sent from that key, as a one-shot caller (the reply
//     goes to the target's own chat, nobody is marked waiting).
func TestAPICharacterSpawnOwnIdentity(t *testing.T) {
	t.Parallel()
	const parent = "clutch/c42"
	const spawnModel = "anthropic/claude-haiku-4-5"

	calls := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		calls++
		if calls == 1 {
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "whoami", Input: json.RawMessage(`{}`)},
					{Type: "tool_use", ID: "tu_2", Name: "send_to_session", Input: json.RawMessage(`{"session_key":"scout/c7","message":"hello from a spawn"}`)},
				},
				StopReason: "tool_use", Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: provider.TextContent("done"), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	whoami := NewWhoamiTool(WhoamiDeps{
		AgentID:         "clutch",
		PlatformForChat: func(string, int64) string { return "app" },
		SessionModel:    func(string) string { return "parent-session-model" },
	})
	whoamiOut := make(chan string, 1)
	inner := whoami.Execute
	whoami.Execute = func(ctx context.Context, p json.RawMessage) (ToolResult, error) {
		r, err := inner(ctx, p)
		whoamiOut <- r.Text
		return r, err
	}
	reg.Register(whoami)

	type delivery struct{ to, msg, replyTo string }
	delivered := make(chan delivery, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, _ string, _ *AgentOrigin) {
		delivered <- delivery{sk, msg, replyTo}
	})
	waited := make(chan string, 1)
	reg.Register(NewSendToSessionTool(&mockSessionAppender{}, notifier, nil, nil, func(caller, _ string) { waited <- caller }, nil))

	deps := SpawnDeps{
		Client: newTestAnthropicClient(server.URL, "test-token"), Registry: reg, AgentID: "clutch",
		FallbackModel: spawnModel, FallbackFormat: "anthropic", MaxToolLoops: func() int { return 5 },
	}
	params, _ := json.Marshal(map[string]string{"prompt": "who are you?", "context": "character"})
	if _, err := NewSpawner(deps, nil).SpawnTool().Execute(WithSessionKey(context.Background(), parent), params); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	rep := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(<-whoamiOut), "\n") {
		k, v, _ := strings.Cut(line, ": ")
		rep[k] = v
	}
	self := rep["session_key"]
	if self == parent || !strings.HasPrefix(self, parent+"/b") {
		t.Errorf("whoami session_key = %q, want the spawn's own key %s/b<ts>, not the spawner's", self, parent)
	}
	if rep["chat_id"] != "42" || rep["platform"] != "app" {
		t.Errorf("whoami chat_id/platform = %q/%q, want the spawner's chat 42/app", rep["chat_id"], rep["platform"])
	}
	if rep["model"] != spawnModel {
		t.Errorf("whoami model = %q, want the model the spawn runs on, %q", rep["model"], spawnModel)
	}

	d := <-delivered
	if d.to != "scout/c7" {
		t.Errorf("send_to_session delivered to %q, want scout/c7", d.to)
	}
	if !strings.Contains(d.msg, "MESSAGE FROM SESSION "+self) {
		t.Errorf("send_to_session message is not from the spawn's own key %q:\n%s", self, d.msg)
	}
	if d.replyTo != "" {
		t.Errorf("send_to_session replyTo = %q, want \"\" (a one-shot's reply goes to the target's own chat)", d.replyTo)
	}
	select {
	case c := <-waited:
		t.Errorf("onWait fired for %q: nobody waits on a one-shot's reply", c)
	default:
	}
}

// TestSpawnSelfOnlyOnItsContext proves whoami and send_to_session keep using
// the context's session key everywhere else: no spawn identity, no change.
func TestSpawnSelfOnlyOnItsContext(t *testing.T) {
	t.Parallel()
	rep := resolveWhoami(WithSessionKey(context.Background(), "clutch/c42"), WhoamiDeps{AgentID: "clutch", SessionModel: func(string) string { return "m" }})
	if rep.sessionKey != "clutch/c42" || rep.model != "m" {
		t.Errorf("whoami outside a spawn = %+v, want clutch/c42 on m", rep)
	}
}
