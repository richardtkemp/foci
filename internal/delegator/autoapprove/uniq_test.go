package autoapprove

import (
	"encoding/json"
	"testing"
)

// TestUniqReadOnlyOperands proves even an explicit uniq rule cannot approve
// its output operand, including when flags or shell expansions obscure it.
func TestUniqReadOnlyOperands(t *testing.T) {
	t.Parallel()
	rules := Compile([]string{"Bash:uniq", "Bash:cat"})
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"uniq", true}, {"uniq -c input", true}, {"uniq -cdui input", true},
		{"uniq -f 2 -s3 input", true}, {"uniq -w 2 input", true},
		{"uniq --skip-fields=2 --skip-chars 3 --check-chars=4 input", true},
		{"uniq --all-repeated=separate input", true}, {"uniq --group=prepend input", true},
		{"uniq -D input", true}, {"uniq -Dseparate input", true},
		{"uniq -- input", true}, {"uniq 'input*'", true},
		{"uniq -cf2 input", true}, {"uniq -cs 2 input", true},
		{"uniq --count --repeated --ignore-case --unique --zero-terminated input", true},
		{"uniq --group input", true}, {"uniq --all-repeated input", true},
		{"uniq --help", true}, {"uniq --version", true},
		{"INPUT=a; uniq \"$INPUT\"", true}, {"uniq -c -", true},
		{"uniq a b", false}, {"uniq input -c", false},
		{"uniq -f", false}, {"uniq -f x input", false},
		{"uniq --skip-fields", false}, {"uniq --skip-fields=x input", false},
		{"uniq --count=value input", false}, {"uniq -x input", false},
		{"ARGS='a b'; uniq $ARGS", false}, {"CMD=uniq; $CMD a b", false},
		{"command uniq a b", false}, {"/usr/bin/uniq a b", false},
		{"uniq a b 2>/dev/null", false}, {"uniq -c a b", false},
		{"uniq -f 2 a b", false}, {"uniq -f2 a b", false},
		{"uniq --skip-fields 2 a b", false}, {"uniq --skip-fields=2 a b", false},
		{"uniq --all-repeated separate output", false},
		{"uniq -D separate output", false},
		{"uniq --group prepend output", false},
		{"uniq --skip-f 2 input", false},
		{"uniq -- a -output", false}, {"uniq - b", false},
		{"uniq a ''", false}, {"uniq a b -c", false},
		{"uniq *", false}, {"uniq input*", false}, {"uniq {a,b}", false},
		{"uniq a $(cat path)", false}, {"uniq a $OUTPUT", false},
		{"OUTPUT=b; uniq a \"$OUTPUT\"", false},
		{"uniq a > b", false}, {"uniq --unknown a", false},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"command": tc.cmd})
			if got, _ := MatchWithEnv(rules, "Bash", input, nil); got != tc.want {
				t.Errorf("allowed=%v, want %v", got, tc.want)
			}
		})
	}
}
