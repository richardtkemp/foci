package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestParseTodoDoneArgs pins `foci todo done`'s command-line shape: flags only
// before the id (every -a/--agent form plus the FOCI_AGENT fallback), the id
// a positive integer, and the reason the remaining argv verbatim (joined with
// single spaces) — including words that look like flags: the one placement
// line (Dick, 2026-10-17) keeps every word from the reason onward in the
// reason, never read as a flag, never refused.
func TestParseTodoDoneArgs(t *testing.T) {
	t.Setenv("FOCI_AGENT", "") // pin the env fallback empty unless a row sets it

	tests := []struct {
		name      string
		args      []string
		want      todoDoneArgs
		wantErr   string
		setEnvVal string // when non-empty, t.Setenv FOCI_AGENT to this first
	}{
		{
			name: "flag and words",
			args: []string{"-a", "clutch", "5", "reason", "words"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "reason words"},
		},
		{
			name: "single quoted reason element is verbatim",
			args: []string{"-a", "clutch", "7", "landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)"},
			want: todoDoneArgs{agent: "clutch", id: 7, reason: "landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)"},
		},
		{
			// Whitespace INSIDE one argv element is reason byte for byte:
			// the join only inserts the single spaces BETWEEN elements,
			// it must never touch the bytes within one.
			name: "whitespace inside one argv element is kept byte for byte",
			args: []string{"-a", "clutch", "7", "a  b\tc\nd"},
			want: todoDoneArgs{agent: "clutch", id: 7, reason: "a  b\tc\nd"},
		},
		{
			name: "quotes dollars backticks",
			args: []string{"-a", "clutch", "7", "landed abc (run \"x\" $(id) `y`)"},
			want: todoDoneArgs{agent: "clutch", id: 7, reason: "landed abc (run \"x\" $(id) `y`)"},
		},
		{
			name: "--agent=value",
			args: []string{"--agent=clutch", "5", "r"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "r"},
		},
		{
			name: "-a=value",
			args: []string{"-a=clutch", "5", "r"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "r"},
		},
		{
			name: "-- ends the flags and is dropped",
			args: []string{"--", "5", "r"},
			want: todoDoneArgs{id: 5, reason: "r"},
		},
		{
			name:      "FOCI_AGENT fallback",
			args:      []string{"5", "r"},
			want:      todoDoneArgs{agent: "envagent", id: 5, reason: "r"},
			setEnvVal: "envagent",
		},
		{
			name: "reason starting with a dash",
			args: []string{"-a", "clutch", "5", "-x", "a"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "-x a"},
		},
		{
			name: "reason word that is a negative number",
			args: []string{"-a", "clutch", "5", "-3", "penalty"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "-3 penalty"},
		},
		{
			name: "reason word of a lone dash",
			args: []string{"-a", "clutch", "5", "-", "n"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "- n"},
		},
		{
			name: "help in the leading flags",
			args: []string{"-a", "clutch", "--help"},
			want: todoDoneArgs{agent: "clutch", help: true},
		},
		{
			name: "help alone",
			args: []string{"-h"},
			want: todoDoneArgs{help: true},
		},
		{
			name: "reason of --help",
			args: []string{"-a", "clutch", "5", "--help"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "--help"},
		},
		{
			name: "--help after first reason word",
			args: []string{"-a", "clutch", "5", "r", "--help"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "r --help"},
		},
		// The ruling: flag-shaped words in the reason are reason, byte for
		// byte — a connection flag's value included (the operator ruled
		// credential-shaped words are text, 2026-10-17), and an agent-flag
		// spelling after the id is reason, not a selector.
		{
			name: "reason containing an --addr pair",
			args: []string{"-a", "clutch", "7", "landed", "--addr", "h:p"},
			want: todoDoneArgs{agent: "clutch", id: 7, reason: "landed --addr h:p"},
		},
		{
			name: "reason containing an attached --api-key",
			args: []string{"-a", "clutch", "7", "r", "--api-key=k"},
			want: todoDoneArgs{agent: "clutch", id: 7, reason: "r --api-key=k"},
		},
		{
			name: "reason containing --socket",
			args: []string{"-a", "clutch", "7", "--socket"},
			want: todoDoneArgs{agent: "clutch", id: 7, reason: "--socket"},
		},
		{
			name: "reason containing -a",
			args: []string{"-a", "clutch", "5", "-a", "other"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "-a other"},
		},
		{
			name: "reason containing --agent",
			args: []string{"-a", "clutch", "5", "--agent", "evil"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "--agent evil"},
		},
		{
			name: "reason containing an attached agent spelling",
			args: []string{"-a", "clutch", "5", "--agent=evil"},
			want: todoDoneArgs{agent: "clutch", id: 5, reason: "--agent=evil"},
		},
		{name: "no arguments", args: nil, wantErr: "usage"},
		{name: "id missing", args: []string{"-a", "clutch"}, wantErr: "usage"},
		{name: "reason missing", args: []string{"-a", "clutch", "5"}, wantErr: "todo #5: a reason is required"},
		{name: "id not a number", args: []string{"abc", "r"}, wantErr: `todo id must be a positive integer, got "abc"`},
		{name: "id zero", args: []string{"0", "r"}, wantErr: `todo id must be a positive integer, got "0"`},
		// The id grammar is digits only (memory.ParseTodoID, shared with
		// the /todo verbs): a plus-signed token is a bad id, not ticket +7.
		{name: "id plus-signed", args: []string{"+7", "r"}, wantErr: `todo id must be a positive integer, got "+7"`},
		{name: "id plus-signed after the agent flag", args: []string{"-a", "clutch", "+7", "r"}, wantErr: `todo id must be a positive integer, got "+7"`},
		{name: "id with leading zeros is the number", args: []string{"007", "r"}, want: todoDoneArgs{id: 7, reason: "r"}},
		// Before the id only -a/--agent is a flag; every other word is the
		// id position and the id parser names it — a `-`-prefixed number
		// like any other non-digit word.
		{name: "id negative", args: []string{"-2", "r"}, wantErr: `todo id must be a positive integer, got "-2"`},
		{name: "id negative after the agent flag", args: []string{"-a", "clutch", "-3", "r"}, wantErr: `todo id must be a positive integer, got "-3"`},
		{name: "id negative out of int64 range", args: []string{"-99999999999999999999", "r"}, wantErr: `todo id must be a positive integer, got "-99999999999999999999"`},
		{name: "double-dash number is the id position", args: []string{"--3", "5", "r"}, wantErr: `todo id must be a positive integer, got "--3"`},
		{name: "bare dash is the id position", args: []string{"-", "5", "r"}, wantErr: `todo id must be a positive integer, got "-"`},
		{
			name:    "unknown leading flag is the id position",
			args:    []string{"-x", "5", "r"},
			wantErr: `todo id must be a positive integer, got "-x"`,
		},
		// A connection flag before the id cannot route the request from
		// there — the transport was fixed before the "todo" word — so the
		// error says where it goes, and names the flag only: an "=value"
		// tail may be a credential whose echo would reach cron logs and
		// the landing's log.
		{
			name:    "connection flag before the id",
			args:    []string{"--addr", "h:p", "7", "r"},
			wantErr: "--addr must come before the todo command word",
		},
		{
			name:    "socket flag before the id",
			args:    []string{"-a", "clutch", "--socket", "/x", "7", "r"},
			wantErr: "--socket must come before the todo command word",
		},
		{
			name:    "attached credential before the id is not echoed",
			args:    []string{"--api-key=sekret", "7", "r"},
			wantErr: "--api-key must come before the todo command word",
		},
		{name: "-a without value", args: []string{"-a"}, wantErr: "flag -a needs a value"},
		// A repeated agent flag is refused as a duplicate — the first
		// pair is consumed and the leftover one must not surface as
		// "flag -a needs a value", which is false: the value was supplied.
		{name: "repeated -a", args: []string{"-a", "a", "-a", "b", "7", "r"}, wantErr: "only one -a/--agent may precede the id"},
		{name: "repeated attached spellings", args: []string{"-a=a", "--agent=b", "5", "r"}, wantErr: "only one -a/--agent may precede the id"},
		{name: "pair then trailing bare -a", args: []string{"-a", "a", "-a", "5", "r"}, wantErr: "only one -a/--agent may precede the id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnvVal != "" {
				t.Setenv("FOCI_AGENT", tt.setEnvVal)
			}
			got, err := parseTodoDoneArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseTodoDoneArgs(%q) = %+v, want an error containing %q", tt.args, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTodoDoneArgs(%q): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parseTodoDoneArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// recordedTodoDone is one request a test gateway received.
type recordedTodoDone struct {
	Method string
	Path   string
	Body   string
}

// newRecordingGateway serves /todo/done, recording every request and
// answering through the settable reply function.
type recordingGateway struct {
	mu      sync.Mutex
	got     []recordedTodoDone
	replyFn func(w http.ResponseWriter, r *http.Request)
}

func (g *recordingGateway) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.got = append(g.got, recordedTodoDone{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	reply := g.replyFn
	g.mu.Unlock()
	if reply != nil {
		reply(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"response": "#7: done"})
}

func (g *recordingGateway) requests() []recordedTodoDone {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]recordedTodoDone(nil), g.got...)
}

// decodeTodoDoneBody decodes a recorded /todo/done request body into the
// {agent, id, reason} wire contract. The reason field is bytes on the wire
// (base64 in the reason_base64 field); it is returned as the string those
// bytes form.
func decodeTodoDoneBody(t *testing.T, raw string) (agent string, id int64, reason string) {
	t.Helper()
	var body struct {
		Agent  string `json:"agent"`
		ID     int64  `json:"id"`
		Reason []byte `json:"reason_base64"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode /todo/done body %q: %v", raw, err)
	}
	return body.Agent, body.ID, string(body.Reason)
}

// TestCmdTodoDone_PostsRequest proves the subcommand POSTs the {agent, id,
// reason_base64} JSON contract to /todo/done with the reason from argv
// untouched — and no plain "reason" field, which the route refuses. The
// rows include whitespace inside ONE argv element (double spaces, a tab,
// a newline): the wire body carries those bytes exactly — the join adds
// single spaces BETWEEN elements only.
func TestCmdTodoDone_PostsRequest(t *testing.T) {
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()

	for _, reason := range []string{
		`landed abc (run "x" $(id) ` + "`y`" + `)`,
		"a  b\tc\nd",
	} {
		before := len(gw.requests())
		if err := cmdTodo(srv.URL, []string{"done", "-a", "clutch", "7", reason}); err != nil {
			t.Fatalf("cmdTodo(%q): %v", reason, err)
		}

		reqs := gw.requests()[before:]
		if len(reqs) != 1 {
			t.Fatalf("gateway saw %d request(s), want 1", len(reqs))
		}
		if reqs[0].Method != http.MethodPost {
			t.Errorf("method = %q, want POST", reqs[0].Method)
		}
		if reqs[0].Path != "/todo/done" {
			t.Errorf("path = %q, want /todo/done", reqs[0].Path)
		}
		if strings.Contains(reqs[0].Body, `"reason"`) {
			t.Errorf("body = %q carries a plain reason field; the only reason key must be reason_base64", reqs[0].Body)
		}
		agent, id, gotReason := decodeTodoDoneBody(t, reqs[0].Body)
		if agent != "clutch" || id != 7 || gotReason != reason {
			t.Errorf("body = {%q, %d, %q}, want {clutch, 7, %q}", agent, id, gotReason, reason)
		}
	}
}

// TestRunGatewayCommand_Todo drives main's real command dispatch (extracted
// as runGatewayCommand) with `todo done`, in-process: the black-box tests
// exec the built binary, which coverage cannot see, so the dispatch case
// itself needs a caller. Pins that a command line starting "todo" routes to
// cmdTodo with the command word sliced away, POSTing the parsed
// {agent, id, reason}.
func TestRunGatewayCommand_Todo(t *testing.T) {
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()

	if err := runGatewayCommand(srv.URL, []string{"todo", "done", "-a", "clutch", "7", "landed abc"}); err != nil {
		t.Fatalf("runGatewayCommand: %v", err)
	}

	reqs := gw.requests()
	if len(reqs) != 1 {
		t.Fatalf("gateway saw %d request(s), want 1", len(reqs))
	}
	if reqs[0].Method != http.MethodPost || reqs[0].Path != "/todo/done" {
		t.Errorf("request = %s %q, want POST /todo/done", reqs[0].Method, reqs[0].Path)
	}
	agent, id, reason := decodeTodoDoneBody(t, reqs[0].Body)
	if agent != "clutch" || id != 7 || reason != "landed abc" {
		t.Errorf("body = {%q, %d, %q}, want {clutch, 7, landed abc}", agent, id, reason)
	}
}

// TestMainGlobalFlagScope drives the REAL main() — the black-box twins
// (TestCLITodoDone_BareEnvironment, TestCLICommand_FreeText) exec a
// subprocess, which per-test coverage cannot see, so main's own
// global-flag parse would otherwise be unpinned. Every arm first re-runs
// its line in a SUBPROCESS and only then in-process: a regressed main()
// os.Exits on its error path, and an in-process exit would kill the whole
// test binary mid-run — taking every later test's diagnostics with it —
// where the subprocess pre-flight fails this arm alone, with the
// subprocess's own error line. Pins every side of the scope rule that can
// run to success in-process:
//
//   - the freeTextCommands (todo, command, send) keep flag look-alikes
//     in their free text — a close reason, a dispatched slash text word
//     or a message word that looks like a gate stays text, whatever it
//     looks like (the operator ruling, 2026-10-17); on a whole-line
//     regression main would consume a connection-flag look-alike before
//     the command ever saw it, and the wire assertions here would catch
//     the missing words;
//
//   - every OTHER command keeps the historical whole-line scan: a global
//     flag placed after the command word still routes the request and is
//     kept out of the command's own arguments (`foci status --addr=h:p`);
//
//   - the scope itself is the freeTextCommands declaration: removing a
//     command from it moves that command back to the whole-line scan
//     (main consults the declaration, not a command-name check).
func TestMainGlobalFlagScope(t *testing.T) {
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	// No unix socket anywhere (temp HOME, empty FOCI_GW_SOCK), so main must
	// resolve the transport from --addr to reach the gateway.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FOCI_GW_SOCK", "")
	t.Setenv("FOCI_API_KEY", "")
	t.Setenv("FOCI_AGENT", "")

	origArgs, origClient := os.Args, client
	defer func() { os.Args, client = origArgs, origClient }()

	// runMain drives one main() arm: the subprocess pre-flight first (a
	// regression must fail THIS arm, not abort the package), then the
	// in-process run that covers main's parse for per-test coverage. The
	// subprocess inherits the arm's environment (t.Setenv above), so it
	// resolves the same transport and its request lands in the same
	// recording gateway.
	runMain := func(t *testing.T, argv []string) {
		t.Helper()
		var out bytes.Buffer
		sub := exec.Command(testBinary, argv[1:]...)
		sub.Stdout, sub.Stderr = &out, &out
		if err := sub.Run(); err != nil {
			t.Fatalf("subprocess %v: %v\n%s", argv, err, out.String())
		}
		os.Args = argv
		main() // must return, not exit: the gateway answers 200
	}

	// The todo side: the reason keeps flag look-alikes.
	for _, tt := range []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "gate look-alike inside the reason stays in the reason",
			argv: []string{"foci", "--addr", addr, "todo", "done", "-a", "clutch", "7", "landed", "--if-active", "8h"},
			want: "landed --if-active 8h",
		},
		{
			name: "dash words inside the reason stay in the reason",
			argv: []string{"foci", "--addr", addr, "todo", "done", "-a", "clutch", "7", "landed", "-n", "now"},
			want: "landed -n now",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runMain(t, tt.argv)

			reqs := gw.requests()
			if len(reqs) == 0 {
				t.Fatal("main reached no gateway — the leading --addr must route the request")
			}
			agent, id, reason := decodeTodoDoneBody(t, reqs[len(reqs)-1].Body)
			if agent != "clutch" || id != 7 || reason != tt.want {
				t.Errorf("wire body = {%q, %d, %q}, want {clutch, 7, %q}", agent, id, reason, tt.want)
			}
		})
	}

	// The send side: the message keeps flag look-alikes, like the todo
	// reason and the command text (send registers with
	// freeTextCommands too — its message is free text).
	for _, tt := range []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "gate look-alike inside the message stays in the message",
			argv: []string{"foci", "--addr", addr, "send", "-a", "clutch", "hello", "--if-active", "8h"},
			want: `"text":"hello --if-active 8h"`,
		},
		{
			name: "connection-flag pair inside the message stays in the message",
			argv: []string{"foci", "--addr", addr, "send", "-a", "clutch", "hello", "--api-key", "sekret"},
			want: `"text":"hello --api-key sekret"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runMain(t, tt.argv)

			reqs := gw.requests()
			if len(reqs) == 0 {
				t.Fatal("main reached no gateway — the leading --addr must route the request")
			}
			req := reqs[len(reqs)-1]
			if req.Path != "/send" {
				t.Fatalf("path = %q, want /send", req.Path)
			}
			if !strings.Contains(req.Body, tt.want) {
				t.Errorf("body = %q, want it to contain %q", req.Body, tt.want)
			}
		})
	}

	// The branch and eval sides: their texts are free text too (the
	// registration is what makes main keep flag look-alikes in their own
	// arguments instead of consuming them as globals).
	for _, tt := range []struct {
		name     string
		argv     []string
		wantPath string
		wantIn   string
	}{
		{
			name:     "branch text keeps a switch look-alike",
			argv:     []string{"foci", "--addr", addr, "branch", "check", "--no-gate"},
			wantPath: "/branch",
			wantIn:   `"text":"check --no-gate"`,
		},
		{
			name:     "eval command keeps a credential look-alike",
			argv:     []string{"foci", "--addr", addr, "eval", "ls", "--api-key", "sekret"},
			wantPath: "/send",
			wantIn:   "ls --api-key sekret",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runMain(t, tt.argv)

			reqs := gw.requests()
			if len(reqs) == 0 {
				t.Fatal("main reached no gateway — the leading --addr must route the request")
			}
			req := reqs[len(reqs)-1]
			if req.Path != tt.wantPath {
				t.Fatalf("path = %q, want %q", req.Path, tt.wantPath)
			}
			if !strings.Contains(req.Body, tt.wantIn) {
				t.Errorf("body = %q, want it to contain %q", req.Body, tt.wantIn)
			}
		})
	}

	// A connection flag typed between the "send" word and the text is
	// the placement error through the real binary too (it cannot route
	// the request from there): non-zero exit, one stderr line naming the
	// flag and the rule, nothing sent.
	t.Run("send connection flag before the text errors", func(t *testing.T) {
		before := len(gw.requests())
		var out bytes.Buffer
		sub := exec.Command(testBinary, "send", "-a", "clutch", "--addr", "h:p", "hello")
		sub.Stdout, sub.Stderr = &out, &out
		if err := sub.Run(); err == nil {
			t.Fatalf("misplaced --addr must exit non-zero; output: %q", out.String())
		}
		if line := strings.TrimSpace(out.String()); !strings.Contains(line, "--addr must come before the send command word") {
			t.Errorf("stderr = %q, want the placement rule naming --addr and the send word", line)
		}
		if n := len(gw.requests()) - before; n != 0 {
			t.Errorf("%d request(s) reached the gateway, want none", n)
		}
	})

	// The every-other-command side: a global flag AFTER the command word
	// is still a global flag there — routed as transport, never
	// delivered as text. (The todo/command/send arms above prove the
	// words survive on the free-text paths; these prove the same words
	// still work as flags elsewhere. status comes first because it fails
	// fastest on a regressed tree: it has no retry loop, so the process
	// reports the failure instead of spending a minute in send's.)
	for _, tt := range []struct {
		name     string
		argv     []string
		wantPath string
		wantIn   string // must appear in the request body
		wantOut  string // must NOT appear in the request body
	}{
		{
			name:     "status: attached --addr= form after the command word still routes",
			argv:     []string{"foci", "status", "--addr=" + addr},
			wantPath: "/status",
			wantIn:   "",
			wantOut:  "--addr",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			runMain(t, tt.argv)

			reqs := gw.requests()[before:]
			if len(reqs) == 0 {
				t.Fatal("main reached no gateway — an --addr after the command word must still route the request")
			}
			req := reqs[len(reqs)-1]
			if req.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", req.Path, tt.wantPath)
			}
			if tt.wantIn != "" && !strings.Contains(req.Body, tt.wantIn) {
				t.Errorf("body = %q, want it to contain %q", req.Body, tt.wantIn)
			}
			if strings.Contains(req.Body, tt.wantOut) {
				t.Errorf("body = %q, the global flag leaked into the command's arguments", req.Body)
			}
		})
	}

	// The scope rule is declared data, not a name check in main:
	// removing a command from freeTextCommands is the whole change —
	// main then reads that command's global flags from the whole line
	// again, so a message that IS a global-flag pair loses the pair to
	// the transport scan. Un-declaring `send` for the duration of this
	// arm must change what reaches the gateway. The pre-flight
	// subprocess cannot see the map change (it is process-local), but it
	// succeeds either way: its declared scan keeps the look-alikes in
	// the message, which also reaches the gateway and exits 0.
	t.Run("removing a command from freeTextCommands restores the whole-line scan", func(t *testing.T) {
		delete(freeTextCommands, "send")
		defer func() { freeTextCommands["send"] = true }()

		argv := []string{"foci", "--addr", addr, "send", "hello", "--api-key", "sekret"}
		var out bytes.Buffer
		sub := exec.Command(testBinary, argv[1:]...)
		sub.Stdout, sub.Stderr = &out, &out
		if err := sub.Run(); err != nil {
			t.Fatalf("subprocess %v: %v\n%s", argv, err, out.String())
		}

		before := len(gw.requests())
		os.Args = argv
		main() // must return, not exit: the gateway answers 200

		reqs := gw.requests()[before:]
		if len(reqs) == 0 {
			t.Fatal("main reached no gateway — the leading --addr must route the request")
		}
		req := reqs[len(reqs)-1]
		if req.Path != "/send" {
			t.Fatalf("path = %q, want /send", req.Path)
		}
		if !strings.Contains(req.Body, `"text":"hello"`) {
			t.Errorf("body = %q, want the whole-line scan to consume the flag pair from the message", req.Body)
		}
	})

	// `command` is declared free-text for real (its slash text may look
	// like any flag): the leading --addr routes the request, and the
	// dispatched text — passed as ONE quoted argument to keep the exact
	// spacing — keeps its flag-shaped words instead of losing them to
	// the parse. The unquoted forms dispatch the same words verbatim
	// (pinned black-box in TestCLICommand_FreeText).
	t.Run("command keeps gate look-alikes in its quoted slash text", func(t *testing.T) {
		before := len(gw.requests())
		runMain(t, []string{"foci", "--addr", addr, "command", "-a", "clutch", "/todo done 7 landed --if-active 8h"})

		reqs := gw.requests()[before:]
		if len(reqs) == 0 {
			t.Fatal("main reached no gateway — the leading --addr must route the request")
		}
		req := reqs[len(reqs)-1]
		if req.Path != "/command" {
			t.Fatalf("path = %q, want /command", req.Path)
		}
		cmd, agent := commandRequestBody(t, req.Body)
		if cmd != "/todo done 7 landed --if-active 8h" || agent != "clutch" {
			t.Errorf("body = {command %q, agent %q}, want the text typed exactly for clutch", cmd, agent)
		}
	})
}

