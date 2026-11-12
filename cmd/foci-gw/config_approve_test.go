package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/configapprove"
	"foci/internal/configcheck"
)

const approveTestConfig = `[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[agent_loop]
max_tool_loops = 10

[[agents]]
id = "test-agent"
workspace = "/tmp/test-agent-ws"
`

// approveRig is the approval service behind the real /config/request handler,
// with a fake presenter (records each prompt) and a fake timer.
type approveRig struct {
	path    string
	svc     *configapprove.Service
	mux     *http.ServeMux
	mu      sync.Mutex
	prompts []configapprove.Prompt
	timers  []func()
}

func newApproveRig(t *testing.T) *approveRig {
	t.Helper()
	r := &approveRig{path: filepath.Join(t.TempDir(), "foci.toml")}
	if err := os.WriteFile(r.path, []byte(approveTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	r.svc = configapprove.New(configapprove.Deps{
		ConfigPath: r.path,
		FileMode:   0o600,
		Present: func(p configapprove.Prompt) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.prompts = append(r.prompts, p)
			return nil
		},
		Close: func(string, string) {},
		AfterFunc: func(_ time.Duration, f func()) func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.timers = append(r.timers, f)
			return func() bool { return true }
		},
	})
	r.mux = http.NewServeMux()
	r.mux.HandleFunc("/config/request", handleConfigRequest(
		func() *configapprove.Service { return r.svc },
		func(id string) bool { return id == testAgentID },
	))
	return r
}

func (r *approveRig) submit(t *testing.T, body string) configapprove.Status {
	t.Helper()
	w := postJSON(r.mux, "/config/request", body)
	if w.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	var st configapprove.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (r *approveRig) poll(t *testing.T, id string) (int, configapprove.Status) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/config/request?id="+id+"&wait=0", nil)
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	var st configapprove.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("poll body %q: %v", w.Body.String(), err)
	}
	return w.Code, st
}

func (r *approveRig) file(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (r *approveRig) prompt(t *testing.T) configapprove.Prompt {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(r.prompts))
	}
	return r.prompts[0]
}

const setLoops = `{"session":"test-agent/app/1","op":"set","path":"agent_loop.max_tool_loops","value":"20"}`

// TestConfigRequestHTTP_Approve drives the approval end to end through the
// HTTP handler: POST answers pending with no write, the prompt goes to the
// requesting agent, Approve writes the file, and the poll reports applied.
func TestConfigRequestHTTP_Approve(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	st := r.submit(t, setLoops)
	if st.Outcome != configapprove.Pending || st.ID == "" {
		t.Fatalf("submit = %+v", st)
	}
	if _, got := r.poll(t, st.ID); got.Outcome != configapprove.Pending {
		t.Fatalf("poll before answer = %+v", got)
	}
	if r.file(t) != approveTestConfig {
		t.Fatal("written before approval")
	}
	p := r.prompt(t)
	if p.AgentID != testAgentID || !strings.Contains(p.Text, "test-agent/app/1") {
		t.Errorf("prompt = %+v", p)
	}
	p.OnAnswer(true)
	if _, got := r.poll(t, st.ID); got.Outcome != configapprove.Applied {
		t.Fatalf("poll = %+v", got)
	}
	if !strings.Contains(r.file(t), "max_tool_loops = 20") {
		t.Errorf("file:\n%s", r.file(t))
	}
}

// TestConfigRequestHTTP_Deny proves Deny → denied, file unchanged.
func TestConfigRequestHTTP_Deny(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	st := r.submit(t, setLoops)
	r.prompt(t).OnAnswer(false)
	if _, got := r.poll(t, st.ID); got.Outcome != configapprove.Denied {
		t.Fatalf("poll = %+v", got)
	}
	if r.file(t) != approveTestConfig {
		t.Error("deny wrote the file")
	}
}

// TestConfigRequestHTTP_TimeoutThenPress proves timeout → timed out, and a
// press after it writes nothing.
func TestConfigRequestHTTP_TimeoutThenPress(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	st := r.submit(t, setLoops)
	r.mu.Lock()
	fire := r.timers[0]
	r.mu.Unlock()
	fire()
	r.prompt(t).OnAnswer(true)
	if _, got := r.poll(t, st.ID); got.Outcome != configapprove.TimedOut {
		t.Fatalf("poll = %+v", got)
	}
	if r.file(t) != approveTestConfig {
		t.Error("a late press wrote the file")
	}
}

