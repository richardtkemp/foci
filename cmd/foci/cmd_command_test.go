package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestScanCommandLineStopsAtFirstNonFlag pins where `foci command`'s one
// scan stops: at the first token that is not one of its own flags.
// Everything from there on is the slash command's free text — the scan
// never consumes a word of it, whatever the word looks like, and the
// words are dispatched exactly as typed (only cmdCommand's boundary
// checks read the FIRST text word). Flags before the text keep their
// meaning, and the FOCI_AGENT fallback applies when no -a leads.
func TestScanCommandLineStopsAtFirstNonFlag(t *testing.T) {
	for _, kv := range []string{"FOCI_AGENT", "FOCI_IF_WARM", "FOCI_WAIT_COLD", "FOCI_HUMAN", "FOCI_SESSION", "FOCI_NO_GATE"} {
		t.Setenv(kv, "")
	}

	tests := []struct {
		name        string
		args        []string
		setEnvAgent string // when non-empty, FOCI_AGENT is set to this first
		wantAgent   string
		wantIfWarm  string
		wantWaitCol string
		wantSession string
		wantHuman   bool
		wantText    []string
	}{
		{
			name:      "flag look-alikes inside the text stay text",
			args:      []string{"-a", "clutch", "/todo", "done", "7", "landed", "--addr", "1.2.3.4:5", "--api-key=k"},
			wantAgent: "clutch",
			wantText:  []string{"/todo", "done", "7", "landed", "--addr", "1.2.3.4:5", "--api-key=k"},
		},
		{
			name:      "gate look-alike inside the text stays text",
			args:      []string{"-a", "clutch", "/todo", "done", "7", "landed", "--if-active", "8h"},
			wantAgent: "clutch",
			wantText:  []string{"/todo", "done", "7", "landed", "--if-active", "8h"},
		},
		{
			name:      "agent look-alike inside the text stays text",
			args:      []string{"/todo", "done", "7", "landed", "-a", "x"},
			wantAgent: "",
			wantText:  []string{"/todo", "done", "7", "landed", "-a", "x"},
		},
		{
			name:        "flags before the text keep their meaning",
			args:        []string{"--if-active", "8h", "--wait-cold", "55m", "--human", "-s", "work", "-a", "helen", "/reset"},
			wantAgent:   "helen",
			wantIfWarm:  "8h",
			wantWaitCol: "55m",
			wantSession: "work",
			wantHuman:   true,
			wantText:    []string{"/reset"},
		},
		{
			name:        "a valueless trailing flag is text, not a flag",
			args:        []string{"/reset", "--wait-cold"},
			wantText:    []string{"/reset", "--wait-cold"},
			wantWaitCol: "",
		},
		{
			name:        "FOCI_AGENT fallback with no -a on the line",
			args:        []string{"/ping"},
			setEnvAgent: "envagent",
			wantAgent:   "envagent",
			wantText:    []string{"/ping"},
		},
		{
			name:     "no flags at all",
			args:     []string{"ping"},
			wantText: []string{"ping"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnvAgent != "" {
				t.Setenv("FOCI_AGENT", tt.setEnvAgent)
			}
			var flags commandFlags
			lead, err := scanCommandLine(tt.args, &flags, commandLead, true)
			if err != nil {
				t.Fatalf("scanCommandLine(%v, true): %v", tt.args, err)
			}
			flags.applyEnvDefaults()
			if lead.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", lead.agent, tt.wantAgent)
			}
			if flags.ifWarm != tt.wantIfWarm {
				t.Errorf("ifWarm = %q, want %q", flags.ifWarm, tt.wantIfWarm)
			}
			if flags.waitCold != tt.wantWaitCol {
				t.Errorf("waitCold = %q, want %q", flags.waitCold, tt.wantWaitCol)
			}
			if flags.sessionFlag.session != tt.wantSession {
				t.Errorf("session = %q, want %q", flags.sessionFlag.session, tt.wantSession)
			}
			if flags.humanFlag.human != tt.wantHuman {
				t.Errorf("human = %v, want %v", flags.humanFlag.human, tt.wantHuman)
			}
			if !slices.Equal(lead.rest, tt.wantText) {
				t.Fatalf("text = %v, want %v", lead.rest, tt.wantText)
			}
		})
	}
}

