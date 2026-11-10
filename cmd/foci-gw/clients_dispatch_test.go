package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/epdispatch"
	"foci/internal/provider"
)

// TestClientRegistry_DispatchesLocalEndpoint (#3019): an endpoint with
// max_in_flight 1 gets one call at a time; a waiting background turn's call
// (keepalive) goes after a waiting user turn's call that arrived later; a
// max_in_flight edit applies live without rebuilding the client.
func TestClientRegistry_DispatchesLocalEndpoint(t *testing.T) {
	name := fmt.Sprintf("gate-test-%d", time.Now().UnixNano()) // epdispatch is process-wide
	var mu sync.Mutex
	var order []string
	inFlight, maxInFlight := 0, 0
	entered := make(chan string, 4)
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		order = append(order, body.Model)
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		entered <- body.Model
		if body.Model == "first" {
			<-hold
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, body.Model)
	}))
	var holdOnce sync.Once
	unhold := func() { holdOnce.Do(func() { close(hold) }) }
	t.Cleanup(srv.Close)
	t.Cleanup(unhold) // before srv.Close: a failed test must not hang

	cfgFor := func(limit int) *config.Config {
		return &config.Config{Endpoints: map[string]config.EndpointConfig{
			name: {Format: "openai", URL: srv.URL, APIKey: "gate.key", MaxInFlight: limit},
		}}
	}
	store := testSecrets(t, "[gate]\nkey = \"k\"\n")
	r := newClientRegistry(cfgFor(1), store, context.Background())
	c := r.GetClient(name, "openai")
	if c == nil {
		t.Fatal("GetClient returned nil")
	}
	d := epdispatch.For(name)

	send := func(ctx context.Context, model string, errs chan<- error) {
		req := &provider.MessageRequest{Model: model, MaxTokens: 10, Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}}
		_, err := provider.Send(ctx, c, req, nil, nil, nil, nil)
		errs <- err
	}
	waitWaiting := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			s := d.Status()
			if s.Waiting[epdispatch.High]+s.Waiting[epdispatch.Medium]+s.Waiting[epdispatch.Low] == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting never reached %d: %+v", n, s)
			}
			time.Sleep(time.Millisecond)
		}
	}

	errs := make(chan error, 4)
	go send(agent.WithTrigger(context.Background(), "telegram"), "first", errs)
	if got := <-entered; got != "first" {
		t.Fatalf("first request = %q", got)
	}
	go send(agent.WithTrigger(context.Background(), agent.TriggerKeepalive), "keepalive", errs)
	waitWaiting(1)
	go send(agent.WithTrigger(context.Background(), "telegram"), "user", errs)
	waitWaiting(2)
	if s := d.Status(); s.Waiting[epdispatch.Low] != 1 || s.Waiting[epdispatch.High] != 1 {
		t.Fatalf("tiers: %+v, want the keepalive Low and the user turn High", s)
	}

	before := provider.Pin(c)
	unhold()
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	got := fmt.Sprint(order)
	mx := maxInFlight
	mu.Unlock()
	if got != "[first user keepalive]" {
		t.Errorf("server order = %s, want [first user keepalive]", got)
	}
	if mx != 1 {
		t.Errorf("max in flight at the server = %d, want 1", mx)
	}

	r.Reconfigure(cfgFor(3))
	if l := d.Status().Limit; l != 3 {
		t.Errorf("limit after the edit = %d, want 3", l)
	}
	if provider.Pin(c) != before {
		t.Error("a max_in_flight edit rebuilt the client")
	}
}

