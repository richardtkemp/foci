package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/configapprove"
)

// fakeConfigGateway serves /config/request: POST answers with submit, and
// each GET answers with the next scripted poll (status code + status).
type fakeConfigGateway struct {
	mu     sync.Mutex
	submit configapprove.Status
	polls  []struct {
		code int
		st   configapprove.Status
	}
	posted map[string]any
}

func (f *fakeConfigGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&f.posted)
		_ = json.NewEncoder(w).Encode(f.submit)
		return
	}
	p := f.polls[0]
	if len(f.polls) > 1 {
		f.polls = f.polls[1:]
	}
	w.WriteHeader(p.code)
	_ = json.NewEncoder(w).Encode(p.st)
}

func (f *fakeConfigGateway) poll(code int, o configapprove.Outcome, reason string) {
	f.polls = append(f.polls, struct {
		code int
		st   configapprove.Status
	}{code, configapprove.Status{ID: "cfg-1", Outcome: o, Path: "agent_loop.max_tool_loops", Old: "10", New: "20", Reason: reason}})
}

// TestConfigSet_PrintsEachOutcome proves `foci config set` against a gateway
// prints each of the four outcomes on one line, exits 0 only for applied,
// follows pending polls until the outcome, and reports a gateway restart
// (404 on the poll) as failed instead of hanging.
func TestConfigSet_PrintsEachOutcome(t *testing.T) {
	configPollSleep = func(time.Duration) {}
	t.Cleanup(func() { configPollSleep = time.Sleep })
	t.Setenv("FOCI_SESSION_KEY", "main/app/1")

	pending := configapprove.Status{ID: "cfg-1", Outcome: configapprove.Pending, Path: "agent_loop.max_tool_loops", Old: "10", New: "20"}
	cases := []struct {
		name     string
		setup    func(*fakeConfigGateway)
		wantLine string
		wantCode int
	}{
		{"applied", func(f *fakeConfigGateway) {
			f.poll(200, configapprove.Pending, "")
			f.poll(200, configapprove.Applied, "")
		}, "applied: agent_loop.max_tool_loops = 20 (was 10)", 0},
		{"denied", func(f *fakeConfigGateway) { f.poll(200, configapprove.Denied, "x") }, "denied:", 1},
		{"timed out", func(f *fakeConfigGateway) { f.poll(200, configapprove.TimedOut, "x") }, "timed out:", 1},
		{"failed", func(f *fakeConfigGateway) { f.poll(200, configapprove.Failed, "the file changed") }, "failed: the file changed", 1},
		{"restart", func(f *fakeConfigGateway) { f.poll(404, configapprove.Failed, "unknown request") }, "failed: the gateway restarted", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeConfigGateway{submit: pending}
			tc.setup(f)
			srv := httptest.NewServer(f)
			defer srv.Close()
			var out, errOut strings.Builder
			code := cmdConfig(srv.URL, []string{"set", "agent_loop.max_tool_loops", "20"}, &out, &errOut)
			if code != tc.wantCode || !strings.HasPrefix(out.String(), tc.wantLine) {
				t.Errorf("exit %d stdout %q, want %d %q (stderr %q)", code, out.String(), tc.wantCode, tc.wantLine, errOut.String())
			}
			if strings.Count(strings.TrimSpace(out.String()), "\n") != 0 {
				t.Errorf("want one outcome line, got %q", out.String())
			}
			if f.posted["session"] != "main/app/1" || f.posted["op"] != "set" || f.posted["value"] != "20" {
				t.Errorf("posted %v", f.posted)
			}
		})
	}
}

// TestConfigSet_RefusedAtOnce proves a request the gateway refuses on
// submit prints failed with the reason, exit 1, with no polling.
func TestConfigSet_RefusedAtOnce(t *testing.T) {
	t.Setenv("FOCI_SESSION_KEY", "main/app/1")
	f := &fakeConfigGateway{submit: configapprove.Status{Outcome: configapprove.Failed, Reason: "unknown key"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := cmdConfig(srv.URL, []string{"unset", "a.b"}, &out, &errOut); code != 1 || out.String() != "failed: unknown key\n" {
		t.Errorf("exit %d stdout %q", code, out.String())
	}
	if f.posted["op"] != "unset" {
		t.Errorf("posted %v", f.posted)
	}
}

// TestConfigSet_NeedsSession proves set without a session is a usage error.
func TestConfigSet_NeedsSession(t *testing.T) {
	t.Setenv("FOCI_SESSION_KEY", "")
	var out, errOut strings.Builder
	if code := cmdConfig("http://unused", []string{"set", "a.b", "1"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "FOCI_SESSION_KEY") {
		t.Errorf("exit %d stderr %q", code, errOut.String())
	}
}

// TestConfigGet proves get reads the config file: a set value renders as
// TOML, an agent key resolves through agents[id], an unset key says so.
func TestConfigGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	body := "[agent_loop]\nmax_tool_loops = 10\n\n[[agents]]\nid = \"main\"\nworkspace = \"/w\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{
		"agent_loop.max_tool_loops": "agent_loop.max_tool_loops = 10\n",
		"agents[main].workspace":    "agents[main].workspace = \"/w\"\n",
		"agent_loop.streaming":      "agent_loop.streaming is unset in " + path + " (the default applies)\n",
	} {
		var out, errOut strings.Builder
		if code := cmdConfig("", []string{"get", args, "--config", path}, &out, &errOut); code != 0 || out.String() != want {
			t.Errorf("get %s: exit %d stdout %q stderr %q", args, code, out.String(), errOut.String())
		}
	}
}

// TestConfigHelp proves `foci -h` lists config and `foci config -h` prints
// every operation, through the built binary.
func TestConfigHelp(t *testing.T) {
	var out, errOut strings.Builder
	if code := cmdConfig("", []string{"-h"}, &out, &errOut); code != 0 {
		t.Errorf("exit %d", code)
	}
	for _, want := range []string{"show", "get <path>", "set <path> <value>", "unset <path>", "wait <request-id>", "applied, denied, timed out"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
}
