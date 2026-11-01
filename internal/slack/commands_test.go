package slack

// commands_test.go: the interim command rule (#2459 req 7) — no foci command
// can be run from Slack. Two proofs: behavioural (command-looking text from
// the REAL Socket Mode wire reaches the session's agent inbox verbatim, with
// zero replies — an intercepted command would have consumed the message and
// answered), and structural (the package's production sources import no
// command or dispatch machinery at all, so there is no path a Slack message
// could take to a dispatcher or a wizard; part B adds commands properly).

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestCommandTextThroughSocketIsOrdinaryMessage proves command-looking text
// arriving over the real socket is enqueued for the agent exactly as typed
// and produces no reply: no command ran, no wizard started, nothing
// intercepted. The third text is a channel mention — the mention is
// stripped, the command-looking remainder is still ordinary text.
func TestCommandTextThroughSocketIsOrdinaryMessage(t *testing.T) {
	h := newHarness(t, "helen", map[string]string{"C200": "helen"})
	stop := runSocket(t, h.fake, h.mgr.handleEnvelope)
	defer stop()

	before := h.fake.totalCalls()
	h.fake.pushEnvelope(t, envelopeFor(dmEvent("U1", "D100", "/restart", "2")))
	h.fake.pushEnvelope(t, envelopeFor(dmEvent("U1", "D100", "/secrets set k v", "3")))
	h.fake.pushEnvelope(t, envelopeFor(mentionEvent("U1", "C200", "<@UBOTID> /stop", "4", "")))
	envs := h.waitEnvelopes(h.mgr.core("helen"), 3)
	want := []string{"/restart", "/secrets set k v", "/stop"}
	for i, env := range envs {
		if env.Text != want[i] {
			t.Errorf("envelope %d text = %q, want %q verbatim (no interceptor consumed it)", i, env.Text, want[i])
		}
	}
	// A dispatcher would have answered (a confirmation, a refusal, a wizard
	// prompt); here nothing was posted and no other Web API call was made.
	if calls := h.fake.totalCalls(); calls != before {
		t.Errorf("command-looking text made %d Web API calls, want 0 (no command ran)", calls-before)
	}
}

// TestNoCommandMachineryImported pins the structural half of the rule: the
// slack package's production files import neither foci/internal/command nor
// foci/internal/dispatch — the only packages whose symbols a command
// dispatcher or wizard intake could call (telegram and discord intercept in
// their own receive handlers through them). A Slack message therefore has no
// route to a command or wizard, whatever it looks like (#2459 part B owns
// the real command surface).
func TestNoCommandMachineryImported(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"foci/internal/command":  true,
		"foci/internal/dispatch": true,
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if banned[path] {
				t.Errorf("%s: the %s transport source imports %s — text from Slack must never reach a command dispatcher or wizard (#2459 req 7; part B owns commands)", name, platformName, path)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no production sources found under the %s package — the invariant was not checked", platformName)
	}
}
