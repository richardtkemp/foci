package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestParseBranchFlagsSessionConsumed is the R1 red test at the parser
// level: `foci branch` must consume -s/--session in all four forms, leaving
// only the message words in rest. On the base the flag tokens fall into the
// `default:` arm and become message text.
func TestParseBranchFlagsSessionConsumed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"-s value", []string{"-s", "Fabro", "question"}},
		{"-s=value", []string{"-s=Fabro", "question"}},
		{"--session value", []string{"--session", "Fabro", "question"}},
		{"--session=value", []string{"--session=Fabro", "question"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rest := parseBranchFlags(tc.args)
			if len(rest) != 1 || rest[0] != "question" {
				t.Errorf("R1: parseBranchFlags(%v) rest = %v, want only the message words [question] — the session flag leaked into the text", tc.args, rest)
			}
		})
	}

	// A valueless -s at the end of the args stays in the message words,
	// exactly as `foci send` treats it (no next token to consume).
	_, rest := parseBranchFlags([]string{"hello", "-s"})
	if len(rest) != 2 || rest[0] != "hello" || rest[1] != "-s" {
		t.Errorf("rest = %v, want [hello -s] — a bare valueless flag is message text", rest)
	}
}

// TestBranchSessionFlagForwarded is the R1 red test at the body level:
// `foci branch` must put the selector on the /branch request body as
// "session" and keep the message words as the text, in all four flag forms,
// with FOCI_SESSION as the fallback and an explicit flag beating the env.
func TestBranchSessionFlagForwarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"-s value", []string{"-a", "clutch", "-s", "Fabro", "question"}},
		{"-s=value", []string{"-a", "clutch", "-s=Fabro", "question"}},
		{"--session value", []string{"-a", "clutch", "--session", "Fabro", "question"}},
		{"--session=value", []string{"-a", "clutch", "--session=Fabro", "question"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := captureBody(t, func(base string) error {
				return cmdBranch(base, tc.args)
			})
			if body["session"] != "Fabro" {
				t.Errorf("R1: body session = %v, want Fabro — the selector never reached the gateway", body["session"])
			}
			if body["text"] != "question" {
				t.Errorf("R1: body text = %v, want only the message words (the session flag leaked?)", body["text"])
			}
		})
	}

	t.Run("FOCI_SESSION alone sets it", func(t *testing.T) {
		t.Setenv("FOCI_SESSION", "research")
		body := captureBody(t, func(base string) error {
			return cmdBranch(base, []string{"-a", "clutch", "question"})
		})
		if body["session"] != "research" {
			t.Errorf("R1: body session = %v, want research from FOCI_SESSION", body["session"])
		}
	})

	t.Run("flag beats the env", func(t *testing.T) {
		t.Setenv("FOCI_SESSION", "research")
		body := captureBody(t, func(base string) error {
			return cmdBranch(base, []string{"-a", "clutch", "-s", "Fabro", "question"})
		})
		if body["session"] != "Fabro" {
			t.Errorf("R1: body session = %v, want Fabro — the flag must win over FOCI_SESSION", body["session"])
		}
	})
}

// TestBranchUsageDocumentsSession is the R2 red test: `foci branch -h` must
// document the -s/--session flag — in the usage line, in the flags table with
// its FOCI_SESSION env var — and its intro must offer the named session as
// the parent instead of claiming the branch always forks the main chat.
func TestBranchUsageDocumentsSession(t *testing.T) {
	help := captureStderr(t, branchUsage)
	for _, want := range []string{
		"[-s session]",       // R2: the usage line shows the flag
		"-s, --session",      // R2: the flags table documents it
		"FOCI_SESSION",       // R2: with its env var
		"-s/--session names", // R2: the intro names the flag as the parent selector
	} {
		if !strings.Contains(help, want) {
			t.Errorf("R2: branchUsage output does not mention %q", want)
		}
	}
	if strings.Contains(help, "main chat.") {
		t.Errorf("R2: branchUsage still says the branch always forks the agent's main chat")
	}
}

// TestBranchSyncSilentSessionCLI is the R6 red test, end to end on the CLI:
// `foci branch -s Fabro --sync --silent "q"` must send async:false,
// silent:true and session:"Fabro" on the /branch body, and print the
// response field on stdout.
func TestBranchSyncSilentSessionCLI(t *testing.T) {
	var mu sync.Mutex
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"canned branch reply"}`))
	}))
	defer srv.Close()

	stdout, _ := capture(t, func() {
		if err := cmdBranch(srv.URL, []string{"-a", "clutch", "-s", "Fabro", "--sync", "--silent", "question"}); err != nil {
			t.Errorf("cmdBranch: %v", err)
		}
	})

	mu.Lock()
	body := got
	mu.Unlock()
	if body["session"] != "Fabro" {
		t.Errorf("R6: body session = %v, want Fabro", body["session"])
	}
	if async, ok := body["async"].(bool); !ok || async {
		t.Errorf("R6: body async = %v, want false (--sync)", body["async"])
	}
	if silent, ok := body["silent"].(bool); !ok || !silent {
		t.Errorf("R6: body silent = %v, want true", body["silent"])
	}
	if body["text"] != "question" {
		t.Errorf("R6: body text = %v, want question only", body["text"])
	}
	if strings.TrimSpace(stdout) != "canned branch reply" {
		t.Errorf("R6: stdout = %q, want the response field printed", stdout)
	}
}

// TestBranchNoSessionKeyByDefault pins the unchanged default: with no -s
// flag and no FOCI_SESSION, the /branch body carries no "session" key, so
// the gateway branches the agent's default session.
func TestBranchNoSessionKeyByDefault(t *testing.T) {
	t.Setenv("FOCI_SESSION", "")
	body := captureBody(t, func(base string) error {
		return cmdBranch(base, []string{"-a", "clutch", "hello"})
	})
	if _, ok := body["session"]; ok {
		t.Errorf("body carries session = %v with no flag and no env — the default session must stay implicit", body["session"])
	}
}
