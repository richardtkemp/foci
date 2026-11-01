package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadWebhookFixture writes body (plus the minimum Load needs) to a temp
// foci.toml and loads it.
func loadWebhookFixture(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	content := "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

func TestWebhooks_LoadOldForm(t *testing.T) {
	cfg, err := loadWebhookFixture(t, "[system.webhooks]\ndeploy = \"deploy.md\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := cfg.System.Webhooks["deploy"]
	if h.Prompt != "deploy.md" || h.Session != "" {
		t.Fatalf("deploy = %+v, want prompt deploy.md, no session", h)
	}
	if sel, perCall := h.SessionPolicy(); sel != "" || perCall {
		t.Errorf("SessionPolicy() = (%q, %v), want persistent", sel, perCall)
	}
	if len(cfg.UndefinedKeys) != 0 {
		t.Errorf("UndefinedKeys = %v, want none", cfg.UndefinedKeys)
	}
}

func TestWebhooks_LoadTableForms(t *testing.T) {
	body := `[system.webhooks]
a = { prompt = "a.md" }
b = { prompt = "b.md", session = "persistent" }
c = { prompt = "c.md", session = "new-per-call" }
d = { prompt = "d.md", session = "ops" }
old = "old.md"

[system.webhooks.e]
prompt = "e.md"
session = "new-per-call"
`
	cfg, err := loadWebhookFixture(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// check-config fails on unknown keys, so the table form must not leave any.
	if len(cfg.UndefinedKeys) != 0 {
		t.Errorf("UndefinedKeys = %v, want none", cfg.UndefinedKeys)
	}
	cases := []struct {
		id, prompt, sel string
		perCall         bool
	}{
		{"a", "a.md", "", false},
		{"b", "b.md", "", false},
		{"c", "c.md", "", true},
		{"d", "d.md", "ops", false},
		{"e", "e.md", "", true},
		{"old", "old.md", "", false},
	}
	for _, c := range cases {
		h, ok := cfg.System.Webhooks[c.id]
		if !ok {
			t.Errorf("hook %q missing", c.id)
			continue
		}
		if h.Prompt != c.prompt {
			t.Errorf("hook %q prompt = %q, want %q", c.id, h.Prompt, c.prompt)
		}
		sel, perCall := h.SessionPolicy()
		if sel != c.sel || perCall != c.perCall {
			t.Errorf("hook %q SessionPolicy() = (%q, %v), want (%q, %v)", c.id, sel, perCall, c.sel, c.perCall)
		}
	}
}

// An agent's entry for a hook ID replaces the global entry whole: the
// global session setting must not leak into the agent's old-form entry.
func TestWebhooks_PerAgentMergeIsWholeEntry(t *testing.T) {
	body := `[system.webhooks]
deploy = { prompt = "g.md", session = "new-per-call" }
alert = "alert.md"

[[agents]]
id = "a"

[agents.system.webhooks]
deploy = "a.md"
page = { prompt = "page.md", session = "pager" }
`
	cfg, err := loadWebhookFixture(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rc := Resolve(cfg, cfg.Agents[0])
	if d := rc.Webhooks["deploy"]; d.Prompt != "a.md" || d.Session != "" {
		t.Errorf("deploy = %+v, want the agent's entry whole (a.md, no session)", d)
	}
	if a := rc.Webhooks["alert"]; a.Prompt != "alert.md" {
		t.Errorf("alert = %+v, want global alert.md", a)
	}
	if p := rc.Webhooks["page"]; p.Prompt != "page.md" || p.Session != "pager" {
		t.Errorf("page = %+v, want page.md / pager", p)
	}
}

func TestWebhooks_InvalidEntriesFailLoad(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"missing prompt", "[system.webhooks]\nx = { session = \"new-per-call\" }\n", "missing prompt"},
		{"empty prompt", "[system.webhooks]\nx = { prompt = \"\" }\n", "prompt must not be empty"},
		{"empty old-form prompt", "[system.webhooks]\nx = \"\"\n", "prompt must not be empty"},
		{"unknown setting", "[system.webhooks]\nx = { prompt = \"x.md\", sesion = \"new-per-call\" }\n", `unknown setting "sesion"`},
		{"empty session", "[system.webhooks]\nx = { prompt = \"x.md\", session = \"\" }\n", "session must not be empty"},
		{"blank session", "[system.webhooks]\nx = { prompt = \"x.md\", session = \"  \" }\n", "session must not be empty"},
		{"non-string session", "[system.webhooks]\nx = { prompt = \"x.md\", session = 3 }\n", "session must be a string"},
		{"wrong type", "[system.webhooks]\nx = 3\n", "must be a prompt path string or a table"},
		{"agent scope", "[[agents]]\nid = \"a\"\n\n[agents.system.webhooks]\nx = { prompt = \"x.md\", session = \"\" }\n", `agent "a" webhooks: hook "x"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadWebhookFixture(t, c.body)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
			if !strings.Contains(err.Error(), `hook "x"`) {
				t.Errorf("error = %v, want it to name hook \"x\"", err)
			}
		})
	}
}

func TestWebhooks_PathSeparatorInHookIDStillRejected(t *testing.T) {
	_, err := loadWebhookFixture(t, "[system.webhooks]\n\"a/b\" = { prompt = \"x.md\" }\n")
	if err == nil || !strings.Contains(err.Error(), "path separators") {
		t.Fatalf("err = %v, want a path-separator error", err)
	}
}

func TestWebhookConfig_MarshalTOMLKeepsForm(t *testing.T) {
	var old, tbl WebhookConfig
	_ = old.UnmarshalTOML("a.md")
	_ = tbl.UnmarshalTOML(map[string]any{"prompt": "b.md", "session": "new-per-call"})
	if b, _ := old.MarshalTOML(); string(b) != `"a.md"` {
		t.Errorf("old form = %s", b)
	}
	if b, _ := tbl.MarshalTOML(); string(b) != `{ prompt = "b.md", session = "new-per-call" }` {
		t.Errorf("table form = %s", b)
	}
}
