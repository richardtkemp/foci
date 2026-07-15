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
