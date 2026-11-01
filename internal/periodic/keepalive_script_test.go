package periodic

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/session"
	"foci/shared/prompts"
)

// Not parallel: log.SetOutput and procx.SetOperatorEnv are process-global.

// writeScript writes an executable shell script and returns its path.
func writeScript(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func captureKeepaliveLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(nil) })
	return &out
}

// keepaliveWarnLines returns the WARN lines in a captured log. Reading it after
// waitIdle is safe: waitIdle's lock gives the happens-before edge.
func keepaliveWarnLines(out string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARN") {
			got = append(got, l)
		}
	}
	return got
}

// indexWithTouches is indexWithTouch for several sessions.
func indexWithTouches(t *testing.T, keys []string, touchAgo time.Duration) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	for _, sk := range keys {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:  sk,
			FilePath:    "/tmp/test.jsonl",
			CreatedAt:   time.Now().Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat,
			Status:      session.SessionStatusActive,
		})
		idx.TouchCacheTouch(sk, time.Now().Add(-touchAgo))
	}
	return idx
}

type branchCall struct{ session, prompt string }

// scriptRunner builds a runner warming three open app chats, the first of
// which is the default session, with the given keepalive prompt config.
func scriptRunner(t *testing.T, prompt string, searchDirs []string) (*Runner, *[]branchCall, *sync.Mutex) {
	t.Helper()
	keys := []string{"test/main", "test/chat2", "test/chat3"}
	var mu sync.Mutex
	var calls []branchCall
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled:          true,
			Interval:         "1m",
			Prompt:           prompt,
			WarmOpenAppChats: true,
		},
		cacheTTL:               time.Hour,
		promptSearchDirs:       searchDirs,
		keepaliveScriptTimeout: 5 * time.Second,
		sessionIndex:           indexWithTouches(t, keys, 10*time.Minute),
		openChatSessionsFn:     func() []string { return keys },
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/main" },
			branchFn: func(_, sk, promptText string, _ bool) bool {
				mu.Lock()
				calls = append(calls, branchCall{sk, promptText})
				mu.Unlock()
				return true
			},
		},
		done: make(chan struct{}),
	}
	return r, &calls, &mu
}

// TestMaybeKeepalive_ScriptRunsPerSession: a keepalive.sh in the prompt dir
// runs once per target with <agentID> <sessionKey> and the per-run inputs,
// and its stdout is the prompt that session receives.
func TestMaybeKeepalive_ScriptRunsPerSession(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "runs")
	writeScript(t, dir, "keepalive.sh",
		`echo run >> `+marker+`
echo "args=$1,$2 idx=$FOCI_KEEPALIVE_INDEX count=$FOCI_KEEPALIVE_COUNT default=$FOCI_KEEPALIVE_DEFAULT"
`, 0o700)
	// A keepalive.md in the same dir must lose to the script.
	if err := os.WriteFile(filepath.Join(dir, "keepalive.md"), []byte("markdown prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	r, calls, mu := scriptRunner(t, "", []string{dir})
	r.maybeKeepalive(context.Background())
	waitIdle(t, r)

	mu.Lock()
	defer mu.Unlock()
	want := []branchCall{
		{"test/main", "args=test,test/main idx=0 count=3 default=1"},
		{"test/chat2", "args=test,test/chat2 idx=1 count=3 default=0"},
		{"test/chat3", "args=test,test/chat3 idx=2 count=3 default=0"},
	}
	if len(*calls) != len(want) {
		t.Fatalf("branch calls = %+v, want %+v", *calls, want)
	}
	for i, w := range want {
		if (*calls)[i] != w {
			t.Errorf("call %d = %+v, want %+v", i, (*calls)[i], w)
		}
	}
	runs, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(runs), "run"); n != 3 {
		t.Errorf("script ran %d times, want once per target (3)", n)
	}
}

// TestMaybeKeepalive_ExplicitScriptPath: an executable at the configured
// [keepalive] prompt path is run, not read.
func TestMaybeKeepalive_ExplicitScriptPath(t *testing.T) {
	p := writeScript(t, t.TempDir(), "ka-anything", `echo "explicit $2"`+"\n", 0o700)
	r, calls, mu := scriptRunner(t, p, nil)
	r.maybeKeepalive(context.Background())
	waitIdle(t, r)
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 3 || (*calls)[1].prompt != "explicit test/chat2" {
		t.Errorf("calls = %+v, want each session's own script output", *calls)
	}
}

// TestMaybeKeepalive_MarkdownPromptUnchanged (characterisation): a markdown
// prompt is sent verbatim, the same text to every target, as before #1351.
func TestMaybeKeepalive_MarkdownPromptUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keepalive.md"), []byte("  md ping \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, calls, mu := scriptRunner(t, "", []string{dir})
	r.maybeKeepalive(context.Background())
	waitIdle(t, r)
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 3 {
		t.Fatalf("calls = %+v, want 3", *calls)
	}
	for _, c := range *calls {
		if c.prompt != "md ping" {
			t.Errorf("session %s got %q, want %q", c.session, c.prompt, "md ping")
		}
	}
}

