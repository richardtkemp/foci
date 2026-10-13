// Tests for #2306: foci_todo's POSITIONAL item id is validated in the
// wrapper, before any jq or foci-call. The six id actions (get, complete,
// drop, reopen, start, remove) took ANY word as the id and handed it
// straight to jq --argjson, so `foci_todo get foo` printed jq's three-line
// invalid-JSON text (twice for the params-building actions) and then called
// foci-call with an empty payload. A second positional silently replaced
// the first (`get 1 2` showed item #2; `complete 999999 done` took `done`
// as the id), `--id` and a positional fought with last-one-wins, and `get`
// with no id at all leaked the same jq text. All of these are now usage
// errors naming foci_todo <action> and the offending value. The three
// Test…Unchanged functions are characterisation: they pin today's payloads
// and the #1811 --id message so the new checks cannot displace them (listed
// in .factory/red-gate-exempt.txt). The digits rule is also pinned under an
// installed collating UTF-8 locale: bash collates a [0-9] glob range by
// locale, so only there does the range accept non-ASCII digits (٣), which
// the C-locale test environment cannot see at all.
package tools

import (
	osexec "os/exec"
	"strings"
	"testing"
)

// todoIDShellBody returns the real generated foci_todo function (nil store —
// the wrapper never touches the store; the real todoActions allowlists,
// usage strings and alias normalisation are what these tests exercise).
func todoIDShellBody(t *testing.T) string {
	t.Helper()
	requireBashJq(t)
	return generateShellFunc(NewTodoTool(nil, "agent-posid"))
}

// assertTodoIDRefusal runs one foci_todo command and asserts the #2306
// refusal contract: rc 1, foci-call never ran, exactly one error line
// naming the action and the offending value, the action's own usage line
// after it, and no jq internals.
func assertTodoIDRefusal(t *testing.T, body, cmd, wantErr, wantUsage string) {
	t.Helper()
	calls, rc, out := runCountingShellFunc(t, body, cmd, 0)
	if rc != 1 {
		t.Errorf("%s: rc=%d, want 1 (usage error)\nout=%s", cmd, rc, out)
	}
	if calls != 0 {
		t.Errorf("%s: foci-call ran %d times, want 0 on a refused call\nout=%s", cmd, calls, out)
	}
	if !strings.Contains(out, wantErr) {
		t.Errorf("%s: error should contain %q\nout=%s", cmd, wantErr, out)
	}
	if !strings.Contains(out, wantUsage) {
		t.Errorf("%s: the action's usage line must follow the error\nout=%s", cmd, out)
	}
	if n := strings.Count(out, "error:"); n != 1 {
		t.Errorf("%s: want exactly one error line, got %d\nout=%s", cmd, n, out)
	}
	for _, leak := range []string{"jq:", "argjson", "invalid JSON argument"} {
		if strings.Contains(out, leak) {
			t.Errorf("%s: jq internals leaked (%q) instead of a usage error\nout=%s", cmd, leak, out)
		}
	}
}

// todoShellSingleCall runs one foci_todo command and returns the exact
// request of the single foci-call it made ("" if none), failing the test on
// any other outcome. Used to pin payloads byte-for-byte.
func todoShellSingleCall(t *testing.T, body, cmd string) string {
	t.Helper()
	calls, rc, out := runCountingShellFunc(t, body, cmd, 0)
	if rc != 0 {
		t.Fatalf("%s: rc=%d, want 0\nout=%s", cmd, rc, out)
	}
	if calls != 1 {
		t.Fatalf("%s: %d foci-calls, want exactly 1\nout=%s", cmd, calls, out)
	}
	var req string
	for _, ln := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(ln, "FCALL|"); ok {
			req = v
		}
	}
	return req
}

