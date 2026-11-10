package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/epdispatch"
	"foci/internal/fap"
	"foci/internal/fastload"
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
	s := newPinSummariser(app, nil, func(string) (string, bool) { return "sk-test", true })
	s.openRouterURL = url
	s.load = fastload.New()
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
	if body["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v, want \"none\" (#3026; OpenRouter ignores it for Llama)", body["reasoning_effort"])
	}
}

// TestPinSummary_LiveConfigure proves a config change applies to the next
// request without a new summariser, "any" drops the provider preference, and
// "off" stops the call entirely.
func TestPinSummary_LiveConfigure(t *testing.T) {
	stub := &openRouterStub{reply: "x"}
	srv := stub.serve(t)
	s := testSummariser(srv.URL, nil)

	s.configure(&config.AppSpecific{PinSummaryModel: "meta-llama/llama-3.3-70b-instruct", PinSummaryProvider: "any"}, nil)
	if _, err := s.summarise(context.Background(), "hi", "", ""); err != nil {
		t.Fatalf("summarise: %v", err)
	}
	s.configure(&config.AppSpecific{PinSummaryProvider: "cerebras, groq"}, nil)
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

	s.configure(&config.AppSpecific{PinSummaryModel: "OFF"}, nil)
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

	noKey := newPinSummariser(nil, nil, func(string) (string, bool) { return "", false })
	noKey.openRouterURL = srv.URL
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

// TestFastSummary_PerTaskEndpoint (#3012): the chat name and the sections can
// go to a model of their own on another [endpoints] entry (a local
// OpenAI-format server) while the pin label stays on OpenRouter. The local
// call carries the model id as written, no OpenRouter provider preference and
// no key when the endpoint names none; each call is recorded per target in
// the load figures; and an [endpoints] edit moves the target live.
func TestFastSummary_PerTaskEndpoint(t *testing.T) {
	remote := &openRouterStub{reply: "Remote label"}
	remoteSrv := remote.serve(t)
	local := &openRouterStub{reply: "Local name"}
	localSrv := local.serve(t)

	app := &config.AppSpecific{
		AutoNameModel: "qwen3.6:35b-a3b", AutoNameEndpoint: "local",
		ChatSectionsModel: "qwen3.6:35b-a3b", ChatSectionsEndpoint: "local",
	}
	endpoints := map[string]config.EndpointConfig{
		"local": {Format: "openai", URL: localSrv.URL + "/v1/", HTTPTimeout: "10m"},
	}
	s := newPinSummariser(app, endpoints, func(name string) (string, bool) {
		if name == pinSummarySecret {
			return "sk-or", true
		}
		return "", false
	})
	s.openRouterURL = remoteSrv.URL
	s.load = fastload.New()
	for _, u := range []summaryUse{useShared, useAutoName, useSections} {
		if !s.usable(u) {
			t.Fatalf("use %d not usable", u)
		}
	}

	if got, err := s.summarise(context.Background(), "pin this", "", ""); err != nil || got != "Remote label" {
		t.Fatalf("pin summary = %q, %v", got, err)
	}
	name := summaryAsk{what: "auto chat name", use: useAutoName, system: "s", maxInputRunes: 100, maxTokens: 10, timeout: time.Second}
	if got, err := s.ask(context.Background(), name, "the chat", "", ""); err != nil || got != "Local name" {
		t.Fatalf("auto name = %q, %v", got, err)
	}
	sec := summaryAsk{what: "chat sections", use: useSections, system: "s", maxInputRunes: 100, maxTokens: 10, raw: true}
	if _, err := s.ask(context.Background(), sec, "the chat", "", ""); err != nil {
		t.Fatalf("sections: %v", err)
	}

	if n := len(remote.requests()); n != 1 {
		t.Errorf("OpenRouter requests = %d, want 1 (the pin label)", n)
	}
	lreqs := local.requests()
	if len(lreqs) != 2 {
		t.Fatalf("local requests = %d, want 2", len(lreqs))
	}
	for _, r := range lreqs {
		if r["model"] != "qwen3.6:35b-a3b" {
			t.Errorf("local model = %v, want the id as written", r["model"])
		}
		if _, has := r["provider"]; has {
			t.Errorf("local request carries an OpenRouter provider preference: %v", r["provider"])
		}
	}
	local.mu.Lock()
	auth := append([]string(nil), local.auth...)
	local.mu.Unlock()
	for _, a := range auth {
		if a != "" {
			t.Errorf("local Authorization = %q, want none (the endpoint names no key)", a)
		}
	}
	if tgt := s.target(useAutoName); tgt.timeout != 10*time.Minute {
		t.Errorf("local timeout = %v, want the endpoint's http_timeout", tgt.timeout)
	}

	loads := map[string]fastload.Load{}
	for _, l := range s.load.Loads(time.Hour) {
		loads[l.Target] = l
	}
	lt := loads[fastload.Target("qwen3.6:35b-a3b", "local")]
	if lt.Runs != 2 || lt.ByTask["auto chat name"] != 1 || lt.ByTask["chat sections"] != 1 {
		t.Errorf("local load = %+v, want 2 runs (1 name, 1 sections)", lt)
	}
	if rt := loads[fastload.Target(defaultPinSummaryModel, "openrouter")]; rt.Runs != 1 {
		t.Errorf("openrouter load = %+v, want 1 run", rt)
	}

	// The endpoint is edited to need a key that is not set: the tasks on it
	// stop, the pin label does not.
	endpoints["local"] = config.EndpointConfig{Format: "openai", URL: localSrv.URL + "/v1", APIKey: "local.api_key"}
	s.configure(app, endpoints)
	if s.usable(useAutoName) || s.usable(useSections) {
		t.Error("local tasks still usable with their key missing")
	}
	if !s.usable(useShared) {
		t.Error("pin label stopped by an edit to another endpoint")
	}
	if p := s.problems(); len(p) != 1 || !strings.Contains(p[0], "local.api_key") {
		t.Errorf("problems = %v, want one naming local.api_key", p)
	}

	// An endpoint that does not speak the OpenAI format is refused.
	endpoints["local"] = config.EndpointConfig{Format: "anthropic", URL: localSrv.URL}
	s.configure(app, endpoints)
	if _, err := s.ask(context.Background(), name, "the chat", "", ""); err == nil || !strings.Contains(err.Error(), "OpenAI") {
		t.Errorf("anthropic-format endpoint: err = %v, want the format refusal", err)
	}
	if n := len(local.requests()); n != 2 {
		t.Errorf("local requests = %d after the refusals, want still 2", n)
	}
}

// TestFastSummary_TargetTimeout: a call to a target with an endpoint timeout
// of its own is bounded by it, not the task's, and the failure is recorded.
func TestFastSummary_TargetTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	app := &config.AppSpecific{AutoNameModel: "m", AutoNameEndpoint: "local"}
	s := newPinSummariser(app, map[string]config.EndpointConfig{
		"local": {Format: "openai", URL: srv.URL, HTTPTimeout: "50ms"},
	}, nil)
	s.load = fastload.New()
	ask := summaryAsk{what: "auto chat name", use: useAutoName, system: "s", maxInputRunes: 10, maxTokens: 5, timeout: time.Hour}
	guard, cancel := context.WithTimeout(context.Background(), 30*time.Second) // hang guard only
	defer cancel()
	_, err := s.ask(guard, ask, "x", "", "")
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if guard.Err() != nil {
		t.Fatal("the hang guard fired: the endpoint's timeout did not bound the call")
	}
	l := s.load.Loads(time.Hour)
	if len(l) != 1 || l[0].Failed != 1 || l[0].InFlight != 0 {
		t.Errorf("load = %+v, want one failed call, none in flight", l)
	}
}

