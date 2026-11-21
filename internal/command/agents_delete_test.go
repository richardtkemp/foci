package command

import (
	"context"
	"strings"
	"testing"
)

// /agents delete, rm and remove route to AgentDeleteFn with the requesting
// session and source; HTTP and the agent's own chat are refused before it.
func TestAgentsDeleteRoutes(t *testing.T) {
	var got []AgentDeleteArgs
	cc := CommandContext{AgentDeleteFn: func(_ context.Context, a AgentDeleteArgs) string { got = append(got, a); return "asked" }}
	cmd := AgentsCommand()
	run := func(args, session, source string) string {
		resp, err := cmd.Execute(context.Background(), Request{Args: args, SessionKey: session, Source: source}, cc)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Text
	}
	for _, verb := range []string{"delete", "rm", "remove", "RM"} {
		got = nil
		if txt := run(verb+" bob", "clutch/c1", "app"); txt != "asked" {
			t.Errorf("%s: reply %q", verb, txt)
		}
		if len(got) != 1 || got[0] != (AgentDeleteArgs{ID: "bob", Session: "clutch/c1", Source: "app"}) {
			t.Errorf("%s: args %+v", verb, got)
		}
	}

	got = nil
	if txt := run("delete bob", "clutch/c1", SourceHTTP); !strings.Contains(txt, "HTTP API") {
		t.Errorf("HTTP not refused: %q", txt)
	}
	if txt := run("delete bob", "bob/c1", SourceTelegram); !strings.Contains(txt, "cannot delete itself") {
		t.Errorf("own chat not refused: %q", txt)
	}
	for _, bad := range []string{"delete", "delete bob rob", "rm --yes"} {
		if txt := run(bad, "clutch/c1", "app"); !strings.HasPrefix(txt, "Usage: /agents delete") {
			t.Errorf("%q: %q", bad, txt)
		}
	}
	if len(got) != 0 {
		t.Errorf("a refused delete reached the gateway: %+v", got)
	}
}