// TestKeepaliveScript_FailureFallsBackToPing: a failing script never silences
// a keepalive — the session gets the embedded ping and one WARN naming agent,
// session and reason. The timeout case uses a short injected bound and a
// script whose CHILD holds stdout, so it also proves the group is killed.
func TestKeepaliveScript_FailureFallsBackToPing(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		timeout            time.Duration
	}{
		{"exit 1", "echo partial; echo boom >&2; exit 1\n", "exit status 1", 10 * time.Second},
		{"empty output", "echo '   '\n", "empty output", 10 * time.Second},
		{"timeout", "sleep 30\necho late\n", "timed out", 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureKeepaliveLog(t)
			p := writeScript(t, t.TempDir(), "keepalive.sh", tc.body, 0o700)
			r := &Runner{
				log:                    log.NewComponentLogger("keepalive:test"),
				agentID:                "agentX",
				keepaliveScriptTimeout: tc.timeout,
			}
			done := make(chan string, 1)
			go func() {
				done <- r.keepaliveScriptPrompt(context.Background(), p, keepaliveRun{sessionKey: "agentX/s1", count: 1})
			}()
			var got string
			select {
			case got = <-done:
			case <-time.After(20 * time.Second): // hang-guard only
				t.Fatal("keepalive script run did not return; the bound did not hold")
			}
			if got != prompts.Keepalive() {
				t.Errorf("prompt = %q, want the embedded ping", got)
			}
			warns := keepaliveWarnLines(out.String())
			if len(warns) != 1 {
				t.Fatalf("WARN lines = %q, want exactly 1", warns)
			}
			for _, s := range []string{"agentX", "agentX/s1", tc.reason} {
				if !strings.Contains(warns[0], s) {
					t.Errorf("WARN %q missing %q", warns[0], s)
				}
			}
		})
	}
}

// TestKeepaliveScript_SpawnErrorFallsBack: a script that cannot be started
// (here: removed between resolution and run) falls back to the ping too.
func TestKeepaliveScript_SpawnErrorFallsBack(t *testing.T) {
	out := captureKeepaliveLog(t)
	r := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "agentX"}
	got := r.keepaliveScriptPrompt(context.Background(), filepath.Join(t.TempDir(), "gone.sh"), keepaliveRun{sessionKey: "agentX/s1"})
	if got != prompts.Keepalive() {
		t.Errorf("prompt = %q, want the embedded ping", got)
	}
	if w := keepaliveWarnLines(out.String()); len(w) != 1 || !strings.Contains(w[0], "agentX/s1") {
		t.Errorf("WARN lines = %q, want one naming the session", w)
	}
}

// TestKeepaliveScript_UnreadableExecutable (#1351 F2): an executable that
// cannot be read (here mode 0111, so not even by its owner — the spec's 0711
// file owned by someone else) is a script: its output is the prompt and
// nothing is logged; when it fails, the one WARN is about the script, not
// about a read.
func TestKeepaliveScript_UnreadableExecutable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0111 file regardless")
	}

	t.Run("works", func(t *testing.T) {
		out := captureKeepaliveLog(t)
		dir := t.TempDir()
		// /bin/sh must read a script to run it, so an unreadable shell script
		// cannot work; an unreadable BINARY can. Use a copy of this test
		// binary in helper mode (see TestMain): it echoes its arguments.
		copyTestBinary(t, filepath.Join(dir, "keepalive.sh"), 0o111)
		t.Setenv(kaHelperEnv, "1")
		r, calls, mu := scriptRunner(t, "", []string{dir})
		r.maybeKeepalive(context.Background())
		waitIdle(t, r)
		mu.Lock()
		defer mu.Unlock()
		if len(*calls) != 3 || (*calls)[0].prompt != "test test/main" {
			t.Errorf("calls = %+v, want echo of <agent> <session>", *calls)
		}
		if w := keepaliveWarnLines(out.String()); len(w) != 0 {
			t.Errorf("WARN lines = %q, want none", w)
		}
	})

	t.Run("fails", func(t *testing.T) {
		out := captureKeepaliveLog(t)
		dir := t.TempDir()
		// Unreadable shell script: sh cannot read it, so the run fails.
		p := writeScript(t, dir, "keepalive.sh", "echo hi\n", 0o111)
		r := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "test", keepaliveScriptTimeout: 10 * time.Second}
		src := prompts.ResolveKeepaliveSource(p)
		if src.Script != p {
			t.Fatalf("source = %+v, want script %s", src, p)
		}
		got := r.keepaliveScriptPrompt(context.Background(), src.Script, keepaliveRun{sessionKey: "test/main"})
		if got != prompts.Keepalive() {
			t.Errorf("prompt = %q, want the embedded ping", got)
		}
		w := keepaliveWarnLines(out.String())
		if len(w) != 1 || !strings.Contains(w[0], "keepalive script") || strings.Contains(w[0], "read ") {
			t.Errorf("WARN lines = %q, want exactly one, about the script and not a read", w)
		}
	})
}

// kaHelperEnv switches the test binary into helper mode (TestMain): print the
// arguments and exit, standing in for an unreadable compiled keepalive script.
const kaHelperEnv = "FOCI_1351_KA_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(kaHelperEnv) == "1" {
		fmt.Println(strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
	// Keep scripts these tests run away from a live foci (writing-code/go.md).
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR", "FOCI_SESSION_KEY"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// copyTestBinary copies the running test binary to dst with mode.
func copyTestBinary(t *testing.T, dst string, mode os.FileMode) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatal(err)
	}
}

// TestKeepaliveScript_RunsAsOperator (S1): the script gets the operator
// environment, as the agent's own shell does, not the daemon's (Trusted).
func TestKeepaliveScript_RunsAsOperator(t *testing.T) {
	procx.SetOperatorEnv(map[string]string{"FOCI_1351_OPERATOR": "yes"})
	t.Cleanup(func() { procx.SetOperatorEnv(nil) })
	p := writeScript(t, t.TempDir(), "keepalive.sh", `echo "op=$FOCI_1351_OPERATOR"`+"\n", 0o700)
	r := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "test"}
	if got := r.keepaliveScriptPrompt(context.Background(), p, keepaliveRun{sessionKey: "test/main"}); got != "op=yes" {
		t.Errorf("prompt = %q, want op=yes (operator environment)", got)
	}
}
