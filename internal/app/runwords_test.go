package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/turnevent"
	"foci/internal/usercmd"
)

// The cases of the app's CodeBlockRunTest.kt (foci-client): the server reads a
// block's first word exactly as the app does (#3209), or a block loses its button.
func TestCodeBlockRunWord_MatchesApp(t *testing.T) {
	type c struct {
		code, lang string
		want       string // "" = no word
	}
	var cases []c
	for _, lang := range []string{"", "bash", "sh", "zsh", "shell", "Bash", "SHELL", " sh ", "bash title=x"} {
		cases = append(cases, c{"ls -la /\n", lang, "ls"})
	}
	for _, lang := range []string{"python", "go", "text", "json", "diff", "console", "mermaid", "shell-session", "bashrc"} {
		cases = append(cases, c{"ls -la /\n", lang, ""})
	}
	for _, sb := range []string{"#!/bin/bash", "#!/bin/sh", "#! /bin/zsh", "#!/usr/bin/env bash", "#!/usr/bin/env -S bash -e", "#!/usr/bin/env A=1 sh"} {
		cases = append(cases, c{sb + "\nls", "", "ls"})
	}
	for _, sb := range []string{"#!/usr/bin/env python3", "#!/usr/bin/python3", "#!/usr/bin/ruby", "#!/usr/bin/env", "#!"} {
		cases = append(cases, c{sb + "\nls", "", ""})
	}
	cases = append(cases,
		c{"$ ls\n", "console", ""},
		c{"\n\n  ls\t-la\n", "", "ls"},
		c{"/usr/bin/env bash", "sh", "/usr/bin/env"},
		c{"./run.sh --now\necho done\n", "", "./run.sh"},
		c{"git\nstatus", "", "git"},
		c{"cd /tmp\nls", "", "cd"},
		c{"export A=1", "bash", "export"},
		c{"foci_todo list", "", "foci_todo"},
		c{"for i in 1 2; do echo $i; done", "sh", "for"},
		c{"{ ls; }", "", "{"},
		c{"(cd /tmp && ls)", "", "("},
		c{"# list it\nls -la", "", "ls"},
		c{"  # one\n\n# two\n  ls", "bash", "ls"},
		c{"# one\r\nls\r\n", "", "ls"},
		c{"FOO=1 cmd --flag", "", "cmd"},
		c{"FOO=1 BAR=2 cmd", "sh", "cmd"},
		c{"FOO=\"a b\" BAR='c d' BAZ=e\\ f cmd", "", "cmd"},
		c{"FOO=\"a\\\"b\" cmd", "", "cmd"},
		c{"FOO=$(date +%s) cmd", "", "cmd"},
		c{"FOO=$(a \"b)\" (c)) cmd", "", "cmd"},
		c{"FOO=`date` cmd", "", "cmd"},
		c{"LIST=(a b) cmd", "", "cmd"},
		c{"PATH+=:x cmd", "", "cmd"},
		c{"FOO= cmd", "", "cmd"},
		c{"FOO=1 \\\n  cmd", "", "cmd"},
		c{"FOO=1\nBAR=2\ncmd", "", "cmd"},
		c{"# set up\nFOO=1 # why\n\ncmd", "", "cmd"},
		c{"FOO=1; cmd", "", "cmd"},
		c{"FOO=1;cmd", "", "cmd"},
		c{"FOO=1 && cmd", "", "cmd"},
		c{"FOO=1", "", ""},
		c{"FOO=1 BAR=2\n", "", ""},
		c{"# only a note", "", ""},
		c{"# one\n\n# two\nFOO=1 # three", "", ""},
		c{"FOO=1;", "", ""},
		c{"FOO=\"open cmd", "", ""},
		c{"FOO='open cmd", "", ""},
		c{"FOO=$(open cmd", "", ""},
		c{"FOO=1 | cmd", "", ""},
		c{";; cmd", "", ""},
		c{"1FOO=1 cmd", "", "1FOO=1"},
		c{"a-b=1 cmd", "", "a-b=1"},
		c{"=1 cmd", "", "=1"},
		c{"ls;pwd", "", "ls"},
		c{"ls|wc -l", "", "ls"},
		c{"sleep 1&", "", "sleep"},
		c{"echo>f", "", "echo"},
		c{"foo() { :; }", "", ""},
		c{"print(\"hi\")", "", ""},
		c{"FOO=1 print(1)", "", ""},
		c{"print (1)", "", "print"},
		c{"# comment\nls", "python", ""},
		c{"FOO=1 ls", "text", ""},
		c{"", "", ""},
		c{"  \n\t\n", "bash", ""},
	)
	for _, tc := range cases {
		got, ok := codeBlockRunWord(tc.code, tc.lang)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("codeBlockRunWord(%q, %q) = %q, %v; want %q", tc.code, tc.lang, got, ok, tc.want)
		}
	}
}

func TestFencedCodeBlocks(t *testing.T) {
	text := strings.Join([]string{
		"Run this:",
		"```bash",
		"/tmp/civicos-db-snooze.sh",
		"```",
		"and this:",
		"~~~~ sh",
		"ls -la",
		"```",
		"~~~~",
		"1. step one",
		"   ```",
		"   git status",
		"   ```",
		"> ```zsh",
		"> pwd",
		"> ```",
		"inline ```not a fence``` here",
		"```go",
		"fmt.Println()",
		"```",
		"````",
		"echo unclosed",
	}, "\n")
	got := fencedCodeBlocks(text)
	want := []fencedBlock{
		{"bash", "/tmp/civicos-db-snooze.sh"},
		{"sh", "ls -la\n```"},
		{"", "   git status"},
		{"zsh", "pwd"},
		{"go", "fmt.Println()"},
		{"", "echo unclosed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fencedCodeBlocks =\n%q\nwant\n%q", got, want)
	}
}

