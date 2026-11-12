package configcheck

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestCheckContentAllowUnknown_ReturnsUnknownKeys proves the approval check
// (#3046) passes content whose only problem is a key this binary does not
// know, and returns that key for the prompt's warning.
func TestCheckContentAllowUnknown_ReturnsUnknownKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "foci.toml")
	unknown, err := CheckContentAllowUnknown(validConfigTOML+"\n[tools]\nfuture_key = \"2h\"\n", path)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !slices.Equal(unknown, []string{"tools.future_key"}) {
		t.Fatalf("unknown = %v, want [tools.future_key]", unknown)
	}
	unknown, err = CheckContentAllowUnknown(validConfigTOML, path)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("clean config: unknown = %v err = %v, want none", unknown, err)
	}
}

// TestCheckContentAllowUnknown_OtherFailuresStillFail proves only the
// unknown-key rule is relaxed: a known key with a wrong type (a load error)
// and a check that runs after the unknown-key rule (Slack credentials) still
// fail, even when the content also holds an unknown key.
func TestCheckContentAllowUnknown_OtherFailuresStillFail(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"wrong type": validConfigTOML + "\n[agent_loop]\nmax_tool_loops = \"many\"\n[tools]\nfuture_key = 1\n",
		"slack credentials": `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[tools]
future_key = 1

[[agents]]
id = "helen"

[[platforms]]
id = "slack"
[platforms.slack]
default_agent = "helen"
[[platforms.slack.channels]]
id = "C1"
agent = "helen"
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "foci.toml") // no secrets.toml beside it
			if _, err := CheckContentAllowUnknown(body, path); err == nil {
				t.Fatal("err = nil, want a failure")
			}
		})
	}
}