// TestScanCommandLineValuelessFlagErrors pins the valueless-flag rule of
// the one scan: in the FREE-TEXT disposition a detached value-flag
// spelling that ends the line (-s, --if-warm, a dangling -a) is where
// the text would start, and dispatching it as the text (the old scan
// sent "/-s", review round 22 F3) types a command nobody meant — it is
// an error naming the flag instead. The controls prove the rule's
// bounds: the same flags WITH their value still parse, the same words
// after the text has started are text (the operator ruling), and on the
// fixed-word disposition (ping/pair, no free text to guard) the word
// stays a word — the base behaviour.
func TestScanCommandLineValuelessFlagErrors(t *testing.T) {
	for _, kv := range []string{"FOCI_AGENT", "FOCI_IF_WARM", "FOCI_WAIT_COLD", "FOCI_HUMAN", "FOCI_SESSION", "FOCI_NO_GATE"} {
		t.Setenv(kv, "")
	}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "dangling -a alone", args: []string{"-a"}, want: "flag -a needs a value"},
		{name: "dangling -s after the agent flag", args: []string{"-a", "clutch", "-s"}, want: "flag -s needs a value"},
		{name: "dangling gate after the agent flag", args: []string{"-a", "clutch", "--if-warm"}, want: "flag --if-warm needs a value"},
		{name: "dangling gate alias", args: []string{"--if-active"}, want: "flag --if-active needs a value"},
		{name: "dangling wait flag", args: []string{"--wait-cold"}, want: "flag --wait-cold needs a value"},
		{name: "dangling --deadline alias", args: []string{"--deadline"}, want: "flag --deadline needs a value"},
		{name: "dangling second -a", args: []string{"-a", "helen", "-a"}, want: "flag -a needs a value"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := scanCommandLine(tt.args, &commandFlags{}, commandLead, true)
			if err == nil || err.Error() != tt.want {
				t.Errorf("scanCommandLine(%v, true) err = %v, want %q", tt.args, err, tt.want)
			}
		})
	}

	// A value-flag with its value still parses everywhere.
	var flags commandFlags
	lead, err := scanCommandLine([]string{"-a", "clutch", "-s", "work", "--if-warm", "8h", "/ping"}, &flags, commandLead, true)
	if err != nil {
		t.Fatalf("valued flags must parse: %v", err)
	}
	if lead.agent != "clutch" || flags.sessionFlag.session != "work" || flags.ifWarm != "8h" || !slices.Equal(lead.rest, []string{"/ping"}) {
		t.Errorf("scan = {%q, %q, %q, %v}, want {clutch, work, 8h, [/ping]}", lead.agent, flags.sessionFlag.session, flags.ifWarm, lead.rest)
	}

	// The same words INSIDE the text are text, valueless or not (the
	// ruling): the text starts at /reset, so the trailing -s is a word.
	var flags2 commandFlags
	lead, err = scanCommandLine([]string{"/reset", "-s"}, &flags2, commandLead, true)
	if err != nil {
		t.Fatalf("a valueless word inside the text must not error: %v", err)
	}
	if !slices.Equal(lead.rest, []string{"/reset", "-s"}) {
		t.Errorf("text = %v, want [/reset -s]", lead.rest)
	}

	// The same words on the fixed-word disposition stay words — no free
	// text starts there, so there is nothing for a dangling value-flag
	// to guard; the word is dispatched as typed (base behaviour).
	for _, tt := range []struct {
		name     string
		args     []string
		wantRest []string
	}{
		{name: "dangling -a alone", args: []string{"-a"}, wantRest: []string{"-a"}},
		{name: "dangling -s after the agent flag", args: []string{"-a", "clutch", "-s"}, wantRest: []string{"-s"}},
		{name: "dangling wait flag", args: []string{"--wait-cold"}, wantRest: []string{"--wait-cold"}},
	} {
		t.Run(tt.name+" (whole line)", func(t *testing.T) {
			lead, err := scanCommandLine(tt.args, &commandFlags{}, commandLead, false)
			if err != nil {
				t.Fatalf("scanCommandLine(%v, false): %v — the fixed-word disposition keeps the word", tt.args, err)
			}
			if !slices.Equal(lead.rest, tt.wantRest) {
				t.Errorf("rest = %v, want %v", lead.rest, tt.wantRest)
			}
		})
	}
}

