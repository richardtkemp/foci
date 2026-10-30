package main

import (
	"slices"
	"strings"
	"testing"
)

// TestParseCommandFlagsSessionConsumed is the #2296 red test at the parser
// level: the command family's scan must consume -s/--session in all four
// forms, leaving only the command words in rest. On the base the flag
// tokens fall into the command string.
func TestParseCommandFlagsSessionConsumed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"-s value", []string{"-s", "Fabro", "/ping"}},
		{"-s=value", []string{"-s=Fabro", "/ping"}},
		{"--session value", []string{"--session", "Fabro", "/ping"}},
		{"--session=value", []string{"--session=Fabro", "/ping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var flags commandFlags
			lead, err := scanCommandLine(tc.args, &flags, commandLead, false)
			if err != nil {
				t.Fatalf("#2296: scanCommandLine(%v): %v", tc.args, err)
			}
			if flags.sessionFlag.session != "Fabro" {
				t.Errorf("#2296: parsed selector = %q, want Fabro", flags.sessionFlag.session)
			}
			if len(lead.rest) != 1 || lead.rest[0] != "/ping" {
				t.Errorf("#2296: scanCommandLine(%v) rest = %v, want only the command word [/ping] — the session flag leaked into the command string", tc.args, lead.rest)
			}
		})
	}

	// A valueless -s at the end of the args stays in the command words,
	// exactly as `foci send` treats it (no next token to consume) — the
	// base expectation, held by the ping/pair disposition of the one scan.
	// The needs-a-value error belongs to the free-text disposition only,
	// where the word would START the dispatched text (pinned by
	// TestParseCommandLineSessionForms and TestScanCommandLineValuelessFlagErrors).
	lead, err := scanCommandLine([]string{"/ping", "-s"}, &commandFlags{}, commandLead, false)
	if err != nil || !slices.Equal(lead.rest, []string{"/ping", "-s"}) {
		t.Errorf("rest = %v err = %v, want [/ping -s] — a bare valueless flag is command text", lead.rest, err)
	}
}

// TestParseCommandLineSessionForms is the bounded-scan twin of
// TestParseCommandFlagsSessionConsumed: `foci command`'s own scan
// consumes -s/--session in all four forms before the slash text starts,
// and a valueless trailing -s inside the text is text, not a flag.
func TestParseCommandLineSessionForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"-s value", []string{"-s", "Fabro", "/ping"}},
		{"-s=value", []string{"-s=Fabro", "/ping"}},
		{"--session value", []string{"--session", "Fabro", "/ping"}},
		{"--session=value", []string{"--session=Fabro", "/ping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var flags commandFlags
			lead, err := scanCommandLine(tc.args, &flags, commandLead, true)
			if err != nil {
				t.Fatalf("scanCommandLine(%v, true): %v", tc.args, err)
			}
			if flags.sessionFlag.session != "Fabro" {
				t.Errorf("parsed selector = %q, want Fabro", flags.sessionFlag.session)
			}
			if len(lead.rest) != 1 || lead.rest[0] != "/ping" {
				t.Errorf("text = %v, want only the command word [/ping]", lead.rest)
			}
		})
	}

	if lead, err := scanCommandLine([]string{"/ping", "-s"}, &commandFlags{}, commandLead, true); err != nil || !slices.Equal(lead.rest, []string{"/ping", "-s"}) {
		t.Errorf("text = %v err = %v, want [/ping -s] and no error — a bare valueless word inside the text is command text", lead.rest, err)
	}
}

// TestCommandSessionFlagForwarded is the #2296 red test at the body level:
// `foci command -s <sel> /ping` must put the selector on the /command body
// as "session" and keep the command word as the command, in all four flag
// forms.
func TestCommandSessionFlagForwarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"-s value", []string{"-a", "clutch", "-s", "Fabro", "/ping"}},
		{"-s=value", []string{"-a", "clutch", "-s=Fabro", "/ping"}},
		{"--session value", []string{"-a", "clutch", "--session", "Fabro", "/ping"}},
		{"--session=value", []string{"-a", "clutch", "--session=Fabro", "/ping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := captureBody(t, func(base string) error {
				return cmdCommand(base, tc.args)
			})
			if body["session"] != "Fabro" {
				t.Errorf("#2296: body session = %v, want Fabro — the selector never reached the gateway", body["session"])
			}
			if body["command"] != "/ping" {
				t.Errorf("#2296: body command = %v, want /ping only (the session flag leaked?)", body["command"])
			}
		})
	}
}

// TestCommandUsageDocumentsSession is the #2296 doc red test: `foci command
// -h` must document -s/--session in the usage line and the flags table, say
// which session the activity gates evaluate on, and state the deliberate
// carve-out that command does NOT read FOCI_SESSION.
func TestCommandUsageDocumentsSession(t *testing.T) {
	// Whitespace-normalised: the help text wraps at 78 columns, so phrases
	// must match across line breaks.
	help := strings.Join(strings.Fields(captureStderr(t, commandUsage)), " ")
	for _, want := range []string{
		"[-s session]",  // the usage line shows the flag
		"-s, --session", // the flags table documents it
		"-s names it; otherwise the agent's default session", // the gates paragraph says which session they evaluate on
		"does not read FOCI_SESSION",                         // the env carve-out is stated
	} {
		if !strings.Contains(help, want) {
			t.Errorf("#2296: commandUsage output does not mention %q", want)
		}
	}
}

// TestCommandSessionFlagNoEnvDefault characterises the deliberate env
// carve-out (#2296 ruling): unlike `foci send`, `foci command` never applies
// the FOCI_SESSION default — with the env var set and no -s, the body
// carries no session key, so a cron or script that happens to export it
// keeps dispatching on the agent's default session.
func TestCommandSessionFlagNoEnvDefault(t *testing.T) {
	t.Setenv("FOCI_SESSION", "research")
	body := captureBody(t, func(base string) error {
		return cmdCommand(base, []string{"-a", "clutch", "/reset"})
	})
	if _, ok := body["session"]; ok {
		t.Errorf("body carries session = %v from FOCI_SESSION — command must not apply the env default", body["session"])
	}
}

// TestCommandUsageErrorMentionsSession pins the usage error cmdCommand returns
// when the command word is missing: it must offer -s/--session exactly like
// the -h text, so a user sent to the error line still discovers the flag.
// cmdCommand errors before any HTTP call, so the base URL is never used.
func TestCommandUsageErrorMentionsSession(t *testing.T) {
	err := cmdCommand("http://unused.invalid", []string{"-a", "clutch", "--wait-cold", "55m"})
	if err == nil {
		t.Fatal("#2296: no command word returned nil, want the usage error")
	}
	if !strings.Contains(err.Error(), "[-s session]") {
		t.Errorf("#2296: usage error = %q, want it to document [-s session]", err.Error())
	}
}
