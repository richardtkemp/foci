package cctmux

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test fakes
// ---------------------------------------------------------------------------

// fakeTmux is an in-memory tmuxExecFunc that records every invocation
// (args as passed, socket flags included, plus stdin) and answers via an
// optional scriptable respond function.
type fakeTmux struct {
	mu      sync.Mutex
	calls   [][]string
	stdins  []string
	respond func(args []string, stdin string) (string, error)
}

func (f *fakeTmux) exec(_ context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.stdins = append(f.stdins, stdin)
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return "", nil
	}
	return respond(args, stdin)
}

// allCalls returns a copy of all recorded invocations.
func (f *fakeTmux) allCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// callsFor returns recorded invocations whose first argument equals cmd.
func (f *fakeTmux) callsFor(cmd string) [][]string {
	var out [][]string
	for _, c := range f.allCalls() {
		if len(c) > 0 && c[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

// newFakePane returns a tmuxPane wired to a fresh fakeTmux recorder.
func newFakePane(windowName string) (*tmuxPane, *fakeTmux) {
	f := &fakeTmux{}
	return &tmuxPane{windowName: windowName, exec: f.exec}, f
}

// shortEnterDelays shrinks the Enter-retry backoff schedule for the duration
// of the test so sendText-based paths complete in milliseconds.
func shortEnterDelays(t *testing.T) {
	t.Helper()
	old := enterRetryDelays
	enterRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { enterRetryDelays = old })
}

// hasSubsequence reports whether want appears as a contiguous subsequence of args.
func hasSubsequence(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

// TestPaneCreate_CommandConstruction proves create builds the tmux new-session
// invocation correctly: detached session named after the window, optional size
// and workdir flags, and a login-shell command embedding quoted env exports
// and quoted claude args.
func TestPaneCreate_CommandConstruction(t *testing.T) {
	cases := []struct {
		name       string
		pane       tmuxPane
		claudeArgs []string
		envVars    []string
		wantFlags  [][]string // contiguous arg subsequences that must appear
		skipFlags  [][]string // subsequences that must NOT appear
		wantInCmd  []string   // substrings of the final shell command
	}{
		{
			name:       "full options",
			pane:       tmuxPane{windowName: "cc-main", workDir: "/work", cols: 200, rows: 50},
			claudeArgs: []string{"--model", "opus"},
			envVars:    []string{"FOCI_SOCK=/tmp/sock"},
			wantFlags:  [][]string{{"new-session", "-d", "-s", "cc-main"}, {"-x", "200", "-y", "50"}, {"-c", "/work"}},
			// The full command is shell-quoted again for "sh -l -c", so
			// inner quotes are nested — assert on the raw payloads.
			wantInCmd: []string{"export ", "FOCI_SOCK=/tmp/sock", "claude ", "--model", "opus", "sh -l -c "},
		},
		{
			name:      "minimal options omit size and workdir",
			pane:      tmuxPane{windowName: "cc-min"},
			skipFlags: [][]string{{"-x"}, {"-c"}},
			wantInCmd: []string{"claude"},
		},
		{
			name:      "size requires both cols and rows",
			pane:      tmuxPane{windowName: "cc-half", cols: 100},
			skipFlags: [][]string{{"-x"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTmux{}
			tc.pane.exec = f.exec
			if err := tc.pane.create(context.Background(), tc.claudeArgs, tc.envVars...); err != nil {
				t.Fatalf("create: %v", err)
			}
			calls := f.allCalls()
			if len(calls) != 1 {
				t.Fatalf("got %d tmux calls, want 1", len(calls))
			}
			args := calls[0]
			for _, want := range tc.wantFlags {
				if !hasSubsequence(args, want) {
					t.Errorf("args %v missing subsequence %v", args, want)
				}
			}
			for _, skip := range tc.skipFlags {
				if hasSubsequence(args, skip) {
					t.Errorf("args %v should not contain %v", args, skip)
				}
			}
			shellCmd := args[len(args)-1]
			for _, want := range tc.wantInCmd {
				if !strings.Contains(shellCmd, want) {
					t.Errorf("shell command %q missing %q", shellCmd, want)
				}
			}
		})
	}
}

// TestPaneCreate_Error proves create wraps a tmux failure with the window
// name and the command's output for diagnosis.
func TestPaneCreate_Error(t *testing.T) {
	pane, f := newFakePane("cc-err")
	f.respond = func([]string, string) (string, error) {
		return "duplicate session: cc-err\n", errors.New("exit status 1")
	}
	err := pane.create(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "cc-err") || !strings.Contains(err.Error(), "duplicate session") {
		t.Errorf("error %q should mention window name and tmux output", err)
	}
}

// ---------------------------------------------------------------------------
// sendText
// ---------------------------------------------------------------------------

// TestPaneSendText_ShortUsesLiteralSendKeys proves short single-line input is
// sent via send-keys -l (TUI-prompt compatible) followed by Enter retries.
func TestPaneSendText_ShortUsesLiteralSendKeys(t *testing.T) {
	shortEnterDelays(t)
	pane, f := newFakePane("cc-w")

	if err := pane.sendText(context.Background(), "y"); err != nil {
		t.Fatalf("sendText: %v", err)
	}

	calls := f.allCalls()
	if !hasSubsequence(calls[0], []string{"send-keys", "-t", "cc-w", "-l", "y"}) {
		t.Errorf("first call %v should be literal send-keys", calls[0])
	}
	if len(f.callsFor("load-buffer")) != 0 {
		t.Error("short input must not use the paste-buffer path")
	}
	enters := 0
	for _, c := range calls {
		if hasSubsequence(c, []string{"send-keys", "-t", "cc-w", "Enter"}) {
			enters++
		}
	}
	if enters != len(enterRetryDelays) {
		t.Errorf("got %d Enter sends, want %d", enters, len(enterRetryDelays))
	}
}

// TestPaneSendText_LongUsesPasteBuffer proves input over 10 chars is loaded
// into the tmux buffer via stdin then pasted, rather than typed literally.
func TestPaneSendText_LongUsesPasteBuffer(t *testing.T) {
	shortEnterDelays(t)
	pane, f := newFakePane("cc-w")

	text := "this is a long prompt"
	if err := pane.sendText(context.Background(), text); err != nil {
		t.Fatalf("sendText: %v", err)
	}

	if len(f.callsFor("load-buffer")) != 1 {
		t.Fatal("expected exactly one load-buffer call")
	}
	if f.stdins[0] != text {
		t.Errorf("load-buffer stdin = %q, want %q", f.stdins[0], text)
	}
	paste := f.callsFor("paste-buffer")
	if len(paste) != 1 || !hasSubsequence(paste[0], []string{"paste-buffer", "-t", "cc-w"}) {
		t.Errorf("paste-buffer calls = %v", paste)
	}
}

// TestPaneSendText_MultilineUsesPasteBuffer proves even short input takes the
// paste-buffer path when it contains a newline (literal send-keys would
// submit each line separately).
func TestPaneSendText_MultilineUsesPasteBuffer(t *testing.T) {
	shortEnterDelays(t)
	pane, f := newFakePane("cc-w")

	if err := pane.sendText(context.Background(), "a\nb"); err != nil {
		t.Fatalf("sendText: %v", err)
	}
	if len(f.callsFor("load-buffer")) != 1 {
		t.Error("multi-line input must use the paste-buffer path")
	}
}

// TestPaneSendText_EmptyOnlySendsEnter proves empty text skips both input
// paths and only fires the Enter retries (empty submit is a no-op for CC).
func TestPaneSendText_EmptyOnlySendsEnter(t *testing.T) {
	shortEnterDelays(t)
	pane, f := newFakePane("cc-w")

	if err := pane.sendText(context.Background(), ""); err != nil {
		t.Fatalf("sendText: %v", err)
	}
	for _, c := range f.allCalls() {
		if !hasSubsequence(c, []string{"send-keys", "-t", "cc-w", "Enter"}) {
			t.Errorf("unexpected non-Enter call %v for empty text", c)
		}
	}
	if len(f.allCalls()) != len(enterRetryDelays) {
		t.Errorf("got %d calls, want %d Enter sends", len(f.allCalls()), len(enterRetryDelays))
	}
}

// TestPaneSendText_ErrorPaths proves each tmux failure point (literal
// send-keys, load-buffer, paste-buffer, Enter) is wrapped with a distinct
// prefix identifying the failed step.
func TestPaneSendText_ErrorPaths(t *testing.T) {
	shortEnterDelays(t)
	cases := []struct {
		name     string
		text     string
		failCmd  string // first arg of the call to fail
		wantWrap string
	}{
		{"literal send-keys fails", "y", "send-keys", "send-keys literal"},
		{"load-buffer fails", "a long prompt here", "load-buffer", "load-buffer"},
		{"paste-buffer fails", "a long prompt here", "paste-buffer", "paste-buffer"},
		{"enter fails", "", "send-keys", "send-keys Enter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane, f := newFakePane("cc-w")
			f.respond = func(args []string, _ string) (string, error) {
				if args[0] == tc.failCmd {
					return "boom", errors.New("exit status 1")
				}
				return "", nil
			}
			err := pane.sendText(context.Background(), tc.text)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantWrap) {
				t.Errorf("error %q should contain %q", err, tc.wantWrap)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Key sending, liveness, capture, env, kill
// ---------------------------------------------------------------------------

// TestPaneSendSpecialVsKeystroke proves sendSpecial omits -l so tmux
// interprets key names (C-c, Escape) while sendKeystroke includes -l so the
// character is sent literally.
func TestPaneSendSpecialVsKeystroke(t *testing.T) {
	pane, f := newFakePane("cc-w")

	if err := pane.sendSpecial(context.Background(), "C-c"); err != nil {
		t.Fatalf("sendSpecial: %v", err)
	}
	if err := pane.sendKeystroke(context.Background(), "1"); err != nil {
		t.Fatalf("sendKeystroke: %v", err)
	}

	calls := f.allCalls()
	if !hasSubsequence(calls[0], []string{"send-keys", "-t", "cc-w", "C-c"}) || hasSubsequence(calls[0], []string{"-l"}) {
		t.Errorf("sendSpecial call = %v, want interpreted key without -l", calls[0])
	}
	if !hasSubsequence(calls[1], []string{"send-keys", "-t", "cc-w", "-l", "1"}) {
		t.Errorf("sendKeystroke call = %v, want literal -l send", calls[1])
	}
}

// TestPaneIsAlive proves isAlive maps has-session success to true and
// failure to false.
func TestPaneIsAlive(t *testing.T) {
	pane, f := newFakePane("cc-w")
	if !pane.isAlive(context.Background()) {
		t.Error("isAlive should be true when has-session succeeds")
	}
	if !hasSubsequence(f.allCalls()[0], []string{"has-session", "-t", "cc-w"}) {
		t.Errorf("has-session call = %v", f.allCalls()[0])
	}

	f.respond = func([]string, string) (string, error) {
		return "", errors.New("no session")
	}
	if pane.isAlive(context.Background()) {
		t.Error("isAlive should be false when has-session fails")
	}
}

// TestPaneReadPanePID proves PID parsing from list-panes output: clean PID,
// trailing newline, multiple panes (first wins), and the empty / non-numeric /
// exec-failure error paths.
func TestPaneReadPanePID(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		err     error
		wantPID int
		wantErr string
	}{
		{name: "single pane", out: "4242\n", wantPID: 4242},
		{name: "multiple panes takes first", out: "100\n200\n", wantPID: 100},
		{name: "empty output", out: "\n", wantErr: "no pane PID"},
		{name: "non-numeric", out: "abc\n", wantErr: "parse pane PID"},
		{name: "tmux failure", out: "", err: errors.New("exit 1"), wantErr: "list-panes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane, f := newFakePane("cc-w")
			f.respond = func([]string, string) (string, error) { return tc.out, tc.err }

			pid, err := pane.readPanePID(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readPanePID: %v", err)
			}
			if pid != tc.wantPID {
				t.Errorf("pid = %d, want %d", pid, tc.wantPID)
			}
		})
	}
}

// TestPaneCapturePane proves capturePane requests printable output with 500
// lines of scrollback and returns the content, wrapping tmux failures.
func TestPaneCapturePane(t *testing.T) {
	pane, f := newFakePane("cc-w")
	f.respond = func([]string, string) (string, error) { return "pane content", nil }

	out, err := pane.capturePane(context.Background())
	if err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	if out != "pane content" {
		t.Errorf("content = %q", out)
	}
	if !hasSubsequence(f.allCalls()[0], []string{"capture-pane", "-t", "cc-w", "-p", "-S", "-500"}) {
		t.Errorf("capture-pane call = %v", f.allCalls()[0])
	}

	f.respond = func([]string, string) (string, error) { return "", errors.New("exit 1") }
	if _, err := pane.capturePane(context.Background()); err == nil || !strings.Contains(err.Error(), "capture-pane") {
		t.Errorf("err = %v, want capture-pane wrap", err)
	}
}

// TestPaneSetEnvAndKill proves setEnv issues set-environment with key/value
// on the session and kill issues kill-session on the window name.
func TestPaneSetEnvAndKill(t *testing.T) {
	pane, f := newFakePane("cc-w")

	if err := pane.setEnv(context.Background(), "FOO", "bar"); err != nil {
		t.Fatalf("setEnv: %v", err)
	}
	if err := pane.kill(context.Background()); err != nil {
		t.Fatalf("kill: %v", err)
	}

	calls := f.allCalls()
	if !hasSubsequence(calls[0], []string{"set-environment", "-t", "cc-w", "FOO", "bar"}) {
		t.Errorf("setEnv call = %v", calls[0])
	}
	if !hasSubsequence(calls[1], []string{"kill-session", "-t", "cc-w"}) {
		t.Errorf("kill call = %v", calls[1])
	}
}

// ---------------------------------------------------------------------------
// runTmuxStdin / loadBufferFromStdin
// ---------------------------------------------------------------------------

// TestRunTmuxStdin_SocketFlag proves the configured socket path is prepended
// as "-S <path>" to every invocation, and omitted when unset.
func TestRunTmuxStdin_SocketFlag(t *testing.T) {
	f := &fakeTmux{}
	withSocket := &tmuxPane{windowName: "cc-w", socketPath: "/tmp/foci.sock", exec: f.exec}
	if _, err := withSocket.runTmux(context.Background(), "has-session", "-t", "cc-w"); err != nil {
		t.Fatalf("runTmux: %v", err)
	}
	if got := f.allCalls()[0]; got[0] != "-S" || got[1] != "/tmp/foci.sock" {
		t.Errorf("call = %v, want -S /tmp/foci.sock prefix", got)
	}

	noSocket, f2 := newFakePane("cc-w")
	if _, err := noSocket.runTmux(context.Background(), "has-session"); err != nil {
		t.Fatalf("runTmux: %v", err)
	}
	if got := f2.allCalls()[0]; got[0] == "-S" {
		t.Errorf("call = %v, should not have socket flag", got)
	}
}

// TestPaneLoadBufferFromStdin proves the text is piped to "load-buffer -"
// via stdin, and that failures wrap the command's trimmed output.
func TestPaneLoadBufferFromStdin(t *testing.T) {
	pane, f := newFakePane("cc-w")
	if err := pane.loadBufferFromStdin(context.Background(), "buffer text"); err != nil {
		t.Fatalf("loadBufferFromStdin: %v", err)
	}
	if !hasSubsequence(f.allCalls()[0], []string{"load-buffer", "-"}) {
		t.Errorf("call = %v", f.allCalls()[0])
	}
	if f.stdins[0] != "buffer text" {
		t.Errorf("stdin = %q, want %q", f.stdins[0], "buffer text")
	}

	f.respond = func([]string, string) (string, error) {
		return "server exited\n", errors.New("exit 1")
	}
	err := pane.loadBufferFromStdin(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "server exited") {
		t.Errorf("err = %v, want wrapped tmux output", err)
	}
}
