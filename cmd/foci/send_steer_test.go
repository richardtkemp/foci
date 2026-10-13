package main

import (
	"strings"
	"testing"
)

// TestSendSteerFlagForwarded proves `foci send --steer hi` puts the delivery
// choice on the request body and never leaks the flag into the message text.
func TestSendSteerFlagForwarded(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdSend(base, []string{"--steer", "redirect this"})
	})
	if body["steer"] != "steer" {
		t.Errorf("steer = %v, want \"steer\"", body["steer"])
	}
	if body["text"] != "redirect this" {
		t.Errorf("text = %v, want %q (the flag leaked into the message?)", body["text"], "redirect this")
	}
}

// TestSendQueueFlagForwarded is the --queue half: the wire value is "queue"
// and the message text stays clean.
func TestSendQueueFlagForwarded(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdSend(base, []string{"--queue", "after this turn"})
	})
	if body["steer"] != "queue" {
		t.Errorf("steer = %v, want \"queue\"", body["steer"])
	}
	if body["text"] != "after this turn" {
		t.Errorf("text = %v, want %q", body["text"], "after this turn")
	}
}

// TestSendSteerAbsentOmittedFromBody pins the wire shape: without --steer or
// --queue (and no FOCI_STEER) the request body carries NO steer key at all —
// absent means the agent's config default, byte-for-byte as before #1318.
func TestSendSteerAbsentOmittedFromBody(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdSend(base, []string{"hi"})
	})
	if _, ok := body["steer"]; ok {
		t.Errorf("body has steer = %v; the key must be absent without a choice", body["steer"])
	}
}

// TestSendSteerEnvVarSetsChoice proves FOCI_STEER ("steer"/"queue") flows
// through exactly like the flag, and that any other value is ignored — no
// choice, not an error.
func TestSendSteerEnvVarSetsChoice(t *testing.T) {
	t.Run("queue", func(t *testing.T) {
		t.Setenv("FOCI_STEER", "queue")
		body := captureBody(t, func(base string) error {
			return cmdSend(base, []string{"hi"})
		})
		if body["steer"] != "queue" {
			t.Errorf("steer = %v, want queue (FOCI_STEER not applied?)", body["steer"])
		}
	})
	t.Run("steer", func(t *testing.T) {
		t.Setenv("FOCI_STEER", "steer")
		body := captureBody(t, func(base string) error {
			return cmdSend(base, []string{"hi"})
		})
		if body["steer"] != "steer" {
			t.Errorf("steer = %v, want steer (FOCI_STEER not applied?)", body["steer"])
		}
	})
	t.Run("other values ignored", func(t *testing.T) {
		t.Setenv("FOCI_STEER", "bogus")
		body := captureBody(t, func(base string) error {
			return cmdSend(base, []string{"hi"})
		})
		if _, ok := body["steer"]; ok {
			t.Errorf("body has steer = %v; an unknown FOCI_STEER must mean no choice", body["steer"])
		}
	})
}

// TestSendSteerAndQueueMutuallyExclusive proves giving both modes — two
// flags, or a flag plus the other switch's env var — is a usage error
// returned before any request is made.
func TestSendSteerAndQueueMutuallyExclusive(t *testing.T) {
	t.Run("both flags", func(t *testing.T) {
		err := cmdSend("http://unused.invalid", []string{"--steer", "--queue", "hi"})
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("err = %v, want the mutual-exclusion usage error", err)
		}
	})
	t.Run("flag versus other switch's env", func(t *testing.T) {
		t.Setenv("FOCI_STEER", "steer")
		err := cmdSend("http://unused.invalid", []string{"--queue", "hi"})
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("err = %v, want the mutual-exclusion usage error", err)
		}
	})
}

// TestParseSendFlags_SteerQueue pins the pure parse: the bare switches are
// consumed and never leak into the trailing message words.
func TestParseSendFlags_SteerQueue(t *testing.T) {
	flags, rest := parseSendFlags([]string{"--steer", "hello", "world"})
	if !flags.steerFlag || flags.queueFlag {
		t.Errorf("flags = steer:%v queue:%v, want steer only", flags.steerFlag, flags.queueFlag)
	}
	if strings.Join(rest, " ") != "hello world" {
		t.Errorf("rest = %v, want the trailing message words", rest)
	}

	flags, rest = parseSendFlags([]string{"--queue", "-a", "x", "msg"})
	if flags.steerFlag || !flags.queueFlag {
		t.Errorf("flags = steer:%v queue:%v, want queue only", flags.steerFlag, flags.queueFlag)
	}
	if strings.Join(rest, " ") != "msg" {
		t.Errorf("rest = %v, want the trailing message words", rest)
	}
}

// TestSendUsageMentionsSteerFlags pins the -h contract (#1318 req 5): the
// usage lists both flags with their env var.
func TestSendUsageMentionsSteerFlags(t *testing.T) {
	help := captureStderr(t, sendUsage)
	for _, want := range []string{"--steer", "--queue", "FOCI_STEER"} {
		if !strings.Contains(help, want) {
			t.Errorf("send help missing %q", want)
		}
	}
}
