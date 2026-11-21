package config

import (
	"strings"
	"testing"
)

// TestMergedAllowedTools_DefaultOnly proves that when the per-agent override
// is nil/empty, MergedAllowedTools returns only the default rules as a
// comma-separated list. This is the baseline case every CC agent hits when
// the operator doesn't set [agents.backend_config].allowed_tools.
func TestMergedAllowedTools_DefaultOnly(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Write(/tmp/**)", "Edit(/tmp/**)"}}

	got := c.MergedAllowedTools(nil)
	want := "Write(/tmp/**),Edit(/tmp/**)"
	if got != want {
		t.Errorf("nil:      got %q, want %q", got, want)
	}

	got = c.MergedAllowedTools("")
	if got != want {
		t.Errorf("empty:    got %q, want %q", got, want)
	}

	got = c.MergedAllowedTools([]any{})
	if got != want {
		t.Errorf("emptyany: got %q, want %q", got, want)
	}
}

// TestMergedAllowedTools_StringPerAgent proves that a comma-separated string
// under backend_config.allowed_tools is split into individual rules and
// appended after the defaults. Covers the legacy TOML form where users wrote
// the value as a single quoted string.
func TestMergedAllowedTools_StringPerAgent(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Write(/tmp/**)"}}
	got := c.MergedAllowedTools("Bash(git:*), Read")
	want := "Write(/tmp/**),Bash(git:*),Read"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMergedAllowedTools_SlicePerAgent proves that a TOML array
// (decoded into []any) under backend_config.allowed_tools is accepted and
// each element is added as an individual rule. This is the natural TOML
// form once operators learn about the feature.
func TestMergedAllowedTools_SlicePerAgent(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Write(/tmp/**)"}}
	got := c.MergedAllowedTools([]any{"Bash(git:*)", "Read"})
	want := "Write(/tmp/**),Bash(git:*),Read"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMergedAllowedTools_Dedup proves that a rule appearing in both the
// default list and the per-agent list shows up only once in the merged
// output. Keeps the --allowedTools argv minimal and avoids log noise.
func TestMergedAllowedTools_Dedup(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Write(/tmp/**)", "Edit(/tmp/**)"}}
	got := c.MergedAllowedTools("Write(/tmp/**), Bash(git:*)")
	want := "Write(/tmp/**),Edit(/tmp/**),Bash(git:*)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMergedAllowedTools_EmptyDefault proves that an explicit empty default
// (user zeroed out [cc_backend] default_allowed_tools) disables the feature
// but still passes through the per-agent rules unchanged. Lets operators
// opt out of the /tmp defaults without losing per-agent permission control.
func TestMergedAllowedTools_EmptyDefault(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: nil}
	got := c.MergedAllowedTools("Bash(git:*)")
	want := "Bash(git:*)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got = c.MergedAllowedTools(nil)
	if got != "" {
		t.Errorf("both empty: got %q, want empty", got)
	}
}

// TestMergedAllowedTools_WhitespaceTrim proves that leading/trailing whitespace
// in per-agent rules is trimmed — users writing `" Bash(git:*), Read "` get
// the same result as the tight form. Dedup works against the trimmed form
// so `"Write(/tmp/**), Write(/tmp/**) "` still collapses to one entry.
func TestMergedAllowedTools_WhitespaceTrim(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Write(/tmp/**)"}}
	got := c.MergedAllowedTools("  Write(/tmp/**)  , Bash(git:*) ")
	want := "Write(/tmp/**),Bash(git:*)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFociConfigAllowedTools proves the builtin rules (#3232) pre-approve the
// three approval-gated `foci config` subcommands, for the bare name and for
// each CLI path given, and skip an empty path.
func TestFociConfigAllowedTools(t *testing.T) {
	got := strings.Join(FociConfigAllowedTools("", "/usr/local/bin/foci"), ",")
	want := "Bash(foci config set:*),Bash(foci config unset:*),Bash(foci config add:*)," +
		"Bash(/usr/local/bin/foci config set:*),Bash(/usr/local/bin/foci config unset:*),Bash(/usr/local/bin/foci config add:*)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestMergedAllowedTools_Builtin proves builtin rules come after the default
// and per-agent rules, and a builtin rule the operator also listed appears
// once, where the operator put it.
func TestMergedAllowedTools_Builtin(t *testing.T) {
	c := CCBackendConfig{DefaultAllowedTools: []string{"Read(/tmp/**)"}}
	got := c.MergedAllowedTools("Bash(foci config set:*)", "Bash(foci config set:*)", "Bash(foci config unset:*)")
	want := "Read(/tmp/**),Bash(foci config set:*),Bash(foci config unset:*)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// No default, no per-agent rule: the builtin rules alone.
	if got := (CCBackendConfig{}).MergedAllowedTools(nil, "Bash(foci config add:*)"); got != "Bash(foci config add:*)" {
		t.Errorf("builtin only: got %q", got)
	}
}
