package command

import (
	"reflect"
	"testing"
)

// TestResponseDisplayParts proves the one renderer for a command reply's
// message parts: Parts wins over Text, a text-only response is one part,
// and a response with nothing readable yields no parts (so a transport that
// iterates them sends nothing rather than a blank message).
func TestResponseDisplayParts(t *testing.T) {
	cases := []struct {
		name string
		resp Response
		want []string
	}{
		{"text only", Response{Text: "hi"}, []string{"hi"}},
		{"parts", Response{Parts: []string{"a", "b"}}, []string{"a", "b"}},
		{"parts win over text", Response{Text: "ignored", Parts: []string{"a", "b"}}, []string{"a", "b"}},
		{"blank text", Response{Text: "   "}, nil},
		{"empty response", Response{}, nil},
	}
	for _, tc := range cases {
		if got := tc.resp.DisplayParts(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: DisplayParts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestResponseDisplayText proves the single-message rendering: the display
// parts joined by a blank line (the separator the platform command-reply
// paths already used), else the Text, else nothing.
func TestResponseDisplayText(t *testing.T) {
	cases := []struct {
		name string
		resp Response
		want string
	}{
		{"text only", Response{Text: "hi"}, "hi"},
		{"parts joined by a blank line", Response{Parts: []string{"PART-ONE", "PART-TWO"}}, "PART-ONE\n\nPART-TWO"},
		{"parts win over text", Response{Text: "ignored", Parts: []string{"a", "b"}}, "a\n\nb"},
		{"blank text", Response{Text: "   "}, ""},
		{"empty response", Response{}, ""},
	}
	for _, tc := range cases {
		if got := tc.resp.DisplayText(); got != tc.want {
			t.Errorf("%s: DisplayText = %q, want %q", tc.name, got, tc.want)
		}
	}
}
