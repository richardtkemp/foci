package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// whoamiTestDeps returns deps wired like production: agent "clutch" on the
// delegated "claude-code" backend, chat 123 registered on telegram, a model
// lookup that echoes its session key (so each row proves the lookup used the
// key on the context, not a value captured at construction).
func whoamiTestDeps() WhoamiDeps {
	return WhoamiDeps{
		AgentID:   "clutch",
		Backend:   "claude-code",
		Delegated: true,
		PlatformForChat: func(agentID string, chatID int64) string {
			if agentID == "clutch" && chatID == 123 {
				return "telegram"
			}
			return ""
		},
		SessionModel: func(sessionKey string) string { return "m:" + sessionKey },
	}
}

// callWhoami runs the tool with the given session key on the context and
// returns its text output, failing the test on any error.
func callWhoami(t *testing.T, d WhoamiDeps, sessionKey string) string {
	t.Helper()
	ctx := context.Background()
	if sessionKey != "" {
		ctx = WithSessionKey(ctx, sessionKey)
	}
	res, err := NewWhoamiTool(d).Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("whoami(%q): unexpected error: %v", sessionKey, err)
	}
	return res.Text
}

// TestWhoamiToolShape pins the tool's declared shape: exported to the exec
// bridge, an object schema with no required properties (a bare foci_whoami
// must validate), a documented --json output, and a one-sentence read-only
// description naming every field it returns.
func TestWhoamiToolShape(t *testing.T) {
	t.Parallel()

	tool := NewWhoamiTool(whoamiTestDeps())
	if !tool.ExecExport {
		t.Error("whoami must set ExecExport (foci_whoami shell function)")
	}
	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
		t.Fatalf("parameters are not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("parameters type = %q, want object", schema.Type)
	}
	if len(schema.Required) != 0 {
		t.Errorf("required = %v, want none", schema.Required)
	}
	if len(schema.Properties) != 0 {
		t.Errorf("properties = %v, want none", schema.Properties)
	}
	if tool.JSONOutput == "" {
		t.Error("JSONOutput must document the --json shape")
	}
	desc := strings.ToLower(tool.Description)
	for _, want := range []string{"read-only", "agent id", "session key", "chat id", "platform", "backend", "transport", "model"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description should mention %q: %q", want, tool.Description)
		}
	}
}

// TestWhoamiTextOutput pins the exact text report per session-key shape —
// chat, branch, spawn-under-chat, independent, malformed and absent — proving
// field order, values, the none/unknown markers, and (by the exact-string
// equality) that nothing beyond the seven fields is ever returned.
func TestWhoamiTextOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string // "" = no session key on the context
		want string
	}{
		{
			name: "chat session",
			key:  "clutch/c123",
			want: "agent_id: clutch\n" +
				"session_key: clutch/c123\n" +
				"chat_id: 123\n" +
				"platform: telegram\n" +
				"backend: claude-code\n" +
				"transport: delegated\n" +
				"model: m:clutch/c123\n",
		},
		{
			name: "branch of a chat",
			key:  "clutch/c123/b1709596800",
			want: "agent_id: clutch\n" +
				"session_key: clutch/c123/b1709596800\n" +
				"chat_id: 123\n" +
				"platform: telegram\n" +
				"backend: claude-code\n" +
				"transport: delegated\n" +
				"model: m:clutch/c123/b1709596800\n",
		},
		{
			name: "independent spawn under a chat",
			key:  "clutch/c123/i1709596801",
			want: "agent_id: clutch\n" +
				"session_key: clutch/c123/i1709596801\n" +
				"chat_id: 123\n" +
				"platform: telegram\n" +
				"backend: claude-code\n" +
				"transport: delegated\n" +
				"model: m:clutch/c123/i1709596801\n",
		},
		{
			name: "independent session",
			key:  "clutch/iresearch",
			want: "agent_id: clutch\n" +
				"session_key: clutch/iresearch\n" +
				"chat_id: none\n" +
				"platform: unknown\n" +
				"backend: claude-code\n" +
				"transport: delegated\n" +
				"model: m:clutch/iresearch\n",
		},
		{
			name: "malformed key is echoed, not guessed from",
			key:  "not-a-session-key",
			want: "agent_id: clutch\n" +
				"session_key: not-a-session-key\n" +
				"chat_id: none\n" +
				"platform: unknown\n" +
				"backend: claude-code\n" +
				"transport: delegated\n" +
				"model: m:not-a-session-key\n",
		},
		{
			name: "no session key on context",
			key:  "",
			want: "agent_id: clutch\n" +
				"session_key: none\n" +
				"chat_id: none\n" +
				"platform: unknown\n" +
				"backend: claude-code\n" +
				"transport: delegated\n" +
				"model: unknown\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := callWhoami(t, whoamiTestDeps(), tt.key); got != tt.want {
				t.Errorf("whoami(%q) =\n%s\nwant\n%s", tt.key, got, tt.want)
			}
		})
	}
}

