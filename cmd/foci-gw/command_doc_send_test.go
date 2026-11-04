package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
)

// docRecordingConn records which document send a command reply used. It is
// session-bound, like the app or a Slack view: it has no SessionButtonSender.
type docRecordingConn struct {
	platform.Connection

	err        error
	plainSends int
	chatSends  []int64
}

func (c *docRecordingConn) SendDocument(string, string) error {
	c.plainSends++
	return c.err
}

func (c *docRecordingConn) SendDocumentToChat(chatID int64, _, _ string) error {
	c.chatSends = append(c.chatSends, chatID)
	return c.err
}

// chatAddressingDocConn is a connection that addresses chats itself, like a
// Telegram or Discord bot: its plain SendDocument goes to the LAST chat, which
// can belong to another user.
type chatAddressingDocConn struct {
	docRecordingConn
	defaultKey string
	chatID     int64
}

func (c *chatAddressingDocConn) DefaultSessionKey() string { return c.defaultKey }
func (c *chatAddressingDocConn) ChatID() int64             { return c.chatID }
func (c *chatAddressingDocConn) SendTextWithButtonsToSession(string, string, []platform.ButtonChoice, string) (string, error) {
	return "", nil
}
func (c *chatAddressingDocConn) EditMessageTextInSession(string, string, string) error { return nil }
func (c *chatAddressingDocConn) EditMessageWithButtonsInSession(string, string, string, []platform.ButtonChoice, string) error {
	return nil
}

var _ platform.SessionButtonSender = (*chatAddressingDocConn)(nil)

// docConnMgr separates the session's own connection from the primary, the
// distinction route.ConnFor reports as its outcome.
type docConnMgr struct {
	session platform.Connection
	primary platform.Connection
}

func (m docConnMgr) Primary(string) platform.Connection       { return m.primary }
func (m docConnMgr) AllForAgent(string) []platform.Connection { return nil }
func (m docConnMgr) ForSession(string) platform.Connection    { return m.session }
func (m docConnMgr) ForSessionOrPrimary(string, string) platform.Connection {
	if m.session != nil {
		return m.session
	}
	return m.primary
}
func (m docConnMgr) AcquireFacet(string) (platform.Connection, bool) { return nil, false }
func (m docConnMgr) HasFacet(string) bool                            { return false }
func (m docConnMgr) StartAll(context.Context)                        {}
func (m docConnMgr) Wait()                                           {}

// runDocCommand dispatches a command that answers with a temp document, through
// the real dispatch core, and returns its reply and the temp file's path.
func runDocCommand(t *testing.T, mgr platform.ConnectionManager, sessionKey string, reply command.Response) (command.Response, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply.DocPath = path
	doc := &command.Command{
		Name: "doc",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return reply, nil
		},
	}
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{doc}})
	inst := d.agents.at(testAgentID)
	resp, ok := runAgentCommand(context.Background(), inst.cmds, inst.cc, mgr, inst.id, sessionKey, "/doc")
	if !ok {
		t.Fatal("runAgentCommand: command did not execute")
	}
	return resp, path
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp document still on disk (stat err=%v): the send must remove it on every outcome", err)
	}
}

func assertNotDeliveredNote(t *testing.T, resp command.Response, want bool) {
	t.Helper()
	got := strings.Contains(resp.DisplayText(), "not delivered")
	if got != want {
		t.Errorf("reply says not delivered = %v, want %v; reply: %q", got, want, resp.DisplayText())
	}
}

// A session whose chat has no connection of its own falls back to the primary.
// On a bot shared by two users, the document must still reach the asking
// session's chat, never the primary's default or last chat.
func TestRunAgentCommandDoc_PrimaryFallbackGoesToSessionChat(t *testing.T) {
	primary := &chatAddressingDocConn{defaultKey: testAgentID + "/c111", chatID: 111}
	resp, path := runDocCommand(t, docConnMgr{primary: primary}, testAgentID+"/c777", command.Response{Text: "sent"})

	if primary.plainSends != 0 || len(primary.chatSends) != 1 || primary.chatSends[0] != 777 {
		t.Errorf("sends: plain=%d toChat=%v, want exactly one to chat 777", primary.plainSends, primary.chatSends)
	}
	assertRemoved(t, path)
	assertNotDeliveredNote(t, resp, false)
}

