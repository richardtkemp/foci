package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/ccaccount"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/fociroot"
	"foci/internal/procx"
	"foci/internal/relogin"
)

// The #3104 phase-2 gateway wiring tests: which gate an agent's login uses,
// which environment and tmux session its login pane gets, which accounts the
// startup check probes, and where each account's expiry notice goes.

// isolateHostCC gives a test a single-home install with no CC variables in
// this process and no operator overlay. The tests here change process-wide
// state (env, reloginRun, relogin.G), so none of them runs in parallel.
func isolateHostCC(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(fociroot.EnvVar, "")
	t.Setenv(fociroot.CCConfigDirEnv, "")
	t.Setenv(fociroot.CCSecureStorageEnv, "")
	_ = os.Unsetenv(fociroot.CCSecureStorageEnv)
	prev := procx.OperatorOverlay()
	procx.SetOperatorEnv(nil)
	t.Cleanup(func() {
		m := map[string]string{}
		for _, kv := range prev {
			if k, v, ok := strings.Cut(kv, "="); ok {
				m[k] = v
			}
		}
		procx.SetOperatorEnv(m)
	})
	prevAccounts := gwLoginAccounts
	gwLoginAccounts = &loginAccounts{}
	t.Cleanup(func() { gwLoginAccounts = prevAccounts })
	return home
}