// TestScanCommandLineDuplicateAgentFlag pins the second-agent-flag rule of
// the free-text disposition: a COMPLETE -a/--agent pair typed after the
// scan already holds an agent sits before the text, where the line reads
// flags — so it is refused naming the rule, never dispatched as text
// (the historical fall-through dispatched "/-a b /ping" and exited 0).
// The bounds: the FIRST pair still selects, a dangling second -a is the
// valueless-flag error (its value is missing — that diagnosis is true),
// and the fixed-word disposition (ping/pair, no free text) keeps the
// pair as ordinary words.
func TestScanCommandLineDuplicateAgentFlag(t *testing.T) {
	for _, kv := range []string{"FOCI_AGENT", "FOCI_IF_WARM", "FOCI_WAIT_COLD", "FOCI_HUMAN", "FOCI_SESSION", "FOCI_NO_GATE"} {
		t.Setenv(kv, "")
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "second detached pair", args: []string{"-a", "a", "-a", "b", "/ping"}},
		{name: "second attached spelling", args: []string{"-a", "a", "--agent=b", "/ping"}},
		{name: "first attached, second detached", args: []string{"--agent=a", "-a", "b", "/ping"}},
		{name: "second pair after other flags", args: []string{"--human", "-a", "a", "-a", "b", "/ping"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lead, err := scanCommandLine(tt.args, &commandFlags{}, commandLead, true)
			if err == nil || err.Error() != "only one -a/--agent may precede the text" {
				t.Errorf("scanCommandLine(%v, true) = (rest %v, err %v), want the duplicate-agent error", tt.args, lead.rest, err)
			}
		})
	}

	// The first pair still selects the agent, wherever it leads.
	lead, err := scanCommandLine([]string{"--human", "-a", "helen", "/ping"}, &commandFlags{}, commandLead, true)
	if err != nil || lead.agent != "helen" || !slices.Equal(lead.rest, []string{"/ping"}) {
		t.Errorf("scan = {%q, %v, %v}, want {helen, [/ping], nil error}", lead.agent, lead.rest, err)
	}

	// A dangling second -a has no value: the valueless-flag error names
	// that, not the duplicate (the pair is not complete).
	_, err = scanCommandLine([]string{"-a", "helen", "-a"}, &commandFlags{}, commandLead, true)
	if err == nil || err.Error() != "flag -a needs a value" {
		t.Errorf("dangling second -a err = %v, want %q", err, "flag -a needs a value")
	}

	// The fixed-word disposition has no free text: the second pair stays
	// ordinary words, as it always was.
	lead, err = scanCommandLine([]string{"-a", "a", "x", "-a", "b"}, &commandFlags{}, commandLead, false)
	if err != nil || lead.agent != "a" || !slices.Equal(lead.rest, []string{"x", "-a", "b"}) {
		t.Errorf("whole-line scan = {%q, %v, %v}, want {a, [x -a b], nil error}", lead.agent, lead.rest, err)
	}
}

// TestScanCommandLineWholeLine pins the ping/pair disposition of the one
// scan: their lines carry no free text (a host argument, a fixed slash
// word), so the flags are consumed wherever they sit and every other
// word is returned — the historical whole-line scan. The agent flag is
// consumed on its first appearance wherever it sits; a later -a is an
// ordinary word, and so is a valueless value-flag that ends the line
// (base behaviour; the needs-a-value error guards the free-text
// disposition only — TestScanCommandLineValuelessFlagErrors).
func TestScanCommandLineWholeLine(t *testing.T) {
	t.Setenv("FOCI_AGENT", "")
	t.Setenv("FOCI_NO_GATE", "")
	for _, tt := range []struct {
		name      string
		args      []string
		wantAgent string
		wantNoGat bool
		wantRest  []string
	}{
		{name: "gate before agent", args: []string{"--no-gate", "--if-warm", "8h", "-a", "helen"}, wantAgent: "helen", wantNoGat: true},
		{name: "agent before gate", args: []string{"-a", "helen", "--no-gate"}, wantAgent: "helen", wantNoGat: true},
		{name: "stray word between flags", args: []string{"x", "--no-gate", "-a", "helen"}, wantAgent: "helen", wantNoGat: true, wantRest: []string{"x"}},
		{name: "second -a stays a word", args: []string{"-a", "a", "x", "-a", "b"}, wantAgent: "a", wantRest: []string{"x", "-a", "b"}},
		{name: "empty line", args: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var flags commandFlags
			lead, err := scanCommandLine(tt.args, &flags, commandLead, false)
			if err != nil {
				t.Fatalf("scanCommandLine(%v, false): %v", tt.args, err)
			}
			flags.applyEnvDefaults()
			if lead.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", lead.agent, tt.wantAgent)
			}
			if flags.noGate != tt.wantNoGat {
				t.Errorf("noGate = %v, want %v", flags.noGate, tt.wantNoGat)
			}
			if !slices.Equal(lead.rest, tt.wantRest) {
				t.Fatalf("rest = %v, want %v", lead.rest, tt.wantRest)
			}
		})
	}
}

