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
	"foci/internal/fastload"
)

// thinkingStub is an OpenAI-format endpoint serving a thinking model the way
// Ollama serves qwen3.8 (#3026): a request without reasoning_effort "none"
// spends its whole max_tokens on reasoning and returns empty content with
// finish_reason "length"; with it, the model answers at once. rejectField
// makes it refuse any request that carries reasoning_effort with HTTP 400,
// as an endpoint that does not know the field does.
type thinkingStub struct {
	mu          sync.Mutex
	bodies      []map[string]any
	rejectField bool
	reply       string
}

func (s *thinkingStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("stub: bad request body %q: %v", raw, err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		reject, reply := s.rejectField, s.reply
		s.mu.Unlock()
		effort, has := body["reasoning_effort"]
		if has && reject {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`)
			return
		}
		maxTok, _ := body["max_tokens"].(float64)
		msg := map[string]any{"role": "assistant", "content": reply}
		finish, used := "stop", 6
		if effort != "none" {
			msg = map[string]any{"role": "assistant", "content": "", "reasoning": "The user wants a short label, so I should think about"}
			finish, used = "length", int(maxTok)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "qwen3.8:27b-mlx",
			"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 40, "completion_tokens": used},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *thinkingStub) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// thinkingSummariser points every fast-summary task at a local endpoint on
// url, as the work Mac's config does.
func thinkingSummariser(url string) *pinSummariser {
	app := &config.AppSpecific{PinSummaryModel: "qwen3.8:27b-mlx", PinSummaryEndpoint: "local"}
	s := newPinSummariser(app, map[string]config.EndpointConfig{
		"local": {Format: "openai", URL: url + "/v1"},
	}, nil)
	s.load = fastload.New()
	return s
}

// TestFastSummary_ThinkingModelAnswers (#3026): every fast-summary task (pin
// label, Stop-view summary, chat name, chat sections) gets an answer from a
// thinking model on an OpenAI-format endpoint, because the request asks it
// not to reason.
func TestFastSummary_ThinkingModelAnswers(t *testing.T) {
	stub := &thinkingStub{reply: "Voice relay deployed on NUC"}
	s := thinkingSummariser(stub.serve(t).URL)

	asks := []summaryAsk{
		{what: "pin summary", system: pinSummaryPrompt, maxInputRunes: pinSummaryMaxInputRunes, maxTokens: pinSummaryMaxTokens, maxWords: pinSummaryMaxWords},
		subagentSummaryAsk,
		{what: autoNameWhat, use: useAutoName, system: autoNameSystemPrompt, maxInputRunes: 1000, maxTokens: autoNameMaxTokens, maxWords: 6},
		{what: sectionsWhat, use: useSections, system: sectionSystemPrompt, maxInputRunes: 1000, maxTokens: sectionMaxTokens, raw: true},
	}
	for _, a := range asks {
		got, err := s.ask(context.Background(), a, "We deployed the voice relay on the NUC.", "", "")
		if err != nil || got != "Voice relay deployed on NUC" {
			t.Errorf("%s: answer = %q, err = %v; want the model's label", a.what, got, err)
		}
	}
	for i, r := range stub.requests() {
		if r["reasoning_effort"] != "none" {
			t.Errorf("request %d: reasoning_effort = %v, want \"none\"", i, r["reasoning_effort"])
		}
	}
}

// TestFastSummary_ReasoningFieldRejected (#3026): an endpoint that refuses
// reasoning_effort still gets the call, once more without the field, and
// later calls to it leave the field out.
func TestFastSummary_ReasoningFieldRejected(t *testing.T) {
	stub := &thinkingStub{reply: "A label", rejectField: true}
	srv := stub.serve(t)
	app := &config.AppSpecific{PinSummaryModel: "llama", PinSummaryEndpoint: "plain"}
	s := newPinSummariser(app, map[string]config.EndpointConfig{
		"plain": {Format: "openai", URL: srv.URL + "/v1"},
	}, nil)
	s.load = fastload.New()

	// The stub answers a request without the field as a thinking model, so
	// the answer is empty: what matters here is the request sequence.
	_, _ = s.summarise(context.Background(), "hi", "", "")
	_, _ = s.summarise(context.Background(), "hi", "", "")
	reqs := stub.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3 (field, retry without, then without)", len(reqs))
	}
	if _, has := reqs[0]["reasoning_effort"]; !has {
		t.Error("first request has no reasoning_effort")
	}
	for i := 1; i < 3; i++ {
		if _, has := reqs[i]["reasoning_effort"]; has {
			t.Errorf("request %d still sends reasoning_effort after the endpoint refused it", i)
		}
	}
	if l := s.load.Loads(time.Hour); len(l) != 1 || l[0].Runs != 2 {
		t.Errorf("load = %+v, want one target with 2 runs (a retry is part of its call)", l)
	}

	// A config edit forgets the refusal: the endpoint may now be another
	// server.
	s.configure(app, map[string]config.EndpointConfig{"plain": {Format: "openai", URL: srv.URL + "/v1"}})
	_, _ = s.summarise(context.Background(), "hi", "", "")
	if reqs = stub.requests(); len(reqs) != 5 {
		t.Errorf("requests after the edit = %d, want 5 (the field tried again)", len(reqs))
	}
}

// TestFastSummary_OtherHTTP400NotRetried: a 400 that does not name the field
// is an ordinary failure, with no retry.
func TestFastSummary_OtherHTTP400NotRetried(t *testing.T) {
	stub := &openRouterStub{status: http.StatusBadRequest}
	srv := stub.serve(t)
	s := testSummariser(srv.URL, nil)
	if _, err := s.summarise(context.Background(), "hi", "", ""); err == nil {
		t.Fatal("HTTP 400: want an error")
	}
	if n := len(stub.requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (no retry)", n)
	}
}

// TestFastSummary_EmptyAtTokenCapNamesCause (#3026): an answer that is empty
// because the model hit max_tokens while reasoning says so, not just "empty
// summary".
func TestFastSummary_EmptyAtTokenCapNamesCause(t *testing.T) {
	stub := &thinkingStub{reply: "x", rejectField: true}
	s := thinkingSummariser(stub.serve(t).URL)
	for _, a := range []summaryAsk{
		{what: "pin summary", maxInputRunes: 100, maxTokens: 40},
		{what: "chat sections", use: useSections, maxInputRunes: 100, maxTokens: 40, raw: true},
	} {
		_, err := s.ask(context.Background(), a, "hi", "", "")
		if err == nil {
			t.Fatalf("%s: want an error", a.what)
		}
		for _, want := range []string{"finish_reason=length", "40", "reasoning"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %q, want it to contain %q", a.what, err, want)
			}
		}
	}
}
