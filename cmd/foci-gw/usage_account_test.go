package main

import (
	"encoding/json"
	"foci/internal/account"
	"foci/internal/ccaccount"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// The #3104 phase-3 gateway tests: the account an agent's sessions get from
// the real configureDelegated, the stored hold key rename (Q21), and the targets
// /usage and the spawn reserve read with.

// registerUsageAccountCatcher registers a hook-catching test backend whose
// Spec takes its Account from the real Claude Code Spec, returning
// where its instances record their HostHooks.
func registerUsageAccountCatcher(t *testing.T, name string) *delegator.HostHooks {
	t.Helper()
	cc, ok := delegator.SpecFor("claude-code")
	if !ok || cc.Account == nil {
		t.Fatal("the claude-code Spec has no Account")
	}
	if s, ok := delegator.SpecFor(name); ok {
		be, _ := s.New(nil)
		got := be.(*hookCatcher).hooks
		*got = delegator.HostHooks{}
		return got
	}
	got := &delegator.HostHooks{}
	spec := delegator.Spec{
		Name:         name,
		DisplayName:  name,
		New:          func(map[string]any) (delegator.Delegator, error) { return &hookCatcher{hooks: got}, nil },
		Prototype:    (*hookCatcher)(nil),
		Caps:         map[delegator.Capability]delegator.Support{},
		ModelcapsKey: name,
		LedgerKey:    name,
		Account:      cc.Account,
	}
	for _, c := range delegator.AllCapabilities() {
		spec.Caps[c] = delegator.No("test backend")
	}
	spec.Caps[delegator.CapHostHooks] = delegator.Yes()
	spec.Caps[delegator.CapRelogin] = delegator.Yes()
	delegator.Register(spec)
	return got
}

// setupUsageAgent configures agent id through the real configureDelegated on
// its own test backend, on the account of folder ("" = the host's), whose
// hold is reg's.
func setupUsageAgent(t *testing.T, reg *usagehold.Registry, id, folder string) loginAgent {
	t.Helper()
	backend := "test-usage-acct-" + id
	hooks := registerUsageAccountCatcher(t, backend)
	p, shared := wiringFixture(t)
	p.acfg.ID = id
	p.acfg.Backend = backend
	p.cfg.DataDir = t.TempDir()
	if gwAccounts.Holds != reg {
		gwAccounts = &account.Registry{Holds: reg, History: currentUsageHistory}
	}
	ag := &agent.Agent{AgentID: id}
	var bc config.BackendConfig
	if folder != "" {
		bc.Accounts = []string{folder}
	}
	if _, ok := configureDelegated(ag, p, shared, backend, bc); !ok {
		t.Fatal("configureDelegated returned ok=false")
	}
	t.Cleanup(ag.StopUsageHold)
	if _, err := ag.DelegatedManager.NewBackendFor(ag.AccountFor("")); err != nil {
		t.Fatal(err)
	}
	h := *hooks
	return loginAgent{ag: ag, hooks: &h}
}

// TestUsageWiring_HoldPerAccount (H1 through the gateway wiring): a usage
// window beta's backend reports holds beta and beta2 (one folder, spelled
// two ways) and not alpha (host folder); a rejected window stops B and alpha
// still runs.
func TestUsageWiring_HoldPerAccount(t *testing.T) {
	isolateHostCC(t)
	reg := usagehold.New()
	ccB := filepath.Join(t.TempDir(), "ccB")
	alpha := setupUsageAgent(t, reg, "alpha", "")
	beta := setupUsageAgent(t, reg, "beta", ccB)
	beta2 := setupUsageAgent(t, reg, "beta2", ccB+"/")

	if beta.hooks.OnUsageWindow == nil {
		t.Fatal("beta's backend got no OnUsageWindow hook")
	}
	beta.hooks.OnUsageWindow(usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, ResetAt: time.Now().Add(2 * time.Hour)})
	if beta.ag.UsageHold("").Phase != usagehold.Hold || beta2.ag.UsageHold("").Phase != usagehold.Hold {
		t.Errorf("B after beta's 99%% window: beta %v, beta2 %v; want both held", beta.ag.UsageHold("").Phase, beta2.ag.UsageHold("").Phase)
	}
	if alpha.ag.UsageHold("").Phase != usagehold.Open {
		t.Errorf("alpha (host account) %v after B's window, want open", alpha.ag.UsageHold("").Phase)
	}
	beta.hooks.OnUsageWindow(usagehold.Event{Window: "five_hour", Reached: true, ResetAt: time.Now().Add(2 * time.Hour)})
	if beta2.ag.UsageHold("").Phase != usagehold.Stop {
		t.Errorf("beta2 after B's rejected window: %v, want stop", beta2.ag.UsageHold("").Phase)
	}
	if blocked, why := alpha.ag.UsageHoldBlocks("", true); blocked {
		t.Errorf("alpha's keepalive blocked by B's stop: %s", why)
	}
	hostLoc, err := ccaccount.Host()
	if err != nil {
		t.Fatal(err)
	}
	host := hostLoc.Account
	if want := "claude-code:" + host.Key(); alpha.ag.AccountFor("").Key() != want {
		t.Errorf("alpha's account %q, want the host login's %q", alpha.ag.AccountFor("").Key(), want)
	}
	if name := beta.ag.AccountFor("").Name(); name != ccB {
		t.Errorf("beta's account name %q, want its folder %q (no email known)", name, ccB)
	}
}