func TestRunnableWords(t *testing.T) {
	text := "```bash\n/tmp/civicos-db-snooze.sh\n```\n```\nnope --x\n```\n```sh\n/tmp/civicos-db-snooze.sh again\n```\n```python\nls\n```\n```\nFOO=1 git status\n```"
	var asked []string
	got := runnableWords(text, func(w string) bool {
		asked = append(asked, w)
		return w != "nope"
	})
	if want := []string{"/tmp/civicos-db-snooze.sh", "git"}; !reflect.DeepEqual(got, want) {
		t.Errorf("runnableWords = %q, want %q", got, want)
	}
	if want := []string{"/tmp/civicos-db-snooze.sh", "nope", "git"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("asked %q, want each candidate word once, %q", asked, want)
	}
	if got := runnableWords("no code here, just `ls`", func(string) bool { t.Fatal("asked"); return true }); got != nil {
		t.Errorf("no block: %q", got)
	}
	if got := runnableWords(text, nil); got != nil {
		t.Errorf("nil runnable: %q", got)
	}
}

// cmdAgent is a fakeAgent with "!" support: runnable lists the words it can start.
type cmdAgent struct {
	fakeAgent
	runnable map[string]bool
}

func (a *cmdAgent) CheckUserCommand(string) error                                { return nil }
func (a *cmdAgent) RedactUserCommand(s string) string                            { return s }
func (a *cmdAgent) UserCommandRunnable(word string) bool                         { return a.runnable[word] }
func (a *cmdAgent) EnqueueUserCommandResult(agent.Envelope, usercmd.Result) bool { return true }
func (a *cmdAgent) RunUserCommand(context.Context, string, string, func(string) (string, error)) usercmd.Result {
	return usercmd.Result{}
}

var _ userCommandAgent = (*cmdAgent)(nil)

const runBlockText = "Try:\n```bash\n/tmp/civicos-db-snooze.sh\n```\nor\n```\nmissing-tool\n```"

func wantRunnableWords(t *testing.T, d map[string]any, want ...string) {
	t.Helper()
	raw, _ := d["runnableWords"].([]any)
	var got []string
	for _, w := range raw {
		s, _ := w.(string)
		got = append(got, s)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runnableWords = %q, want %q (frame %v)", got, want, d)
	}
}

// The ticket's case (#3209): the agent's reply carries its blocks' runnable
// words when it is sent, so the app draws the button without asking.
func TestAgentReply_CarriesRunnableWords(t *testing.T) {
	h := newTestHub()
	b := h.ensureBinding(nil, "cam", "conv-1")
	client := fakeClient()
	b.attach(client)
	conn := &appConn{hub: h, agentID: "cam", agentRef: &cmdAgent{runnable: map[string]bool{"/tmp/civicos-db-snooze.sh": true}}}

	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	defer cleanup()
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: runBlockText})
	var msg map[string]any
	for _, f := range drain(t, client) {
		if f.t == "message" {
			msg = f.d
		}
	}
	if msg == nil {
		t.Fatal("no message frame")
	}
	wantRunnableWords(t, msg, "/tmp/civicos-db-snooze.sh")

	// A streamed reply: text.end carries them for its final text.
	sink2, cleanup2 := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	defer cleanup2()
	ctx := context.Background()
	sink2.Emit(ctx, turnevent.TurnStart{})
	sink2.Emit(ctx, turnevent.TextDelta{Delta: runBlockText})
	sink2.Emit(ctx, turnevent.TurnComplete{FinalText: runBlockText})
	var end map[string]any
	for _, f := range drain(t, client) {
		if f.t == "text.end" {
			end = f.d
		}
	}
	if end == nil {
		t.Fatal("no text.end frame")
	}
	wantRunnableWords(t, end, "/tmp/civicos-db-snooze.sh")
}

func TestSentMessages_CarryRunnableWords(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := withIdent(&convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}, "", "ag/c7")
	h.bySession[b.key()] = b
	conn := &appConn{hub: h, agentID: "ag", agentRef: &cmdAgent{runnable: map[string]bool{"/tmp/civicos-db-snooze.sh": true}}}

	if err := conn.SendToSession(b.key(), runBlockText); err != nil {
		t.Fatal(err)
	}
	id, err := conn.SendEditableInjected(b.key(), runBlockText)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.EditInjectedMessage(b.key(), id, "now\n```\nmissing-tool\n```"); err != nil {
		t.Fatal(err)
	}
	got := drain(t, c)
	if len(got) != 3 {
		t.Fatalf("frames = %v", types(got))
	}
	wantRunnableWords(t, got[0].d, "/tmp/civicos-db-snooze.sh")
	wantRunnableWords(t, got[1].d, "/tmp/civicos-db-snooze.sh")
	if got[2].t != "message.edit" {
		t.Fatalf("third frame = %s", got[2].t)
	}
	wantRunnableWords(t, got[2].d) // the edit replaces the list: none now

	// An agent with no "!" support marks nothing.
	plain := &appConn{hub: h, agentID: "ag", agentRef: &fakeAgent{}}
	if err := plain.SendToSession(b.key(), runBlockText); err != nil {
		t.Fatal(err)
	}
	got = drain(t, c)
	if _, ok := got[0].d["runnableWords"]; ok {
		t.Errorf("no-! agent: %v", got[0].d)
	}
}