// TestCommandTextIsVerbatimAfterItStarts proves the operator ruling at
// the body level: once the text has started, every word is dispatched
// typed exactly, whatever it looks like — a trailing gate pair, a
// trailing --human, a trailing agent pair riding into a /todo close
// reason — while the same flags BEFORE the text still reach the body as
// flags.
func TestCommandTextIsVerbatimAfterItStarts(t *testing.T) {
	for _, kv := range []string{"FOCI_AGENT", "FOCI_IF_WARM", "FOCI_WAIT_COLD", "FOCI_HUMAN", "FOCI_SESSION", "FOCI_NO_GATE"} {
		t.Setenv(kv, "")
	}
	for _, tt := range []struct {
		name       string
		args       []string
		wantCmd    string
		wantAgent  string
		wantAbsent []string // body keys that must NOT be there (the words were text, not flags)
	}{
		{
			name:       "trailing gate pair is command text",
			args:       []string{"-a", "helen", "/reset", "--if-warm", "8h"},
			wantCmd:    "/reset --if-warm 8h",
			wantAgent:  "helen",
			wantAbsent: []string{"if_active"},
		},
		{
			name:       "trailing attached gate spelling is command text",
			args:       []string{"/cache", "--if-warm=8h"},
			wantCmd:    "/cache --if-warm=8h",
			wantAbsent: []string{"if_active"},
		},
		{
			name:       "trailing --human is command text",
			args:       []string{"/cache", "--human"},
			wantCmd:    "/cache --human",
			wantAbsent: []string{"human"},
		},
		{
			name:       "trailing --no-gate is command text",
			args:       []string{"--wait-cold", "55m", "-a", "helen", "/reset", "--no-gate"},
			wantCmd:    "/reset --no-gate",
			wantAgent:  "helen",
			wantAbsent: []string{"wait_none"},
		},
		{
			name:       "agent pair inside a todo close reason is reason",
			args:       []string{"/todo", "done", "10", "-a", "b"},
			wantCmd:    "/todo done 10 -a b",
			wantAbsent: []string{"agent"},
		},
		{
			name:      "connection flag pair after the text is command text",
			args:      []string{"-a", "clutch", "/ping", "--addr", "h:p"},
			wantCmd:   "/ping --addr h:p",
			wantAgent: "clutch",
		},
		{
			name:      "credential-shaped word inside the text is text",
			args:      []string{"-a", "clutch", "/todo", "new", "buy", "milk", "--api-key", "sekret"},
			wantCmd:   "/todo new buy milk --api-key sekret",
			wantAgent: "clutch",
		},
		{
			name:       "valueless trailing gate word is command text",
			args:       []string{"/reset", "--wait-cold"},
			wantCmd:    "/reset --wait-cold",
			wantAbsent: []string{"wait_cold"},
		},
		{
			name:      "help word inside the text is text (F2: reason landed -h)",
			args:      []string{"-a", "clutch", "/todo", "done", "7", "landed", "-h"},
			wantCmd:   "/todo done 7 landed -h",
			wantAgent: "clutch",
		},
		{
			name:       "one quoted argument keeps inner spacing and flag-shaped words",
			args:       []string{"-a", "clutch", "/todo done 7 landed --if-active 8h"},
			wantCmd:    "/todo done 7 landed --if-active 8h",
			wantAgent:  "clutch",
			wantAbsent: []string{"if_active"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := captureBody(t, func(base string) error {
				return cmdCommand(base, tt.args)
			})
			if body["command"] != tt.wantCmd {
				t.Errorf("command = %v, want %q", body["command"], tt.wantCmd)
			}
			// An absent agent key and an empty one both mean "no agent
			// was selected" — the text words selected nothing.
			gotAgent, _ := body["agent"].(string)
			if gotAgent != tt.wantAgent {
				t.Errorf("agent = %v, want %q", body["agent"], tt.wantAgent)
			}
			for _, key := range tt.wantAbsent {
				if _, ok := body[key]; ok {
					t.Errorf("body carries %q = %v — words inside the text must not be parsed as flags", key, body[key])
				}
			}
		})
	}

	// The other side: the same flags BEFORE the text still reach the body.
	body := captureBody(t, func(base string) error {
		return cmdCommand(base, []string{"--wait-cold", "55m", "-a", "helen", "/reset"})
	})
	if body["command"] != "/reset" || body["agent"] != "helen" || body["wait_cold"] != "55m" {
		t.Errorf("body = %v, want {/reset, helen, wait_cold 55m} — leading flags must keep their meaning", body)
	}
}

