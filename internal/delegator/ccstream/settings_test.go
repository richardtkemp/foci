package ccstream

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// launchedSkillSync returns the syncClaudeAiSkills value in a launch's
// --settings, failing when the flag or the key is missing.
func launchedSkillSync(t *testing.T, args []string) (sync bool, settings map[string]any) {
	t.Helper()
	raw := flagValue(args, "--settings")
	if raw == "" {
		t.Fatalf("no --settings in launch args %q", args)
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("--settings %q: %v", raw, err)
	}
	v, ok := settings[syncClaudeAISkillsKey].(bool)
	if !ok {
		t.Fatalf("--settings %q has no boolean %s", raw, syncClaudeAISkillsKey)
	}
	return v, settings
}

// TestStart_ClaudeAISkills proves every CC launch carries syncClaudeAiSkills
// (#2953): false with no hook (the default, also when foci's hook binary is
// missing and no hook settings are built), the hook's value otherwise, read
// again at each Start so a config edit reaches the next session, and next to
// a read-only batch's ask rule.
func TestStart_ClaudeAISkills(t *testing.T) {
	t.Run("no hook: hidden", func(t *testing.T) {
		got, _ := launchedSkillSync(t, launchArgs(t, map[string]any{}, false, delegator.BatchAccessDefault))
		if got {
			t.Errorf("syncClaudeAiSkills = true with no hook, want false")
		}
	})
	t.Run("hook read at each start", func(t *testing.T) {
		on := false
		hooks := &delegator.HostHooks{ClaudeAISkills: func() bool { return on }}
		for _, want := range []bool{false, true, false} {
			on = want
			got, _ := launchedSkillSync(t, launchArgsWithHooks(t, map[string]any{}, false, delegator.BatchAccessDefault, hooks))
			if got != want {
				t.Errorf("syncClaudeAiSkills = %v, want %v", got, want)
			}
		}
	})
	t.Run("read-only batch keeps its ask rule", func(t *testing.T) {
		got, settings := launchedSkillSync(t, launchArgs(t, map[string]any{}, true, delegator.BatchAccessReadOnly))
		if got {
			t.Errorf("syncClaudeAiSkills = true, want false")
		}
		if _, ok := settings["permissions"]; !ok {
			t.Errorf("read-only batch --settings lost permissions: %v", settings)
		}
	})
}

// TestLaunchSettings keeps what the settings already hold and adds the key.
func TestLaunchSettings(t *testing.T) {
	t.Parallel()
	hooks, err := buildHookSettingsJSON("/bin/hook", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, in string
		on       bool
	}{
		{"empty, off", "", false},
		{"empty, on", "", true},
		{"hooks, off", hooks, false},
		{"hooks, on", hooks, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := launchSettings(c.in, c.on)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("%q: %v", out, err)
			}
			if got[syncClaudeAISkillsKey] != c.on {
				t.Errorf("%s = %v, want %v", syncClaudeAISkillsKey, got[syncClaudeAISkillsKey], c.on)
			}
			if _, ok := got["hooks"]; ok != (c.in != "") {
				t.Errorf("hooks present = %v, want %v: %s", ok, c.in != "", out)
			}
		})
	}
	if _, err := launchSettings("{not json", false); err == nil {
		t.Error("launchSettings accepted invalid JSON")
	}
}