// TestWhoamiBranchSession pins requirement 4 specifically: a branch or spawn
// key under a chat reports the FULL key as session_key while chat_id and
// platform are those of the parent chat.
func TestWhoamiBranchSession(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"clutch/c123/b1709596800", "clutch/c123/i1709596801"} {
		out := callWhoami(t, whoamiTestDeps(), key)
		for _, want := range []string{
			"session_key: " + key + "\n",
			"chat_id: 123\n",
			"platform: telegram\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("whoami(%q) missing %q in output:\n%s", key, want, out)
			}
		}
	}
}

// TestWhoamiMissingValues proves a missing fact is reported explicitly and
// never turns into an error or a substituted guess: an unregistered platform,
// nil lookup funcs, and no session key all still succeed; and the platform
// lookup is never even attempted for a session without a chat id.
func TestWhoamiMissingValues(t *testing.T) {
	t.Parallel()

	t.Run("platform not in index", func(t *testing.T) {
		t.Parallel()
		out := callWhoami(t, whoamiTestDeps(), "clutch/c999")
		if !strings.Contains(out, "chat_id: 999\nplatform: unknown\n") {
			t.Errorf("unknown platform should report unknown, got:\n%s", out)
		}
	})

	t.Run("nil platform lookup", func(t *testing.T) {
		t.Parallel()
		d := whoamiTestDeps()
		d.PlatformForChat = nil
		if out := callWhoami(t, d, "clutch/c123"); !strings.Contains(out, "platform: unknown\n") {
			t.Errorf("nil PlatformForChat should report unknown, got:\n%s", out)
		}
	})

	t.Run("nil model lookup", func(t *testing.T) {
		t.Parallel()
		d := whoamiTestDeps()
		d.SessionModel = nil
		if out := callWhoami(t, d, "clutch/c123"); !strings.Contains(out, "model: unknown\n") {
			t.Errorf("nil SessionModel should report unknown, got:\n%s", out)
		}
	})

	t.Run("model lookup returning empty", func(t *testing.T) {
		t.Parallel()
		d := whoamiTestDeps()
		d.SessionModel = func(string) string { return "" }
		if out := callWhoami(t, d, "clutch/c123"); !strings.Contains(out, "model: unknown\n") {
			t.Errorf("empty SessionModel result should report unknown, got:\n%s", out)
		}
	})

	t.Run("platform lookup never called without a chat", func(t *testing.T) {
		t.Parallel()
		d := whoamiTestDeps()
		d.PlatformForChat = func(agentID string, chatID int64) string {
			t.Errorf("PlatformForChat called with (%q, %d); must not be called for a session with no chat id", agentID, chatID)
			return "telegram"
		}
		if out := callWhoami(t, d, "clutch/iresearch"); !strings.Contains(out, "platform: unknown\n") {
			t.Errorf("independent session should report unknown platform, got:\n%s", out)
		}
	})
}

// TestWhoamiIgnoresExtraParams pins that the no-property schema is the whole
// contract: a call carrying unexpected parameters is not an error and cannot
// change the report — identity comes from the context and the injected
// lookups, never from arguments (a forged session_key param is ignored too).
func TestWhoamiIgnoresExtraParams(t *testing.T) {
	t.Parallel()

	ctx := WithSessionKey(context.Background(), "clutch/c123")
	tool := NewWhoamiTool(whoamiTestDeps())
	bare, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("bare call: unexpected error: %v", err)
	}
	extra, err := tool.Execute(ctx, json.RawMessage(`{"bogus": 1, "session_key": "clutch/c999"}`))
	if err != nil {
		t.Fatalf("call with unexpected parameters: unexpected error: %v", err)
	}
	if extra.Text != bare.Text {
		t.Errorf("unexpected parameters must not change the report:\nbare:\n%s\nextra:\n%s", bare.Text, extra.Text)
	}
	if !strings.Contains(bare.Text, "session_key: clutch/c123\n") {
		t.Errorf("report should come from the context, got:\n%s", bare.Text)
	}
}

