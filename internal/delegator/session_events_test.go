package delegator

import "testing"

// TestToolEndNotReported_Routing (#3188): the helper goes to
// OnToolEndNotReported when set, falls back to OnToolEnd with "" output when
// not, and is a no-op on a nil receiver or with neither callback.
func TestToolEndNotReported_Routing(t *testing.T) {
	t.Parallel()
	var plain, flagged []string
	se := &SessionEvents{
		OnToolEnd: func(id, _, output string, _ bool) {
			plain = append(plain, id+"="+output)
		},
		OnToolEndNotReported: func(id, _ string, _ bool) { flagged = append(flagged, id) },
	}
	se.ToolEndNotReported("a", "t", false)
	if len(flagged) != 1 || flagged[0] != "a" || len(plain) != 0 {
		t.Errorf("with OnToolEndNotReported set: flagged=%q plain=%q", flagged, plain)
	}

	se.OnToolEndNotReported = nil
	se.ToolEndNotReported("b", "t", true)
	if len(plain) != 1 || plain[0] != "b=" {
		t.Errorf("fallback to OnToolEnd: plain=%q, want [b=]", plain)
	}

	var nilSE *SessionEvents
	nilSE.ToolEndNotReported("c", "t", false)
	(&SessionEvents{}).ToolEndNotReported("d", "t", false)
}
