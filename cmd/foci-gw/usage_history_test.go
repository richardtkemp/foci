package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/usagehold"
	"foci/internal/usagelog"
)

// The #3103 gateway tests: /mana cached through the real /command handler,
// and every usage report a backend hands over reaching the usage history
// under the agent's account.

func pctW(name string, pct float64, reset time.Time) delegator.UsageReportWindow {
	return delegator.UsageReportWindow{Window: name, Length: delegator.UsageWindowLength(name), Used: &pct, UsedUnit: delegator.UsageUnitPercent, ResetsAt: reset}
}

// postCommand POSTs body to /command on a real HTTP server around mux, as
// `foci command` does, and returns the status and the decoded JSON body.
func postCommand(t *testing.T, srv *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second} // a hang-guard, not a timing bound
	resp, err := client.Post(srv.URL+"/command", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp.StatusCode, out
}

// TestCommand_ManaCached is H3 and H5 (requirement 4): /mana cached through
// the real /command handler returns the whole reply in "response", while the
// agent's usage query blocks; /usage cached gives the same reply; the json
// form arrives whole; with nothing stored the reply says so, still 200.
func TestCommand_ManaCached(t *testing.T) {
	t.Parallel()
	st, err := usagelog.Open(filepath.Join(t.TempDir(), "usage-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	r5, r7 := now.Add(2*time.Hour).Truncate(time.Second), now.Add(72*time.Hour).Truncate(time.Second)
	if err := st.Save(delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: now.Add(-20 * time.Minute),
		Backend: "claude-code", Account: "claude-code:A", AgentID: "test",
		Windows: []delegator.UsageReportWindow{pctW("five_hour", 30, r5), pctW("seven_day", 20, r7)}}); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	block := func(ctx context.Context, _ delegator.UsageTarget) (*delegator.UsageInfo, error) {
		select {
		case <-blocked:
		case <-ctx.Done():
		}
		return nil, context.Canceled
	}
	mana := command.ManaCommand(block, delegator.UsageTarget{AgentID: "test"}, true,
		&usagelog.Binding{Store: st, Backend: "claude-code", Account: "claude-code:A", AgentID: "test"})
	empty := command.ManaCommand(block, delegator.UsageTarget{AgentID: "test"}, true,
		&usagelog.Binding{Store: st, Backend: "claude-code", Account: "claude-code:none", AgentID: "test"})
	empty.Name, empty.Aliases = "mana_empty", nil
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{mana, empty}})
	srv := httptest.NewServer(newTestMux(d))
	defer srv.Close()

	start := time.Now()
	code, out := postCommand(t, srv, `{"command":"/mana cached"}`)
	if took := time.Since(start); took > time.Second {
		t.Errorf("/mana cached through /command took %s, want under 1 s", took)
	}
	text, _ := out["response"].(string)
	if code != http.StatusOK || !strings.Contains(text, "30%") || !strings.Contains(text, "20%") ||
		!strings.Contains(text, "received 20m ago") || !strings.Contains(text, "For a script: /mana cached json") {
		t.Fatalf("/mana cached: %d %q", code, text)
	}
	t.Logf("response:\n%s", text)

	_, alias := postCommand(t, srv, `{"command":"/usage cached"}`)
	if alias["response"] != text {
		t.Errorf("/usage cached = %q, want the /mana cached reply", alias["response"])
	}

	code, out = postCommand(t, srv, `{"command":"/mana cached json"}`)
	var j struct {
		HasFigures bool `json:"has_figures"`
		Windows    []struct {
			Window      string   `json:"window"`
			UsedPercent *float64 `json:"used_percent"`
			ResetsAt    *int64   `json:"resets_at"`
		} `json:"windows"`
	}
	if err := json.Unmarshal([]byte(out["response"].(string)), &j); err != nil || code != http.StatusOK {
		t.Fatalf("json form through /command: %d %v %q", code, err, out["response"])
	}
	if !j.HasFigures || len(j.Windows) != 2 || *j.Windows[0].UsedPercent != 30 || *j.Windows[0].ResetsAt != r5.Unix() || *j.Windows[1].UsedPercent != 20 {
		t.Errorf("json windows = %+v", j)
	}

	code, out = postCommand(t, srv, `{"command":"/mana_empty cached"}`)
	if code != http.StatusOK || !strings.Contains(out["response"].(string), "holds no usage figures") {
		t.Errorf("nothing stored: %d %q", code, out["response"])
	}
	code, out = postCommand(t, srv, `{"command":"/mana_empty cached json"}`)
	if code != http.StatusOK || !strings.Contains(out["response"].(string), `"has_figures": false`) {
		t.Errorf("nothing stored, json: %d %q", code, out["response"])
	}
}

// usageSinkCatcher is a test backend with a usage query that records the
// usage sink the gateway hands it.
type usageSinkCatcher struct {
	hookCatcher
	sink *delegator.UsageSink
}

