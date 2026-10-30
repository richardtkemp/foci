package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestParseSendFlags tests basic flag parsing with agent, session, and timing flags.
func TestParseSendFlags(t *testing.T) {
	tests := []struct {
		name               string
		args               []string
		wantAgent          string
		wantSession        string
		wantIfActive       string
		wantIfInactive     string
		wantIfUserActive   string
		wantIfUserInactive string
		wantRest           []string
	}{
		{
			name:     "no flags",
			args:     []string{"hello", "world"},
			wantRest: []string{"hello", "world"},
		},
		{
			name:         "--if-warm with value (canonical)",
			args:         []string{"--if-warm", "8h", "hello"},
			wantIfActive: "8h",
			wantRest:     []string{"hello"},
		},
		{
			name:           "--if-cold=value (canonical)",
			args:           []string{"--if-cold=30m", "hello"},
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
		{
			name:         "--if-active with value (alias)",
			args:         []string{"--if-active", "8h", "hello"},
			wantIfActive: "8h",
			wantRest:     []string{"hello"},
		},
		{
			name:         "--if-active=value",
			args:         []string{"--if-active=30m", "hello"},
			wantIfActive: "30m",
			wantRest:     []string{"hello"},
		},
		{
			name:         "all flags together",
			args:         []string{"-a", "clutch", "-s", "main", "--if-active", "4h", "hello"},
			wantAgent:    "clutch",
			wantSession:  "main",
			wantIfActive: "4h",
			wantRest:     []string{"hello"},
		},
		{
			// The one placement rule (Dick, 2026-10-17): send's flags are
			// read only before the message text starts — a gate pair after
			// the first message word is message text, never a flag.
			name:     "--if-active after the text is text",
			args:     []string{"hello", "--if-active", "12h"},
			wantRest: []string{"hello", "--if-active", "12h"},
		},
		{
			name:     "--if-active without value at end",
			args:     []string{"hello", "--if-active"},
			wantRest: []string{"hello", "--if-active"},
		},
		{
			name:           "--if-inactive with value",
			args:           []string{"--if-inactive", "30m", "hello"},
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
		{
			name:           "--if-inactive=value",
			args:           []string{"--if-inactive=1h", "hello"},
			wantIfInactive: "1h",
			wantRest:       []string{"hello"},
		},
		{
			name:           "both --if-active and --if-inactive",
			args:           []string{"--if-active", "8h", "--if-inactive", "30m", "hello"},
			wantIfActive:   "8h",
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
		// --- TODO #753: user-attention gates ---
		{
			name:             "--if-user-active with value",
			args:             []string{"--if-user-active", "2h", "hello"},
			wantIfUserActive: "2h",
			wantRest:         []string{"hello"},
		},
		{
			name:             "--if-user-active=value",
			args:             []string{"--if-user-active=45m", "hello"},
			wantIfUserActive: "45m",
			wantRest:         []string{"hello"},
		},
		{
			name:               "--if-user-inactive with value",
			args:               []string{"--if-user-inactive", "30m", "hello"},
			wantIfUserInactive: "30m",
			wantRest:           []string{"hello"},
		},
		{
			name:               "--if-user-inactive=value",
			args:               []string{"--if-user-inactive=1h", "hello"},
			wantIfUserInactive: "1h",
			wantRest:           []string{"hello"},
		},
		{
			name:               "all four gate flags coexist",
			args:               []string{"--if-active", "8h", "--if-inactive", "30m", "--if-user-active", "2h", "--if-user-inactive", "10m", "hello"},
			wantIfActive:       "8h",
			wantIfInactive:     "30m",
			wantIfUserActive:   "2h",
			wantIfUserInactive: "10m",
			wantRest:           []string{"hello"},
		},
		{
			name:     "--if-user-active without value at end",
			args:     []string{"hello", "--if-user-active"},
			wantRest: []string{"hello", "--if-user-active"},
		},
		{
			name:     "--if-user-inactive without value at end",
			args:     []string{"hello", "--if-user-inactive"},
			wantRest: []string{"hello", "--if-user-inactive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest, err := parseSendFlags(tt.args)
			if err != nil {
				t.Fatalf("parseSendFlags(%v): %v", tt.args, err)
			}
			if flags.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", flags.agent, tt.wantAgent)
			}
			if flags.session != tt.wantSession {
				t.Errorf("session = %q, want %q", flags.session, tt.wantSession)
			}
			if flags.ifWarm != tt.wantIfActive {
				t.Errorf("ifWarm = %q, want %q", flags.ifWarm, tt.wantIfActive)
			}
			if flags.ifCold != tt.wantIfInactive {
				t.Errorf("ifCold = %q, want %q", flags.ifCold, tt.wantIfInactive)
			}
			if flags.ifUserActive != tt.wantIfUserActive {
				t.Errorf("ifUserActive = %q, want %q", flags.ifUserActive, tt.wantIfUserActive)
			}
			if flags.ifUserInactive != tt.wantIfUserInactive {
				t.Errorf("ifUserInactive = %q, want %q", flags.ifUserInactive, tt.wantIfUserInactive)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

// TestParseSendFlagsAsyncSync tests --async, --sync, --wait, and --no-wait flag parsing.
func TestParseSendFlagsAsyncSync(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAsync bool
		wantSync  bool
		wantRest  []string
	}{
		{"--async", []string{"--async", "hello"}, true, false, []string{"hello"}},
		{"--no-wait", []string{"--no-wait", "hello"}, true, false, []string{"hello"}},
		{"--sync", []string{"--sync", "hello"}, false, true, []string{"hello"}},
		{"--wait", []string{"--wait", "hello"}, false, true, []string{"hello"}},
		{"--sync with other flags", []string{"-a", "clutch", "--sync", "hello"}, false, true, []string{"hello"}},
		{"no async/sync flags", []string{"hello"}, false, false, []string{"hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest, err := parseSendFlags(tt.args)
			if err != nil {
				t.Fatalf("parseSendFlags(%v): %v", tt.args, err)
			}
			if flags.async != tt.wantAsync {
				t.Errorf("async = %v, want %v", flags.async, tt.wantAsync)
			}
			if flags.sync != tt.wantSync {
				t.Errorf("sync = %v, want %v", flags.sync, tt.wantSync)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

func TestParseSendFlagsWait(t *testing.T) {
	flags, rest, err := parseSendFlags([]string{
		"--wait-cold", "5m", "--wait-active", "30s", "--deadline", "3h", "--no-gate", "hi",
	})
	if err != nil {
		t.Fatalf("parseSendFlags: %v", err)
	}
	if flags.waitCold != "5m" {
		t.Errorf("waitCold = %q, want 5m", flags.waitCold)
	}
	if flags.waitWarm != "30s" { // --wait-active is the alias for --wait-warm
		t.Errorf("waitWarm = %q, want 30s", flags.waitWarm)
	}
	if flags.waitTimeout != "3h" { // --deadline is the alias for --wait-timeout
		t.Errorf("waitTimeout = %q, want 3h", flags.waitTimeout)
	}
	if !flags.noGate {
		t.Error("noGate = false, want true")
	}
	if len(rest) != 1 || rest[0] != "hi" {
		t.Errorf("rest = %v, want [hi]", rest)
	}

	body := map[string]interface{}{}
	flags.waitFlags.addToBody(body)
	if body["wait_cold"] != "5m" || body["wait_warm"] != "30s" || body["wait_timeout"] != "3h" || body["wait_none"] != true {
		t.Errorf("addToBody wire keys wrong: %+v", body)
	}
}

// TestParseSendFlagsMessageFlags tests -mt, -mf, --message-text, and --message-file flag parsing.
func TestParseSendFlagsMessageFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantMT   string
		wantMF   string
		wantRest []string
	}{
		{"-mt with value", []string{"-mt", "hello"}, "hello", "", nil},
		{"--mt with value", []string{"--mt", "hello"}, "hello", "", nil},
		{"--message-text with value", []string{"--message-text", "hello"}, "hello", "", nil},
		{"-mt=value", []string{"-mt=hello"}, "hello", "", nil},
		{"--mt=value", []string{"--mt=hello"}, "hello", "", nil},
		{"--message-text=value", []string{"--message-text=hello"}, "hello", "", nil},
		{"-mf with value", []string{"-mf", "/tmp/f"}, "", "/tmp/f", nil},
		{"--mf with value", []string{"--mf", "/tmp/f"}, "", "/tmp/f", nil},
		{"--message-file with value", []string{"--message-file", "/tmp/f"}, "", "/tmp/f", nil},
		{"-mf=value", []string{"-mf=/tmp/f"}, "", "/tmp/f", nil},
		{"--mf=value", []string{"--mf=/tmp/f"}, "", "/tmp/f", nil},
		{"--message-file=value", []string{"--message-file=/tmp/f"}, "", "/tmp/f", nil},
		{"-mt with other flags", []string{"-a", "clutch", "-mt", "hi", "extra"}, "hi", "", []string{"extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest, err := parseSendFlags(tt.args)
			if err != nil {
				t.Fatalf("parseSendFlags(%v): %v", tt.args, err)
			}
			if flags.messageText != tt.wantMT {
				t.Errorf("messageText = %q, want %q", flags.messageText, tt.wantMT)
			}
			if flags.messageFile != tt.wantMF {
				t.Errorf("messageFile = %q, want %q", flags.messageFile, tt.wantMF)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

// TestSendFreeTextBoundary pins the one placement rule on `foci send`'s
// line (Dick, 2026-10-17, applied to send by the round-27 review): send's
// flags are read only before the message text starts; every word from the
// first message word on is message verbatim, whatever it looks like. The
// three boundary refusals the rule carries elsewhere apply here too: a
// connection flag, a value-flag that ends the line, a second complete -a
// pair.
func TestSendFreeTextBoundary(t *testing.T) {
	t.Setenv("FOCI_AGENT", "")
	for _, kv := range []string{"FOCI_IF_WARM", "FOCI_WAIT_COLD", "FOCI_HUMAN", "FOCI_SESSION", "FOCI_NO_GATE", "FOCI_STEER", "FOCI_MODEL"} {
		t.Setenv(kv, "")
	}

	// Flag look-alikes after the first message word are message words;
	// the same flags before it keep their meaning.
	for _, tt := range []struct {
		name       string
		args       []string
		wantText   []string
		wantIfWarm string
		wantAgent  string
	}{
		{
			name:     "trailing gate pair is message text",
			args:     []string{"hello", "--if-warm", "8h"},
			wantText: []string{"hello", "--if-warm", "8h"},
		},
		{
			name:       "leading gate pair still gates",
			args:       []string{"--if-warm", "8h", "hello"},
			wantText:   []string{"hello"},
			wantIfWarm: "8h",
		},
		{
			name:     "trailing agent pair is message text",
			args:     []string{"hello", "-a", "research"},
			wantText: []string{"hello", "-a", "research"},
		},
		{
			name:      "leading agent pair still selects",
			args:      []string{"-a", "research", "hello"},
			wantText:  []string{"hello"},
			wantAgent: "research",
		},
		{
			name:     "a help word inside the message is message",
			args:     []string{"hello", "-h"},
			wantText: []string{"hello", "-h"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest, err := parseSendFlags(tt.args)
			if err != nil {
				t.Fatalf("parseSendFlags(%v): %v", tt.args, err)
			}
			if !slices.Equal(rest, tt.wantText) {
				t.Errorf("rest = %v, want %v", rest, tt.wantText)
			}
			if flags.ifWarm != tt.wantIfWarm {
				t.Errorf("ifWarm = %q, want %q", flags.ifWarm, tt.wantIfWarm)
			}
			if flags.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", flags.agent, tt.wantAgent)
			}
		})
	}

	// The three boundary refusals, decided before anything is sent.
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{args: []string{"--addr", "h:p", "hello"}, wantErr: "--addr must come before the send command word"},
		{args: []string{"-a", "clutch", "--socket"}, wantErr: "--socket must come before the send command word"},
		{args: []string{"--if-warm"}, wantErr: "flag --if-warm needs a value"},
		{args: []string{"-a", "clutch", "-s"}, wantErr: "flag -s needs a value"},
		{args: []string{"-mt"}, wantErr: "flag -mt needs a value"},
		{args: []string{"-a", "a", "-a", "b", "hello"}, wantErr: "only one -a/--agent may precede the text"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			_, _, err := parseSendFlags(tt.args)
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("parseSendFlags(%v) err = %v, want %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestSendFreeTextBody proves the placement rule at the wire level: a
// gate pair typed after the message starts reaches the gateway as message
// text and never as a body flag, while the same pair before the text
// still gates the send.
func TestSendFreeTextBody(t *testing.T) {
	t.Setenv("FOCI_IF_WARM", "")
	body := captureBody(t, func(base string) error {
		return cmdSend(base, []string{"-a", "clutch", "hello", "--if-warm", "8h"})
	})
	if body["text"] != "hello --if-warm 8h" {
		t.Errorf("text = %v, want the trailing pair inside the message", body["text"])
	}
	if _, ok := body["if_active"]; ok {
		t.Errorf("body carries if_active = %v — a gate after the text is message, not a gate", body["if_active"])
	}

	body = captureBody(t, func(base string) error {
		return cmdSend(base, []string{"-a", "clutch", "--if-warm", "8h", "hello"})
	})
	if body["text"] != "hello" {
		t.Errorf("text = %v, want hello only", body["text"])
	}
	if body["if_active"] != "8h" {
		t.Errorf("if_active = %v, want 8h — the leading gate must keep its meaning", body["if_active"])
	}
}

// TestBranchFreeTextBody is TestSendFreeTextBody's branch side: a switch
// typed after the branch text starts is text; before it, it is the flag.
func TestBranchFreeTextBody(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdBranch(base, []string{"-a", "clutch", "morning check", "--no-gate"})
	})
	if body["text"] != "morning check --no-gate" {
		t.Errorf("text = %v, want the trailing switch inside the message", body["text"])
	}
	if v, ok := body["wait_none"]; ok {
		t.Errorf("body carries wait_none = %v — a switch after the text is message, not a flag", v)
	}

	body = captureBody(t, func(base string) error {
		return cmdBranch(base, []string{"-a", "clutch", "--no-gate", "morning check"})
	})
	if body["text"] != "morning check" {
		t.Errorf("text = %v, want morning check only", body["text"])
	}
	if body["wait_none"] != true {
		t.Errorf("wait_none = %v, want true — the leading switch must keep its meaning", body["wait_none"])
	}

	// The boundary refusals carry to branch's line as well.
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "connection flag before the text", args: []string{"--addr", "h:p", "check"}, wantErr: "--addr must come before the branch command word"},
		{name: "dangling gate", args: []string{"--if-warm"}, wantErr: "flag --if-warm needs a value"},
		{name: "second agent pair", args: []string{"-a", "a", "-a", "b", "check"}, wantErr: "only one -a/--agent may precede the text"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			err := cmdBranch(srv.URL, tt.args)
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("cmdBranch(%v) err = %v, want %q", tt.args, err, tt.wantErr)
			}
			if n := len(gw.requests()) - before; n != 0 {
				t.Errorf("%d request(s) reached the gateway, want none", n)
			}
		})
	}
}

// TestEvalFreeText proves `foci eval`'s shell command is free text under
// the one placement rule: a flag-looking word inside it (the reviewer's
// repro: `foci eval grep -a foo file.txt`, which selected agent foo and
// dropped the words on the whole-line scan) is command text verbatim and
// selects nothing, while the flags before it keep their meaning.
func TestEvalFreeText(t *testing.T) {
	t.Setenv("FOCI_AGENT", "")
	body := captureBody(t, func(base string) error {
		return cmdEval(base, []string{"grep", "-a", "foo", "file.txt"})
	})
	if _, ok := body["agent"]; ok {
		t.Errorf("body carries agent = %v — an -a inside the command text must not select", body["agent"])
	}
	if !strings.Contains(fmt.Sprint(body["text"]), "grep -a foo file.txt") {
		t.Errorf("text = %v, want the command words verbatim", body["text"])
	}

	body = captureBody(t, func(base string) error {
		return cmdEval(base, []string{"-a", "research", "ls", "-la"})
	})
	if body["agent"] != "research" {
		t.Errorf("agent = %v, want research — the leading pair still selects", body["agent"])
	}
	// Exact text: the leading "-a research" pair selects the agent and is
	// not part of the shell command.
	if want := "Run this command and show the output:\n```\nls -la\n```"; body["text"] != want {
		t.Errorf("text = %q, want %q — the leading agent pair leaked into the command?", body["text"], want)
	}

	// The boundary refusals, and help before the text.
	gw := &recordingGateway{}
	srv := httptest.NewServer(http.HandlerFunc(gw.handle))
	defer srv.Close()
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "connection flag before the text", args: []string{"--addr", "h:p", "ls"}, wantErr: "--addr must come before the eval command word"},
		{name: "dangling -a", args: []string{"-a"}, wantErr: "flag -a needs a value"},
		{name: "second agent pair", args: []string{"-a", "a", "-a", "b", "ls"}, wantErr: "only one -a/--agent may precede the text"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(gw.requests())
			err := cmdEval(srv.URL, tt.args)
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("cmdEval(%v) err = %v, want %q", tt.args, err, tt.wantErr)
			}
			if n := len(gw.requests()) - before; n != 0 {
				t.Errorf("%d request(s) reached the gateway, want none", n)
			}
		})
	}
	stderr := captureStderr(t, func() {
		if err := cmdEval(srv.URL, []string{"-h"}); err != nil {
			t.Errorf("eval -h returned error: %v", err)
		}
	})
	if !strings.Contains(stderr, "Usage: foci eval") {
		t.Errorf("eval -h stderr = %q, want the usage", stderr)
	}
}

// TestParseBranchFlagsWait proves `foci branch` parses the wait gates with
// the same names, aliases and env vars as `foci send`, and never leaves one
// in the message text.
func TestParseBranchFlagsWait(t *testing.T) {
	flags, rest, err := parseBranchFlags([]string{
		"--wait-cold", "55m", "--wait-active", "30s", "--deadline", "3h", "--no-gate", "morning", "check",
	})
	if err != nil {
		t.Fatalf("parseBranchFlags: %v", err)
	}
	if flags.waitCold != "55m" {
		t.Errorf("waitCold = %q, want 55m", flags.waitCold)
	}
	if flags.waitWarm != "30s" { // --wait-active is the alias for --wait-warm
		t.Errorf("waitWarm = %q, want 30s", flags.waitWarm)
	}
	if flags.waitTimeout != "3h" { // --deadline is the alias for --wait-timeout
		t.Errorf("waitTimeout = %q, want 3h", flags.waitTimeout)
	}
	if !flags.noGate {
		t.Error("noGate = false, want true")
	}
	if len(rest) != 2 || rest[0] != "morning" || rest[1] != "check" {
		t.Errorf("rest = %v, want the message words only", rest)
	}
}

// TestParseBranchFlagsWaitEnv proves the FOCI_WAIT_* env defaults flow through
// parseBranchFlags exactly as they do for send.
func TestParseBranchFlagsWaitEnv(t *testing.T) {
	t.Setenv("FOCI_WAIT_COLD", "55m")
	t.Setenv("FOCI_WAIT_TIMEOUT", "4h")
	t.Setenv("FOCI_NO_GATE", "1")
	flags, _, err := parseBranchFlags([]string{"hi"})
	if err != nil {
		t.Fatalf("parseBranchFlags: %v", err)
	}
	if flags.waitCold != "55m" || flags.waitTimeout != "4h" || !flags.noGate {
		t.Errorf("env defaults not applied: %+v", flags.waitFlags)
	}

	// An explicit flag wins over the env var.
	flags, _, err = parseBranchFlags([]string{"--wait-cold", "10m", "hi"})
	if err != nil {
		t.Fatalf("parseBranchFlags: %v", err)
	}
	if flags.waitCold != "10m" {
		t.Errorf("waitCold = %q, want the flag value 10m", flags.waitCold)
	}
}

// TestParseCommandFlagsWait proves the command family's disposition of the
// one scan (the ping/pair shorthands) parses the wait gates with the same
// names, aliases and env vars as `foci send`, and never leaves one in the
// command words.
func TestParseCommandFlagsWait(t *testing.T) {
	var flags commandFlags
	lead, err := scanCommandLine([]string{
		"--wait-cold", "55m", "--wait-user-inactive", "2h", "--deadline", "3h", "reset",
	}, &flags, commandLead, false)
	if err != nil {
		t.Fatalf("scanCommandLine: %v", err)
	}
	flags.applyEnvDefaults()
	if flags.waitCold != "55m" || flags.waitUserInactive != "2h" || flags.waitTimeout != "3h" {
		t.Errorf("wait flags not parsed: %+v", flags.waitFlags)
	}
	if len(lead.rest) != 1 || lead.rest[0] != "reset" {
		t.Errorf("rest = %v, want the command word only", lead.rest)
	}
}

// TestParseCommandFlagsWaitEnv proves the FOCI_WAIT_* env defaults flow
// through the command family's scan.
func TestParseCommandFlagsWaitEnv(t *testing.T) {
	t.Setenv("FOCI_WAIT_COLD", "55m")
	t.Setenv("FOCI_NO_GATE", "1")
	var flags commandFlags
	lead, err := scanCommandLine([]string{"/reset"}, &flags, commandLead, false)
	if err != nil {
		t.Fatalf("scanCommandLine: %v", err)
	}
	flags.applyEnvDefaults()
	if flags.waitCold != "55m" || !flags.noGate {
		t.Errorf("env defaults not applied: %+v", flags.waitFlags)
	}
	if len(lead.rest) != 1 || lead.rest[0] != "/reset" {
		t.Errorf("rest = %v, want [/reset]", lead.rest)
	}
}

// TestParseCommandLineEnvDefaults pins the same env defaults on the
// free-text disposition `foci command` itself uses: FOCI_WAIT_* and
// FOCI_HUMAN flow into the flags while the slash text stays untouched —
// the in-process twin of the black-box command tests, which per-test
// coverage cannot see.
func TestParseCommandLineEnvDefaults(t *testing.T) {
	t.Setenv("FOCI_WAIT_COLD", "55m")
	t.Setenv("FOCI_NO_GATE", "1")
	t.Setenv("FOCI_HUMAN", "1")
	var flags commandFlags
	lead, err := scanCommandLine([]string{"/reset"}, &flags, commandLead, true)
	if err != nil {
		t.Fatalf("scanCommandLine: %v", err)
	}
	flags.applyEnvDefaults()
	if flags.waitCold != "55m" || !flags.noGate || !flags.humanFlag.human {
		t.Errorf("env defaults not applied: %+v", flags)
	}
	if len(lead.rest) != 1 || lead.rest[0] != "/reset" {
		t.Errorf("text = %v, want [/reset]", lead.rest)
	}
}

// captureBody runs fn against a throwaway capture server and returns the
// decoded JSON body of the single request it posted.
func captureBody(t *testing.T, fn func(base string) error) map[string]interface{} {
	t.Helper()
	ch := make(chan map[string]interface{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ch <- body
		_, _ = w.Write([]byte(`{"response":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	if err := fn(srv.URL); err != nil {
		t.Fatalf("command failed: %v", err)
	}
	return <-ch
}

// TestBranchWaitFlagsForwarded proves `foci branch` puts the wait gates on
// the request body as the same wire keys send uses, and that no wait flag
// leaks into the message text.
func TestBranchWaitFlagsForwarded(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdBranch(base, []string{"--wait-cold", "55m", "--deadline", "3h", "--no-gate", "morning check"})
	})
	if body["text"] != "morning check" {
		t.Errorf("text = %v, want the message only (a wait flag leaked?)", body["text"])
	}
	if body["wait_cold"] != "55m" {
		t.Errorf("wait_cold = %v, want 55m", body["wait_cold"])
	}
	if body["wait_timeout"] != "3h" {
		t.Errorf("wait_timeout = %v, want 3h (from --deadline)", body["wait_timeout"])
	}
	if body["wait_none"] != true {
		t.Errorf("wait_none = %v, want true (from --no-gate)", body["wait_none"])
	}
}

// TestCommandWaitFlagsForwarded proves `foci command` puts the wait gates on
// the request body and none leak into the command string
// (`foci command --wait-cold 55m reset` sends command text for reset only).
func TestCommandWaitFlagsForwarded(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdCommand(base, []string{"--wait-cold", "55m", "--wait-user-inactive", "2h", "reset"})
	})
	if body["command"] != "/reset" {
		t.Errorf("command = %v, want /reset only (a wait flag leaked?)", body["command"])
	}
	if body["wait_cold"] != "55m" {
		t.Errorf("wait_cold = %v, want 55m", body["wait_cold"])
	}
	if body["wait_user_inactive"] != "2h" {
		t.Errorf("wait_user_inactive = %v, want 2h", body["wait_user_inactive"])
	}
}

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns what fn
// wrote. The usage functions print to stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestBranchAndCommandHelpListWaitFlags pins req 8's help contract: branch
// and command help list the wait flags with send's descriptions, including
// the persistence/restart/always-async/2h facts.
func TestBranchAndCommandHelpListWaitFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"branch", branchUsage},
		{"command", commandUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			help := captureStderr(t, tc.fn)
			for _, want := range []string{
				"--wait-warm", "--wait-cold", "--wait-user-active", "--wait-user-inactive",
				"--wait-timeout", "--no-gate", "2h", "survive a gateway restart", "always async",
			} {
				if !strings.Contains(help, want) {
					t.Errorf("%s help missing %q", tc.name, want)
				}
			}
		})
	}

	// The --sync/--wait clarification (send and branch only — command has no
	// sync): it waits for the reply and is NOT a --wait-* gate.
	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"send", sendUsage},
		{"branch", branchUsage},
	} {
		t.Run(tc.name+"/sync", func(t *testing.T) {
			help := captureStderr(t, tc.fn)
			if !strings.Contains(help, "NOT") || !strings.Contains(help, "NOT one of the\n--wait-* gates") && !strings.Contains(help, "NOT a --wait-* gate") {
				t.Errorf("%s help: --sync/--wait line must say it is not a --wait-* gate", tc.name)
			}
			if !strings.Contains(help, "even with --sync") {
				t.Errorf("%s help: gate section must say an unmet --wait-* gate defers immediately even with --sync", tc.name)
			}
		})
	}
}

// TestParseAPIKeyFlag tests --api-key flag parsing in various formats,
// through the shared global-flag scanner (parseGlobalFlags) every command
// uses.
func TestParseAPIKeyFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantKey  string
		wantRest []string
	}{
		{
			name:     "no flag",
			args:     []string{"send", "hello"},
			wantKey:  "",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "--api-key with value",
			args:     []string{"--api-key", "my-secret", "send", "hello"},
			wantKey:  "my-secret",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "--api-key=value",
			args:     []string{"--api-key=my-secret", "send", "hello"},
			wantKey:  "my-secret",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "flag in middle",
			args:     []string{"send", "--api-key", "key123", "hello"},
			wantKey:  "key123",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "--api-key without value at end",
			args:     []string{"send", "--api-key"},
			wantKey:  "",
			wantRest: []string{"send", "--api-key"},
		},
		{
			name:     "empty args",
			args:     []string{},
			wantKey:  "",
			wantRest: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globals, rest := parseGlobalFlags(tt.args)
			if globals.apiKey != tt.wantKey {
				t.Errorf("apiKey = %q, want %q", globals.apiKey, tt.wantKey)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

// TestParseAgentFlag tests -a and --agent flag parsing in various forms.
func TestParseAgentFlag(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAgent string
		wantRest  []string
	}{
		{
			name:      "no flag",
			args:      []string{"hello", "world"},
			wantAgent: "",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "-a with value",
			args:      []string{"-a", "research", "hello"},
			wantAgent: "research",
			wantRest:  []string{"hello"},
		},
		{
			name:      "--agent with value",
			args:      []string{"--agent", "main", "hello"},
			wantAgent: "main",
			wantRest:  []string{"hello"},
		},
		{
			name:      "-a=value",
			args:      []string{"-a=scout", "hello"},
			wantAgent: "scout",
			wantRest:  []string{"hello"},
		},
		{
			name:      "--agent=value",
			args:      []string{"--agent=scout", "hello"},
			wantAgent: "scout",
			wantRest:  []string{"hello"},
		},
		{
			name:      "flag after positional args",
			args:      []string{"hello", "world", "-a", "research"},
			wantAgent: "research",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "flag in middle",
			args:      []string{"hello", "--agent", "research", "world"},
			wantAgent: "research",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "empty args",
			args:      []string{},
			wantAgent: "",
			wantRest:  []string{},
		},
		{
			name:      "-a without value at end",
			args:      []string{"hello", "-a"},
			wantAgent: "",
			wantRest:  []string{"hello", "-a"},
		},
		{
			name:      "only -a and value",
			args:      []string{"-a", "research"},
			wantAgent: "research",
			wantRest:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, rest := parseAgentFlag(tt.args)
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tt.wantAgent)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return // both empty, ok
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

// TestSplitGlobalFlags pins where the global --addr/--api-key/--socket scan
// stops: at the command word, the first argument that is not one of them. A
// command's free-text argument — a todo close reason of "--api-key sekret"
// or "--addr=x" — must reach the command untouched, while the documented
// form (global flags before the command) still parses, and anything unknown
// is left for the command dispatch to refuse as before.
func TestSplitGlobalFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantFlags []string
		wantRest  []string
	}{
		{
			name:      "no global flags",
			args:      []string{"send", "hello"},
			wantFlags: nil,
			wantRest:  []string{"send", "hello"},
		},
		{
			name:      "global flags before the command",
			args:      []string{"--socket", "/x.sock", "--api-key=k", "--addr", "a:1", "status"},
			wantFlags: []string{"--socket", "/x.sock", "--api-key=k", "--addr", "a:1"},
			wantRest:  []string{"status"},
		},
		{
			name:      "flag look-alikes after the command word are not global",
			args:      []string{"todo", "done", "-a", "a", "7", "--api-key", "sekret"},
			wantFlags: nil,
			wantRest:  []string{"todo", "done", "-a", "a", "7", "--api-key", "sekret"},
		},
		{
			name:      "=attached look-alike after the command word is not global",
			args:      []string{"todo", "done", "7", "--addr=nowhere:9"},
			wantFlags: nil,
			wantRest:  []string{"todo", "done", "7", "--addr=nowhere:9"},
		},
		{
			name:      "unknown -argument ends the global section",
			args:      []string{"--bogus", "send", "hi"},
			wantFlags: nil,
			wantRest:  []string{"--bogus", "send", "hi"},
		},
		{
			name:      "trailing --flag without a value is left for the dispatch",
			args:      []string{"--addr"},
			wantFlags: nil,
			wantRest:  []string{"--addr"},
		},
		{
			name:      "no arguments",
			args:      nil,
			wantFlags: nil,
			wantRest:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := splitGlobalFlags(tt.args)
			if !slices.Equal(flags, tt.wantFlags) {
				t.Errorf("splitGlobalFlags(%v) flags = %v, want %v", tt.args, flags, tt.wantFlags)
			}
			if !slices.Equal(rest, tt.wantRest) {
				t.Errorf("splitGlobalFlags(%v) rest = %v, want %v", tt.args, rest, tt.wantRest)
			}
		})
	}
}

// TestParseGlobalFlags pins the whole-line contract every command but
// `todo` relies on (the F1 side that is base behaviour): the three
// transport flags are extracted — and removed — from anywhere on the
// line, in both spellings, and everything else is left for the command.
// TestMainGlobalFlagScope drives the same rule through the real main().
func TestParseGlobalFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantAddr string
		wantKey  string
		wantSock string
		wantRest []string
	}{
		{
			name:     "flags before the command word",
			args:     []string{"--addr", "a:1", "--api-key", "k", "--socket", "/s", "send", "hi"},
			wantAddr: "a:1",
			wantKey:  "k",
			wantSock: "/s",
			wantRest: []string{"send", "hi"},
		},
		{
			name:     "two-token flag after the command word",
			args:     []string{"send", "-a", "x", "--addr", "a:1", "hi"},
			wantAddr: "a:1",
			wantRest: []string{"send", "-a", "x", "hi"},
		},
		{
			name:     "attached flags after the command word",
			args:     []string{"status", "--addr=a:1", "--api-key=k", "--socket=/s"},
			wantAddr: "a:1",
			wantKey:  "k",
			wantSock: "/s",
			wantRest: []string{"status"},
		},
		{
			name:     "no flags",
			args:     []string{"send", "hi"},
			wantRest: []string{"send", "hi"},
		},
		{
			name:     "trailing flag without a value stays for the command",
			args:     []string{"send", "--addr"},
			wantRest: []string{"send", "--addr"},
		},
		{
			name:     "only the exact flag names match",
			args:     []string{"send", "--addrs", "a:1", "hi"},
			wantRest: []string{"send", "--addrs", "a:1", "hi"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globals, rest := parseGlobalFlags(tt.args)
			if globals.addr != tt.wantAddr || globals.apiKey != tt.wantKey || globals.socket != tt.wantSock {
				t.Errorf("parseGlobalFlags(%v) = {%q, %q, %q}, want {%q, %q, %q}",
					tt.args, globals.addr, globals.apiKey, globals.socket, tt.wantAddr, tt.wantKey, tt.wantSock)
			}
			if !slices.Equal(rest, tt.wantRest) {
				t.Errorf("parseGlobalFlags(%v) rest = %v, want %v", tt.args, rest, tt.wantRest)
			}
		})
	}
}

// TestFlagMatch pins the one spelling authority every flag scan delegates
// to: exact token, =attached value, and non-matches (including a longer
// flag that merely starts like the named one).
func TestFlagMatch(t *testing.T) {
	for _, tt := range []struct {
		tok        string
		name       string
		wantValue  string
		wantAttach bool
		wantOK     bool
	}{
		{tok: "--addr", name: "--addr", wantOK: true},
		{tok: "--addr=a:1", name: "--addr", wantValue: "a:1", wantAttach: true, wantOK: true},
		{tok: "-a", name: "-a", wantOK: true},
		{tok: "-a=x", name: "-a", wantValue: "x", wantAttach: true, wantOK: true},
		{tok: "--addrs", name: "--addr", wantOK: false},
		{tok: "--agent", name: "-a", wantOK: false},
		{tok: "addr", name: "--addr", wantOK: false},
		{tok: "--addr", name: "--api-key", wantOK: false},
	} {
		value, attached, ok := flagMatch(tt.tok, tt.name)
		if value != tt.wantValue || attached != tt.wantAttach || ok != tt.wantOK {
			t.Errorf("flagMatch(%q, %q) = {%q, %v, %v}, want {%q, %v, %v}",
				tt.tok, tt.name, value, attached, ok, tt.wantValue, tt.wantAttach, tt.wantOK)
		}
	}
}

// TestSendAndBranchMessageSourceEnvDefaults proves FOCI_MESSAGE_TEXT and
// FOCI_MESSAGE_FILE are the env fallbacks of -mt and -mf for both send and
// branch: with no message words on the line, the env value is the text.
func TestSendAndBranchMessageSourceEnvDefaults(t *testing.T) {
	file := t.TempDir() + "/msg.txt"
	if err := os.WriteFile(file, []byte("from the file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []struct {
		name string
		run  func(base string, args []string) error
	}{
		{"send", cmdSend},
		{"branch", cmdBranch},
	} {
		for _, tc := range []struct {
			env, value, want string
		}{
			{"FOCI_MESSAGE_TEXT", "from the env", "from the env"},
			{"FOCI_MESSAGE_FILE", file, "from the file"},
		} {
			t.Run(cmd.name+"/"+tc.env, func(t *testing.T) {
				t.Setenv("FOCI_MESSAGE_TEXT", "")
				t.Setenv("FOCI_MESSAGE_FILE", "")
				t.Setenv(tc.env, tc.value)
				body := captureBody(t, func(base string) error { return cmd.run(base, nil) })
				if body["text"] != tc.want {
					t.Errorf("text = %v, want %q from %s", body["text"], tc.want, tc.env)
				}
			})
		}
	}
}

// TestSendUsageFlagRowsAreParsed proves every row of `foci send -h`'s
// Flags block is a flag row the parser accepts in the leading position
// (or the indented continuation of one): the help cannot list a flag the
// parser lacks, and nothing but flag rows can sit in the block.
func TestSendUsageFlagRowsAreParsed(t *testing.T) {
	help := captureStderr(t, sendUsage)
	_, block, ok := strings.Cut(help, "\nFlags:\n")
	if !ok {
		t.Fatalf("send usage has no Flags block:\n%s", help)
	}
	block, _, _ = strings.Cut(block, "\n\n")
	rows := 0
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, strings.Repeat(" ", 28)) && strings.TrimSpace(line) != "" {
			continue // continuation of the row above
		}
		if !strings.HasPrefix(line, "  -") {
			t.Errorf("Flags row %q is not a flag row (\"  -flag  description\") or a continuation", line)
			continue
		}
		rows++
		flag := strings.TrimSuffix(strings.Fields(line)[0], ",")
		_, rest, err := parseSendFlags([]string{flag, "1m", "msg"})
		if err != nil {
			t.Errorf("help lists %s, but the parser refuses it: %v", flag, err)
			continue
		}
		if len(rest) == 0 || rest[0] == flag || rest[len(rest)-1] != "msg" {
			t.Errorf("help lists %s, but the parser does not take it as a flag (rest = %q)", flag, rest)
		}
	}
	if rows < 20 {
		t.Errorf("found %d flag rows, want the full list (block parse broken?):\n%s", rows, block)
	}
}
