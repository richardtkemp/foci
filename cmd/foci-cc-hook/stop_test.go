package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator/stoprule"
)

// announceRule is the #2089 example rule: a present/future-tense start claim.
var announceRule = stoprule.Rule{
	Name: "announce_start",
	Text: []string{`(?i)\b(I'm|I am|I will|I'll) (now )?(start|kick|launch|send|run)(ing)?\b`},
}

// Transcript lines in the CC 2.1.280 shapes (see stoprule/turn.go).
const (
	tPrompt  = `{"type":"user","promptId":"p1","message":{"role":"user","content":"deploy it"}}`
	tText    = `{"type":"assistant","message":{"content":[{"type":"text","text":"I'm starting the deploy now."}]}}`
	tBgCall  = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"make deploy","run_in_background":true}}]}}`
	tBgReslt = `{"type":"user","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"Command running in background with ID: b1"}]},"toolUseResult":{"backgroundTaskId":"b1"}}`
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stopBody(t *testing.T, transcript, last string, active bool) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name":        "Stop",
		"transcript_path":        transcript,
		"prompt_id":              "p1",
		"stop_hook_active":       active,
		"last_assistant_message": last,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runStop(t *testing.T, body []byte) stopOutput {
	t.Helper()
	enc, err := stoprule.Encode([]stoprule.Rule{announceRule})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := handle([]string{"foci-cc-hook", installIDFlag, "id", stopRulesFlag, enc}, body)
	if !ok {
		t.Fatal("handle rejected fixture")
	}
	so, isStop := out.(stopOutput)
	if !isStop {
		t.Fatalf("Stop envelope produced %T, want stopOutput", out)
	}
	if so.InstallID != "id" || so.HookEvent != "Stop" {
		t.Errorf("install_id/hook_event = %q/%q", so.InstallID, so.HookEvent)
	}
	return so
}

// TestStop_AnnounceWithoutLaunchBlocks: a final reply announcing work, in a
// turn that launched nothing, is blocked with a reason quoting the claim.
func TestStop_AnnounceWithoutLaunchBlocks(t *testing.T) {
	path := writeTranscript(t, tPrompt, tText)
	out := runStop(t, stopBody(t, path, "Done reading. I'm starting the deploy now.", false))
	if out.Decision != "block" || out.Result != stoprule.ResultFire || out.Rule != "announce_start" {
		t.Fatalf("out = %+v, want a block by announce_start", out)
	}
	if !strings.Contains(out.Reason, "I'm starting") || !strings.Contains(out.Reason, "nothing is running") {
		t.Errorf("reason = %q", out.Reason)
	}
}

// TestStop_AnnounceWithBackgroundLaunchPasses: the same reply passes when
// the turn launched a background job.
func TestStop_AnnounceWithBackgroundLaunchPasses(t *testing.T) {
	path := writeTranscript(t, tPrompt, tBgCall, tBgReslt, tText)
	out := runStop(t, stopBody(t, path, "Done reading. I'm starting the deploy now.", false))
	if out.Decision != "" || out.Result != stoprule.ResultPassLaunched || out.Launches != 1 {
		t.Fatalf("out = %+v, want pass_launched with 1 launch", out)
	}
}

// TestStop_ActiveAllows: the Stop after a block (stop_hook_active) is always
// allowed, so a turn is blocked at most once.
func TestStop_ActiveAllows(t *testing.T) {
	path := writeTranscript(t, tPrompt, tText)
	out := runStop(t, stopBody(t, path, "I'm starting the deploy now.", true))
	if out.Decision != "" || out.Result != stoprule.ResultPassActive {
		t.Fatalf("out = %+v, want pass_active and no decision", out)
	}
}

// TestStop_NonMatchingPasses: a reply with no start claim passes.
func TestStop_NonMatchingPasses(t *testing.T) {
	path := writeTranscript(t, tPrompt, tText)
	out := runStop(t, stopBody(t, path, "The deploy finished; all green.", false))
	if out.Decision != "" || out.Result != stoprule.ResultPass || out.Rule != "" {
		t.Fatalf("out = %+v, want a plain pass", out)
	}
	if out.Excerpt == "" {
		t.Error("a pass should still carry an excerpt for the log")
	}
}

// TestStop_NoRulesPasses: a Stop with no --stop-rules never blocks.
func TestStop_NoRulesPasses(t *testing.T) {
	out, _ := handle([]string{"foci-cc-hook"}, stopBody(t, "", "I'm starting it now.", false))
	so := out.(stopOutput)
	if so.Decision != "" {
		t.Fatalf("out = %+v, want no decision", so)
	}
}

// TestStop_UnreadableTranscriptFailsOpen: a matching reply whose transcript
// can't be read passes and reports why.
func TestStop_UnreadableTranscriptFailsOpen(t *testing.T) {
	out := runStop(t, stopBody(t, "/nonexistent/t.jsonl", "I'm starting it now.", false))
	if out.Decision != "" || out.Result != stoprule.ResultPassError || out.Error == "" {
		t.Fatalf("out = %+v, want pass_error", out)
	}
}
