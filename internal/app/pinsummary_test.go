package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/fap"
)

// openRouterStub is a chat-completions endpoint that records each request and
// answers with reply (or status, when non-200).
type openRouterStub struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	reply  string
	status int
}

func (s *openRouterStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("stub: bad request body %q: %v", raw, err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		status, reply := s.status, s.reply
		s.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			http.Error(w, `{"error":{"message":"nope"}}`, status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "gen-1",
			"model": "meta-llama/llama-3.1-8b-instruct",
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 8},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *openRouterStub) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

func testSummariser(url string, app *config.AppSpecific) *pinSummariser {
	s := newPinSummariser(app, func() (string, bool) { return "sk-test", true })
	s.url = url
	return s
}

// TestPinSummary_Request pins what goes to OpenRouter by default: the Llama
// 3.1 8B model pinned to Groq with fallbacks allowed, a system prompt with the
// strict ten-word limit, the key as a bearer token, and a long message cut to
// its start.
func TestPinSummary_Request(t *testing.T) {
	stub := &openRouterStub{reply: "Flight times for the Athens trip"}
	srv := stub.serve(t)
	s := testSummariser(srv.URL, nil)

	long := strings.Repeat("é", pinSummaryMaxInputRunes+500)
	got, err := s.summarise(context.Background(), long, "agent:clutch:app:1", "clutch")
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if got != "Flight times for the Athens trip" {
		t.Errorf("summary = %q", got)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	body := reqs[0]
	if body["model"] != "meta-llama/llama-3.1-8b-instruct" {
		t.Errorf("model = %v, want the Llama 3.1 8B default", body["model"])
	}
	prov, _ := body["provider"].(map[string]any)
	if order, _ := prov["order"].([]any); len(order) != 1 || order[0] != "groq" || prov["allow_fallbacks"] != true {
		t.Errorf("provider = %v, want order [groq] with fallbacks", body["provider"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want system + user", body["messages"])
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "TEN WORDS") {
		t.Errorf("system message = %v, want the strict ten-word limit", sys)
	}
	user := msgs[1].(map[string]any)["content"].(string)
	if n := len([]rune(user)); n != pinSummaryMaxInputRunes {
		t.Errorf("user content = %d runes, want truncated to %d", n, pinSummaryMaxInputRunes)
	}
	if stub.auth[0] != "Bearer sk-test" {
		t.Errorf("Authorization = %q", stub.auth[0])
	}
}

// TestPinSummary_LiveConfigure proves a config change applies to the next
// request without a new summariser, "any" drops the provider preference, and
// "off" stops the call entirely.
func TestPinSummary_LiveConfigure(t *testing.T) {
	stub := &openRouterStub{reply: "x"}
	srv := stub.serve(t)
	s := testSummariser(srv.URL, nil)

	s.configure(&config.AppSpecific{PinSummaryModel: "meta-llama/llama-3.3-70b-instruct", PinSummaryProvider: "any"})
	if _, err := s.summarise(context.Background(), "hi", "", ""); err != nil {
		t.Fatalf("summarise: %v", err)
	}
	s.configure(&config.AppSpecific{PinSummaryProvider: "cerebras, groq"})
	if _, err := s.summarise(context.Background(), "hi", "", ""); err != nil {
		t.Fatalf("summarise: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0]["model"] != "meta-llama/llama-3.3-70b-instruct" {
		t.Errorf("first model = %v, want the configured one", reqs[0]["model"])
	}
	if _, has := reqs[0]["provider"]; has {
		t.Errorf("provider=any still sent a preference: %v", reqs[0]["provider"])
	}
	if reqs[1]["model"] != defaultPinSummaryModel {
		t.Errorf("second model = %v, want the default back", reqs[1]["model"])
	}
	order, _ := reqs[1]["provider"].(map[string]any)["order"].([]any)
	if len(order) != 2 || order[0] != "cerebras" || order[1] != "groq" {
		t.Errorf("order = %v, want [cerebras groq]", order)
	}

	s.configure(&config.AppSpecific{PinSummaryModel: "OFF"})
	got, err := s.summarise(context.Background(), "hi", "", "")
	if got != "" || err != nil {
		t.Errorf("off: summary=%q err=%v, want empty and nil", got, err)
	}
	if n := len(stub.requests()); n != 2 {
		t.Errorf("off still called the model: %d requests", n)
	}
}

// TestPinSummary_Failures: no key, an HTTP error and an empty answer all
// return an error, so the handler answers with an empty summary.
func TestPinSummary_Failures(t *testing.T) {
	stub := &openRouterStub{status: http.StatusInternalServerError}
	srv := stub.serve(t)

	noKey := newPinSummariser(nil, func() (string, bool) { return "", false })
	noKey.url = srv.URL
	if _, err := noKey.summarise(context.Background(), "hi", "", ""); err == nil {
		t.Error("no key: want an error")
	}
	if n := len(stub.requests()); n != 0 {
		t.Errorf("no key still called the model: %d requests", n)
	}

	s := testSummariser(srv.URL, nil)
	if _, err := s.summarise(context.Background(), "hi", "", ""); err == nil {
		t.Error("HTTP 500: want an error")
	}
	stub.mu.Lock()
	stub.status, stub.reply = 0, "  \"\"  "
	stub.mu.Unlock()
	if _, err := s.summarise(context.Background(), "hi", "", ""); err == nil {
		t.Error("empty answer: want an error")
	}
	if _, err := s.summarise(context.Background(), "   ", "", ""); err == nil {
		t.Error("blank text: want an error")
	}
}

func TestCleanPinSummary(t *testing.T) {
	for in, want := range map[string]string{
		"Flight times for Athens":          "Flight times for Athens",
		"  \"Flight times for Athens.\"  ": "Flight times for Athens",
		"“Flight times”.":                  "Flight times",
		"'Quoted'":                         "Quoted",
		"Two\nlines here.":                 "Two lines here",
		"Ends with dots...":                "Ends with dots...",
		"Keeps \"inner\" quotes":           "Keeps \"inner\" quotes",
		"":                                 "",
	} {
		if got := cleanPinSummary(in); got != want {
			t.Errorf("cleanPinSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHandlePinSummarize: one pin.summary answers each pin.summarize on the
// asking socket, carrying the request id and the summary — or an empty
// summary when the call fails or the hub has no summariser.
func TestHandlePinSummarize(t *testing.T) {
	stub := &openRouterStub{reply: "Pinned thing."}
	srv := stub.serve(t)
	h := newTestHub()
	h.pinSum = testSummariser(srv.URL, nil)

	c := fakeClient()
	h.handlePinSummarize(c, fap.PinSummarize{RequestID: "r1", ConversationID: "c1", MessageID: "m1", Text: "a message"})
	f := awaitPinSummary(t, c)
	if f["requestId"] != "r1" || f["messageId"] != "m1" || f["summary"] != "Pinned thing" {
		t.Errorf("answer = %v, want r1/m1/Pinned thing", f)
	}

	stub.mu.Lock()
	stub.status = http.StatusBadGateway
	stub.mu.Unlock()
	h.handlePinSummarize(c, fap.PinSummarize{RequestID: "r2", MessageID: "m1", Text: "a message"})
	if f := awaitPinSummary(t, c); f["requestId"] != "r2" || f["summary"] != nil {
		t.Errorf("failed answer = %v, want r2 with no summary", f)
	}

	bare := newTestHub()
	// bare has no summariser (newTestHub sets none).
	bare.handlePinSummarize(c, fap.PinSummarize{RequestID: "r3", MessageID: "m1", Text: "x"})
	if f := awaitPinSummary(t, c); f["requestId"] != "r3" || f["summary"] != nil {
		t.Errorf("no-summariser answer = %v, want r3 with no summary", f)
	}
}

// awaitPinSummary waits for the next frame on c, which must be a pin.summary.
func awaitPinSummary(t *testing.T, c *wsClient) map[string]any {
	t.Helper()
	select {
	case b := <-c.send:
		var env struct {
			T string         `json:"t"`
			D map[string]any `json:"d"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("bad frame: %v", err)
		}
		if env.T != fap.TypePinSummary {
			t.Fatalf("frame type = %q, want %q", env.T, fap.TypePinSummary)
		}
		return env.D
	case <-time.After(10 * time.Second):
		t.Fatal("no pin.summary within 10s")
		return nil
	}
}
