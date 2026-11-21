package configapprove

import (
	"strings"
	"testing"
)

// TestSubmit_BareWordForStringField proves #3199 part 1: a bare word set on
// a string-typed registry field (logging.level) or a duration field
// (agents[x].keepalive.interval) is quoted for the user, so `foci config set logging.level
// DEBUG` prompts for the string "DEBUG" and Approve writes it quoted.
func TestSubmit_BareWordForStringField(t *testing.T) {
	cases := []struct{ path, value, wantNew, wantLine string }{
		{"logging.level", "DEBUG", `"DEBUG"`, `level = "DEBUG"`},
		{"agents[main].keepalive.interval", "5m", `"5m"`, `interval = "5m"`},
		{"logging.level", `"WARN"`, `"WARN"`, `level = "WARN"`},
	}
	for _, c := range cases {
		t.Run(c.path+"="+c.value, func(t *testing.T) {
			h := newHarness(t)
			st := h.set(c.path, c.value)
			if st.Outcome != Pending {
				t.Fatalf("submit: %+v", st)
			}
			if st.New != c.wantNew {
				t.Errorf("new = %q, want %q", st.New, c.wantNew)
			}
			h.prompt(0).OnAnswer(true)
			if got := h.outcome(st.ID); got.Outcome != Applied {
				t.Fatalf("outcome = %+v", got)
			}
			if f := h.file(); !strings.Contains(f, c.wantLine) {
				t.Errorf("file lacks %q:\n%s", c.wantLine, f)
			}
		})
	}
}

// TestSubmit_NotTOMLSaysHowToQuote proves #3199 part 1's other arm: a bare
// word that cannot be quoted for the user (the field is not a string, or
// the path names no registry field) fails with a reason that shows the
// quoted form, and no prompt is shown. A malformed quoted value is not
// re-quoted into a string.
func TestSubmit_NotTOMLSaysHowToQuote(t *testing.T) {
	cases := []struct{ path, value, want string }{
		{"agent_loop.max_tool_loops", "twenty", `'"twenty"'`},
		{"no_such_section.key", "DEBUG", `'"DEBUG"'`},
		// An agent's top-level scalars are not registry fields.
		{"agents[main].model", "sonnet", `'"sonnet"'`},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			h := newHarness(t)
			st := h.set(c.path, c.value)
			if st.Outcome != Failed || !strings.Contains(st.Reason, c.want) || !strings.Contains(st.Reason, "must be quoted") {
				t.Fatalf("status = %+v", st)
			}
			if len(h.prompts) != 0 || h.file() != baseConfig {
				t.Error("prompted or wrote")
			}
		})
	}
	t.Run("unterminated quote", func(t *testing.T) {
		h := newHarness(t)
		st := h.set("logging.level", `"DEBUG`)
		if st.Outcome != Failed || strings.Contains(st.Reason, "must be quoted") {
			t.Fatalf("status = %+v", st)
		}
		if len(h.prompts) != 0 {
			t.Error("prompted")
		}
	})
	t.Run("bad duration", func(t *testing.T) {
		h := newHarness(t)
		if st := h.set("agent_loop.max_tool_loops", "5m"); st.Outcome != Failed {
			t.Fatalf("status = %+v", st)
		}
	})
}