func (u *usageSinkCatcher) SetUsageSink(s delegator.UsageSink) { *u.sink = s }

// registerUsageSinkCatcher registers a usage-capable test backend whose
// usage account comes from the real Claude Code Spec.
func registerUsageSinkCatcher(t *testing.T, name string) *delegator.UsageSink {
	t.Helper()
	if s, ok := delegator.SpecFor(name); ok {
		be, _ := s.New(nil)
		got := be.(*usageSinkCatcher).sink
		*got = nil
		return got
	}
	cc, _ := delegator.SpecFor("claude-code")
	got := new(delegator.UsageSink)
	hooks := &delegator.HostHooks{}
	spec := delegator.Spec{
		Name: name, DisplayName: name, ModelcapsKey: name, LedgerKey: name,
		New: func(map[string]any) (delegator.Delegator, error) {
			return &usageSinkCatcher{hookCatcher: hookCatcher{hooks: hooks}, sink: got}, nil
		},
		Prototype:    (*usageSinkCatcher)(nil),
		Caps:         map[delegator.Capability]delegator.Support{},
		UsageAccount: cc.UsageAccount,
		UsageQuery: func(context.Context, delegator.UsageTarget) (*delegator.UsageInfo, error) {
			return nil, context.Canceled
		},
	}
	for _, c := range delegator.AllCapabilities() {
		spec.Caps[c] = delegator.No("test backend")
	}
	spec.Caps[delegator.CapHostHooks] = delegator.Yes()
	spec.Caps[delegator.CapUsageQuery] = delegator.Yes()
	spec.Caps[delegator.CapUsageReports] = delegator.Yes()
	if err := spec.Validate(); err != nil {
		t.Fatalf("test Spec invalid: %v", err)
	}
	delegator.Register(spec)
	return got
}

// TestUsageWiring_BackendReportsStoredPerAccount (requirements 6-8 through
// the real configureDelegated): a usage-capable backend gets a usage sink,
// and what it reports is stored with the agent, backend and the agent's own
// usage account: two agents on two Claude Code logins never share one.
func TestUsageWiring_BackendReportsStoredPerAccount(t *testing.T) {
	isolateHostCC(t)
	st, err := usagelog.Open(filepath.Join(t.TempDir(), "usage-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	setUsageHistory(st)
	t.Cleanup(func() { setUsageHistory(nil) })

	setup := func(id string, env map[string]string) (*agent.Agent, *delegator.UsageSink) {
		backend := "test-usage-sink-" + id
		sink := registerUsageSinkCatcher(t, backend)
		p, shared := wiringFixture(t)
		p.acfg.ID, p.acfg.Backend = id, backend
		p.cfg.DataDir = t.TempDir()
		ag := &agent.Agent{AgentID: id, UsageHoldRegistry: usagehold.New()}
		if _, ok := configureDelegated(ag, p, shared, backend, config.BackendConfig{Env: env}); !ok {
			t.Fatal("configureDelegated returned ok=false")
		}
		t.Cleanup(ag.StopUsageHold)
		if _, err := ag.DelegatedManager.NewBackend(); err != nil {
			t.Fatal(err)
		}
		if *sink == nil {
			t.Fatalf("%s's backend got no usage sink", id)
		}
		return ag, sink
	}
	alpha, sinkA := setup("alpha", nil)
	beta, sinkB := setup("beta", map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(t.TempDir(), "ccB")})
	if alpha.UsageAccount() == beta.UsageAccount() {
		t.Fatalf("alpha and beta share account %q", alpha.UsageAccount())
	}

	payload := json.RawMessage(`{"type":"rate_limit_event"}`)
	(*sinkA)(delegator.UsageReport{Source: delegator.UsageSourceCCEvent, Payload: payload,
		Windows: []delegator.UsageReportWindow{pctW("five_hour", 11, time.Now().Add(time.Hour))}})
	(*sinkB)(delegator.UsageReport{Source: delegator.UsageSourceCCEvent, Payload: payload,
		Windows: []delegator.UsageReportWindow{pctW("five_hour", 77, time.Now().Add(time.Hour))}})

	for _, tc := range []struct {
		ag   *agent.Agent
		want float64
	}{{alpha, 11}, {beta, 77}} {
		reps, err := st.Reports(tc.ag.UsageAccount())
		if err != nil {
			t.Fatal(err)
		}
		if len(reps) != 1 {
			t.Fatalf("%s: %d reports under its account, want 1", tc.ag.AgentID, len(reps))
		}
		r := reps[0]
		if r.AgentID != tc.ag.AgentID || r.Backend != "test-usage-sink-"+tc.ag.AgentID || r.Source != delegator.UsageSourceCCEvent ||
			r.ReceivedAt.IsZero() || !bytes.Equal(r.Payload, payload) || *r.Windows[0].Used != tc.want {
			t.Errorf("%s's stored report = %+v", tc.ag.AgentID, r)
		}
	}
}