// A branch of a chat session parses to the root chat, the chat it renders in.
func TestRunAgentCommandDoc_BranchKeyGoesToRootChat(t *testing.T) {
	primary := &chatAddressingDocConn{defaultKey: testAgentID + "/c111", chatID: 111}
	_, _ = runDocCommand(t, docConnMgr{primary: primary}, testAgentID+"/c777/b123", command.Response{Text: "sent"})

	if len(primary.chatSends) != 1 || primary.chatSends[0] != 777 {
		t.Errorf("toChat=%v, want chat 777", primary.chatSends)
	}
}

// A chatless key (agent/i<name>) has no chat to name, so the destination rule
// of the session's prompts applies: the default chat, not the last chat.
func TestRunAgentCommandDoc_ChatlessKeyGoesToDefaultChat(t *testing.T) {
	primary := &chatAddressingDocConn{defaultKey: testAgentID + "/c111", chatID: 111}
	_, _ = runDocCommand(t, docConnMgr{primary: primary}, testAgentID+"/ijob", command.Response{Text: "sent"})

	if primary.plainSends != 0 || len(primary.chatSends) != 1 || primary.chatSends[0] != 111 {
		t.Errorf("sends: plain=%d toChat=%v, want exactly one to default chat 111", primary.plainSends, primary.chatSends)
	}
}

// A session-bound connection (the app, a Slack view) addresses itself. Handing
// it the key's chat ID would re-resolve a session it is already bound to.
func TestRunAgentCommandDoc_SessionBoundConnUsesItsOwnSession(t *testing.T) {
	conn := &docRecordingConn{}
	resp, path := runDocCommand(t, docConnMgr{session: conn}, testAgentID+"/c777", command.Response{Text: "sent"})

	if conn.plainSends != 1 || len(conn.chatSends) != 0 {
		t.Errorf("sends: plain=%d toChat=%v, want one plain send", conn.plainSends, conn.chatSends)
	}
	assertRemoved(t, path)
	assertNotDeliveredNote(t, resp, false)
}

// Nothing live to send through: the reply must not claim the document went out.
func TestRunAgentCommandDoc_NoConnectionIsReported(t *testing.T) {
	resp, path := runDocCommand(t, docConnMgr{}, testAgentID+"/c777", command.Response{Text: "Diff sent."})

	assertRemoved(t, path)
	assertNotDeliveredNote(t, resp, true)
	if !strings.Contains(resp.Text, "Diff sent.") {
		t.Errorf("the command's own text was lost: %q", resp.Text)
	}
}

// A failed send: the reply must not claim the document went out.
func TestRunAgentCommandDoc_SendFailureIsReported(t *testing.T) {
	conn := &docRecordingConn{err: errors.New("upload refused")}
	resp, path := runDocCommand(t, docConnMgr{session: conn}, testAgentID+"/c777", command.Response{Text: "Diff sent."})

	assertRemoved(t, path)
	assertNotDeliveredNote(t, resp, true)
}

// A command that answers in Parts renders through DisplayParts, which ignores
// Text, so the note must be a part of its own.
func TestRunAgentCommandDoc_FailureNoteReachesPartsReply(t *testing.T) {
	parts := []string{"one", "two"}
	resp, _ := runDocCommand(t, docConnMgr{}, testAgentID+"/c777", command.Response{Parts: parts})

	assertNotDeliveredNote(t, resp, true)
	if len(resp.Parts) != 3 || resp.Parts[0] != "one" || resp.Parts[1] != "two" {
		t.Errorf("parts = %q, want the two originals plus the note", resp.Parts)
	}
	if len(parts) != 2 {
		t.Errorf("the command's own Parts slice was modified: %q", parts)
	}
}

// A reply with no document is untouched, and needs no connection.
func TestRunAgentCommand_NoDocNoNote(t *testing.T) {
	doc := &command.Command{
		Name: "plain",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Text: "ok"}, nil
		},
	}
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{doc}})
	inst := d.agents.at(testAgentID)
	resp, ok := runAgentCommand(context.Background(), inst.cmds, inst.cc, docConnMgr{}, inst.id, testAgentID+"/c777", "/plain")
	if !ok || resp.Text != "ok" {
		t.Errorf("runAgentCommand = (%q, %v), want (\"ok\", true)", resp.Text, ok)
	}
}
