package command

import (
	"errors"
	"strings"
	"testing"

	"foci/internal/config"
)

// TestConfigWriteReplyReportsLiveApplyOutcome is #3243: every chat /config
// write form (wizard, direct set, per-agent set, direct unset, per-agent
// unset) states what the live applier actually did, not what the field's
// hot tag predicts. A failed apply and a hot-tagged key with no applier both
// read "Restart to take effect", with the reason; a successful apply reads
// "Applied live.". The applier is called once, after the write, with the
// write's target.
func TestConfigWriteReplyReportsLiveApplyOutcome(t *testing.T) {
	applyErr := errors.New("reload config: boom")
	outcomes := []struct {
		name    string
		live    bool
		err     error
		want    []string
		notWant []string
	}{
		{"applied", true, nil, []string{"Applied live."}, []string{"Restart"}},
		{"no applier", false, nil, []string{"Restart to take effect", "does not apply live"}, []string{"Applied live"}},
		{"apply failed", true, applyErr, []string{"Restart to take effect", "live apply failed", "boom"}, []string{"Applied live"}},
	}

	// Every key below is hot (NeedsRestart false), so the old schema-only
	// reply would say "Applied live." in all three outcomes.
	forms := []struct {
		name       string
		run        func(t *testing.T, deps ConfigSetDeps) string
		wantTarget config.SetTarget
	}{
		{"wizard", func(t *testing.T, deps ConfigSetDeps) string {
			w := newConfigSetWizard(deps)
			w.Handle("agent_loop")
			w.Handle("max_output_tokens")
			resp, done := w.Handle("32768")
			if !done {
				t.Fatal("wizard not done after value")
			}
			return resp
		}, config.SetTarget{Section: "agent_loop", Key: "max_output_tokens"}},
		{"direct set", func(t *testing.T, deps ConfigSetDeps) string {
			resp, err := ConfigSetDirect(deps, "agent_loop.max_output_tokens=32768")
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}, config.SetTarget{Section: "agent_loop", Key: "max_output_tokens"}},
		{"per-agent set", func(t *testing.T, deps ConfigSetDeps) string {
			return execConfigCommand(t, CommandContext{ConfigSetDeps: &deps}, "set clutch keepalive.interval=1m")
		}, config.SetTarget{Section: "agents", AgentID: "clutch", Key: "keepalive.interval"}},
		{"direct unset", func(t *testing.T, deps ConfigSetDeps) string {
			return execConfigCommand(t, CommandContext{ConfigSetDeps: &deps}, "unset resources.goroutine_monitor_enabled")
		}, config.SetTarget{Section: "resources", Key: "goroutine_monitor_enabled"}},
		{"per-agent unset", func(t *testing.T, deps ConfigSetDeps) string {
			return execConfigCommand(t, CommandContext{ConfigSetDeps: &deps}, "unset clutch keepalive.interval")
		}, config.SetTarget{Section: "agents", AgentID: "clutch", Key: "keepalive.interval"}},
	}

	for _, f := range forms {
		for _, o := range outcomes {
			t.Run(f.name+"/"+o.name, func(t *testing.T) {
				deps, _, _ := unsetDeps(t) // real temp file for the unset forms
				deps.SetInFileFn = func(string, config.SetTarget, string) (string, error) { return "", nil }
				var got []config.SetTarget
				deps.LiveApplyFn = func(target config.SetTarget) (bool, error) {
					got = append(got, target)
					return o.live, o.err
				}

				text := f.run(t, deps)

				for _, w := range o.want {
					if !strings.Contains(text, w) {
						t.Errorf("reply %q missing %q", text, w)
					}
				}
				for _, w := range o.notWant {
					if strings.Contains(text, w) {
						t.Errorf("reply %q must not contain %q", text, w)
					}
				}
				if len(got) != 1 || got[0] != f.wantTarget {
					t.Errorf("LiveApplyFn calls = %+v, want exactly [%+v]", got, f.wantTarget)
				}
			})
		}
	}
}

// TestConfigWriteReplyRestartFieldStaysRestart: a restart-only field reads
// "Restart to take effect." even if an applier claims success, and a write
// that fails never reaches the applier.
func TestConfigWriteReplyRestartFieldStaysRestart(t *testing.T) {
	deps := testConfigSetDeps(nil)
	deps.LookupFn = func(string) (config.ConfigField, bool) {
		return config.ConfigField{Section: "sec", Key: "key", Type: config.FieldInt, NeedsRestart: true}, true
	}
	deps.LiveApplyFn = func(config.SetTarget) (bool, error) { return true, nil }
	text, err := ConfigSetDirect(deps, "sec.key=5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Restart to take effect") || strings.Contains(text, "Applied live") {
		t.Errorf("restart field reply %q, want a restart line and no live claim", text)
	}

	calls := 0
	deps = testConfigSetDeps(func(string, config.SetTarget, string) (string, error) { return "", errors.New("disk full") })
	deps.LiveApplyFn = func(config.SetTarget) (bool, error) { calls++; return true, nil }
	if _, err := ConfigSetDirect(deps, "agent_loop.max_output_tokens=32768"); err == nil {
		t.Fatal("want the write error")
	}
	if calls != 0 {
		t.Errorf("LiveApplyFn called %d times after a failed write", calls)
	}
}
