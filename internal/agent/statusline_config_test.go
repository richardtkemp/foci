package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"foci/internal/config"
)

// statuslineHeaderFor resolves agent "a" from a foci.toml fragment exactly as
// the gateway does (config.Resolve, read through LiveConfigFn) and returns the
// header composeTurnText renders for a turn arriving via trigger.
func statuslineHeaderFor(t *testing.T, tomlSrc, trigger string) string {
	t.Helper()
	var cfg config.Config
	if _, err := toml.Decode(tomlSrc, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("want one [[agents]] block, got %d", len(cfg.Agents))
	}
	rc := config.Resolve(&cfg, cfg.Agents[0])
	a := &Agent{AgentID: "a", LiveConfigFn: func() *config.ResolvedAgentConfig { return rc }}
	RegisterPlatformTrigger("telegram")
	ctx := WithTrigger(context.Background(), trigger)
	return a.composeTurnText(ctx, "a/main", "m", []string{"hi"}, nil).MetaPrefix
}

// TestStatusline_AgentDisplayKeyIsHonoured pins #2185's first question with
// clutch's real config shape: display.statusline inside [[agents]] reaches the
// header, and {default} keeps the default lines.
func TestStatusline_AgentDisplayKeyIsHonoured(t *testing.T) {
	got := statuslineHeaderFor(t, `
[[agents]]
id = "a"
display.statusline = """{default}
[deploy] ${echo undeployed=2}"""
`, "cron")
	if !strings.HasPrefix(got, "[meta] time=") {
		t.Errorf("header %q lacks the {default} [meta] line", got)
	}
	if !strings.HasSuffix(got, "\n[deploy] undeployed=2") {
		t.Errorf("header %q lacks the agent's [deploy] line", got)
	}
}

// TestStatusline_Precedence pins the documented cascade, most specific first:
// agent-platform > agent > global-platform > global > any platform's > default.
// The platform tiers apply only to a turn arriving via that platform.
func TestStatusline_Precedence(t *testing.T) {
	const (
		global     = "[display]\nstatusline = \"G\"\n"
		globalPlat = "[[platforms]]\nid = \"telegram\"\ndisplay.statusline = \"GP\"\n"
		agentHead  = "[[agents]]\nid = \"a\"\n"
		agent      = "display.statusline = \"A\"\n"
		agentPlat  = "[[agents.platforms]]\nid = \"telegram\"\ndisplay.statusline = \"AP\"\n"
	)
	cases := []struct {
		name, src, trigger, want string
	}{
		{"agent-platform beats agent", global + globalPlat + agentHead + agent + agentPlat, "telegram", "AP"},
		{"agent beats global-platform", global + globalPlat + agentHead + agent, "telegram", "A"},
		{"global-platform beats global", global + globalPlat + agentHead, "telegram", "GP"},
		{"global alone", global + agentHead, "telegram", "G"},
		{"agent-platform ignored off-platform", global + agentHead + agentPlat, "cron", "G"},
		{"agent beats global off-platform", global + agentHead + agent, "cron", "A"},
		{"global beats a platform's off-platform", global + globalPlat + agentHead, "cron", "G"},
		{"a platform's is the last fallback", globalPlat + agentHead, "cron", "GP"},
		{"{default} at agent-platform level", agentHead + "[[agents.platforms]]\nid = \"telegram\"\ndisplay.statusline = \"{default}\\n[x] y\"\n", "telegram", "\n[x] y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := statuslineHeaderFor(t, c.src, c.trigger)
			if strings.HasPrefix(c.want, "\n") {
				if !strings.HasPrefix(got, "[meta] ") || !strings.HasSuffix(got, c.want) {
					t.Errorf("header = %q, want the default then %q", got, c.want)
				}
				return
			}
			if got != c.want {
				t.Errorf("header = %q, want %q", got, c.want)
			}
		})
	}
}