// TestConfigRequestHTTP_FileChanged proves a file changed between request
// and approve → failed, nothing written.
func TestConfigRequestHTTP_FileChanged(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	st := r.submit(t, setLoops)
	changed := strings.Replace(approveTestConfig, "= 10", "= 15", 1)
	if err := os.WriteFile(r.path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	r.prompt(t).OnAnswer(true)
	if _, got := r.poll(t, st.ID); got.Outcome != configapprove.Failed {
		t.Fatalf("poll = %+v", got)
	}
	if r.file(t) != changed {
		t.Error("the file was written")
	}
}

// TestConfigRequestHTTP_InvalidNoPrompt proves a request failing validation
// — a wrong type, an edit that would change a second key — answers failed
// at once and shows no prompt. The shared config check
// (configcheck.CheckContentAllowUnknown) is wired as in production. An
// unknown key is not here: it prompts with a warning (#3046).
func TestConfigRequestHTTP_InvalidNoPrompt(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"wrong type":   `{"session":"test-agent/app/1","op":"set","path":"agent_loop.max_tool_loops","value":"\"many\""}`,
		"inside value": `{"session":"test-agent/app/1","op":"set","path":"agent_loop.max_tool_loops.x","value":"1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newApproveRig(t)
			r.svc = configapprove.New(configapprove.Deps{
				ConfigPath:   r.path,
				CheckContent: configcheck.CheckContentAllowUnknown,
				Present: func(configapprove.Prompt) error {
					t.Error("prompted for an invalid request")
					return nil
				},
			})
			st := r.submit(t, body)
			if st.Outcome != configapprove.Failed || st.Reason == "" {
				t.Fatalf("status = %+v", st)
			}
			if r.file(t) != approveTestConfig {
				t.Error("file changed")
			}
		})
	}
}

// TestConfigRequestHTTP_ValidPassesCheck is the positive control for the
// invalid cases: the same base config and the production content check
// accept a valid change and prompt for it.
func TestConfigRequestHTTP_ValidPassesCheck(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	prompted := false
	r.svc = configapprove.New(configapprove.Deps{
		ConfigPath:   r.path,
		CheckContent: configcheck.CheckContentAllowUnknown,
		Present:      func(configapprove.Prompt) error { prompted = true; return nil },
	})
	if st := r.submit(t, setLoops); st.Outcome != configapprove.Pending || !prompted {
		t.Fatalf("status = %+v prompted=%v", st, prompted)
	}
}

// newCheckedApproveRig is newApproveRig with the production content check
// and base as the config file.
func newCheckedApproveRig(t *testing.T, base string) *approveRig {
	t.Helper()
	r := newApproveRig(t)
	if err := os.WriteFile(r.path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	r.svc = configapprove.New(configapprove.Deps{
		ConfigPath:   r.path,
		FileMode:     0o600,
		CheckContent: configcheck.CheckContentAllowUnknown,
		Present: func(p configapprove.Prompt) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.prompts = append(r.prompts, p)
			return nil
		},
		Close:     func(string, string) {},
		AfterFunc: func(time.Duration, func()) func() bool { return func() bool { return true } },
	})
	return r
}

const setFutureKey = `{"session":"test-agent/app/1","op":"set","path":"tools.future_key","value":"\"2h\""}`

// TestConfigRequestHTTP_UnknownKeyWarnsAndWrites is #3046: a key the running
// binary does not know (a key for a later version, set ahead of its deploy)
// does not block the request. The prompt and the status warn, naming the
// key, and Approve writes it.
func TestConfigRequestHTTP_UnknownKeyWarnsAndWrites(t *testing.T) {
	t.Parallel()
	r := newCheckedApproveRig(t, approveTestConfig)
	st := r.submit(t, setFutureKey)
	if st.Outcome != configapprove.Pending {
		t.Fatalf("status = %+v, want pending", st)
	}
	p := r.prompt(t)
	for _, want := range []string{"does not know this key: tools.future_key", "later version"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p.Text)
		}
		if !strings.Contains(st.Warning, want) {
			t.Errorf("status warning lacks %q: %q", want, st.Warning)
		}
	}
	if reply := p.OnAnswer(true); !strings.HasPrefix(reply, "✅") {
		t.Fatalf("approve reply = %q", reply)
	}
	if !strings.Contains(r.file(t), `future_key = "2h"`) {
		t.Errorf("file not written:\n%s", r.file(t))
	}
}

// TestConfigRequestHTTP_UnknownKeyAlreadyInFile proves an unknown key already
// in the file (pre-seeded earlier) neither blocks nor is named in a change
// to another key, and is still named when the change sets it again.
func TestConfigRequestHTTP_UnknownKeyAlreadyInFile(t *testing.T) {
	t.Parallel()
	seeded := approveTestConfig + "\n[tools]\nfuture_key = \"1h\"\n"

	r := newCheckedApproveRig(t, seeded)
	st := r.submit(t, setLoops)
	if st.Outcome != configapprove.Pending || st.Warning != "" {
		t.Fatalf("other key: status = %+v, want pending with no warning", st)
	}
	if p := r.prompt(t); strings.Contains(p.Text, "does not know") {
		t.Errorf("prompt warns about a key the change does not write:\n%s", p.Text)
	}

	r = newCheckedApproveRig(t, seeded)
	st = r.submit(t, setFutureKey)
	if st.Outcome != configapprove.Pending || !strings.Contains(st.Warning, "tools.future_key") {
		t.Fatalf("same key: status = %+v, want pending with a warning naming tools.future_key", st)
	}
}

// TestConfigRequestHTTP_UnknownIDIs404 proves a poll for an id the gateway
// does not know (it restarted) answers 404 with outcome failed.
func TestConfigRequestHTTP_UnknownIDIs404(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	code, st := r.poll(t, "cfg-from-before-restart")
	if code != http.StatusNotFound || st.Outcome != configapprove.Failed {
		t.Fatalf("code %d status %+v", code, st)
	}
}

// TestConfigRequestHTTP_UnknownAgentRejected proves a session naming no
// known agent is a 400, not a prompt.
func TestConfigRequestHTTP_UnknownAgentRejected(t *testing.T) {
	t.Parallel()
	r := newApproveRig(t)
	w := postJSON(r.mux, "/config/request", `{"session":"ghost/app/1","op":"set","path":"a.b","value":"1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code %d", w.Code)
	}
}