// TestWhoamiBackendTransport pins the backend/transport pair: an empty
// configured backend is reported as api, and transport is delegated exactly
// when the injected IsDelegated flag is true.
func TestWhoamiBackendTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		backend           string
		delegated         bool
		wantBackendLine   string
		wantTransportLine string
	}{
		{"api default", "", false, "backend: api\n", "transport: api\n"},
		{"delegated backend", "claude-code", true, "backend: claude-code\n", "transport: delegated\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := whoamiTestDeps()
			d.Backend = tt.backend
			d.Delegated = tt.delegated
			out := callWhoami(t, d, "clutch/c123")
			if !strings.Contains(out, tt.wantBackendLine) || !strings.Contains(out, tt.wantTransportLine) {
				t.Errorf("backend=%q delegated=%v: want %q and %q, got:\n%s", tt.backend, tt.delegated, tt.wantBackendLine, tt.wantTransportLine, out)
			}
		})
	}
}

// TestWhoamiModelReadPerCall proves the model is looked up per call, not
// captured at construction: a /model switch between two calls is reflected in
// the second report.
func TestWhoamiModelReadPerCall(t *testing.T) {
	t.Parallel()

	current := "old-model"
	d := whoamiTestDeps()
	d.SessionModel = func(string) string { return current }
	tool := NewWhoamiTool(d)
	ctx := WithSessionKey(context.Background(), "clutch/c123")

	first, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	current = "new-model"
	second, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !strings.Contains(first.Text, "model: old-model\n") || !strings.Contains(second.Text, "model: new-model\n") {
		t.Errorf("model should be read per call; first:\n%s\nsecond:\n%s", first.Text, second.Text)
	}
}

// TestWhoamiJSONOutput pins the --json shape: an object with exactly the
// seven report keys (chat_id a number, or null when there is no chat) and
// null for the values the text form marks none/unknown.
func TestWhoamiJSONOutput(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, d WhoamiDeps, sessionKey string) map[string]json.RawMessage {
		t.Helper()
		ctx := context.Background()
		if sessionKey != "" {
			ctx = WithSessionKey(ctx, sessionKey)
		}
		res, err := NewWhoamiTool(d).Execute(WithOutputHints(ctx, OutputHints{Format: "json"}), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("whoami(%q) json: %v", sessionKey, err)
		}
		if !res.JSON {
			t.Fatalf("whoami(%q) json: result.JSON = false, want true", sessionKey)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(res.Text), &obj); err != nil {
			t.Fatalf("whoami(%q) json output does not parse: %v\n%s", sessionKey, err, res.Text)
		}
		return obj
	}

	wantKeys := []string{"agent_id", "session_key", "chat_id", "platform", "backend", "transport", "model"}

	t.Run("chat session, all fields present", func(t *testing.T) {
		t.Parallel()
		obj := run(t, whoamiTestDeps(), "clutch/c123")
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := append([]string{}, wantKeys...)
		sort.Strings(want)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Errorf("json keys = %v, want exactly %v", keys, want)
		}
		for field, want := range map[string]string{
			"agent_id":    `"clutch"`,
			"session_key": `"clutch/c123"`,
			"chat_id":     `123`,
			"platform":    `"telegram"`,
			"backend":     `"claude-code"`,
			"transport":   `"delegated"`,
			"model":       `"m:clutch/c123"`,
		} {
			if got := string(obj[field]); got != want {
				t.Errorf("json %s = %s, want %s", field, got, want)
			}
		}
	})

	t.Run("independent session: null chat and platform", func(t *testing.T) {
		t.Parallel()
		obj := run(t, whoamiTestDeps(), "clutch/iresearch")
		if got := string(obj["chat_id"]); got != "null" {
			t.Errorf("json chat_id = %s, want null", got)
		}
		if got := string(obj["platform"]); got != "null" {
			// no chat id → no platform lookup → null
			t.Errorf("json platform = %s, want null", got)
		}
	})

	t.Run("no session key: nulls, no error", func(t *testing.T) {
		t.Parallel()
		obj := run(t, whoamiTestDeps(), "")
		for _, field := range []string{"session_key", "chat_id", "platform", "model"} {
			if got := string(obj[field]); got != "null" {
				t.Errorf("json %s = %s, want null", field, got)
			}
		}
	})
}