// TestTodoShellPositionalIDMustBeWholeNumber proves req 1 of #2306: for get,
// complete, drop, reopen, start and remove a positional argument is the item
// id only if it is one or more ASCII digits (edit's rule). Before, any word
// went straight to jq --argjson — `get foo` and `drop '1 2'` leaked jq's
// invalid-JSON text and then called foci-call with an empty payload, while
// `start -5` and `remove 1.5` were silently SENT as valid JSON numbers. The
// first-bad-argument-order case (`get foo --id 4`) must report `foo`, not
// the id clash, and the empty string is not a number either.
func TestTodoShellPositionalIDMustBeWholeNumber(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, wantErr, wantUsage string }{
		{`foci_todo get foo`,
			"error: foci_todo get: the item id must be a whole number, got 'foo'",
			"usage: foci_todo get <id>"},
		{`foci_todo complete foo --reason x`,
			"error: foci_todo complete: the item id must be a whole number, got 'foo'",
			"usage: foci_todo complete <id>"},
		{`foci_todo drop '1 2'`,
			"error: foci_todo drop: the item id must be a whole number, got '1 2'",
			"usage: foci_todo drop <id>"},
		{`foci_todo reopen abc`,
			"error: foci_todo reopen: the item id must be a whole number, got 'abc'",
			"usage: foci_todo reopen <id>"},
		{`foci_todo start -5`,
			"error: foci_todo start: the item id must be a whole number, got '-5'",
			"usage: foci_todo start <id>"},
		{`foci_todo remove 1.5`,
			"error: foci_todo remove: the item id must be a whole number, got '1.5'",
			"usage: foci_todo remove --id N"},
		{`foci_todo get foo --id 4`,
			"error: foci_todo get: the item id must be a whole number, got 'foo'",
			"usage: foci_todo get <id>"},
		{`foci_todo get ''`,
			"error: foci_todo get: the item id must be a whole number, got ''",
			"usage: foci_todo get <id>"},
	} {
		assertTodoIDRefusal(t, body, c.cmd, c.wantErr, c.wantUsage)
	}
}

// TestTodoShellIDGivenOnce proves req 2 of #2306: the item id may be given
// exactly once, in any one form. Before, a second positional silently
// replaced the first (`get 1 2` showed item #2), `--id` after a positional
// or a second `--id` silently won (#1778's given-twice rule made it an
// error instead). The error names BOTH the id already given and the
// unexpected second value — including when the second value is non-numeric
// (`get --id 4 foo` pins dup-before-form: it is a second id first, a bad
// number second) — and `complete 999999 done` is refused rather than
// reinterpreted as a close reason (the usage line already shows --reason).
func TestTodoShellIDGivenOnce(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, first, second, wantUsage string }{
		{`foci_todo get 1 2`, "1", "2", "usage: foci_todo get <id>"},
		{`foci_todo complete 999999 done`, "999999", "done", "usage: foci_todo complete <id>"},
		{`foci_todo get 3 --id 4`, "3", "4", "usage: foci_todo get <id>"},
		{`foci_todo get --id 4 3`, "4", "3", "usage: foci_todo get <id>"},
		{`foci_todo get --id 4 --id 5`, "4", "5", "usage: foci_todo get <id>"},
		{`foci_todo get --id 4 foo`, "4", "foo", "usage: foci_todo get <id>"},
		{`foci_todo reopen 1 --id 2`, "1", "2", "usage: foci_todo reopen <id>"},
		{`foci_todo remove --id 1 --id 2`, "1", "2", "usage: foci_todo remove --id N"},
		// The second value must be reported verbatim: quoting inside the
		// emitted echo, so no word splitting ('2 3') and no glob expansion
		// ('*' against the working directory's files).
		{`foci_todo get 1 '2 3'`, "1", "2 3", "usage: foci_todo get <id>"},
		{`foci_todo get 1 '*'`, "1", "*", "usage: foci_todo get <id>"},
	} {
		wantErr := "error: foci_todo " + actionOf(c.cmd) +
			": the item id was already given as '" + c.first +
			"'; unexpected second id '" + c.second + "'"
		assertTodoIDRefusal(t, body, c.cmd, wantErr, c.wantUsage)
	}
}

