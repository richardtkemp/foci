package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H12: the rendered prompt of every platform where "!" works tells the agent the
// user can run a command and that its output arrives as a message to reply to.
func TestPlatformPrompt_MentionsBangCommands(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"app", "telegram", "discord", "slack"} {
		var b strings.Builder
		writePlatform(&b, name, nil, false)
		text := b.String()
		for _, want := range []string{"`!ls /`", "shell command", "arrives as a message", "reply to it"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s prompt lacks %q", name, want)
			}
		}
	}
}

// A live workspace copy of the platform file predates the feature: the sentence
// still renders, once, beside the copy's own text.
func TestPlatformPrompt_StaleCopyCannotHideBangCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"app", "telegram", "discord", "slack"} {
		if err := os.WriteFile(filepath.Join(dir, "platform-"+name+".md"), []byte("Stale copy of "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		writePlatform(&b, name, []string{dir}, false)
		text := b.String()
		if !strings.Contains(text, "Stale copy of "+name) {
			t.Fatalf("%s: control: the copy was not used:\n%s", name, text)
		}
		if got := strings.Count(text, "`!ls /`"); got != 1 {
			t.Errorf("%s: the sentence appears %d times:\n%s", name, got, text)
		}
	}
}

func TestPlatformPrompt_NoBangCommandsOnAnUnknownPlatform(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlatform(&b, "signal", nil, false)
	if strings.Contains(b.String(), "`!ls /`") {
		t.Errorf("an unknown platform got the sentence: %s", b.String())
	}
}
