package autoapprove

import (
	"testing"
)

// #2263: GNU programs that parse options with getopt_long (sed, sort) and
// git's parse-options (git grep) accept any unambiguous prefix of a long
// option, with the value after '=' or as the next argument. The unsafe-flag
// tables matched only the exact spelling, so a shortened unsafe flag
// (`sed --in`, `sort --out`, `git grep --open`) slipped past the check, and a
// sed script attached to its flag (`--expression=…`, `-e…`, `-ne…`) was never
// scanned. These tables pin the closed gaps through the public match path.

// TestAbbreviatedLongFlagsNotAutoApproved proves #2263 requirements 1 and 2:
// every sed and sort longFlags entry — plus the git grep pager flag — is
// refused in a shortened spelling, with the value attached after '=' and as
// the next argument, and the abbreviated --in reaches the same refusal when
// it arrives through variable resolution. Every row is auto-approved by the
// pre-#2263 code, which matched long flags only as the exact table spelling.
func TestAbbreviatedLongFlagsNotAutoApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	git := parseAutoApproveRules([]string{"Bash:git *"})
	tests := []struct {
		name  string
		rules []Rule // nil → the built-in readonly set
		cmd   string
	}{
		// sed: --in-place, --file.
		{"sed in-place prefix with =value", nil, "sed --in=.bak f"},
		{"sed in-place prefix value next argument", nil, "sed --in f"},
		{"sed file prefix with =value", nil, "sed --fil=script.sed f"},
		{"sed file prefix value next argument", nil, "sed --fil script.sed f"},
		// sort: --output, --compress-program.
		{"sort output prefix with =value", nil, "sort --out=/tmp/x f"},
		{"sort output prefix value next argument", nil, "sort --out /tmp/x f"},
		{"sort compress prefix with =value", nil, "sort --compress=/bin/sh f"},
		{"sort compress prefix value next argument", nil, "sort --compress /bin/sh f"},
		// git grep: --open-files-in-pager (parse-options accepts prefixes).
		{"git grep pager prefix with =value", git, "git grep --open=less pattern"},
		{"git grep pager prefix value next argument", git, "git grep --open less pattern"},
		// The resolved-argument path (#2216) judges the same vector.
		{"sed in-place prefix resolved from variable", nil, `X='--in'; sed $X f`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := tt.rules
			if rules == nil {
				rules = readonly
			}
			assertApproval(t, rules, tt.cmd, false)
		})
	}
}

// TestSedAttachedScriptFlagsScanned proves #2263 requirement 3: a sed script
// reaches sedArgUnsafe when it is attached to its flag — as the value of
// --expression (exact or abbreviated, after '=' or as the next argument) and
// as the text following an 'e' in a short-flag bundle (-e…, -ne…, including
// the next-word value when the 'e' ends the token). The next-word rows use a
// script that itself starts with '--': a word not starting with '-' is
// scanned as a plain argument anyway and a '-e…' word as an attached script,
// so a '--' word is the shape ONLY the next-word rule reaches. Every script
// here is one sedArgUnsafe refuses, and every row is auto-approved by the
// pre-#2263 code, which scanned only separate non-flag arguments.
func TestSedAttachedScriptFlagsScanned(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, tt := range []struct {
		name string
		cmd  string
	}{
		{"long flag with attached value", `sed --expression='p;e whoami' f`},
		{"abbreviated long flag with attached value", `sed --expr='p;e whoami' f`},
		{"attached to lone -e", `sed '-ep;e whoami'`},
		{"attached to -e at end of a bundle", `sed '-nep;e whoami'`},
		{"long flag value is the next word", `sed --expression '--x;e id' f`},
		{"abbreviated long flag value is the next word", `sed --expr '--x;e id' f`},
		{"-e at token end takes the next word", `sed -e '--x;e id' f`},
		{"bundle-ending e takes the next word", `sed -ne '--x;e id' f`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertApproval(t, rules, tt.cmd, false)
		})
	}
}

// TestExactUnsafeLongFlagsStillRefused characterises the unchanged half of
// #2263: the exact flag forms the tables have always matched — and a sed
// script given as its own argument, the pre-existing scan path — keep their
// refusal. These rows passed before the change; they pin it against
// regressions, not the change itself.
func TestExactUnsafeLongFlagsStillRefused(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	git := parseAutoApproveRules([]string{"Bash:git *"})
	tests := []struct {
		name  string
		rules []Rule // nil → the built-in readonly set
		cmd   string
	}{
		{"sed --in-place", nil, "sed --in-place f"},
		{"sed --in-place=value", nil, "sed --in-place=.bak f"},
		{"sed --file=value", nil, "sed --file=script.sed f"},
		{"sed --file value", nil, "sed --file script.sed f"},
		{"sed -i", nil, "sed -i f"},
		{"sed -f", nil, "sed -f script.sed f"},
		// The separate-script path keeps working (requirement 3).
		{"sed -e script argument", nil, `sed -e 'p;e whoami' f`},
		{"sed --expression script argument", nil, `sed --expression 'p;e whoami' f`},
		{"sed -ne script argument", nil, `sed -ne 'p;e whoami' f`},
		{"sort --output=value", nil, "sort --output=/tmp/x f"},
		{"sort --output value", nil, "sort --output /tmp/x f"},
		{"sort -o", nil, "sort -o /tmp/x f"},
		{"sort --compress-program", nil, "sort --compress-program=/bin/sh f"},
		{"git grep --open-files-in-pager", git, "git grep --open-files-in-pager pattern"},
		{"git grep --open-files-in-pager=value", git, "git grep --open-files-in-pager=less pattern"},
		{"git grep -O", git, "git grep -O pattern"},
		{"rg --pre refused as today", readonly, "rg --pre=./x.sh foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := tt.rules
			if rules == nil {
				rules = readonly
			}
			assertApproval(t, rules, tt.cmd, false)
		})
	}
}

// TestSafeLongFlagFormsStillApproved characterises the approvals #2263 must
// not disturb: safe sed, sort and git grep invocations in every
// script-passing shape, and flag spellings the parsers of rg, yq and git's
// top level reject as abbreviations — unknown flags that stay the non-events
// they always were (their parsers were verified to reject abbreviations, so
// abbreviation matching is deliberately not enabled for them).
func TestSafeLongFlagFormsStillApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	git := parseAutoApproveRules([]string{"Bash:git *"})
	tests := []struct {
		name  string
		rules []Rule // nil → the built-in readonly set
		cmd   string
	}{
		{"sort file", nil, "sort f"},
		{"sort --check", nil, "sort --check f"},
		{"sed -n script", nil, "sed -n p f"},
		{"sed -e safe script", nil, "sed -e p f"},
		{"sed --expression=safe script", nil, "sed --expression=p f"},
		{"sed --expression safe script", nil, "sed --expression p f"},
		{"git grep pattern", git, "git grep foo"},
		// rg, yq and git top-level options reject abbreviations: a shortened
		// spelling is an unknown flag, not the unsafe one.
		{"rg shortened --pre stays unknown", nil, "rg --pr=./x.sh foo"},
		{"yq shortened --inplace stays unknown", nil, "yq --in '.'"},
		{"git shortened --config-env stays unknown", git, "git --config-e=k=v status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := tt.rules
			if rules == nil {
				rules = readonly
			}
			assertApproval(t, rules, tt.cmd, true)
		})
	}
}