// captureRelogin replaces reloginRun with a recorder of each Config, which
// releases the gate at once as the real driver does on its way out.
func captureRelogin(t *testing.T) func() []relogin.Config {
	t.Helper()
	var mu sync.Mutex
	var got []relogin.Config
	prev := reloginRun
	reloginRun = func(_ context.Context, c relogin.Config) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
		c.Gate.Release()
		c.Gate.Complete()
	}
	t.Cleanup(func() { reloginRun = prev })
	return func() []relogin.Config {
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			out := append([]relogin.Config(nil), got...)
			mu.Unlock()
			if len(out) > 0 || time.Now().After(deadline) {
				return out
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// loginAgent is one agent configured by the real configureDelegated on a
// test backend with the Claude Code login, and the HostHooks its backends get.
type loginAgent struct {
	ag    *agent.Agent
	hooks *delegator.HostHooks
}

func setupLoginAgent(t *testing.T, backend, id string, env map[string]string) loginAgent {
	t.Helper()
	hooks := registerHookCatcher(backend, delegator.CapHostHooks, delegator.CapRelogin)
	p, shared := wiringFixture(t)
	p.acfg.ID = id
	p.acfg.Backend = backend
	p.cfg.DataDir = t.TempDir()
	ag := &agent.Agent{AgentID: id}
	if _, ok := configureDelegated(ag, p, shared, backend, config.BackendConfig{Env: env}); !ok {
		t.Fatal("configureDelegated returned ok=false")
	}
	if _, err := ag.DelegatedManager.NewBackend(); err != nil {
		t.Fatal(err)
	}
	h := *hooks
	return loginAgent{ag: ag, hooks: &h}
}

// TestLoginWiring_GatePerAccount (H3 wiring, requirement 3): agents with no
// override get relogin.G; agents on one folder share a gate of their own;
// another folder gets another gate.
func TestLoginWiring_GatePerAccount(t *testing.T) {
	isolateHostCC(t)
	ccB, ccC := filepath.Join(t.TempDir(), "ccB"), filepath.Join(t.TempDir(), "ccC")
	alpha := setupLoginAgent(t, "test-login-acct-a", "alpha", nil)
	beta := setupLoginAgent(t, "test-login-acct-b", "beta", map[string]string{"CLAUDE_CONFIG_DIR": ccB})
	beta2 := setupLoginAgent(t, "test-login-acct-b2", "beta2", map[string]string{"CLAUDE_CONFIG_DIR": ccB + "/"})
	gamma := setupLoginAgent(t, "test-login-acct-c", "gamma", map[string]string{"CLAUDE_CONFIG_DIR": ccC})
	if alpha.ag.LoginGateOrDefault() != relogin.G {
		t.Error("alpha (no override) is not on relogin.G")
	}
	if beta.ag.LoginGate == nil || beta.ag.LoginGate == relogin.G || beta.ag.LoginGate != beta2.ag.LoginGate {
		t.Error("beta and beta2 (one folder) do not share a gate of their own")
	}
	if gamma.ag.LoginGate == beta.ag.LoginGate || gamma.ag.LoginGate == relogin.G {
		t.Error("gamma (another folder) shares a gate")
	}
	if got := beta.ag.LoginGate.Name(); got != ccB {
		t.Errorf("beta's account named %q, want its folder %q until the email is known", got, ccB)
	}
	beta.hooks.OnLoginEmail("b@example.com")
	if got := beta2.ag.LoginGate.Name(); got != "b@example.com" {
		t.Errorf("beta2's account named %q after the probe's email, want b@example.com", got)
	}
	if relogin.G.Name() == "b@example.com" {
		t.Error("beta's email named the host account")
	}
}

// TestLoginWiring_PaneEnvAndSession (H2, H6, requirement 2, F2): a login
// started by beta's 401 or by /login in beta's chat runs on beta's gate, in
// beta's own tmux session, with beta's folder; alpha's runs on relogin.G in
// "foci-cc-login" with exactly fociroot.CCEnv(), as on main.
func TestLoginWiring_PaneEnvAndSession(t *testing.T) {
	isolateHostCC(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	alpha := setupLoginAgent(t, "test-login-pane-a", "alpha", nil)
	beta := setupLoginAgent(t, "test-login-pane-b", "beta", map[string]string{"CLAUDE_CONFIG_DIR": ccB})

	runs := captureRelogin(t)
	beta.hooks.OnAuthFailure("Failed to authenticate. API Error: 401")
	got := runs()
	if len(got) != 1 {
		t.Fatalf("beta's 401 started %d logins, want 1", len(got))
	}
	c := got[0]
	if c.Gate != beta.ag.LoginGate || c.Session == relogin.DefaultSession || !strings.HasPrefix(c.Session, relogin.DefaultSession+"-") {
		t.Errorf("beta's login: gate %p (want %p), session %q", c.Gate, beta.ag.LoginGate, c.Session)
	}
	if !slices.Contains(c.Env, "CLAUDE_CONFIG_DIR="+ccB) {
		t.Errorf("beta's login pane env %q lacks its folder", c.Env)
	}

	runs = captureRelogin(t)
	if !beta.ag.ReloginTrigger("/login", "app/beta") {
		t.Fatal("/login in beta's chat started nothing")
	}
	if got := runs(); len(got) != 1 || !slices.Contains(got[0].Env, "CLAUDE_CONFIG_DIR="+ccB) || got[0].Gate != beta.ag.LoginGate {
		t.Errorf("/login in beta's chat: %+v", got)
	}

	runs = captureRelogin(t)
	if !alpha.ag.ReloginTrigger("/login", "app/alpha") {
		t.Fatal("/login in alpha's chat started nothing")
	}
	got = runs()
	if len(got) != 1 || got[0].Gate != relogin.G || got[0].Session != relogin.DefaultSession || !slices.Equal(got[0].Env, fociroot.CCEnv()) {
		t.Errorf("alpha's login: %+v, want relogin.G, %q, env %q", got, relogin.DefaultSession, fociroot.CCEnv())
	}
}

// TestLoginWiring_MacSeparateRoot (H1, requirement 1, Q12): on a simulated
// separate-root install, beta's login pane unsets the gateway's empty
// CLAUDE_SECURESTORAGE_CONFIG_DIR; alpha's keeps it; gamma's own empty value
// is kept.
func TestLoginWiring_MacSeparateRoot(t *testing.T) {
	isolateHostCC(t)
	t.Setenv(fociroot.EnvVar, filepath.Join(t.TempDir(), "foci"))
	if _, err := fociroot.EnsureCCConfigEnv(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   string
		env  map[string]string
		want string
	}{
		{"alpha", nil, fociroot.CCSecureStorageEnv + "="},
		{"beta", map[string]string{"CLAUDE_CONFIG_DIR": "/x"}, fociroot.CCSecureStorageEnv},
		{"gamma", map[string]string{"CLAUDE_CONFIG_DIR": "/y", "CLAUDE_SECURESTORAGE_CONFIG_DIR": ""}, fociroot.CCSecureStorageEnv + "="},
	} {
		a := setupLoginAgent(t, "test-login-mac-"+c.id, c.id, c.env)
		runs := captureRelogin(t)
		a.ag.ReloginTrigger("/login", "app/"+c.id)
		got := runs()
		if len(got) != 1 {
			t.Fatalf("%s: %d logins", c.id, len(got))
		}
		var secure []string
		for _, kv := range got[0].Env {
			if strings.HasPrefix(kv, fociroot.CCSecureStorageEnv) {
				secure = append(secure, kv)
			}
		}
		if len(secure) != 1 || secure[0] != c.want {
			t.Errorf("%s: pane secure-storage entries %q, want [%q]", c.id, secure, c.want)
		}
	}
}

// readyCounter is a backend whose CheckReady counts its calls.
type readyCounter struct {
	delegator.Delegator
	mu    *sync.Mutex
	calls *[]string
	id    string
}

func (r *readyCounter) CheckReady(context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.calls = append(*r.calls, r.id)
	return true, nil
}

// TestCheckDelegatedReadiness_OncePerAccount (H7, requirement 6, Q19): each
// account is probed once, by the first agent on it: alpha for the host
// account, beta for account B (beta2 shares it), gamma for C.
func TestCheckDelegatedReadiness_OncePerAccount(t *testing.T) {
	spec := delegator.Spec{Caps: map[delegator.Capability]delegator.Support{delegator.CapUnstartedReadinessProbe: delegator.Yes()}}
	var mu sync.Mutex
	var calls []string
	gateB, gateC := &relogin.Gate{}, &relogin.Gate{}
	agents := map[string]*agentInstance{}
	order := []string{"alpha", "beta", "beta2", "gamma"}
	for id, g := range map[string]*relogin.Gate{"alpha": nil, "beta": gateB, "beta2": gateB, "gamma": gateC} {
		ag := &agent.Agent{AgentID: id, LoginGate: g}
		ag.ReloginTrigger = func(string, string) bool { return true }
		ag.DelegatedManager = &agent.DelegatedManager{
			Spec:       spec,
			NewBackend: func() (delegator.Delegator, error) { return &readyCounter{mu: &mu, calls: &calls, id: id}, nil },
		}
		agents[id] = &agentInstance{id: id, ag: ag}
	}
	checkDelegatedReadiness(context.Background(), agents, order)
	slices.Sort(calls)
	if want := []string{"alpha", "beta", "gamma"}; !slices.Equal(calls, want) {
		t.Errorf("probed %q, want one agent per account %q", calls, want)
	}
}

// TestLoginAccounts_ExpiryPerAccount (H8, H9, requirement 7): the host account
// keeps data/cc-login-expiry.json, so a stage main already sent is not sent
// again; another account has its own file, its notice names it, and the
// master gets a notice only for its own account.
func TestLoginAccounts_ExpiryPerAccount(t *testing.T) {
	home := isolateHostCC(t)
	data := t.TempDir()
	cfg := &config.Config{DataDir: data, MasterAgent: "alpha", Agents: []config.AgentConfig{{ID: "alpha"}}}
	now := time.Now()
	hostDate := now.Add(9 * 24 * time.Hour)
	// The file main wrote: stage 1 (the first notice) sent for hostDate.
	legacy, _ := json.Marshal(map[string]any{"expires_at": hostDate, "stage": 1})
	if err := os.WriteFile(filepath.Join(data, "cc-login-expiry.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := gwLoginAccounts.forAgent(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ccB := filepath.Join(t.TempDir(), "ccB")
	b, err := gwLoginAccounts.forAgent(cfg, map[string]string{"CLAUDE_CONFIG_DIR": ccB})
	if err != nil {
		t.Fatal(err)
	}
	if host.gate != relogin.G || host.warner == b.warner || b.gate == relogin.G {
		t.Fatal("host and B share login state")
	}
	var sent []string
	send := func(n string) bool { sent = append(sent, n); return true }
	host.warner.Observe(hostDate, now, send)
	if len(sent) != 0 {
		t.Errorf("upgrade re-sent a stage main recorded: %q", sent)
	}
	b.warner.Observe(now.Add(2*24*time.Hour), now, send)
	if len(sent) != 1 || !strings.Contains(sent[0], "(account: "+ccB+")") {
		t.Errorf("B's notice %q, want one naming %s", sent, ccB)
	}
	if _, err := os.Stat(filepath.Join(data, "cc-login-expiry-"+b.account.ShortKey()+".json")); err != nil {
		t.Errorf("B's state is not in its own file: %v", err)
	}
	if got := masterOnAccount(cfg, host.account); got != "alpha" {
		t.Errorf("master on the host account: %q, want alpha", got)
	}
	if got := masterOnAccount(cfg, b.account); got != "" {
		t.Errorf("master for account B: %q, want none (B's notice goes to an agent on B)", got)
	}
	if got := loginExpiryTarget(masterOnAccount(cfg, b.account), "beta"); got != "beta" {
		t.Errorf("B's notice target %q, want beta", got)
	}
	_ = home
	if acct, _ := ccaccount.ForAgent(nil); acct.Account != host.account {
		t.Errorf("host account %+v, ForAgent(nil) %+v", host.account, acct.Account)
	}
}
