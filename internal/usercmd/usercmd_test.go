package usercmd

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text, script string
		ok           bool
	}{
		{"!ls /", "ls /", true},
		{"  !ls", "ls", true},
		{"\n\t!ls", "ls", true},
		{"!echo a\necho b", "echo a\necho b", true},
		{"!!", "!", true},
		{"! ls", "", false},
		{"!\tls", "", false},
		{"!\nls", "", false},
		{"!", "", false},
		{"  !  ", "", false},
		{"", "", false},
		{"ls !x", "", false},
		{"/stop", "", false},
	} {
		script, ok := Parse(tc.text)
		if script != tc.script || ok != tc.ok {
			t.Errorf("Parse(%q) = %q, %v; want %q, %v", tc.text, script, ok, tc.script, tc.ok)
		}
	}
}

func TestAPITimeout(t *testing.T) {
	t.Parallel()
	if got := APITimeout(2); got != 2*time.Second {
		t.Errorf("configured = %s", got)
	}
	if got := APITimeout(0); got != 30*time.Second {
		t.Errorf("built-in = %s", got)
	}
}

func TestResultTexts(t *testing.T) {
	t.Parallel()
	r := Result{Command: "echo hi", State: StateExited, ExitCode: 3, Output: "hi\n", ForAgent: "hi\n"}
	if got := r.ForAgentText(); !strings.Contains(got, "$ echo hi") || !strings.Contains(got, "[exit status 3]") || !strings.HasSuffix(got, "hi\n") {
		t.Errorf("ForAgentText = %q", got)
	}
	if got := r.DisplayText(); got != "$ echo hi\nhi\n[exit status 3]" {
		t.Errorf("DisplayText = %q", got)
	}

	big := Result{Command: "seq", State: StateExited, Output: strings.Repeat("y", 5000), File: "/data/out.txt", Size: 123456}
	reply := big.ReplyText()
	if !strings.Contains(reply, "/data/out.txt") || !strings.Contains(reply, "123456") {
		t.Errorf("over-limit reply lost the path or size: %q", reply[len(reply)-120:])
	}
	if len(reply) >= 2000 {
		t.Errorf("over-limit reply is %d chars, must fit Discord's 2000", len(reply))
	}
	if got := (Result{Command: "c", State: StateTimedOut, Timeout: 2 * time.Second}).Status(); got != "timed out after 2s" {
		t.Errorf("Status = %q", got)
	}
}