// TestFastSummary_DispatchTiers (#3019): on a local endpoint the chat name
// (Medium) waits while the pin label (High) is in flight, a newer check of
// the same chat replaces a waiting one (by a newer call, and by
// replaceWaiting), and the holds reach the load figures.
func TestFastSummary_DispatchTiers(t *testing.T) {
	ep := fmt.Sprintf("dispatch-test-%d", time.Now().UnixNano()) // epdispatch is process-wide
	unblock := make(chan struct{})
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			<-unblock
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"content": "A short answer"}}},
		})
	}))
	t.Cleanup(srv.Close)
	var unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(unblock) }) }
	t.Cleanup(release) // runs before srv.Close: a failed test must not hang on the held request

	app := &config.AppSpecific{PinSummaryModel: "m", PinSummaryEndpoint: ep}
	s := newPinSummariser(app, map[string]config.EndpointConfig{
		// 2: the cap leaves room, so the chat name waits by the tier rule.
		ep: {Format: "openai", URL: srv.URL, HTTPTimeout: "10m", MaxInFlight: 2},
	}, nil)
	s.load = fastload.New()
	d := epdispatch.For(ep)
	waitFor := func(what string, ok func(epdispatch.Status) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok(d.Status()) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: dispatcher %+v", what, d.Status())
			}
			time.Sleep(time.Millisecond)
		}
	}

	pinDone := make(chan error, 1)
	go func() {
		_, err := s.summarise(context.Background(), "pin this", "", "")
		pinDone <- err
	}()
	waitFor("pin label in flight", func(st epdispatch.Status) bool { return st.InFlight[epdispatch.High] == 1 })

	name := func(chat string) summaryAsk {
		return summaryAsk{what: autoNameWhat, use: useAutoName, chat: chat, system: "s", maxInputRunes: 100, maxTokens: 10, timeout: time.Second}
	}
	type result struct {
		label string
		err   error
	}
	results := make(chan result, 3)
	run := func(label, chat string) {
		go func() {
			_, err := s.ask(context.Background(), name(chat), "the chat", "", "")
			results <- result{label, err}
		}()
	}
	run("c1-old", "c1")
	waitFor("c1-old waits", func(st epdispatch.Status) bool { return st.Waiting[epdispatch.Medium] == 1 })
	run("c1-new", "c1")
	if r := <-results; r.label != "c1-old" || !errors.Is(r.err, epdispatch.ErrReplaced) {
		t.Fatalf("got %+v, want c1-old replaced by c1-new", r)
	}
	waitFor("c1-new waits", func(st epdispatch.Status) bool { return st.Waiting[epdispatch.Medium] == 1 })
	s.replaceWaiting(useAutoName, autoNameWhat, "c1")
	if r := <-results; r.label != "c1-new" || !errors.Is(r.err, epdispatch.ErrReplaced) {
		t.Fatalf("got %+v, want c1-new replaced by replaceWaiting", r)
	}
	run("c2", "c2")
	waitFor("c2 waits", func(st epdispatch.Status) bool { return st.Waiting[epdispatch.Medium] == 1 })

	release()
	if err := <-pinDone; err != nil {
		t.Fatalf("pin label: %v", err)
	}
	if r := <-results; r.label != "c2" || r.err != nil {
		t.Fatalf("got %+v, want c2 sent after the pin label", r)
	}

	var l fastload.Load
	for _, x := range s.load.Loads(time.Hour) {
		if x.Target == fastload.Target("m", ep) {
			l = x
		}
	}
	if l.Held != 3 || l.Replaced != 2 || l.Runs != 2 {
		t.Errorf("load: held %d replaced %d runs %d, want 3, 2, 2 (%+v)", l.Held, l.Replaced, l.Runs, l)
	}
	if l.Queued != 0 {
		t.Errorf("queued = %d: a held call must not also count as queued at the server", l.Queued)
	}
}