// actionOf extracts the canonical action word from one of the test's
// foci_todo commands (always the token after "foci_todo " in these cases,
// after alias normalisation).
func actionOf(cmd string) string {
	_, rest, _ := strings.Cut(cmd, "foci_todo ")
	action, _, _ := strings.Cut(rest, " ")
	return action
}

// TestTodoShellGetRequiresID proves req 3 of #2306: `foci_todo get` with no
// id at all is refused in the wrapper — it used to feed an empty string to
// jq --argjson and leak the same invalid-JSON text. Flags that are not an
// id (`--format md`) do not satisfy the requirement.
func TestTodoShellGetRequiresID(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, cmd := range []string{`foci_todo get`, `foci_todo get --format md`} {
		assertTodoIDRefusal(t, body, cmd,
			"error: foci_todo get: the item id is missing",
			"usage: foci_todo get <id>")
	}
}

// TestTodoShellIDFormsUnchanged characterises req 4 of #2306: every form
// that worked before keeps sending EXACTLY one call with EXACTLY the same
// params. `get 007` pins that leading zeros stay accepted (item 7, as
// today), `show 3` the alias path, and the no-id five that complete/drop/
// reopen/start/remove still send the bare call and get the server's
// "id is required" — only get refuses locally.
func TestTodoShellIDFormsUnchanged(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, wantReq string }{
		{`foci_todo get 5`, `{"tool":"todo","params":{"action":"get","id":5}}`},
		{`foci_todo get --id 5`, `{"tool":"todo","params":{"action":"get","id":5}}`},
		{`foci_todo get 5 --format md`, `{"tool":"todo","params":{"action":"get","id":5}}`},
		{`foci_todo get 007`, `{"tool":"todo","params":{"action":"get","id":7}}`},
		{`foci_todo show 3`, `{"tool":"todo","params":{"action":"get","id":3}}`},
		{`foci_todo complete 5 --reason x`, `{"tool":"todo","params":{"action":"complete","id":5,"reason":"x"}}`},
		{`foci_todo drop --id 5 --reason x`, `{"tool":"todo","params":{"action":"drop","id":5,"reason":"x"}}`},
		{`foci_todo reopen 5`, `{"tool":"todo","params":{"action":"reopen","id":5}}`},
		{`foci_todo start 5`, `{"tool":"todo","params":{"action":"start","id":5}}`},
		{`foci_todo remove 5`, `{"tool":"todo","params":{"action":"remove","id":5}}`},
		{`foci_todo remove --id 5`, `{"tool":"todo","params":{"action":"remove","id":5}}`},
		{`foci_todo complete --ids 1,2 --reason x`, `{"tool":"todo","params":{"action":"complete","ids":[1,2],"reason":"x"}}`},
		{`foci_todo complete`, `{"tool":"todo","params":{"action":"complete"}}`},
		{`foci_todo drop`, `{"tool":"todo","params":{"action":"drop"}}`},
		{`foci_todo reopen`, `{"tool":"todo","params":{"action":"reopen"}}`},
		{`foci_todo start`, `{"tool":"todo","params":{"action":"start"}}`},
		{`foci_todo remove`, `{"tool":"todo","params":{"action":"remove"}}`},
	} {
		if got := todoShellSingleCall(t, body, c.cmd); got != c.wantReq {
			t.Errorf("%s: request = %s, want exactly %s", c.cmd, got, c.wantReq)
		}
	}
}

