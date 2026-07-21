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
	if !strings.Contains(body, `--caption) text="$2"; shift 2`) {
		t.Errorf("generated shell function for send_to_chat does not wire --caption to the text variable\n---\n%s", body)
	}
	if !strings.Contains(body, "--caption") {
		t.Errorf("generated shell function unrecognized-flag list should include --caption\n---\n%s", body)
	}
}
