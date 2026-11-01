package main

// agents_delegated_refuser_test.go: the refuser-connection fakes and the
// characterisation of the question boundary, kept in a file that compiles
// against the base commit (no new-symbol references), so the red gate can
// prove the characterisation passes there.

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/platform"
)

// refuserClosures / refuserChoices are this file's own prompt fixtures
// (named distinctly from agents_delegated_prompt_test.go's promptClosures /
// promptChoices, so this characterisation file compiles on the base commit
// standalone).
func refuserClosures(conn platform.Connection) (approval, question func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice)) {
	return backendPromptFuncs(backendPromptDeps{
		ag:      &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
		connMgr: oneConnMgr{conn: conn},
		agentID: "main",
	})
}

func refuserChoices() []delegator.PromptChoice {
	return []delegator.PromptChoice{{Label: "Allow", Data: "allow"}, {Label: "Deny", Data: "deny"}}
}

// refuserConn is a Slack-like connection: ButtonSender plus
// platform.ApprovalRefuser, recording button sends, notifications and
// documents so a fail-closed denial is observable (nothing sent).
type refuserConn struct {
	buttonStubConn
	notices    []string
	documents  int
	buttonSend int
}

func (c *refuserConn) PlatformName() string   { return "slack" }
func (c *refuserConn) RefusesApprovals() bool { return true }
func (c *refuserConn) SendNotification(text string) {
	c.notices = append(c.notices, text)
}
func (c *refuserConn) SendDocument(string, string) error {
	c.documents++
	return nil
}
func (c *refuserConn) SendTextWithButtons(string, []platform.ButtonChoice, string) (string, error) {
	c.buttonSend++
	return "m-1", nil
}

// TestQuestionPrompt_RefuserConnStillPosts pins the boundary of the interim
// rule: a QUESTION (not an approval) through a connection that refuses
// approvals still posts — ask questions go to the asking session's chat and
// the requester answers their own question.
func TestQuestionPrompt_RefuserConnStillPosts(t *testing.T) {
	conn := &refuserConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c77"}}}

	_, question := refuserClosures(conn)
	question("main/c77", "req-q-refuser", "Which colour?", "Question", "", refuserChoices())

	if conn.buttonSend != 1 {
		t.Errorf("question button sends = %d, want 1 (questions still post)", conn.buttonSend)
	}
	if len(conn.notices) != 0 {
		t.Errorf("question produced %v denial notices, want none", conn.notices)
	}
	platform.CancelInteractiveMessage("req-q-refuser", "done") // keep the global store clean
}