// TestCmdTodoDone_ServerRefusalNamesTodo proves a server refusal comes back
// as ONE error line naming the todo, the agent and the server's cause.
func TestCmdTodoDone_ServerRefusalNamesTodo(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		reply     string
		wantInErr string
	}{
		{name: "unknown todo", status: http.StatusNotFound, reply: "todo #99 not found", wantInErr: "todo #99 not found"},
		{name: "unknown agent", status: http.StatusBadRequest, reply: `unknown agent: "zz"`, wantInErr: `unknown agent: "zz"`},
		{name: "empty reason", status: http.StatusBadRequest, reply: "reason is required when transitioning to done (e.g. 'implemented in abc1234', 'no longer relevant')", wantInErr: "reason is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &recordingGateway{}
			gw.replyFn = func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, tt.reply, tt.status)
			}
			srv := httptest.NewServer(http.HandlerFunc(gw.handle))
			defer srv.Close()

			err := cmdTodoDone(srv.URL, []string{"-a", "clutch", "99", "r"})
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("error must be one line, got: %q", err.Error())
			}
			for _, want := range []string{"#99", `"clutch"`, tt.wantInErr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestCmdTodoDone_UnreachableGatewayDoesNotRetry proves an unreachable
// gateway fails after exactly ONE attempt — the landing must get its exit
// status now, not a minute of retries.
func TestCmdTodoDone_UnreachableGatewayDoesNotRetry(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("recorder cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	err := cmdTodoDone(srv.URL, []string{"-a", "clutch", "99", "r"})
	if err == nil {
		t.Fatal("want an error, got none")
	}
	for _, want := range []string{"#99", "unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("gateway saw %d attempt(s), want exactly 1 (no retry loop)", n)
	}
}

// TestCLITodoDone_BareEnvironment drives the real CLI binary as the factory's
// landing does: an environment with only HOME and PATH — no FOCI_SOCK, no
// BASH_ENV, no FOCI_* at all — discovering the gateway on
// $HOME/data/foci-gw.sock, and reporting verdicts through the exit status
// and one stderr line.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestCLITodoDone_BareEnvironment(t *testing.T) {
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

	baseEnv := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, "FOCI_") || strings.HasPrefix(kv, "BASH_ENV") {
			t.Fatalf("bare env must not carry %q", kv)
		}
	}

	run := func(env []string, args ...string) (string, string, error) {
		cmd := exec.Command(testBinary, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	oneStderrLine := func(t *testing.T, stderr string) string {
		t.Helper()
		line := strings.TrimSpace(stderr)
		if line == "" || strings.Contains(line, "\n") {
			t.Fatalf("stderr must be exactly one non-empty line, got %q", stderr)
		}
		return line
	}

	// Success: exit 0, the spec reasons stored verbatim on the wire —
	// including bytes that are not valid UTF-8 and whitespace INSIDE one
	// argv element (double spaces, a tab, a newline), which must survive
	// the JSON hop unchanged.
	for _, reason := range []string{
		"landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)",
		"landed abc (run \"x\" $(id) `y`)",
		"bad\xffbyte",
		"a  b\tc\nd",
	} {
		stdout, stderr, err := run(baseEnv, "todo", "done", "-a", "clutch", "7", reason)
		if err != nil {
			t.Fatalf("todo done: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, "#7: done") {
			t.Errorf("stdout = %q, want it to contain #7: done", stdout)
		}
		reqs := gw.requests()
		if len(reqs) == 0 {
			t.Fatal("gateway recorded no request")
		}
		agent, id, gotReason := decodeTodoDoneBody(t, reqs[len(reqs)-1].Body)
		if agent != "clutch" || id != 7 || gotReason != reason {
			t.Errorf("wire body = {%q, %d, %q}, want reason verbatim %q", agent, id, gotReason, reason)
		}
	}

	// The ruling (Dick, 2026-10-17): from the reason onward every word is
	// reason verbatim, whatever it looks like. Flag-shaped reason words —
	// including a connection flag pair and an agent-flag spelling — close
	// the todo with those words stored, and the LEADING -a keeps choosing
	// the agent.
	for _, tt := range []struct {
		args   []string
		agent  string
		reason string
	}{
		{args: []string{"todo", "done", "-a", "clutch", "7", "--api-key", "sekret"}, agent: "clutch", reason: "--api-key sekret"},
		{args: []string{"todo", "done", "-a", "clutch", "7", "--socket", "/evil/x.sock"}, agent: "clutch", reason: "--socket /evil/x.sock"},
		{args: []string{"todo", "done", "-a", "clutch", "7", "r", "--addr=nowhere:9", "x"}, agent: "clutch", reason: "r --addr=nowhere:9 x"},
		{args: []string{"todo", "done", "-a", "clutch", "7", "landed", "-a", "other"}, agent: "clutch", reason: "landed -a other"},
		{args: []string{"todo", "done", "7", "--agent=evil", "r"}, agent: "", reason: "--agent=evil r"},
	} {
		stdout, stderr, err := run(baseEnv, tt.args...)
		if err != nil {
			t.Fatalf("%v must exit 0; stderr: %q", tt.args, stderr)
		}
		if !strings.Contains(stdout, "#7: done") {
			t.Errorf("%v: stdout = %q, want it to contain #7: done", tt.args, stdout)
		}
		gotAgent, id, gotReason := decodeTodoDoneBody(t, gw.requests()[len(gw.requests())-1].Body)
		if gotAgent != tt.agent || id != 7 || gotReason != tt.reason {
			t.Errorf("%v: wire body = {%q, %d, %q}, want {%q, 7, %q}", tt.args, gotAgent, id, gotReason, tt.agent, tt.reason)
		}
	}

	// Other flag-shaped reason words stay reason: the trailing-dash form,
	// a lone "-n" after the id (the leading scan stops at the id), is a
	// reason word, not a flag.
	stdout, stderr, err := run(baseEnv, "todo", "done", "-a", "clutch", "7", "-n")
	if err != nil {
		t.Fatalf("todo done -n: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "#7: done") {
		t.Errorf("stdout = %q, want it to contain #7: done", stdout)
	}
	if _, _, gotReason := decodeTodoDoneBody(t, gw.requests()[len(gw.requests())-1].Body); gotReason != "-n" {
		t.Errorf("wire reason = %q, want %q", gotReason, "-n")
	}

	// Shape errors: non-zero exit, exactly one stderr line — no usage dump
	// before the verdict (an unknown subcommand included).
	for _, tt := range []struct {
		args   []string
		wantIn string
	}{
		{args: []string{"todo", "done", "-a", "clutch", "7"}, wantIn: "reason is required"},
		{args: []string{"todo", "done", "-a", "clutch", "x", "r"}, wantIn: `todo id must be a positive integer, got "x"`},
		{args: []string{"todo", "--addr", "x", "done", "-a", "a", "7", "r"}, wantIn: "unknown subcommand: --addr"},
		{args: []string{"todo", "done", "-a", "clutch", "--addr", "h:p", "7", "r"}, wantIn: "--addr must come before the todo command word"},
	} {
		stdout, stderr, err := run(baseEnv, tt.args...)
		if err == nil {
			t.Fatalf("%v must exit non-zero; stdout: %q stderr: %q", tt.args, stdout, stderr)
		}
		if line := oneStderrLine(t, stderr); !strings.Contains(line, tt.wantIn) {
			t.Errorf("%v: stderr line = %q, want it to contain %q", tt.args, line, tt.wantIn)
		}
	}

	// Server refusals: non-zero exit, exactly one stderr line naming the todo.
	gw.mu.Lock()
	gw.replyFn = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "todo #99 not found", http.StatusNotFound)
	}
	gw.mu.Unlock()
	stdout, stderr, err = run(baseEnv, "todo", "done", "-a", "clutch", "99", "r")
	if err == nil {
		t.Fatalf("404 must exit non-zero; stdout: %q stderr: %q", stdout, stderr)
	}
	if line := oneStderrLine(t, stderr); !strings.Contains(line, "#99") {
		t.Errorf("stderr line = %q, want it to name #99", line)
	}

	gw.mu.Lock()
	gw.replyFn = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `unknown agent: "zz"`, http.StatusBadRequest)
	}
	gw.mu.Unlock()
	stdout, stderr, err = run(baseEnv, "todo", "done", "-a", "zz", "1", "r")
	if err == nil {
		t.Fatalf("400 must exit non-zero; stdout: %q stderr: %q", stdout, stderr)
	}
	if line := oneStderrLine(t, stderr); !strings.Contains(line, `"zz"`) {
		t.Errorf("stderr line = %q, want it to name the agent", line)
	}

	// Unreachable gateway: one line, no retry banner.
	gw.mu.Lock()
	gw.replyFn = nil
	gw.mu.Unlock()
	deadHome := t.TempDir() // no data/foci-gw.sock
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for dead addr: %v", err)
	}
	deadAddr := l.Addr().String()
	_ = l.Close()
	stdout, stderr, err = run(
		[]string{"HOME=" + deadHome, "PATH=" + os.Getenv("PATH"), "FOCI_ADDR=" + deadAddr},
		"todo", "done", "-a", "clutch", "99", "r")
	if err == nil {
		t.Fatalf("unreachable gateway must exit non-zero; stdout: %q stderr: %q", stdout, stderr)
	}
	line := oneStderrLine(t, stderr)
	if !strings.Contains(line, "#99") || !strings.Contains(line, "unreachable") {
		t.Errorf("stderr line = %q, want it to name #99 and the unreachable gateway", line)
	}
	if strings.Contains(line, "gave up after") {
		t.Errorf("stderr line = %q, contains the retry-loop banner", line)
	}
}

