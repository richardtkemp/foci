package askgw

import "testing"

func TestFormatNotifyText(t *testing.T) {
	zero, one := 0, 1
	cases := []struct {
		name string
		nf   NotifyFrame
		want string
	}{
		{"exit zero", NotifyFrame{ExitCode: &zero}, "✅ completed, exit 0"},
		{"exit nonzero", NotifyFrame{ExitCode: &one}, "❌ completed, exit 1"},
		{"empty", NotifyFrame{}, "✅ completed"},
		// #2745: the sender's status is shown as sent, with no ✅ added.
		{"status not run", NotifyFrame{Status: "❌ NOT run"}, "❌ NOT run"},
		{"status with message", NotifyFrame{Status: "⚠️ did not finish", Message: "client gone"}, "⚠️ did not finish\nclient gone"},
	}
	for _, c := range cases {
		if got := formatNotifyText(&c.nf); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
