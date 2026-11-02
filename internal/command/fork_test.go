package command

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
)

// branchingManager is a DelegatedManager whose backend declares CapBranch.
func branchingManager() *agent.DelegatedManager {
	return &agent.DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return facetBrancherBackend{}, nil },
		Spec:       delegator.Spec{Caps: map[delegator.Capability]delegator.Support{delegator.CapBranch: delegator.Yes()}},
	}
}

// forkCC builds a CommandContext over a fresh store, with mgr as the agent's
// delegated manager (nil = API agent). minted records each surfaced chat;
// firsts, when non-nil, records the first message each mint was given.
func forkCC(t *testing.T, mgr *agent.DelegatedManager, minted *[]string, firsts ...*[]FirstMessage) CommandContext {
	t.Helper()
	store := session.NewStore(t.TempDir())
	return CommandContext{
		Agent:       &agent.Agent{Sessions: store, DelegatedManager: mgr},
		Sessions:    store,
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{ID: "test"},
		MintFacetConversation: func(_, sessionKey string, first FirstMessage) (string, error) {
			*minted = append(*minted, sessionKey)
			for _, f := range firsts {
				*f = append(*f, first)
			}
			return "conv-fork", nil
		},
	}
}

func paletteNames(t *testing.T, cc CommandContext) map[string]bool {
	t.Helper()
	r := NewRegistry()
	r.Register(FacetCommand())
	r.Register(ForkCommand())
	r.Register(PingCommand())
	names := map[string]bool{}
	for _, c := range r.VisibleList(context.Background(), Request{Source: "app"}, cc) {
		names[c.Name] = true
	}
	// The Telegram menu lists through AvailableList with an empty Request:
	// it must agree with the app palette on the branch rule.
	for _, c := range r.AvailableList(context.Background(), Request{}, cc) {
		if !names[c.Name] {
			t.Errorf("AvailableList has /%s but VisibleList does not", c.Name)
		}
	}
	return names
}

// TestBranchCommandsListing: /facet and /fork are listed exactly where the
// agent can branch (#957) — an API agent or a branch-capable backend — and
// hidden on a delegated backend that cannot branch.
func TestBranchCommandsListing(t *testing.T) {
	var minted []string
	cases := []struct {
		name string
		mgr  *agent.DelegatedManager
		want bool
	}{
		{"non-branching delegated backend", &agent.DelegatedManager{}, false},
		{"branching delegated backend", branchingManager(), true},
		{"API agent", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := paletteNames(t, forkCC(t, tc.mgr, &minted))
			if !names["ping"] {
				t.Fatalf("control: /ping missing from palette %v", names)
			}
			for _, cmd := range []string{"facet", "fork"} {
				if names[cmd] != tc.want {
					t.Errorf("/%s listed = %v, want %v", cmd, names[cmd], tc.want)
				}
			}
		})
	}
}

// TestFacetRefusedOnNonBranchingBackend: hiding /facet does not change its
// run-time refusal, word for word.
func TestFacetRefusedOnNonBranchingBackend(t *testing.T) {
	var minted []string
	cc := forkCC(t, &agent.DelegatedManager{}, &minted)
	ctx := tools.WithSessionKey(context.Background(), "test/c1")
	_, err := FacetCommand().Execute(ctx, Request{Source: "app"}, cc)
	if err == nil || err.Error() != "this agent's backend cannot branch, so it cannot host a facet" {
		t.Fatalf("err = %v, want the cannot-host-a-facet refusal", err)
	}
	if len(minted) != 0 {
		t.Errorf("a refused facet surfaced a chat: %v", minted)
	}
}

func appendHistory(t *testing.T, store *session.Store, key string, texts ...string) []provider.Message {
	t.Helper()
	var msgs []provider.Message
	for i, txt := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, provider.Message{Role: role, Content: []provider.ContentBlock{{Type: "text", Text: txt}}})
	}
	if err := store.TestAppendAll(key, msgs); err != nil {
		t.Fatalf("append: %v", err)
	}
	return msgs
}

func transcriptText(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			out = append(out, m.Role+":"+b.Text)
		}
	}
	return out
}

// TestForkAtEnd: /fork with no argument makes a new chat (not a facet) whose
// conversation is the parent's full history, and leaves the parent unchanged.
func TestForkAtEnd(t *testing.T) {
	var minted []string
	cc := forkCC(t, nil, &minted)
	const parent = "test/c1"
	want := appendHistory(t, cc.Sessions, parent, "first question", "first answer", "second question", "second answer")
	ctx := tools.WithSessionKey(context.Background(), parent)

	resp, err := ForkCommand().Execute(ctx, Request{Source: "app"}, cc)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(minted) != 1 {
		t.Fatalf("minted %d chats, want 1", len(minted))
	}
	branchKey := minted[0]
	if resp.OpenConversationID != "conv-fork" || !strings.Contains(resp.Text, "conv-fork") {
		t.Errorf("response %+v does not name and open the new chat", resp)
	}

	got, err := cc.Sessions.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("load fork: %v", err)
	}
	if !reflect.DeepEqual(transcriptText(got), transcriptText(want)) {
		t.Errorf("fork transcript = %v, want %v", transcriptText(got), transcriptText(want))
	}

	meta, err := cc.Sessions.GetBranchMeta(branchKey)
	if err != nil || meta == nil {
		t.Fatalf("branch meta: %v %v", meta, err)
	}
	if meta.BranchType != "fork" {
		t.Errorf("branch_type = %q, want fork", meta.BranchType)
	}
	if meta.Orientation != "" {
		t.Errorf("a fork got an orientation prompt: %q", meta.Orientation)
	}
	if cc.Agent.SessionNoCompact(branchKey) {
		t.Error("a fork has compaction disabled; only facets do")
	}

	parentNow, err := cc.Sessions.Load(parent)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if !reflect.DeepEqual(transcriptText(parentNow), transcriptText(want)) {
		t.Errorf("parent changed: %v", transcriptText(parentNow))
	}
}