// TestCLIHelp_TodoDone proves the help texts exist where the factory would
// look: `foci todo done -h`, `foci todo -h`, and the `foci help` listing.
//
// disconnected-test-ok: black-box CLI integration test; execs compiled binary
func TestCLIHelp_TodoDone(t *testing.T) {
	env := []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}

	run := func(args ...string) (string, error) {
		cmd := exec.Command(testBinary, args...)
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		return out.String(), err
	}

	out, err := run("todo", "done", "-h")
	if err != nil {
		t.Fatalf("todo done -h: %v\n%s", err, out)
	}
	for _, want := range []string{"foci todo done", "<reason", "services without an agent exec"} {
		if !strings.Contains(out, want) {
			t.Errorf("todo done -h output = %q, want it to contain %q", out, want)
		}
	}

	out, err = run("todo", "-h")
	if err != nil {
		t.Fatalf("todo -h: %v\n%s", err, out)
	}
	if !strings.Contains(out, "foci todo") {
		t.Errorf("todo -h output = %q, want it to contain %q", out, "foci todo")
	}

	out, err = run("help")
	if err != nil {
		t.Fatalf("help: %v\n%s", err, out)
	}
	// The factory-landing line: this close command exists precisely for
	// services without an agent exec, and the top-level help must keep
	// saying so (spec req 7).
	for _, want := range []string{"todo done", "services without an agent exec", "factory's landing"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q:\n%s", want, out)
		}
	}
}

