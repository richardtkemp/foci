package provision

import "testing"

// TestResolveModelAlias_TracksNewestInFamily is the regression test for the
// drift that motivated the change: the old fixed map still returned opus-4-6
// and sonnet-4-6 months after opus-5 and sonnet-5 shipped, and fable-5 on the
// day fable-5-1 did. Nothing failed loudly — a new agent was simply created on
// a superseded model. These expectations must be UPDATED when a new family
// member lands, which is the point: the test is where the staleness now shows.
func TestResolveModelAlias_TracksNewestInFamily(t *testing.T) {
	for _, tc := range []struct{ alias, want string }{
		{"fable", "anthropic/claude-fable-5-1"},
		{"opus", "anthropic/claude-opus-5"},
		{"sonnet", "anthropic/claude-sonnet-5"},
		{"haiku", "anthropic/claude-haiku-4-5"},
		{"", "anthropic/claude-sonnet-5"},       // empty defaults to sonnet
		{"FABLE", "anthropic/claude-fable-5-1"}, // case-insensitive
		{" opus ", "anthropic/claude-opus-5"},   // trimmed
	} {
		if got := ResolveModelAlias(tc.alias); got != tc.want {
			t.Errorf("ResolveModelAlias(%q) = %q, want %q", tc.alias, got, tc.want)
		}
	}
}

// TestResolveModelAlias_PassesThroughNonAliases guards the other half of the
// contract: anything that is not one of the four family words is returned
// UNCHANGED, including its case. Callers pass user-typed model ids through this
// function, so rewriting or lowercasing one would corrupt it.
func TestResolveModelAlias_PassesThroughNonAliases(t *testing.T) {
	for _, in := range []string{
		"anthropic/claude-opus-4-6", // an explicit pin must survive
		"google/gemini-3.1-pro-preview",
		"openrouter/moonshotai/Kimi-K2.5",
		"claude-custom-model", // not developer/model_id shaped, still untouched
		"mythos",              // not an alias we define
	} {
		if got := ResolveModelAlias(in); got != in {
			t.Errorf("ResolveModelAlias(%q) = %q, want it returned unchanged", in, got)
		}
	}
}

// TestResolveModelAlias_SkipsVariantsAndPointers pins the exclusions that make
// "newest" mean a plain current model. The registry holds claude-opus-latest
// (a moving pointer), claude-opus-5-fast and claude-opus-5[1m] (price variants
// of a model, not newer models), and claude-3-haiku (version BEFORE the family
// token). Any of them winning would hand new agents a variant they did not ask
// for, or an id the endpoint may not accept.
func TestResolveModelAlias_SkipsVariantsAndPointers(t *testing.T) {
	for _, alias := range []string{"fable", "opus", "sonnet", "haiku"} {
		got := ResolveModelAlias(alias)
		for _, bad := range []string{"latest", "-fast", "[1m]"} {
			if len(got) >= len(bad) && contains(got, bad) {
				t.Errorf("ResolveModelAlias(%q) = %q, which is a %q variant, not a plain model",
					alias, got, bad)
			}
		}
	}
	if got := ResolveModelAlias("haiku"); got == "anthropic/claude-3-haiku" {
		t.Error("haiku resolved to claude-3-haiku: a version before the family token was " +
			"read as a version after it")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