// TestTodoShellIDFlagErrorUnchanged characterises the #1811 --id check the
// new positional rules must not displace: `--id foo` keeps its own message
// (with the e.g. hint), even when an id was already given positionally —
// the value check runs before the given-once check, so `get 3 --id foo`
// still reports the bad --id value, not the clash.
func TestTodoShellIDFlagErrorUnchanged(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, wantErr string }{
		{`foci_todo get --id foo`,
			"error: --id expects a JSON number, but this value is not JSON: foo"},
		{`foci_todo get 3 --id foo`,
			"error: --id expects a JSON number, but this value is not JSON: foo"},
	} {
		calls, rc, out := runCountingShellFunc(t, body, c.cmd, 0)
		if rc != 1 || calls != 0 {
			t.Errorf("%s: rc=%d calls=%d, want rc 1 and 0 foci-calls\nout=%s", c.cmd, rc, calls, out)
		}
		if !strings.Contains(out, c.wantErr) || !strings.Contains(out, "e.g. --id 30") {
			t.Errorf("%s: the #1811 --id message must keep its own text and hint\nout=%s", c.cmd, out)
		}
		if strings.Contains(out, "whole number") || strings.Contains(out, "already given") {
			t.Errorf("%s: a bad --id value must not reach the #2306 positional checks\nout=%s", c.cmd, out)
		}
	}
}

// TestTodoShellEditPositionalUnchanged characterises edit's id-or-text
// positional (spec-excluded from #2306): a numeric first positional is the
// id, any other word is text, and a second numeric positional after an id
// is text — exactly as today.
func TestTodoShellEditPositionalUnchanged(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, wantReq string }{
		{`foci_todo edit 6 fix the thing`, `{"tool":"todo","params":{"action":"edit","id":6,"text":"fix the thing"}}`},
		{`foci_todo edit foo`, `{"tool":"todo","params":{"action":"edit","text":"foo"}}`},
		{`foci_todo edit 1 2`, `{"tool":"todo","params":{"action":"edit","id":1,"text":"2"}}`},
	} {
		if got := todoShellSingleCall(t, body, c.cmd); got != c.wantReq {
			t.Errorf("%s: request = %s, want exactly %s", c.cmd, got, c.wantReq)
		}
	}
}

// TestTodoShellFlagOnlyActionsRefusePositional closes the #2306 review's
// blocking finding: list and list-all take no positional argument at all,
// but the positional dispatch had no arm for them, so `foci_todo list 3`
// silently sent {"action":"list"} and returned 0 — the word vanished, the
// #1778 silently-dropped-input class. They now refuse the word, naming the
// action and the value, before any foci-call. The refusal must not
// over-fire (bare and flag-only list forms keep sending exactly today's
// payload), and an empty action keeps today's ending: its words are
// dropped, but the call still fails on the master usage line — there is no
// action to name in a refusal.
func TestTodoShellFlagOnlyActionsRefusePositional(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, wantErr, wantUsage string }{
		{`foci_todo list 3`,
			"error: foci_todo list takes no positional argument, got '3'",
			"usage: foci_todo list [--tag T]"},
		{`foci_todo list open`,
			"error: foci_todo list takes no positional argument, got 'open'",
			"usage: foci_todo list [--tag T]"},
		{`foci_todo list-all foo`,
			"error: foci_todo list-all takes no positional argument, got 'foo'",
			"usage: foci_todo list-all [--tag T]"},
	} {
		assertTodoIDRefusal(t, body, c.cmd, c.wantErr, c.wantUsage)
	}
	for _, c := range []struct{ cmd, wantReq string }{
		{`foci_todo list`, `{"tool":"todo","params":{"action":"list"}}`},
		{`foci_todo list --tag x`, `{"tool":"todo","params":{"action":"list","tag":"x"}}`},
		{`foci_todo list-all --tag x`, `{"tool":"todo","params":{"action":"list","status":"all","tag":"x"}}`},
	} {
		if got := todoShellSingleCall(t, body, c.cmd); got != c.wantReq {
			t.Errorf("%s: request = %s, want exactly %s", c.cmd, got, c.wantReq)
		}
	}
	calls, rc, out := runCountingShellFunc(t, body, `foci_todo '' foo`, 0)
	if rc != 1 || calls != 0 {
		t.Errorf("foci_todo '' foo: rc=%d calls=%d, want rc 1 and 0 foci-calls\nout=%s", rc, calls, out)
	}
	if !strings.Contains(out, "usage: foci_todo <add|list|list-all|search|get|complete|drop|reopen|start|edit|remove> [args...]") {
		t.Errorf("foci_todo '' foo: an empty action must keep the master usage error\nout=%s", out)
	}
	if strings.Contains(out, "takes no positional") {
		t.Errorf("foci_todo '' foo: no action to name — the refusal must not fire\nout=%s", out)
	}
}