// TestCommandBoundaryRules pins the two words still read at the text
// boundary (the FIRST text word only): a connection flag there is the one
// placement error — it sits before the slash command, where the placement
// rule reads it as a flag, but it cannot route the request from there —
// and -h/--help there asks for the usage. Both precede any dispatch; the
// error names the flag only, never an attached value.
func TestCommandBoundaryRules(t *testing.T) {
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()

	for _, tt := range []struct {
		name    string
		args    []string
		want    string
		wantNot string // an attached value the error must never echo
	}{
		{name: "--addr pair leading the text", args: []string{"--addr", "h:p", "/ping"}, want: "--addr"},
		{name: "attached --api-key leading the text", args: []string{"--api-key=k", "/ping"}, want: "--api-key", wantNot: "=k"},
		{name: "bare trailing --socket leads the text", args: []string{"--socket"}, want: "--socket"},
		{name: "after command's own flags", args: []string{"-a", "clutch", "--addr", "h:p", "/ping"}, want: "--addr"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			err := cmdCommand(srv.URL, tt.args)
			if err == nil {
				t.Fatalf("%v dispatched, want the placement error", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "must come before the command word") {
				t.Errorf("error = %q, want it to name %q and the placement rule", err.Error(), tt.want)
			}
			if tt.wantNot != "" && strings.Contains(err.Error(), tt.wantNot) {
				t.Errorf("error = %q echoes the attached value %q — the value may be a credential", err.Error(), tt.wantNot)
			}
			if n := len(gw.requests()) - before; n != 0 {
				t.Errorf("%v: %d request(s) reached the gateway, want none — the error must precede any send", tt.args, n)
			}
		})
	}

	// The valueless-flag rule (round 22 F3): a value-flag that ends the
	// line, before the text starts, has no value — an error naming the
	// flag, never a dispatch of the word as the command text.
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "dangling -s after the agent flag", args: []string{"-a", "clutch", "-s"}, want: "flag -s needs a value"},
		{name: "dangling gate after the agent flag", args: []string{"-a", "clutch", "--if-warm"}, want: "flag --if-warm needs a value"},
		{name: "dangling -a alone", args: []string{"-a"}, want: "flag -a needs a value"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			err := cmdCommand(srv.URL, tt.args)
			if err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if n := len(gw.requests()) - before; n != 0 {
				t.Errorf("%v: %d request(s) reached the gateway, want none", tt.args, n)
			}
		})
	}

	// The duplicate-agent rule (round 24 F2): a second complete -a pair
	// before the text is refused naming the rule — never dispatched as
	// the text, which the old fall-through did with exit 0.
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "second detached pair", args: []string{"-a", "clutch", "-a", "other", "/ping"}},
		{name: "second attached spelling", args: []string{"-a", "clutch", "--agent=other", "/ping"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			err := cmdCommand(srv.URL, tt.args)
			if err == nil || err.Error() != "only one -a/--agent may precede the text" {
				t.Errorf("error = %v, want the duplicate-agent error", err)
			}
			if n := len(gw.requests()) - before; n != 0 {
				t.Errorf("%v: %d request(s) reached the gateway, want none", tt.args, n)
			}
		})
	}

	// Help as the FIRST text word (also how a bare `command -h` reaches
	// it: -h is not one of command's flags, so the text starts there).
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "help alone", args: []string{"-h"}},
		{name: "help after the agent flag", args: []string{"-a", "clutch", "--help"}},
		{name: "help after a gate pair", args: []string{"--wait-cold", "55m", "-h"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			stderr := captureStderr(t, func() {
				if err := cmdCommand(srv.URL, tt.args); err != nil {
					t.Errorf("%v returned error %v, want usage", tt.args, err)
				}
			})
			if !strings.Contains(stderr, "Usage: foci command") {
				t.Errorf("%v: usage not printed, stderr = %q", tt.args, stderr)
			}
			if n := len(gw.requests()) - before; n != 0 {
				t.Errorf("%v: %d request(s) reached the gateway, want none", tt.args, n)
			}
		})
	}
}

