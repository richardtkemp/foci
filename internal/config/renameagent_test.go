package config

import (
	"strings"
	"testing"
)

// TestPlanRenameAgent_EditsOnlyTheAgentsReferences proves the foci.toml side
// of an agent rename (#1281): the block id, master_agent, [askgw]
// default_agent and exact list elements change; the bot pin lands in an
// existing platforms block without a bot (and not in one that names a bot);
// the kept workspace is written after the id line without touching a
// commented-out workspace line; comments, other agents and look-alike ids
// (bobby) are untouched; the result still loads.
func TestPlanRenameAgent_EditsOnlyTheAgentsReferences(t *testing.T) {
	in := `master_agent = "bob" # the master
[askgw]
default_agent = "bob"

[[mcp_note]]
agents = ["bob", "bobby"]

[[agents]]
id = "bob"
# workspace = "/somewhere/else"
name = "Bob"

[agents.keepalive]
enabled = true

[[agents.platforms]]
id = "telegram"
facet_bots = ["bobling"]

[[agents.platforms]]
id = "discord"
bot = "custom"

[[agents]]
id = "bobby"
`
	got, err := PlanRenameAgent(in, AgentRename{Old: "bob", New: "rob", PinBots: []string{"telegram", "discord"}, KeepWorkspace: "/home/x/bob"})
	if err != nil {
		t.Fatal(err)
	}
	want := `master_agent = "rob" # the master
[askgw]
default_agent = "rob"

[[mcp_note]]
agents = ["rob", "bobby"]

[[agents]]
id = "rob"
workspace = "/home/x/bob"
# workspace = "/somewhere/else"
name = "Bob"

[agents.keepalive]
enabled = true

[[agents.platforms]]
id = "telegram"
bot = "bob"
facet_bots = ["bobling"]

[[agents.platforms]]
id = "discord"
bot = "custom"

[[agents]]
id = "bobby"
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestPlanRenameAgent_AppendsMissingPlatformBlockAndMovesPaths: a platform
// with no block gets a new [[agents.platforms]] block inside the agent's
// region (before the next agent), and a move rewrites the workspace path as a
// whole path or prefix — never a longer name — and never in a comment.
func TestPlanRenameAgent_AppendsMissingPlatformBlockAndMovesPaths(t *testing.T) {
	in := `welcome_file = "/h/bob/w.md" # was /h/bob
other = "/h/bobby/x"

[[agents]]
id = "bob"
workspace = "/h/bob"

[[agents]]
id = "bobby"
`
	got, err := PlanRenameAgent(in, AgentRename{Old: "bob", New: "rob", PinBots: []string{"telegram"}, MoveFrom: "/h/bob", MoveTo: "/h/rob"})
	if err != nil {
		t.Fatal(err)
	}
	want := `welcome_file = "/h/rob/w.md" # was /h/bob
other = "/h/bobby/x"

[[agents]]
id = "rob"
workspace = "/h/rob"

[[agents.platforms]]
id = "telegram"
bot = "bob"

[[agents]]
id = "bobby"
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	refs := FindPathRefs(in, "/h/bob")
	if len(refs) != 2 || refs[0].Line != 1 || refs[1].Line != 6 {
		t.Errorf("FindPathRefs = %+v, want lines 1 and 6", refs)
	}
	if !strings.Contains(RenameAgentCommand("bob"), "foci rename-agent bob <new-id>") {
		t.Error(RenameAgentCommand("bob"))
	}
}

// TestPlanRenameAgent_RefusesUnknownOrTakenIDs: nothing is planned for a
// missing old id or an existing new id.
func TestPlanRenameAgent_RefusesUnknownOrTakenIDs(t *testing.T) {
	in := "[[agents]]\nid = \"bob\"\n\n[[agents]]\nid = \"rob\"\n"
	if _, err := PlanRenameAgent(in, AgentRename{Old: "nobody", New: "x"}); err == nil {
		t.Error("missing old id accepted")
	}
	if _, err := PlanRenameAgent(in, AgentRename{Old: "bob", New: "rob"}); err == nil {
		t.Error("taken new id accepted")
	}
	if err := CheckAgentID("Rob"); err == nil {
		t.Error("CheckAgentID accepted an upper-case id")
	}
	if err := CheckAgentID("data"); err == nil {
		t.Error("CheckAgentID accepted a reserved id")
	}
	if err := CheckAgentID("rob-2"); err != nil {
		t.Error(err)
	}
}