// collatingUTF8Locale returns an installed UTF-8 locale under which bash
// folds a non-ASCII digit into a [0-9] glob range (glibc's en_*.UTF-8 do;
// C and C.UTF-8 do not), or "" when none is installed. That is the
// deployment shape where the pre-fix range check waved ٣/０ through as
// item ids. The locale is probed rather than hardcoded, so the test
// below neither skips where the defect is reproducible nor runs where it
// is not — the probe itself fails (exit 1) under locales that reject ٣.
func collatingUTF8Locale(t *testing.T) string {
	t.Helper()
	out, err := osexec.Command("locale", "-a").Output()
	if err != nil {
		return ""
	}
	for _, name := range strings.Fields(string(out)) {
		if l := strings.ToLower(name); !strings.Contains(l, "utf8") && !strings.Contains(l, "utf-8") {
			continue
		}
		probe := osexec.Command("bash", "-c",
			"export LC_ALL="+bashSingleQuote(name)+"\n"+
				"case ٣ in ''|*[!0-9]*) exit 1 ;; esac")
		if probe.Run() == nil {
			return name
		}
	}
	return ""
}

// todoLocaleCmd prefixes cmd with an in-shell LC_ALL export, so the
// generated function's glob matching collates the way it would for an
// agent shell running in that locale. The assignment inside the script
// overrides any outer LC_ALL, making the run independent of the (scrubbed)
// test-process environment.
func todoLocaleCmd(locale, cmd string) string {
	return "export LC_ALL=" + bashSingleQuote(locale) + "; " + cmd
}

// TestTodoShellPositionalIDRejectsNonASCIIDigitsInUTF8Locale pins the ASCII
// half of req 1 of #2306: bash collates a [!0-9] glob RANGE by locale, so
// under glibc's UTF-8 locales the check as first written accepted
// non-ASCII digits — `get ٣` and `complete ０ --reason x` then leaked jq's
// --argjson text, called foci-call with an empty payload and returned 0,
// the exact defect #2306 exists to fix, invisible in the C-locale test
// environment. The explicit [!0123456789] digit set must refuse them in
// every locale, while plain digits keep working in the same locale. Skips
// only when no installed locale exhibits the collation.
func TestTodoShellPositionalIDRejectsNonASCIIDigitsInUTF8Locale(t *testing.T) {
	t.Parallel()
	locale := collatingUTF8Locale(t)
	if locale == "" {
		t.Skip("no installed UTF-8 locale collates non-ASCII digits into [0-9]")
	}
	body := todoIDShellBody(t)
	for _, c := range []struct{ cmd, wantErr, wantUsage string }{
		{todoLocaleCmd(locale, `foci_todo get ٣`),
			"error: foci_todo get: the item id must be a whole number, got '٣'",
			"usage: foci_todo get <id>"},
		{todoLocaleCmd(locale, `foci_todo complete ０ --reason x`),
			"error: foci_todo complete: the item id must be a whole number, got '０'",
			"usage: foci_todo complete <id>"},
	} {
		assertTodoIDRefusal(t, body, c.cmd, c.wantErr, c.wantUsage)
	}
	// The explicit set must not over-refuse in the same locale: plain
	// digits still send exactly one call with the same payload.
	cmd := todoLocaleCmd(locale, `foci_todo get 5`)
	if got, want := todoShellSingleCall(t, body, cmd),
		`{"tool":"todo","params":{"action":"get","id":5}}`; got != want {
		t.Errorf("%s (LC_ALL=%s): request = %s, want exactly %s", cmd, locale, got, want)
	}
}
