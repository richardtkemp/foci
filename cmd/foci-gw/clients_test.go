package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// fakeOpenAI is an OpenAI-compatible server that counts its calls and records
// the bearer token of the last one. A non-nil hold blocks each call until it
// is closed, after signalling entered.
type fakeOpenAI struct {
	srv     *httptest.Server
	calls   atomic.Int32
	auth    atomic.Value
	entered chan struct{}
	hold    chan struct{}
}

func newFakeOpenAI(t *testing.T, name string) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.auth.Store(r.Header.Get("Authorization"))
		if f.hold != nil {
			f.entered <- struct{}{}
			<-f.hold
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, name)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func testSecrets(t *testing.T, body string) *secrets.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := secrets.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func endpointsCfg(ep config.EndpointConfig) *config.Config {
	return &config.Config{Endpoints: map[string]config.EndpointConfig{"local": ep}}
}

// replyFrom sends one request through the provider layer and returns the
// text the server answered with (the server's name).
func replyFrom(t *testing.T, c provider.Client) string {
	t.Helper()
	req := &provider.MessageRequest{Model: "m", MaxTokens: 10, Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}}
	resp, err := provider.Send(context.Background(), c, req, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return provider.TextOf(resp.Content)
}

// TestClientRegistry_ReconfigureSwapsBehindHandle (#2980): a changed
// [endpoints.local] url and api_key reach the NEXT call through the handle a
// caller already holds, with no restart.
func TestClientRegistry_ReconfigureSwapsBehindHandle(t *testing.T) {
	a, b := newFakeOpenAI(t, "A"), newFakeOpenAI(t, "B")
	store := testSecrets(t, "[local]\napi_key = \"key-one\"\nother = \"key-two\"\n")
	r := newClientRegistry(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL}), store, context.Background())

	held := r.GetClient("local", "openai")
	if held == nil {
		t.Fatal("GetClient returned nil")
	}
	if got := replyFrom(t, held); got != "A" {
		t.Fatalf("before edit: reply from %q, want A", got)
	}

	r.Reconfigure(endpointsCfg(config.EndpointConfig{Format: "openai", URL: b.srv.URL, APIKey: "local.other", HTTPTimeout: "30m"}))

	if got := replyFrom(t, held); got != "B" {
		t.Fatalf("after edit: the held handle replied from %q, want B (url change did not apply live)", got)
	}
	if got, _ := b.auth.Load().(string); got != "Bearer key-two" {
		t.Errorf("after edit: Authorization = %q, want the new api_key secret", got)
	}
	if again := r.GetClient("local", "openai"); again != held {
		t.Error("GetClient after Reconfigure returned a different handle; holders would diverge")
	}
}

// TestClientRegistry_ReconfigureKeepsUnchangedClient: an edit that leaves an
// endpoint's config equal keeps its client (and its retry/cache state).
func TestClientRegistry_ReconfigureKeepsUnchangedClient(t *testing.T) {
	a := newFakeOpenAI(t, "A")
	store := testSecrets(t, "[local]\napi_key = \"k\"\n")
	ep := config.EndpointConfig{Format: "openai", URL: a.srv.URL}
	r := newClientRegistry(endpointsCfg(ep), store, context.Background())
	held := r.GetClient("local", "openai")
	before := provider.Pin(held)
	r.Reconfigure(endpointsCfg(ep))
	if provider.Pin(held) != before {
		t.Error("an unchanged endpoint was rebuilt")
	}
}

// TestClientRegistry_FailedRebuildKeepsOldClient: an edit naming a secret
// that does not exist must not leave a working endpoint with no client.
func TestClientRegistry_FailedRebuildKeepsOldClient(t *testing.T) {
	a := newFakeOpenAI(t, "A")
	store := testSecrets(t, "[local]\napi_key = \"k\"\n")
	r := newClientRegistry(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL}), store, context.Background())
	held := r.GetClient("local", "openai")
	r.Reconfigure(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL, APIKey: "local.missing"}))
	if got := replyFrom(t, held); got != "A" {
		t.Fatalf("reply from %q, want A from the kept client", got)
	}
}