// TestCLICommand_FreeText drives the real CLI binary, as a script would,
// against a gateway on $HOME/data/foci-gw.sock: from the text onward
// every word is dispatched typed exactly, whatever it looks like — the
// operator ruling's two named consequences included (a trailing gate pair
// is command text; an agent pair inside a /todo close reason is reason,
// closing the DEFAULT agent's todo, and a help word inside the text is
// text) — while the connection flags still route only from before the
// "command" word, and the pair shorthand keeps reading its flags after
// the host.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestCLICommand_FreeText(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "data"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sockPath := filepath.Join(home, "data", "foci-gw.sock")

	gw := &recordingGateway{}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(gw.handle)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(testBinary, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	// Flag-shaped words anywhere in the text dispatch verbatim (exit 0,
	// one request, the words in the body, no flag keys on the body).
	for _, tt := range []struct {
		name       string
		args       []string
		wantCmd    string
		wantAgent  string
		wantAbsent []string
	}{
		{
			name:       "trailing gate pair is text (ruling consequence a)",
			args:       []string{"command", "-a", "helen", "/reset", "--if-warm", "8h"},
			wantCmd:    "/reset --if-warm 8h",
			wantAgent:  "helen",
			wantAbsent: []string{"if_active"},
		},
		{
			name:       "agent pair in a close reason is reason (ruling consequence b)",
			args:       []string{"command", "/todo", "done", "10", "-a", "b"},
			wantCmd:    "/todo done 10 -a b",
			wantAbsent: []string{"agent"},
		},
		{
			name:      "help word in a close reason is reason (review F2)",
			args:      []string{"command", "-a", "clutch", "/todo", "done", "7", "landed", "-h"},
			wantCmd:   "/todo done 7 landed -h",
			wantAgent: "clutch",
		},
		{
			name:      "connection flag pair after the text is text",
			args:      []string{"command", "-a", "clutch", "/ping", "--addr", "h:p"},
			wantCmd:   "/ping --addr h:p",
			wantAgent: "clutch",
		},
		{
			name:      "credential-shaped words in a close reason are text",
			args:      []string{"command", "-a", "clutch", "/todo", "done", "7", "landed", "--api-key", "sekret"},
			wantCmd:   "/todo done 7 landed --api-key sekret",
			wantAgent: "clutch",
		},
	} {
		before := len(gw.requests())
		stdout, stderr, err := run(tt.args...)
		if err != nil {
			t.Fatalf("%v must dispatch; stderr: %q stdout: %q", tt.args, stderr, stdout)
		}
		reqs := gw.requests()[before:]
		if len(reqs) != 1 || reqs[0].Path != "/command" {
			t.Fatalf("%v: requests = %+v, want one POST /command", tt.args, reqs)
		}
		cmd, agent := commandRequestBody(t, reqs[0].Body)
		if cmd != tt.wantCmd || agent != tt.wantAgent {
			t.Errorf("%v: body = {command %q, agent %q}, want {%q, %q}", tt.args, cmd, agent, tt.wantCmd, tt.wantAgent)
		}
		var body map[string]interface{}
		if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		for _, key := range tt.wantAbsent {
			if _, ok := body[key]; ok {
				t.Errorf("%v: body carries %q = %v — the words were text, not flags", tt.args, key, body[key])
			}
		}
		if stdout == "" {
			t.Errorf("%v: stdout = %q, want the gateway's response printed", tt.args, stdout)
		}
	}

	// A connection flag BEFORE the slash command is still the one error:
	// it cannot route from there, so the line says where it goes (and
	// never echoes an attached value).
	before := len(gw.requests())
	stdout, stderr, err := run("command", "-a", "clutch", "--addr", "h:p", "/ping")
	if err == nil {
		t.Fatalf("misplaced --addr must exit non-zero; stdout: %q stderr: %q", stdout, stderr)
	}
	if line := strings.TrimSpace(stderr); !strings.Contains(line, "--addr must come before the command word") {
		t.Errorf("stderr = %q, want the placement rule naming --addr", stderr)
	}
	if strings.Contains(stderr, "h:p") {
		t.Errorf("stderr = %q echoes the value", stderr)
	}
	if n := len(gw.requests()) - before; n != 0 {
		t.Errorf("%d request(s) reached the gateway, want none", n)
	}

	// A value-flag that ends the line is the same kind of verdict through
	// the real binary: one stderr line naming the flag, nothing dispatched
	// (the old scan dispatched "/-s" and exited 0).
	before = len(gw.requests())
	stdout, stderr, err = run("command", "-a", "clutch", "-s")
	if err == nil {
		t.Fatalf("dangling -s must exit non-zero; stdout: %q stderr: %q", stdout, stderr)
	}
	if line := strings.TrimSpace(stderr); !strings.Contains(line, "flag -s needs a value") || strings.Contains(line, "\n") {
		t.Errorf("stderr = %q, want exactly one line naming the flag", stderr)
	}
	if n := len(gw.requests()) - before; n != 0 {
		t.Errorf("%d request(s) reached the gateway, want none", n)
	}

	// A second complete agent pair before the text is the same verdict
	// through the real binary (round 24 F2): the old scan dispatched
	// "/-a other /ping" and exited 0.
	before = len(gw.requests())
	stdout, stderr, err = run("command", "-a", "helen", "-a", "other", "/ping")
	if err == nil {
		t.Fatalf("second -a pair must exit non-zero; stdout: %q stderr: %q", stdout, stderr)
	}
	if line := strings.TrimSpace(stderr); !strings.Contains(line, "only one -a/--agent may precede the text") || strings.Contains(line, "\n") {
		t.Errorf("stderr = %q, want exactly one line naming the duplicate-agent rule", stderr)
	}
	if n := len(gw.requests()) - before; n != 0 {
		t.Errorf("%d request(s) reached the gateway, want none", n)
	}

	// The same words inside ONE quoted argument keep exact spacing too.
	before = len(gw.requests())
	stdout, stderr, err = run("command", "-a", "clutch", "/todo done 7 landed --if-active 8h")
	if err != nil {
		t.Fatalf("quoted dispatch: %v\nstderr: %s", err, stderr)
	}
	reqs := gw.requests()[before:]
	if len(reqs) != 1 || reqs[0].Path != "/command" {
		t.Fatalf("requests = %+v, want one POST /command", reqs)
	}
	if cmd, agent := commandRequestBody(t, reqs[0].Body); cmd != "/todo done 7 landed --if-active 8h" || agent != "clutch" {
		t.Errorf("body = {command %q, agent %q}, want the quoted text typed exactly for clutch", cmd, agent)
	}
	if stdout == "" {
		t.Errorf("stdout = %q, want the gateway's response printed", stdout)
	}

	// The pair shorthand has no free text: its flags scan the whole line,
	// so a trailing -a after the host still selects the agent.
	before = len(gw.requests())
	stdout, stderr, err = run("pair", "example.com", "-a", "helen")
	if err != nil {
		t.Fatalf("pair dispatch: %v\nstderr: %s", err, stderr)
	}
	reqs = gw.requests()[before:]
	if len(reqs) != 1 || reqs[0].Path != "/command" {
		t.Fatalf("requests = %+v, want one POST /command", reqs)
	}
	if cmd, agent := commandRequestBody(t, reqs[0].Body); cmd != "/pair example.com" || agent != "helen" {
		t.Errorf("body = {command %q, agent %q}, want {/pair example.com, helen} — pair keeps its whole-line scan", cmd, agent)
	}

	// The ping shorthand is the same flags-only shape: a trailing -a
	// after a stray word still selects the agent, and the stray word
	// dispatches as /ping's text, as it always did.
	before = len(gw.requests())
	stdout, stderr, err = run("ping", "something", "-a", "helen")
	if err != nil {
		t.Fatalf("ping dispatch: %v\nstderr: %s", err, stderr)
	}
	reqs = gw.requests()[before:]
	if len(reqs) != 1 || reqs[0].Path != "/command" {
		t.Fatalf("requests = %+v, want one POST /command", reqs)
	}
	if cmd, agent := commandRequestBody(t, reqs[0].Body); cmd != "/something /ping" || agent != "helen" {
		t.Errorf("body = {command %q, agent %q}, want {/something /ping, helen} — ping keeps its whole-line scan", cmd, agent)
	}
}

