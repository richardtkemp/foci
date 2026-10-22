package pretool

import "testing"

// checkDefaultVerdicts runs Bash calls against the resolved defaults: each
// deny call must be denied by rule, and each allow call by no default at
// all. when-checks run for real, so any that fails open is an error.
func checkDefaultVerdicts(t *testing.T, rule string, deny, allow []string) {
	t.Helper()
	rules, skipped := Resolve(Defaults)
	if len(skipped) > 0 {
		t.Fatalf("defaults skipped: %v", skipped)
	}
	for _, cmd := range deny {
		res := Match(rules, bashCall(cmd))
		if res.Rule == nil || res.Rule.Name != rule {
			t.Errorf("%q: matched %v, want %s", cmd, ruleName(res), rule)
		}
		if len(res.WhenErrors) > 0 {
			t.Errorf("%q: when errors %v", cmd, res.WhenErrors)
		}
	}
	for _, cmd := range allow {
		res := Match(rules, bashCall(cmd))
		if res.Rule != nil {
			t.Errorf("%q: denied by %s, want allowed", cmd, res.Rule.Name)
		}
		if len(res.WhenErrors) > 0 {
			t.Errorf("%q: when errors %v", cmd, res.WhenErrors)
		}
	}
}

func ruleName(res Result) string {
	if res.Rule == nil {
		return "nothing"
	}
	return res.Rule.Name
}

// TestDefaults_KillByPattern (#2441): killing by full command line matches
// the calling shell, whose argv holds the pattern. Every killing shape is
// denied; -A, a process-name match, a pgrep that only lists, and kill by PID
// are not.
func TestDefaults_KillByPattern(t *testing.T) {
	checkDefaultVerdicts(t, "kill_by_pattern",
		[]string{
			// The ticket's repro.
			`pgrep -f "make -C /tmp/rsscap/wt integration"; kill $(pgrep -f "make -C /tmp/rsscap/wt integration")`,
			`pkill -f "make -C /tmp/x"`,
			`pkill -9 -f foo`,
			`pkill -fx foo`,
			`pkill --full foo`,
			`sudo pkill -f foo`,
			`/usr/bin/pkill -f foo`,
			`pkill foo -f`,
			"kill `pgrep -f foo`",
			`kill -9 $(pgrep -fa foo | awk '{print $1}')`,
			`kill $(sudo pgrep -f foo)`,
			`pgrep -f foo | xargs kill`,
			`pgrep -f foo | xargs -r kill -9`,
			`pgrep -f foo | xargs sudo kill`,
			`pgrep -f foo | sort | xargs kill`,
		},
		[]string{
			`pkill -A -f foo`,
			`pkill -Af foo`,
			`pkill -fA foo`,
			`pkill --ignore-ancestors -f foo`,
			`kill $(pgrep -Af foo)`,
			`pgrep -Af foo | xargs kill`,
			`pkill foo`,
			`pkill -9 foo`,
			`kill $(pgrep foo)`,
			`pgrep -f foo`,
			`pgrep -af foo | head`,
			`pgrep -f foo | wc -l`,
			`pkill -- -f`,
			`ps -eo pid,args | grep "[f]oo"`,
			`kill 12345`,
			`kill -9 12345 12346`,
			`git commit -m 'deny pkill -f and kill $(pgrep -f x)'`,
			`echo pkill -f foo`,
		})
}