// TestCmdTodo_DispatchAndHelp pins the in-process dispatch: help for -h and
// --help, a bare `foci todo` refused like every argument-less sibling (with
// the usage printed), and any other shape error ONE verdict line — an
// unknown subcommand names it with no usage block before it (a global flag
// landing where a subcommand belongs is unknown too: todo reads the global
// flags only before its command word), and the error names the flag's head
// only, so an attached credential is never echoed.
func TestCmdTodo_DispatchAndHelp(t *testing.T) {
	base := "http://127.0.0.1:0" // unreachable — proves no HTTP call is made
	for _, flag := range []string{"-h", "--help"} {
		if err := cmdTodo(base, []string{flag}); err != nil {
			t.Errorf("todo %s returned error: %v", flag, err)
		}
	}
	// The argument-less form is the one that still shows the usage block.
	stderr := captureStderr(t, func() {
		if err := cmdTodo(base, nil); err == nil {
			t.Error("bare todo must be an error (exit 1), like every argument-less sibling")
		}
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("bare todo stderr = %q, want the usage block", stderr)
	}
	// Every other shape error is one verdict line, not a usage dump.
	for _, tok := range []string{"bogus", "--socket", "--addr", "--api-key=sekret"} {
		var err error
		stderr := captureStderr(t, func() {
			err = cmdTodo(base, []string{tok, "done"})
		})
		if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
			t.Errorf("unknown subcommand %s error = %v, want the unknown-subcommand verdict", tok, err)
		}
		if strings.Contains(err.Error(), "sekret") {
			t.Errorf("unknown subcommand %s error = %v echoes the attached value", tok, err)
		}
		if strings.Contains(stderr, "Usage:") {
			t.Errorf("unknown subcommand %s printed the usage block before the verdict: %q", tok, stderr)
		}
	}
}
