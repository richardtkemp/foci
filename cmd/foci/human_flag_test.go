package main

import (
	"strings"
	"testing"
)

// TestSendHumanFlagInBody proves `foci send --human hi` puts "human": true on
// the request body and never leaks the flag into the message text.
func TestSendHumanFlagInBody(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdSend(base, []string{"--human", "hi"})
	})
	if body["human"] != true {
		t.Errorf("human = %v, want true", body["human"])
	}
	if body["text"] != "hi" {
		t.Errorf("text = %v, want %q (the flag leaked into the message?)", body["text"], "hi")
	}
}

// TestBranchHumanFlagInBody proves `foci branch --human hi` puts "human":
// true on the request body and never leaks the flag into the branch text.
func TestBranchHumanFlagInBody(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdBranch(base, []string{"--human", "morning check"})
	})
	if body["human"] != true {
		t.Errorf("human = %v, want true", body["human"])
	}
	if body["text"] != "morning check" {
		t.Errorf("text = %v, want %q (the flag leaked into the message?)", body["text"], "morning check")
	}
}

// TestCommandHumanFlagInBody proves `foci command --human ping` puts
// "human": true on the request body and never leaks the flag into the
// command string.
func TestCommandHumanFlagInBody(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdCommand(base, []string{"--human", "ping"})
	})
	if body["human"] != true {
		t.Errorf("human = %v, want true", body["human"])
	}
	if body["command"] != "/ping" {
		t.Errorf("command = %v, want %q (the flag leaked into the command?)", body["command"], "/ping")
	}
}

// TestHumanEnvVarSetsFlag proves FOCI_HUMAN (non-empty = true) flows through
// all three subcommands exactly like the flag.
func TestHumanEnvVarSetsFlag(t *testing.T) {
	cases := []struct {
		name string
		fn   func(base string) error
	}{
		{"send", func(base string) error { return cmdSend(base, []string{"hi"}) }},
		{"branch", func(base string) error { return cmdBranch(base, []string{"hi"}) }},
		{"command", func(base string) error { return cmdCommand(base, []string{"ping"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FOCI_HUMAN", "1")
			body := captureBody(t, tc.fn)
			if body["human"] != true {
				t.Errorf("human = %v, want true (FOCI_HUMAN not applied?)", body["human"])
			}
		})
	}
}

// TestHumanAbsentOmittedFromBody pins the wire shape: without --human or
// FOCI_HUMAN the request body carries NO human key at all — absent must mean
// automated, byte-for-byte as before #1130.
func TestHumanAbsentOmittedFromBody(t *testing.T) {
	cases := []struct {
		name string
		fn   func(base string) error
	}{
		{"send", func(base string) error { return cmdSend(base, []string{"hi"}) }},
		{"branch", func(base string) error { return cmdBranch(base, []string{"hi"}) }},
		{"command", func(base string) error { return cmdCommand(base, []string{"ping"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := captureBody(t, tc.fn)
			if _, ok := body["human"]; ok {
				t.Errorf("body has human = %v; the key must be absent without --human", body["human"])
			}
		})
	}
}

// TestHumanFlagListedInHelp pins the -h contract: each of the three
// subcommands' usage lists --human (with its env var).
func TestHumanFlagListedInHelp(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"send", sendUsage},
		{"branch", branchUsage},
		{"command", commandUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			help := captureStderr(t, tc.fn)
			for _, want := range []string{"--human", "FOCI_HUMAN"} {
				if !strings.Contains(help, want) {
					t.Errorf("%s help missing %q", tc.name, want)
				}
			}
		})
	}
}
