package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"foci/internal/provider"
)

// #2987: [endpoints.*] instruction_role picks the OpenAI message role that
// carries the composed system prompt. Ollama's Qwen renderer silently drops
// the "developer" role, so a local agent ran with no system prompt.

var roleTestSystem = []provider.SystemBlock{
	{Type: "text", Text: "CHARACTER block"},
	{Type: "text", Text: ""}, // empty blocks are skipped, as before
	{Type: "text", Text: "ENVIRONMENT block"},
	{Type: "text", Text: "SKILLS block: skill-marker-2987"},
}

const roleTestWantContent = "CHARACTER block\n\nENVIRONMENT block\n\nSKILLS block: skill-marker-2987"

var roleTestMsgs = []provider.Message{
	{Role: "user", Content: provider.TextContent("hello")},
	{Role: "assistant", Content: provider.TextContent("hi there")},
	{Role: "user", Content: provider.TextContent("again")},
}

type wireMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// wireMessages marshals messages the way the SDK puts them on the wire.
func wireMessages(t *testing.T, v any) []wireMsg {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out []wireMsg
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return out
}

// checkRoleMessages asserts one instruction message with wantRole carrying
// every system block, followed by the conversation, in order.
func checkRoleMessages(t *testing.T, got []wireMsg, wantRole string) {
	t.Helper()
	wantRoles := []string{wantRole, "user", "assistant", "user"}
	if len(got) != len(wantRoles) {
		t.Fatalf("messages = %d (%+v), want %d", len(got), got, len(wantRoles))
	}
	for i, r := range wantRoles {
		if got[i].Role != r {
			t.Errorf("messages[%d].role = %q, want %q", i, got[i].Role, r)
		}
	}
	if s, _ := got[0].Content.(string); s != roleTestWantContent {
		t.Errorf("instruction content = %#v, want %q", got[0].Content, roleTestWantContent)
	}
	for _, m := range got[1:] {
		if m.Role == "developer" || m.Role == "system" {
			t.Errorf("a second instruction message appeared: %+v", m)
		}
	}
}

func TestMessagesToOpenAI_InstructionRole(t *testing.T) {
	for _, tc := range []struct {
		role, want string
	}{
		{"", "developer"}, // unset = today's behaviour
		{"developer", "developer"},
		{"system", "system"},
	} {
		t.Run("role="+tc.role, func(t *testing.T) {
			got := wireMessages(t, messagesToOpenAI(roleTestSystem, roleTestMsgs, tc.role))
			checkRoleMessages(t, got, tc.want)
		})
	}
}

// roleBodyServer records each request's messages and answers either as a
// plain completion or as an SSE stream.
type roleBodyServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	msgs [][]wireMsg
}

func newRoleBodyServer(t *testing.T) *roleBodyServer {
	t.Helper()
	s := &roleBodyServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []wireMsg `json:"messages"`
			Stream   bool      `json:"stream"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("bad request body %s: %v", b, err)
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, body.Messages)
		s.mu.Unlock()
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range []string{
				`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
				`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			} {
				fmt.Fprintf(w, "%s\n\n", c)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *roleBodyServer) last(t *testing.T) []wireMsg {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		t.Fatal("server saw no request")
	}
	return s.msgs[len(s.msgs)-1]
}

// TestClient_InstructionRoleOnTheWire: both the non-streaming and streaming
// paths send the system prompt with the client's instruction role.
func TestClient_InstructionRoleOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		role, want string
	}{
		{"", "developer"},
		{"developer", "developer"},
		{"system", "system"},
	} {
		t.Run("role="+tc.role, func(t *testing.T) {
			srv := newRoleBodyServer(t)
			c := NewClient("k", WithBaseURL(srv.srv.URL), WithInstructionRole(tc.role))
			req := func() *provider.MessageRequest {
				return &provider.MessageRequest{Model: "m", MaxTokens: 10, System: roleTestSystem, Messages: roleTestMsgs}
			}

			if _, err := c.SendMessage(context.Background(), req()); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			checkRoleMessages(t, srv.last(t), tc.want)

			if _, err := c.StreamMessage(context.Background(), req(), &provider.StreamHandler{}); err != nil {
				t.Fatalf("StreamMessage: %v", err)
			}
			checkRoleMessages(t, srv.last(t), tc.want)
		})
	}
}
