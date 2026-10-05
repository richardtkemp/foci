package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// runningBackend is a backend whose SubagentTracker reports a fixed list.
type runningBackend struct {
	*mockBackendDM
	running []delegator.RunningSubagent
}

func (b *runningBackend) RunningSubagents() []delegator.RunningSubagent { return b.running }

// runningAgent builds an agent whose session sk has the given backend list and
// spawns.
func runningAgent(sk string, subs []delegator.RunningSubagent, spawns ...tools.RunningSpawn) *Agent {
	tr := tools.NewSpawnTracker()
	for _, s := range spawns {
		tr.Add(sk, s, func() {})
	}
	mgr := &DelegatedManager{AgentID: "a", backends: map[string]*managedBackend{
		sk: {be: &runningBackend{mockBackendDM: &mockBackendDM{running: true}, running: subs}, sessionKey: sk},
	}}
	return &Agent{DelegatedManager: mgr, Spawns: tr}
}

func renderFor(a *Agent, tmpl, sk string) string {
	return a.renderStatusline(context.Background(), expandStatuslineTemplate(tmpl), statuslineInputs{
		now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), model: "m", platform: "app",
		sm: &sessionMeta{}, agent: a, sessionKey: sk,
	})
}

// TestStatuslineDefault_RunningLine proves the default header lists the
// session's running subagents, background commands and spawns on a [running]
// line (#2127), each with how long it has run.
func TestStatuslineDefault_RunningLine(t *testing.T) {
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	a := runningAgent("a/main",
		[]delegator.RunningSubagent{
			{ID: "t1", Description: "review the parser", Kind: delegator.SubagentKindAgent, Started: ago(3 * time.Minute)},
			{ID: "t2", Description: "", Command: "make test", Kind: delegator.SubagentKindCommand, Started: ago(90 * time.Second)},
		},
		tools.RunningSpawn{ID: "spawn-1", Description: "Find the bug", Started: ago(2 * time.Minute)},
	)
	got := renderFor(a, "", "a/main")
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[meta] ") {
		t.Fatalf("header = %q, want [meta] then [running]", got)
	}
	for _, want := range []string{
		"[running] subagents: 1 (review the parser 3m",
		"| bg_commands: 1 (make test 1m3",
		"| spawns: 1 (Find the bug 2m",
	} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("running line %q lacks %q", lines[1], want)
		}
	}

	// Another session's work is not listed, and the line drops when empty.
	if got := renderFor(a, "", "a/other"); strings.Contains(got, "[running]") {
		t.Errorf("other session header = %q, want no [running] line", got)
	}
}

// TestStatuslineRunning_CapsItems proves a long list is cut to the first few
// with a count of the rest, and long descriptions are truncated.
func TestStatuslineRunning_CapsItems(t *testing.T) {
	var subs []delegator.RunningSubagent
	for range 7 {
		subs = append(subs, delegator.RunningSubagent{Description: strings.Repeat("x", 60)})
	}
	got := runningAgent("a/main", subs).statusSubagents("a/main")
	if !strings.HasPrefix(got, "subagents: 7 (") || !strings.HasSuffix(got, "; +2 more)") {
		t.Errorf("statusSubagents = %q", got)
	}
	if strings.Contains(got, strings.Repeat("x", 41)) {
		t.Errorf("statusSubagents = %q, want descriptions truncated", got)
	}
}

// TestStatuslineCustom_IncludesDefault proves a custom template can keep the
// whole default header with {default} and add its own lines (#2127).
func TestStatuslineCustom_IncludesDefault(t *testing.T) {
	a := runningAgent("a/main", nil, tools.RunningSpawn{ID: "spawn-1", Description: "dig", Started: time.Now()})
	got := renderFor(a, "{default}\n[deploy] undeployed=3", "a/main")
	want := renderFor(a, "", "a/main") + "\n[deploy] undeployed=3"
	if got != want {
		t.Errorf("custom header:\n got: %q\nwant: %q", got, want)
	}
	if !strings.Contains(got, "[running] spawns: 1 (dig") {
		t.Errorf("custom header %q lacks the default [running] line", got)
	}
}