// gatingRig is an HTTP harness agent with the real /config, /agents and
// /android commands, a ConfigSetDeps over a temp file, and a recording
// approval hook.
type gatingRig struct {
	mux      *http.ServeMux
	inst     *agentInstance
	path     string
	mu       sync.Mutex
	requests []string
}

func newGatingRig(t *testing.T) *gatingRig {
	t.Helper()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{command.ConfigCommand(), command.AgentsCommand(), command.AndroidCommand()}})
	inst, _ := d.agents.Get(testAgentID)
	g := &gatingRig{mux: newTestMux(d), inst: inst, path: filepath.Join(t.TempDir(), "foci.toml")}
	if err := os.WriteFile(g.path, []byte(approveTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	g.inst.cc.ConfigSetDeps = &command.ConfigSetDeps{
		Registry:        g.inst.cmds,
		ConfigPath:      g.path,
		AgentID:         testAgentID,
		AgentIDsFn:      func() []string { return []string{testAgentID} },
		SectionsFn:      config.FieldSections,
		FieldsInSection: config.FieldsInSection,
		LookupFn:        config.LookupField,
		SetInFileFn: func(path string, target config.SetTarget, value string) (string, error) {
			return config.SetInFile(path, target, value, 0o600)
		},
		UnsetInFileFn: func(path string, target config.SetTarget) (string, error) {
			return config.UnsetInFile(path, target, 0o600)
		},
		RequestApprovalFn: func(sk string, op configapprove.Op, path, value string) configapprove.Status {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.requests = append(g.requests, sk+" "+string(op)+" "+path+" "+value)
			return configapprove.Status{ID: "cfg-1", Outcome: configapprove.Pending, Path: path, Old: "10", New: value}
		},
	}
	g.inst.cc.AgentNewDeps = &command.AgentNewDeps{Registry: g.inst.cmds}
	return g
}

func (g *gatingRig) file(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(g.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestConfigSetOverHTTP_RequestsApproval proves requirement 8: /config set
// with a direct value through /command and through the /send slash branch
// writes nothing, creates an approval request for the same key path
// `foci config set` uses, and answers with the request id; /config unset
// likewise.
func TestConfigSetOverHTTP_RequestsApproval(t *testing.T) {
	t.Parallel()
	g := newGatingRig(t)
	for _, tc := range []struct{ endpoint, body string }{
		{"/command", `{"command":"/config set agent_loop.max_tool_loops=20"}`},
		{"/send", `{"text":"/config set agent_loop.max_tool_loops=20"}`},
		{"/command", `{"command":"/config set agent_loop max_tool_loops 20"}`},
		{"/command", `{"command":"/config unset agent_loop.max_tool_loops"}`},
	} {
		w := postJSON(g.mux, tc.endpoint, tc.body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "cfg-1") {
			t.Errorf("%s %s: %d %s", tc.endpoint, tc.body, w.Code, w.Body.String())
		}
	}
	if g.file(t) != approveTestConfig {
		t.Errorf("file changed over HTTP:\n%s", g.file(t))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	want := []string{
		testSessionKey + " set agent_loop.max_tool_loops 20",
		testSessionKey + " set agent_loop.max_tool_loops 20",
		testSessionKey + " set agent_loop.max_tool_loops 20",
		testSessionKey + " unset agent_loop.max_tool_loops ",
	}
	if strings.Join(g.requests, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %q, want %q", g.requests, want)
	}
}

// TestConfigSetFromChat_StillWrites proves a chat (platform) dispatch of the
// same command keeps writing directly, without approval.
func TestConfigSetFromChat_StillWrites(t *testing.T) {
	t.Parallel()
	g := newGatingRig(t)
	req := command.RequestFromText("/config set agent_loop.max_tool_loops=20", testSessionKey, "u", 1)
	req.Source = command.SourceTelegram
	if _, ok, err := g.inst.cmds.Dispatch(context.Background(), req, g.inst.cc); !ok || err != nil {
		t.Fatalf("dispatch ok=%v err=%v", ok, err)
	}
	if !strings.Contains(g.file(t), "max_tool_loops = 20") {
		t.Errorf("chat set did not write:\n%s", g.file(t))
	}
	if len(g.requests) != 0 {
		t.Error("chat set asked for approval")
	}
}

// TestFileWizardsRefusedOverHTTP proves the wizards that write foci.toml —
// /config set without a value, /agents new, /android — are refused over
// HTTP and start no wizard, while /config get still answers.
func TestFileWizardsRefusedOverHTTP(t *testing.T) {
	t.Parallel()
	g := newGatingRig(t)
	for _, cmd := range []string{"/config set", "/config set agent_loop", "/agents new", "/android"} {
		w := postJSON(g.mux, "/command", `{"command":"`+cmd+`"}`)
		if !strings.Contains(w.Body.String(), "must be started from chat") {
			t.Errorf("%s: %s", cmd, w.Body.String())
		}
		if g.inst.cmds.WizardActive(testSessionKey) {
			t.Errorf("%s started a wizard over HTTP", cmd)
		}
	}
	w := postJSON(g.mux, "/command", `{"command":"/config get test-agent"}`)
	if !strings.Contains(w.Body.String(), "Live-settable keys for test-agent") {
		t.Errorf("/config get over HTTP: %s", w.Body.String())
	}
	if g.file(t) != approveTestConfig {
		t.Error("file changed")
	}
}

// TestLiveApply_UnprotectedFileAppliesNothing proves live apply fails closed:
// with the protection check failing, Apply returns an error and the applier
// never runs.
func TestLiveApply_UnprotectedFileAppliesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(approveTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	la := newLiveApply(path, func(string) error { return errors.New("config file is not protected") })
	ran := false
	la.register([]string{"logging.level"}, func(*config.Config) error { ran = true; return nil })
	if _, err := la.Apply("logging", "level"); err == nil || !strings.Contains(err.Error(), "not protected") {
		t.Fatalf("Apply err = %v", err)
	}
	if ran {
		t.Error("the applier ran on an unprotected file")
	}
}

// TestLiveRules_UnprotectedFileKeepsPrevious proves the delegated rules
// reload fails closed: with the protection check failing, the previous
// rules are kept.
func TestLiveRules_UnprotectedFileKeepsPrevious(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(approveTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	startup := []string{"startup-rule"}
	loaded := false
	src := liveRules(testAgentID, path, func(string) error { return errors.New("config file is not protected") }, "pretool", startup,
		func(*config.Config, string) ([]string, []string, bool) {
			loaded = true
			return []string{"fresh"}, nil, true
		})
	if got := src(); len(got) != 1 || got[0] != "startup-rule" {
		t.Errorf("rules = %v, want the previous set", got)
	}
	if loaded {
		t.Error("rules resolved from an unprotected file")
	}
}

// TestConfigProtection_SkipAndPlatform proves the startup check's switch:
// skip_security_checks turns it off (the test harness relies on this), and
// otherwise it is the real check on Linux and off elsewhere.
func TestConfigProtection_SkipAndPlatform(t *testing.T) {
	t.Parallel()
	if configProtection(&config.Config{SkipSecurityChecks: true}) != nil {
		t.Error("skip_security_checks must turn the check off")
	}
	got := configProtection(&config.Config{}) != nil
	if got != config.ConfigProtectionEnforced() {
		t.Errorf("check enabled = %v, want %v on this platform", got, config.ConfigProtectionEnforced())
	}
	if err := checkConfigLoad(func(string) error { return errors.New("bad owner") }, "/x"); err == nil || !strings.Contains(err.Error(), "refusing to load") {
		t.Errorf("checkConfigLoad = %v", err)
	}
}