// TestClientRegistry_AutomatedTurnsMedium (#3023): on an endpoint with
// max_in_flight 1, turns that automation started (a cron/foci send = trigger
// "user", a webhook, an agent message, a reminder wake) wait at Medium behind
// a running user turn and behind a user turn that arrives after them, go
// before a background turn, and are never replaced: two cron sends of the
// same chat both reach the server, in arrival order.
func TestClientRegistry_AutomatedTurnsMedium(t *testing.T) {
	name := fmt.Sprintf("gate-medium-%d", time.Now().UnixNano()) // epdispatch is process-wide
	var mu sync.Mutex
	var order []string
	entered := make(chan string, 8)
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		order = append(order, body.Model)
		mu.Unlock()
		entered <- body.Model
		if body.Model == "first" {
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, body.Model)
	}))
	var holdOnce sync.Once
	unhold := func() { holdOnce.Do(func() { close(hold) }) }
	t.Cleanup(srv.Close)
	t.Cleanup(unhold)

	cfg := &config.Config{Endpoints: map[string]config.EndpointConfig{
		name: {Format: "openai", URL: srv.URL, APIKey: "gate.key", MaxInFlight: 1},
	}}
	r := newClientRegistry(cfg, testSecrets(t, "[gate]\nkey = \"k\"\n"), context.Background())
	c := r.GetClient(name, "openai")
	if c == nil {
		t.Fatal("GetClient returned nil")
	}
	d := epdispatch.For(name)

	errs := make(chan error, 8)
	send := func(ctx context.Context, model string) {
		req := &provider.MessageRequest{Model: model, MaxTokens: 10, Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}}
		_, err := provider.Send(ctx, c, req, nil, nil, nil, nil)
		if err != nil {
			err = fmt.Errorf("%s: %w", model, err)
		}
		errs <- err
	}
	waitTiers := func(high, medium, low int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			s := d.Status()
			if s.Waiting[epdispatch.High] == high && s.Waiting[epdispatch.Medium] == medium && s.Waiting[epdispatch.Low] == low {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting never reached high=%d medium=%d low=%d: %+v", high, medium, low, s)
			}
			time.Sleep(time.Millisecond)
		}
	}
	ctxFor := func(trigger string) context.Context { return agent.WithTrigger(context.Background(), trigger) }

	go send(ctxFor("telegram"), "first")
	if got := <-entered; got != "first" {
		t.Fatalf("first request = %q", got)
	}
	go send(ctxFor("user"), "cron1")
	waitTiers(0, 1, 0)
	go send(ctxFor("user"), "cron2")
	waitTiers(0, 2, 0)
	go send(ctxFor("webhook"), "webhook")
	waitTiers(0, 3, 0)
	go send(ctxFor("session_notify"), "agentmsg")
	waitTiers(0, 4, 0)
	go send(ctxFor("scheduled_wake"), "wake")
	waitTiers(0, 5, 0)
	go send(ctxFor(agent.TriggerKeepalive), "keepalive")
	waitTiers(0, 5, 1)
	go send(agent.WithHumanSource(ctxFor("user")), "human")
	waitTiers(1, 5, 1)

	unhold()
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err) // ErrReplaced here = a Medium agent call was dropped
		}
	}
	mu.Lock()
	got := fmt.Sprint(order)
	mu.Unlock()
	if want := "[first human cron1 cron2 webhook agentmsg wake keepalive]"; got != want {
		t.Errorf("server order = %s, want %s", got, want)
	}
}

// TestClientRegistry_OpenRouterNotDispatched: an endpoint on OpenRouter is
// never held, whatever its max_in_flight.
func TestClientRegistry_OpenRouterNotDispatched(t *testing.T) {
	r := newClientRegistry(&config.Config{Endpoints: map[string]config.EndpointConfig{
		"myrouter": {Format: "openai", URL: "https://openrouter.ai/api/v1", MaxInFlight: 1},
	}}, testSecrets(t, ""), context.Background())
	g := r.callGate("myrouter")
	rel1, err := g(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rel2, err := g(ctx, nil) // with a dispatcher at limit 1 this would wait
	if err != nil {
		t.Fatalf("second call on an OpenRouter endpoint was held: %v", err)
	}
	rel1()
	rel2()
}

// TestClientRegistry_NoMaxInFlightHoldsNothing (#3019): at max_in_flight 0
// (the default) a background call goes out while a user turn's call is in
// flight on the endpoint: no dispatcher, no tiers.
func TestClientRegistry_NoMaxInFlightHoldsNothing(t *testing.T) {
	name := fmt.Sprintf("nogate-test-%d", time.Now().UnixNano())
	f := newFakeOpenAI(t, "A")
	f.entered = make(chan struct{}, 2)
	f.hold = make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(f.hold) }) })
	r := newClientRegistry(&config.Config{Endpoints: map[string]config.EndpointConfig{
		name: {Format: "openai", URL: f.srv.URL, APIKey: "gate.key"},
	}}, testSecrets(t, "[gate]\nkey = \"k\"\n"), context.Background())
	c := r.GetClient(name, "openai")
	if c == nil {
		t.Fatal("GetClient returned nil")
	}
	errs := make(chan error, 2)
	send := func(ctx context.Context) {
		req := &provider.MessageRequest{Model: "m", MaxTokens: 10, Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}}
		_, err := provider.Send(ctx, c, req, nil, nil, nil, nil)
		errs <- err
	}
	go send(agent.WithTrigger(context.Background(), "user"))
	<-f.entered
	go send(agent.WithTrigger(context.Background(), agent.TriggerKeepalive))
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the keepalive call was held on an endpoint without max_in_flight")
	}
	once.Do(func() { close(f.hold) })
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if st := epdispatch.For(name).Status(); st.Calls != [3]int{} {
		t.Errorf("the dispatcher saw calls: %+v", st)
	}
}