// TestFastSummary_NoMaxInFlightHoldsNothing (#3019): an endpoint without
// max_in_flight is not dispatched: a chat name goes out while a pin label is
// in flight on it, and nothing is recorded as held.
func TestFastSummary_NoMaxInFlightHoldsNothing(t *testing.T) {
	ep := fmt.Sprintf("nodispatch-test-%d", time.Now().UnixNano())
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			<-unblock
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"content": "A short answer"}}},
		})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(release)

	app := &config.AppSpecific{PinSummaryModel: "m", PinSummaryEndpoint: ep}
	s := newPinSummariser(app, map[string]config.EndpointConfig{
		ep: {Format: "openai", URL: srv.URL, HTTPTimeout: "10m"},
	}, nil)
	s.load = fastload.New()

	pinDone := make(chan error, 1)
	go func() {
		_, err := s.summarise(context.Background(), "pin this", "", "")
		pinDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.load.InFlight(fastload.Target("m", ep)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the pin label never went out")
		}
		time.Sleep(time.Millisecond)
	}
	ask := summaryAsk{what: autoNameWhat, use: useAutoName, chat: "c1", system: "s", maxInputRunes: 100, maxTokens: 10, timeout: time.Second}
	got := make(chan error, 1)
	go func() {
		_, err := s.ask(context.Background(), ask, "the chat", "", "")
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("chat name: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the chat name was held on an endpoint without max_in_flight")
	}
	release()
	if err := <-pinDone; err != nil {
		t.Fatal(err)
	}
	for _, l := range s.load.Loads(time.Hour) {
		if l.Held != 0 {
			t.Errorf("%s: held %d, want 0", l.Target, l.Held)
		}
	}
	if st := epdispatch.For(ep).Status(); st.Calls != [3]int{} {
		t.Errorf("the dispatcher saw calls: %+v", st)
	}
}
