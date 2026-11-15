package platform

import "testing"

func TestParseLimitBreak(t *testing.T) {
	for _, tc := range []struct {
		in, rest string
		ok       bool
	}{
		{"/lb hey one last thing", "hey one last thing", true},
		{"  /LB\thi\nthere ", "hi\nthere", true},
		{"/limit_break go", "go", true},
		{"/limit-break go", "go", true},
		{".lb go", "go", true},
		{"/lb", "", true},
		{"/lb   ", "", true},
		{"/lbs go", "", false},
		{"/lb, go", "", false},
		{"lb go", "", false},
		{"say /lb go", "", false},
		{"/", "", false},
	} {
		rest, ok := ParseLimitBreak(tc.in)
		if rest != tc.rest || ok != tc.ok {
			t.Errorf("ParseLimitBreak(%q) = %q, %v; want %q, %v", tc.in, rest, ok, tc.rest, tc.ok)
		}
	}
}
