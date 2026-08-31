package tools

import (
	"strings"
	"testing"
)

func TestSendToChat_ShellFuncIncludesFilename(t *testing.T) {
	// Verifies the auto-generated foci_send_to_chat shell function
	// wires --filename into the parser via the generic generator. This
	// asserts the schema property is exposed at the shell layer, not
	// just at JSON-call layer.
	tool := NewSendToChatTool(nil, nil, nil)
	body := generateShellFunc(tool)
	if !strings.Contains(body, "--filename)") {
		t.Errorf("generated shell function for send_to_chat does not contain --filename) parser case\n---\n%s", body)
	}
}

func TestSendToChat_ShellFuncAcceptsCaptionAlias(t *testing.T) {
	// #1452: --caption is an alias for --text (the caption/text that
	// accompanies a --file attachment). Verifies the generated shell
	// function wires --caption into the same "text" variable as --text,
	// and that --caption is not rejected as an unrecognized flag.
	tool := NewSendToChatTool(nil, nil, nil)
	body := generateShellFunc(tool)
	// Asserted on the ARM's substance, not on `--caption)` being immediately
	// followed by the assignment: #1778 inserted a both-forms-given guard between
	// them (text is send_to_chat's positional, so `--caption X Y` is now an error
	// rather than a silent merge), and pinning the adjacency made this test fail
	// on a change that kept the wiring exactly as it was.
	var captionArm string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "--caption)") {
			captionArm = line
			break
		}
	}
	if captionArm == "" {
		t.Errorf("generated shell function for send_to_chat has no --caption) parser arm\n---\n%s", body)
	} else if !strings.Contains(captionArm, `text="$2"; shift 2`) {
		t.Errorf("--caption arm does not wire to the text variable: %s", captionArm)
	}
	if !strings.Contains(body, "--caption") {
		t.Errorf("generated shell function unrecognized-flag list should include --caption\n---\n%s", body)
	}
}
