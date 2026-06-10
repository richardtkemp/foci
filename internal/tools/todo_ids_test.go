// Tests for the --ids comma-form normalization in foci_todo (TODO #751).
// Help text and TODO #761's commit message both showed `--ids 1,2,3` form,
// but the bash function passed the value via jq --argjson which only accepts
// valid JSON (so `[1,2,3]` worked, `1,2,3` didn't). The shell function now
// normalises bare comma-separated form to a JSON array before sending.
package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFociTodo_IDsCommaFormNormalised(t *testing.T) {
	// Verifies the normalization snippet appears in the generated todo
	// shell-func body. Structural check guards against the snippet being
	// removed or replaced by a refactor that misses the corner case.
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	// Look for the wrapper case statement around $ids.
	want := `case "$ids" in
      \[*\]) ;;`
	if !strings.Contains(body, want) {
		t.Errorf("foci_todo body missing --ids comma-form normalisation\nwant substring:\n%s\n---body---\n%s", want, body)
	}
}

// disconnected-test-ok: black-box test — execs bash to validate the shell
// normalisation logic; the sibling TestFociTodo_IDsCommaFormNormalised
// references generateShellFunc and guards drift of the real snippet.
func TestFociTodo_IDsNormalisationExecutesCorrectly(t *testing.T) {
	// Run the normalization inline under bash with each input form and
	// assert the resulting $ids value is valid JSON acceptable to
	// jq --argjson.
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"json_array", "[1,2,3]", "[1,2,3]"},
		{"json_array_with_spaces", "[1, 2, 3]", "[1, 2, 3]"}, // bracket form passes through verbatim
		{"comma_form", "1,2,3", "[1,2,3]"},
		{"comma_form_with_spaces", "1, 2, 3", "[1,2,3]"},
		{"single_int", "5", "[5]"},
		{"empty", "", ""},
	}

	script := `
ids="$1"
if [ -n "$ids" ]; then
  case "$ids" in
    \[*\]) ;;
    *) ids="[$(echo "$ids" | tr -d ' ')]" ;;
  esac
fi
printf '%s' "$ids"
`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bash, "-c", script, "_", tc.input)
			cmd.Env = os.Environ()
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash exec failed: %v", err)
			}
			got := string(out)
			if got != tc.want {
				t.Errorf("input=%q got=%q want=%q", tc.input, got, tc.want)
			}
		})
	}
}

// disconnected-test-ok: black-box test — execs bash to validate the shell
// normalisation output; drift of the real snippet is guarded by
// TestFociTodo_IDsCommaFormNormalised, which references generateShellFunc.
func TestFociTodo_IDsResultIsValidJSON(t *testing.T) {
	// After normalization, the result should be valid JSON that jq's
	// --argjson would accept. End-to-end check that the bash logic
	// actually produces parseable JSON for the comma-separated case.
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	script := `
ids="$1"
if [ -n "$ids" ]; then
  case "$ids" in
    \[*\]) ;;
    *) ids="[$(echo "$ids" | tr -d ' ')]" ;;
  esac
fi
printf '%s' "$ids"
`
	for _, input := range []string{"1,2,3", "1, 2, 3", "5", "[1,2,3]"} {
		t.Run(input, func(t *testing.T) {
			cmd := exec.Command(bash, "-c", script, "_", input)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash exec failed: %v", err)
			}
			var arr []int
			if err := json.Unmarshal(out, &arr); err != nil {
				t.Errorf("input=%q produced %q which is not valid JSON int array: %v", input, out, err)
			}
		})
	}
}