// sessionFiles lists every session file under the store's directory.
func sessionFiles(t *testing.T, cc CommandContext, key string) []string {
	t.Helper()
	p, err := cc.Sessions.SessionPath(key)
	if err != nil {
		t.Fatalf("session path: %v", err)
	}
	var files []string
	_ = filepath.Walk(filepath.Dir(p), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// TestForkFirstMessage: text after /fork is handed to the new chat as its
// first message, whole and verbatim (including what used to be the refused
// message=<id> form), on every branchable backend. The sender id rides along
// only for an app request; with no text, nothing is sent (#2780).
func TestForkFirstMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		mgr  *agent.DelegatedManager
	}{
		{"API agent", nil},
		{"branching delegated backend", branchingManager()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []struct {
				req  Request
				want FirstMessage
			}{
				{Request{Source: "app", UserID: "dev1", Args: "  look at   the logs  "}, FirstMessage{Text: "look at   the logs", UserID: "dev1"}},
				{Request{Source: "app", UserID: "dev1", Args: "message=01HZZMSG"}, FirstMessage{Text: "message=01HZZMSG", UserID: "dev1"}},
				{Request{Source: SourceTelegram, UserID: "5970", Args: "hello"}, FirstMessage{Text: "hello"}},
				{Request{Source: "app", UserID: "dev1", Args: "   "}, FirstMessage{}},
				{Request{Source: "app", UserID: "dev1"}, FirstMessage{}},
			} {
				var minted []string
				var firsts []FirstMessage
				cc := forkCC(t, tc.mgr, &minted, &firsts)
				const parent = "test/c1"
				appendHistory(t, cc.Sessions, parent, "q", "a")
				ctx := tools.WithSessionKey(context.Background(), parent)

				resp, err := ForkCommand().Execute(ctx, c.req, cc)
				if err != nil {
					t.Fatalf("args %q: execute: %v", c.req.Args, err)
				}
				if len(firsts) != 1 || firsts[0] != c.want {
					t.Errorf("args %q (source %q): first messages = %+v, want [%+v]", c.req.Args, c.req.Source, firsts, c.want)
				}
				if resp.OpenConversationID != "conv-fork" {
					t.Errorf("args %q: OpenConversationID = %q, want conv-fork", c.req.Args, resp.OpenConversationID)
				}
				if len(minted) == 1 && minted[0] == parent {
					t.Errorf("args %q: minted the parent session, want a branch", c.req.Args)
				}
			}
		})
	}
}

// TestForkRefusedOnNonBranchingBackend: /fork's run-time refusal matches its
// listing rule.
func TestForkRefusedOnNonBranchingBackend(t *testing.T) {
	var minted []string
	cc := forkCC(t, &agent.DelegatedManager{}, &minted)
	ctx := tools.WithSessionKey(context.Background(), "test/c1")
	_, err := ForkCommand().Execute(ctx, Request{Source: "app"}, cc)
	if err == nil || !strings.Contains(err.Error(), "cannot branch") {
		t.Fatalf("err = %v, want the cannot-branch refusal", err)
	}
	if len(minted) != 0 {
		t.Errorf("refused fork surfaced a chat: %v", minted)
	}
}

// TestFacetFirstMessage: in the app, text after /facet goes to the new facet
// chat as its first message. From a chat platform, /facet <text> is refused
// before anything is forked or a facet bot acquired: a facet bot cannot render
// a turn the command layer starts (#2780).
func TestFacetFirstMessage(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		var minted []string
		var firsts []FirstMessage
		cc := forkCC(t, nil, &minted, &firsts)
		ctx := tools.WithSessionKey(context.Background(), "test/c1")
		if _, err := FacetCommand().Execute(ctx, Request{Source: "app", UserID: "dev1", Args: "check the deploy"}, cc); err != nil {
			t.Fatalf("execute: %v", err)
		}
		want := FirstMessage{Text: "check the deploy", UserID: "dev1"}
		if len(firsts) != 1 || firsts[0] != want {
			t.Errorf("first messages = %+v, want [%+v]", firsts, want)
		}
	})
	for _, src := range []string{SourceTelegram, SourceDiscord} {
		t.Run(src, func(t *testing.T) {
			var minted []string
			cc := forkCC(t, nil, &minted)
			const parent = "test/c1"
			appendHistory(t, cc.Sessions, parent, "q", "a")
			before := sessionFiles(t, cc, parent)
			ctx := tools.WithSessionKey(context.Background(), parent)
			_, err := FacetCommand().Execute(ctx, Request{Source: src, Args: "check the deploy"}, cc)
			if err == nil || !strings.Contains(err.Error(), "only in the app") {
				t.Fatalf("err = %v, want the app-only refusal", err)
			}
			if len(minted) != 0 {
				t.Errorf("refused facet surfaced a chat: %v", minted)
			}
			if after := sessionFiles(t, cc, parent); len(after) != len(before) {
				t.Errorf("refused facet created session files: before %v after %v", before, after)
			}
		})
	}
}