// commandRequestBody decodes a recorded /command body into its wire fields.
func commandRequestBody(t *testing.T, raw string) (command, agent string) {
	t.Helper()
	var body struct {
		Command string `json:"command"`
		Agent   string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode /command body %q: %v", raw, err)
	}
	return body.Command, body.Agent
}

// TestCommandAliasesScanWholeLine pins the pair and ping shorthands:
// their lines carry no free text (a host argument, a fixed slash word),
// so they keep the historical whole-line flag scan — a flag word is a
// flag wherever the typist put it, including after the host or, for
// ping, after a stray word, as it always was. The /pair word lands
// before the host, /ping after the flags; a stray word before the flags
// dispatches as /ping's text (the base behaviour a flags-only shorthand
// has no usage error for), which is what keeps these rows from being
// served by `foci command`'s bounded scan: there a word after the text
// has started is text, dispatched verbatim.
func TestCommandAliasesScanWholeLine(t *testing.T) {
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()

	for _, tt := range []struct {
		name      string
		argv      []string
		wantCmd   string
		wantAgent string
	}{
		{name: "pair with -a and host", argv: []string{"pair", "-a", "helen", "example.com"}, wantCmd: "/pair example.com", wantAgent: "helen"},
		{name: "pair with host only", argv: []string{"pair", "example.com"}, wantCmd: "/pair example.com"},
		{name: "pair with -a AFTER the host", argv: []string{"pair", "example.com", "-a", "helen"}, wantCmd: "/pair example.com", wantAgent: "helen"},
		{name: "pair with a gate flag after the host", argv: []string{"pair", "example.com", "--no-gate"}, wantCmd: "/pair example.com"},
		{name: "pair-key alias", argv: []string{"pair-key", "-a", "helen"}, wantCmd: "/pair", wantAgent: "helen"},
		{name: "ping with -a", argv: []string{"ping", "-a", "helen"}, wantCmd: "/ping", wantAgent: "helen"},
		{name: "ping with -a after its gate flags", argv: []string{"ping", "--no-gate", "--if-warm", "8h", "-a", "helen"}, wantCmd: "/ping", wantAgent: "helen"},
		{name: "ping with -a after a stray word", argv: []string{"ping", "something", "-a", "helen"}, wantCmd: "/something /ping", wantAgent: "helen"},
		{name: "ping with a gate flag after a stray word", argv: []string{"ping", "something", "--no-gate"}, wantCmd: "/something /ping"},
		{name: "ping bare", argv: []string{"ping"}, wantCmd: "/ping"},
		// A valueless value-flag that ends the line stays a word here —
		// the shorthands have no free text for the needs-a-value error
		// to guard, so the word dispatches as typed (base behaviour).
		{name: "pair with a dangling -a", argv: []string{"pair", "-a"}, wantCmd: "/pair -a"},
		{name: "ping with a dangling -s", argv: []string{"ping", "-s"}, wantCmd: "/-s /ping"},
		{name: "ping with a dangling gate after a stray word", argv: []string{"ping", "something", "--wait-cold"}, wantCmd: "/something --wait-cold /ping"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			err := runGatewayCommand(srv.URL, tt.argv)
			if err != nil {
				t.Fatalf("runGatewayCommand(%v): %v", tt.argv, err)
			}
			reqs := gw.requests()[before:]
			if len(reqs) != 1 || reqs[0].Path != "/command" {
				t.Fatalf("requests = %+v, want one POST /command", reqs)
			}
			cmd, agent := commandRequestBody(t, reqs[0].Body)
			if cmd != tt.wantCmd {
				t.Errorf("command = %q, want %q", cmd, tt.wantCmd)
			}
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tt.wantAgent)
			}
		})
	}
}

