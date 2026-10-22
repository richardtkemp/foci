package pretool

import "testing"

// TestDefaults_GrepOptionAfterDashDash (#2471): an option-shaped word after
// the pattern that follows a standalone -- is a file operand, so it is
// denied. Options before --, a pattern that starts with -, and a lone -
// (stdin) are not.
func TestDefaults_GrepOptionAfterDashDash(t *testing.T) {
	checkDefaultVerdicts(t, "grep_option_after_dashdash",
		[]string{
			// The ticket's repro.
			`grep -rqw -- "$n" $F/internal $F/cmd --include=*.go 2>/dev/null || echo "NOTFOUND $s"`,
			`for n in a b; do grep -rqw -- "$n" /x --include='*.go'; done`,
			`grep -- foo file -n`,
			`grep -- -foo file -i`,
			`rg -- foo dir --glob '*.go'`,
			`ugrep -- foo dir -r`,
			`egrep -- 'a|b' f --color=never`,
			`sudo grep -- foo /var/log/x -c`,
			`/usr/bin/grep -r -- foo . --exclude-dir=.git`,
		},
		[]string{
			`grep -rqw --include='*.go' -- Search /home/rich/git/foci`,
			`grep -- -foo file`,
			`grep -- --include=x file`,
			`grep -rn -- "$pat" a b c`,
			`grep -- foo -`,
			`grep -rn --include=*.go foo dir`,
			`grep foo file -n`,
			`git grep -e x -- '*.go' -n`,
			`echo grep -- foo file -n`,
			`grep -c -- x f | sort -n`,
		})
}
