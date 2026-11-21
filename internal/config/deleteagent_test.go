package config

import "testing"

func TestPlanDeleteAgent(t *testing.T) {
	in := `# top
master_agent = "a"

[[agents]]
id = "a"
name = "A"

[[agents]]
id = "b"
# b's comment
name = "B"

[agents.platforms]
x = 1

[[agents.platforms]]
id = "telegram"
bot = "b"

[[agents]]
id = "bb"
name = "BB"
`
	want := `# top
master_agent = "a"

[[agents]]
id = "a"
name = "A"

[[agents]]
id = "bb"
name = "BB"
`
	got, err := PlanDeleteAgent(in, "b")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	// The last block: removed up to the end of the file.
	got, err = PlanDeleteAgent(in, "bb")
	if err != nil {
		t.Fatal(err)
	}
	wantLast := `# top
master_agent = "a"

[[agents]]
id = "a"
name = "A"

[[agents]]
id = "b"
# b's comment
name = "B"

[agents.platforms]
x = 1

[[agents.platforms]]
id = "telegram"
bot = "b"
`
	if got != wantLast {
		t.Fatalf("last block, got:\n%q\nwant:\n%q", got, wantLast)
	}

	if _, err := PlanDeleteAgent(in, "nobody"); err == nil {
		t.Fatal("deleting an unknown agent succeeded")
	}
}

func TestAgentDeleteBlockers(t *testing.T) {
	cfg := &Config{MasterAgent: "a"}
	cfg.Askgw.DefaultAgent = "a"
	if got := AgentDeleteBlockers(cfg, "a"); len(got) != 2 {
		t.Fatalf("want master_agent and askgw blockers, got %v", got)
	}
	if got := AgentDeleteBlockers(cfg, "b"); len(got) != 0 {
		t.Fatalf("b has no blockers, got %v", got)
	}
}