// TestRunGatewayCommandUsageErrorsReturn proves the dispatch's usage
// failures come back as errors instead of ending the process: a
// regression driven through runGatewayCommand fails the test with its
// own assertion line, it does not kill the test binary. The no-command
// path prints the usage and returns an error naming the cause; the
// unknown-command path prints its verdict line FIRST, then the usage
// (F4: the usage block must not bury what was wrong), and the error it
// returns is the errAlreadyReported marker, which main turns into the
// exit code without reprinting the line.
func TestRunGatewayCommandUsageErrorsReturn(t *testing.T) {
	base := "http://127.0.0.1:0" // unreachable — proves no HTTP call is made

	stderr := captureStderr(t, func() {
		if err := runGatewayCommand(base, nil); err == nil || !strings.Contains(err.Error(), "command is required") {
			t.Errorf("no command: err = %v, want an error naming the missing command", err)
		}
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("no command: usage not printed, stderr = %q", stderr)
	}

	stderr = captureStderr(t, func() {
		if err := runGatewayCommand(base, []string{"bogus"}); !errors.Is(err, errAlreadyReported) {
			t.Errorf("unknown command: err = %v, want errAlreadyReported (main keeps only the exit code)", err)
		}
	})
	verdict := strings.Index(stderr, "unknown command: bogus")
	usageAt := strings.Index(stderr, "Usage:")
	if verdict < 0 || usageAt < 0 || verdict > usageAt {
		t.Errorf("unknown command: stderr = %q, want the verdict line BEFORE the usage block", stderr)
	}
}
