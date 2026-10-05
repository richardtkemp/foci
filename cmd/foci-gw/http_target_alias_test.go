package main

import (
	"net/http/httptest"
	"testing"

	"foci/internal/route"
	"foci/internal/session"
)

// TestResolveTargetSession_LiteralAliasWithQuestionMark is the #2158 ruling on
// the HTTP selector path (/send, /branch, /webhook): a selector that literally
// names a chat alias resolves to it even though it contains '?'; a selector
// that matches no alias still has its '?' read as the start of options.
func TestResolveTargetSession_LiteralAliasWithQuestionMark(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	if err := d.sessionIndex.SetChatAliasUnique(testAgentID, "app", 77, "what next?"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	res, rcpt, ok := resolveTargetSession(d, w, testAgentID, "what next?", "", "/send")
	if !ok {
		t.Fatalf("literal alias: not resolved, %d %s", w.Code, w.Body.String())
	}
	if want := session.NewChatSessionKey(testAgentID, 77); res.SessionKey != want || res.Rung != route.RungAlias {
		t.Errorf("literal alias: got %+v, want %s via alias", res, want)
	}
	if rcpt.Policy != "" {
		t.Errorf("literal alias: receipt policy = %q, want default", rcpt.Policy)
	}

	// No alias matches the whole selector: '?' starts options.
	w = httptest.NewRecorder()
	res, rcpt, ok = resolveTargetSession(d, w, testAgentID, "fresh?policy=strict", "", "/send")
	if !ok {
		t.Fatalf("options: not resolved, %d %s", w.Code, w.Body.String())
	}
	if res.SessionKey != testAgentID+"/ifresh" || rcpt.Policy != string(route.PolicyStrict) {
		t.Errorf("options: got %+v / %+v, want %s/ifresh with policy strict", res, rcpt, testAgentID)
	}
}