// TestClientRegistry_FailedFirstBuildRetriesAfterEdit: an endpoint that had
// no client (its secret was missing) gets one after an edit fixes it.
func TestClientRegistry_FailedFirstBuildRetriesAfterEdit(t *testing.T) {
	a := newFakeOpenAI(t, "A")
	store := testSecrets(t, "[local]\napi_key = \"k\"\n")
	r := newClientRegistry(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL, APIKey: "local.missing"}), store, context.Background())
	if c := r.GetClient("local", "openai"); c != nil {
		t.Fatalf("GetClient with a missing secret = %T, want nil", c)
	}
	r.Reconfigure(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL}))
	c := r.GetClient("local", "openai")
	if c == nil {
		t.Fatal("GetClient after the fixing edit is still nil")
	}
	if got := replyFrom(t, c); got != "A" {
		t.Fatalf("reply from %q, want A", got)
	}
}

// TestClientRegistry_InFlightCallKeepsItsClient: a call running when the
// edit lands finishes on the client it started with; the next call uses the
// new one.
func TestClientRegistry_InFlightCallKeepsItsClient(t *testing.T) {
	a, b := newFakeOpenAI(t, "A"), newFakeOpenAI(t, "B")
	a.entered, a.hold = make(chan struct{}, 1), make(chan struct{})
	store := testSecrets(t, "[local]\napi_key = \"k\"\n")
	r := newClientRegistry(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL}), store, context.Background())
	held := r.GetClient("local", "openai")

	done := make(chan string, 1)
	go func() { done <- replyFrom(t, held) }()
	<-a.entered
	r.Reconfigure(endpointsCfg(config.EndpointConfig{Format: "openai", URL: b.srv.URL}))
	close(a.hold)
	if got := <-done; got != "A" {
		t.Fatalf("in-flight call finished on %q, want A", got)
	}
	if n := b.calls.Load(); n != 0 {
		t.Fatalf("new endpoint saw %d calls during the in-flight one, want 0", n)
	}
	if got := replyFrom(t, held); got != "B" {
		t.Fatalf("next call replied from %q, want B", got)
	}
}

// TestLiveApply_EndpointsReachRegistry: both edit surfaces reach the
// registry — the app editor's bare section address and the foci config set
// key path (endpoints.<name>.<sub>).
func TestLiveApply_EndpointsReachRegistry(t *testing.T) {
	a, b := newFakeOpenAI(t, "A"), newFakeOpenAI(t, "B")
	store := testSecrets(t, "[local]\napi_key = \"k\"\n")
	old := gwClients
	t.Cleanup(func() { gwClients = old })

	for _, addr := range [][2]string{{"endpoints", ""}, {"endpoints.local", "url"}, {"endpoints", "local"}} {
		t.Run(addr[0]+"/"+addr[1], func(t *testing.T) {
			gwClients = newClientRegistry(endpointsCfg(config.EndpointConfig{Format: "openai", URL: a.srv.URL}), store, context.Background())
			held := gwClients.GetClient("local", "openai")

			configPath := filepath.Join(t.TempDir(), "foci.toml")
			body := "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n\n[endpoints.local]\nformat = \"openai\"\nurl = \"" + b.srv.URL + "\"\n\n[[agents]]\nid = \"a\"\n"
			if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			la := newLiveApply(configPath, nil)
			registerLiveAppliers(la, registryFromMap(map[string]*agentInstance{}))
			applied, err := la.Apply(addr[0], addr[1])
			if err != nil || !applied {
				t.Fatalf("Apply(%q, %q) = %v, %v; want applied", addr[0], addr[1], applied, err)
			}
			if got := replyFrom(t, held); got != "B" {
				t.Fatalf("after Apply: reply from %q, want B", got)
			}
		})
	}
}

// TestLiveApplyCoversLiveMapObjectSections is TestLiveApplyCoversLiveObjectSections
// for mapObject sections ([models.*], [endpoints.*]): Live must match having
// an applier at the bare section address.
func TestLiveApplyCoversLiveMapObjectSections(t *testing.T) {
	la := newLiveApply("", nil)
	registerLiveAppliers(la, registryFromMap(map[string]*agentInstance{}))
	for _, spec := range config.MapObjectFields() {
		has := la.appliers[spec.Section] != nil
		if spec.Live && !has {
			t.Errorf("mapObject section %s is marked Live but has no applier", spec.Section)
		}
		if !spec.Live && has {
			t.Errorf("mapObject section %s has an applier but is not marked Live", spec.Section)
		}
	}
}
