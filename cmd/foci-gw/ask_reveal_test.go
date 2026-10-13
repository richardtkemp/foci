package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/platform"
	"foci/internal/tools"
)

// TestNewAskRevealFn_PostsToSessionChat proves the quiz-reveal wiring (#1316)
// actually leaves the gateway: newAskRevealFn's hook must route through
// route.NotifySessionChat — to the asking session's own chat (the
// SessionNotifier path), under the wiring agent's ID — so a quiz reveal lands
// with the user who answered, as the plain text the notification path
// delivers. Without the wiring the hook is a no-op and a chat user who answers
// a quiz question never sees the answer+explanation. Mutation arm: point the
// hook at a broadcast (plain SendNotification) or another agent's ID and the
// toSession/agentIDs assertions go red.
func TestNewAskRevealFn_PostsToSessionChat(t *testing.T) {
	conn := &sessionNotifyConn{stubConn: &stubConn{sessionKey: "x/c1"}}
	connMgr := &sessionRoutingConnMgr{conn: conn}

	newAskRevealFn("x", connMgr)("x/c1", "Capital?\nAnswer: 2. Oslo")

	if len(conn.broadcasts) != 0 {
		t.Errorf("reveal must not broadcast, got %v", conn.broadcasts)
	}
	if len(conn.toSession) != 1 || conn.toSession[0] != "x/c1|Capital?\nAnswer: 2. Oslo" {
		t.Errorf("toSession = %v, want the reveal verbatim in the session's chat", conn.toSession)
	}
	if len(connMgr.agentIDs) != 1 || connMgr.agentIDs[0] != "x" {
		t.Errorf("agentIDs = %v, want [x] (the wiring agent)", connMgr.agentIDs)
	}
}

// quizRevealTableConn is the connection the tool-table wiring test drives: a
// button-capable chat connection (so the sequential ask prompt registers in
// the platform's real interactive store) built on sessionNotifyConn's recording
// of how notifications leave the gateway. datas collects every posted button's
// callback data ("<promptID>:<idx>"), in order, so a test can press one through
// platform.HandleInteractiveCallback exactly as the platform layers do.
type quizRevealTableConn struct {
	*sessionNotifyConn
	datas []string
}

func (c *quizRevealTableConn) SendTextWithButtons(_ string, buttons []platform.ButtonChoice, _ string) (string, error) {
	for _, b := range buttons {
		c.datas = append(c.datas, b.Data)
	}
	return "quiz-table-msg", nil
}
func (c *quizRevealTableConn) EditMessageText(string, string) error { return nil }
func (c *quizRevealTableConn) EditMessageWithButtons(string, string, []platform.ButtonChoice, string) error {
	return nil
}

// TestToolTable_AskWiresQuizReveal proves the ask row of the tool table passes
// WithReveal to NewAskTool (#1316): a quiz question answered through the REAL
// sequential path — platform interactive store, button press via
// HandleInteractiveCallback — must post the withheld answer+explanation to the
// asking session's chat. TestNewAskRevealFn_PostsToSessionChat pins the hook
// itself; this pins that production tool construction actually wires it:
// without the WithReveal option the hook stays nil, every unit test stays
// green, and a chat user who answers a quiz question never sees the reveal.
// Not parallel: drives the process-global interactive prompt store.
func TestToolTable_AskWiresQuizReveal(t *testing.T) {
	const session = "test/quiz-wiring"
	conn := &quizRevealTableConn{sessionNotifyConn: &sessionNotifyConn{stubConn: &stubConn{sessionKey: session}}}
	connMgr := &sessionRoutingConnMgr{conn: conn}

	registry := tools.NewRegistry()
	registerTools(&toolDeps{
		p:        minimalSetupParams(t, "test"),
		path:     pathAPI,
		registry: registry,
		connMgr:  connMgr,
		agLazy:   func() *agent.Agent { return nil },
		wakeFn:   stubWakeFn,
		out:      &toolOutputs{},
	})
	ask := registry.Get("ask")
	if ask == nil {
		t.Fatal("ask tool not registered on the API path")
	}

	// One quiz question; Execute is async and returns once it is posted.
	input := `{"questions":[{"question":"Capital of <Norway>?","options":[{"label":"Oslo"},{"label":"Bergen"}],"quiz":{"correct":[0],"explanation":"Oslo since 1814."}}]}`
	if _, err := ask.Execute(tools.WithSessionKey(context.Background(), session), json.RawMessage(input)); err != nil {
		t.Fatalf("ask execute: %v", err)
	}
	if len(conn.datas) != 3 { // Oslo, Bergen, Cancel
		t.Fatalf("posted buttons %v, want the two options plus Cancel", conn.datas)
	}

	// Press option 2 (Bergen — the wrong answer) through the real router.
	if _, _, ok := platform.HandleInteractiveCallback(conn.datas[1]); !ok {
		t.Fatal("router did not know the pressed prompt")
	}

	want := strings.Join([]string{
		"Capital of <Norway>?",
		"Answer: 1. Oslo",
		"❌ Incorrect — you answered Bergen.",
		"Explanation: Oslo since 1814.",
	}, "\n")
	if len(conn.toSession) != 1 || conn.toSession[0] != session+"|"+want {
		t.Errorf("session notifications = %v,\nwant exactly [%s|%s]", conn.toSession, session, want)
	}
	if len(conn.broadcasts) != 0 {
		t.Errorf("reveal must not broadcast, got %v", conn.broadcasts)
	}
}
