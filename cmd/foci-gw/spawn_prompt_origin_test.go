package main

import (
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/question"
	"foci/internal/tools"
)

// originTracker has one named spawn (branch b1700000000 of a/imain) and one from
// before names (b1600000000).
func originTracker() *tools.SpawnTracker {
	tr := tools.NewSpawnTracker()
	tr.Add("a/imain", tools.RunningSpawn{
		ID: "spawn-ab12", BranchKey: "a/imain/b1700000000", Name: "deploy\n watcher",
		Description: "wait for the deploy", Started: time.Now(),
	}, func() {})
	tr.Add("a/imain", tools.RunningSpawn{
		ID: "spawn-old", BranchKey: "a/imain/b1600000000",
		Description: "an old task", Started: time.Now(),
	}, func() {})
	return tr
}

// TestSpawnOrigin: a spawn's branch session is named by its id and its name, not
// its prompt (#2721); any other session has no spawn line (#2523). A spawn with
// no name (persisted before names) shows the placeholder.
func TestSpawnOrigin(t *testing.T) {
	t.Parallel()
	tr := originTracker()
	if got := spawnOrigin(tr, "a/imain/b1700000000"); got != "🐣 **From spawn spawn-ab12:** deploy watcher" {
		t.Errorf("spawn line = %q, want the spawn's id and name, not its prompt", got)
	}
	if got := spawnOrigin(tr, "a/imain/b1600000000"); got != "🐣 **From spawn spawn-old:** "+tools.UnnamedSpawnLabel {
		t.Errorf("unnamed spawn line = %q, want the placeholder", got)
	}
	for _, sk := range []string{"a/imain", "a/imain/b1800000000", ""} {
		if got := spawnOrigin(tr, sk); got != "" {
			t.Errorf("spawn line of %q = %q, want none", sk, got)
		}
	}
}

// TestOriginNote_OneRule pins, byte for byte, what every shape of source and
// target says (#2844): these are the texts backend prompts showed before the note
// moved to one place.
func TestOriginNote_OneRule(t *testing.T) {
	t.Parallel()
	const (
		spawn = "a/imain/b1700000000"
		fork  = "a/imain/b1800000000"
		root  = "a/imain"
		named = "🐣 **From spawn spawn-ab12:** deploy watcher"
	)
	moved := func(source string) string {
		return "↪️ **From session " + source + "** (its own chat cannot show this prompt)"
	}
	tr := originTracker()
	for _, tc := range []struct {
		name, source, target string
		spawned              bool
		want                 string
	}{
		{"own chat", root, root, false, "body"},
		{"fork in its own chat", fork, fork, false, "body"},
		{"fork moved up", fork, root, false, moved(fork) + "\n\nbody"},
		{"spawn on its owner chain", spawn, root, true, named + "\n\nbody"},
		{"spawn in its own chat", spawn, spawn, false, named + "\n\nbody"},
		{"spawn off its owner chain", spawn, root, false, moved(spawn) + "\n\n" + named + "\n\nbody"},
		{"no source", "", root, false, "body"},
	} {
		got := delegator.PrependOrigin(originNote(tr, tc.source, tc.target, tc.spawned, "prompt"), "body")
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// batchAskConn is an app-shaped connection that takes a batched ask.
type batchAskConn struct {
	buttonStubConn
	mu   sync.Mutex
	text []string
}

func (c *batchAskConn) SendInteractiveBatch(_ string, qs []platform.BatchQuestion, _ func([]string)) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = append(c.text, qs[0].Text)
	return true, nil
}

// TestAskOrigin: both ask presenters take their note from originNote, so a
// non-spawn ask shown in another chat names its session as a backend prompt does
// (#2844), and an ask in its own chat has none.
func TestAskOrigin(t *testing.T) {
	const (
		asker = "main/c1/b5"
		chat  = "main/c1"
	)
	moved := "↪️ **From session " + asker + "** (its own chat cannot show this question)\n\nWhich way?"
	otherChat := askChats(func(string) ([]string, bool) { return []string{chat}, false })
	ownChat := askChats(func(string) ([]string, bool) { return nil, false })
	for _, tc := range []struct {
		name  string
		chats askChats
		want  string
	}{
		{"moved", otherChat, moved},
		{"own chat", ownChat, "Which way?"},
		{"no walk", nil, "Which way?"},
	} {
		t.Run(tc.name+"/sequential", func(t *testing.T) {
			hub := &fakeAppHub{bound: map[string]bool{chat: true, asker: true}}
			msgID := "ask-2844-" + tc.name
			if id, _ := newAskPresentFn("main", hub, tc.chats, nil)(asker, msgID, "Which way?", "Q", askChoices(), func(string) {}); id == "" {
				t.Fatal("ask not shown")
			}
			t.Cleanup(func() { platform.CancelInteractiveMessage(msgID, "done") })
			if len(hub.texts) != 1 || hub.texts[0] != tc.want {
				t.Errorf("shown %q, want %q", hub.texts, tc.want)
			}
		})
		t.Run(tc.name+"/batched", func(t *testing.T) {
			conn := &batchAskConn{}
			qs := []question.Question{{Question: "Which way?"}, {Question: "And then?"}}
			if !newAskPresentBatchFn("main", oneConnMgr{conn: conn}, tc.chats, nil)(asker, "p", qs, func([]string) {}) {
				t.Fatal("batch not shown")
			}
			if len(conn.text) != 1 || conn.text[0] != tc.want {
				t.Errorf("shown %q, want %q", conn.text, tc.want)
			}
		})
	}
}

// TestMediaChats_SpawnFileNamesTheSpawn: a spawn's file in its owner's chat has
// the spawn line and not the redirected line, as the spawn's prompt does (#2844).
func TestMediaChats_SpawnFileNamesTheSpawn(t *testing.T) {
	t.Parallel()
	const parent, branch = "main/c28441", "main/c28441/b1"
	tools.Spawns.Add(parent, tools.RunningSpawn{ID: "spawn-2844", BranchKey: branch, Name: "report"}, func() {})
	t.Cleanup(func() { tools.Spawns.Stop(parent) })
	chats := mediaChats(backendPromptDeps{owner: func(string) (string, bool) { return parent, true }}, branch)
	if len(chats) != 1 || chats[0].SessionKey != parent || chats[0].Note != "🐣 **From spawn spawn-2844:** report" {
		t.Errorf("chats = %+v, want the owner's chat with the spawn line alone", chats)
	}
}
