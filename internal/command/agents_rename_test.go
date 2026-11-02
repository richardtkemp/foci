package command

import (
	"context"
	"strings"
	"testing"
)

func TestParseAgentRename(t *testing.T) {
	cases := []struct {
		args string
		ok   bool
		want AgentRenameArgs
	}{
		{"bob rob", true, AgentRenameArgs{Old: "bob", New: "rob"}},
		{"bob rob --yes", true, AgentRenameArgs{Old: "bob", New: "rob", Yes: true}},
		{"--move-workspace bob rob -y", true, AgentRenameArgs{Old: "bob", New: "rob", Move: true, Yes: true}},
		{"bob", false, AgentRenameArgs{}},
		{"bob rob extra", false, AgentRenameArgs{}},
		{"bob rob --force", false, AgentRenameArgs{}},
	}
	for _, c := range cases {
		got, ok := parseAgentRename(c.args)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseAgentRename(%q) = %+v, %v; want %+v, %v", c.args, got, ok, c.want, c.ok)
		}
	}
}

// /agents rename routes to AgentRenameFn with the source marked: over HTTP
// the gateway must ask for approval instead of running it.
func TestAgentsRenameRoutes(t *testing.T) {
	var got AgentRenameArgs
	cc := CommandContext{AgentRenameFn: func(_ context.Context, a AgentRenameArgs) string { got = a; return "ok" }}
	cmd := AgentsCommand()
	resp, err := cmd.Execute(context.Background(), Request{Args: "rename bob rob --yes", SessionKey: "bob/c1", Source: SourceHTTP}, cc)
	if err != nil || resp.Text != "ok" {
		t.Fatalf("resp = %q, %v", resp.Text, err)
	}
	if got.Old != "bob" || got.New != "rob" || !got.Yes || !got.FromHTTP || got.Session != "bob/c1" {
		t.Errorf("args = %+v", got)
	}
	resp, _ = cmd.Execute(context.Background(), Request{Args: "rename bob"}, cc)
	if !strings.HasPrefix(resp.Text, "Usage: /agents rename") {
		t.Errorf("malformed: %q", resp.Text)
	}
}