// TestUsageTarget (H4 wiring): /usage and the spawn reserve read an account
// with the launch env of a session on it (the agent's env and the account's
// folder) and the agent's binary, the [cc_backend] binary folded in.
func TestUsageTarget(t *testing.T) {
	isolateHostCC(t)
	cfg := &config.Config{DataDir: t.TempDir()}
	cfg.CCBackend.Binary = "/opt/global-claude"
	spec, _ := delegator.SpecFor("claude-code")
	own := "/tmp/my-claude"
	beta := config.AgentConfig{ID: "beta", Backend: "claude-code",
		BackendConfig: config.BackendConfig{Binary: &own, Env: map[string]string{"OTHER": "1"}, Accounts: []string{"/tmp/ccB"}}}
	got := usageTarget(cfg, beta, agentAccounts(cfg, spec, "beta", beta.BackendConfig.Accounts)[0])
	if got.AgentID != "beta" || got.Binary != own || got.Env["CLAUDE_CONFIG_DIR"] != "/tmp/ccB" || got.Env["OTHER"] != "1" {
		t.Errorf("beta's target = %+v, want its env, its account's folder and its binary", got)
	}
	alpha := config.AgentConfig{ID: "alpha", Backend: "claude-code"}
	if got := usageTarget(cfg, alpha, agentAccounts(cfg, spec, "alpha", nil)[0]); got.Binary != "/opt/global-claude" || got.Env != nil {
		t.Errorf("alpha's target = %+v, want the [cc_backend] binary and no env", got)
	}
}

// seedOldHold writes a main-branch usage_hold:claude-code row: the five-hour
// window at 99%, lifting at lift.
func seedOldHold(t *testing.T, idx *session.SessionIndex, lift time.Time) string {
	t.Helper()
	b, _ := json.Marshal([]usagehold.Window{{Name: "five_hour", Util: 0.99, ResetAt: lift}})
	if err := idx.SetAgentMetadata(usageHoldAgentID, usageHoldMetaPrefix+"claude-code", string(b)); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// usageHoldRows lists the stored hold keys.
func usageHoldRows(t *testing.T, idx *session.SessionIndex) map[string]string {
	t.Helper()
	rows, err := idx.AgentMetadataByPrefix(usageHoldAgentID, usageHoldMetaPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestUsageHoldMigration (H6, requirement 6): a main-branch
// usage_hold:claude-code hold moves at startup to the host account's key. The
// no-override agent is held until its lift time, the per-folder agent is
// open, and no old row remains. A second start changes nothing. A store with
// no old row is left untouched.
func TestUsageHoldMigration(t *testing.T) {
	isolateHostCC(t)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	lift := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	old := seedOldHold(t, idx, lift)
	ccB := filepath.Join(t.TempDir(), "ccB")
	hostLoc, err := ccaccount.Host()
	if err != nil {
		t.Fatal(err)
	}
	host := hostLoc.Account
	newKey := usageHoldMetaPrefix + "claude-code:" + host.Key()

	start := func() (alpha, beta loginAgent) {
		migrateUsageHoldKeys(idx)
		reg := usagehold.New()
		reg.SetStore(usageHoldStore{idx: idx})
		return setupUsageAgent(t, reg, "alpha", ""), setupUsageAgent(t, reg, "beta", ccB)
	}
	check := func(when string, alpha, beta loginAgent) {
		t.Helper()
		st := alpha.ag.UsageHold("")
		if st.Phase != usagehold.Hold || !st.LiftAt.Equal(lift) {
			t.Errorf("%s: alpha %v until %v, want held until %v", when, st.Phase, st.LiftAt, lift)
		}
		if beta.ag.UsageHold("").Phase != usagehold.Open {
			t.Errorf("%s: beta %v, want open", when, beta.ag.UsageHold("").Phase)
		}
		rows := usageHoldRows(t, idx)
		if _, ok := rows[usageHoldMetaPrefix+"claude-code"]; ok {
			t.Errorf("%s: the old usage_hold:claude-code row remains", when)
		}
		if rows[newKey] != old {
			t.Errorf("%s: rows %v, want the old value under %q", when, rows, newKey)
		}
	}
	alpha, beta := start()
	check("first start", alpha, beta)
	alpha.ag.StopUsageHold()
	beta.ag.StopUsageHold()
	alpha, beta = start()
	check("restart", alpha, beta)

	// No old row: nothing is written.
	empty, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { empty.Close() })
	migrateUsageHoldKeys(empty)
	if rows := usageHoldRows(t, empty); len(rows) != 0 {
		t.Errorf("startup with no old row wrote %v", rows)
	}
}

// TestUsageHoldMigration_KeepsNewerState: a hold already stored under the new
// key is not overwritten by an old row (a downgrade wrote one again); the old
// row is dropped.
func TestUsageHoldMigration_KeepsNewerState(t *testing.T) {
	isolateHostCC(t)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	hostLoc, err := ccaccount.Host()
	if err != nil {
		t.Fatal(err)
	}
	host := hostLoc.Account
	newKey := usageHoldMetaPrefix + "claude-code:" + host.Key()
	if err := idx.SetAgentMetadata(usageHoldAgentID, newKey, "NEWER"); err != nil {
		t.Fatal(err)
	}
	seedOldHold(t, idx, time.Now().Add(time.Hour))
	migrateUsageHoldKeys(idx)
	rows := usageHoldRows(t, idx)
	if len(rows) != 1 || rows[newKey] != "NEWER" {
		t.Errorf("rows %v, want only the newer state under %q", rows, newKey)
	}
}
