package relogin

import "testing"

func TestExtractLoginURL(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want string
	}{
		{
			name: "clean single line",
			pane: "Use the url below to sign in\nhttps://claude.ai/oauth/authorize?code=1&state=2\nPaste code here if prompted",
			want: "https://claude.ai/oauth/authorize?code=1&state=2",
		},
		{
			name: "wrapped across lines with box glyphs",
			pane: "│ Use the url below to sign in                     │\n│ https://claude.ai/oauth/authorize?code=abc │\n│ def&state=xyz                              │\n│ Paste code here if prompted                │",
			want: "https://claude.ai/oauth/authorize?code=abcdef&state=xyz",
		},
		{
			// Layout captured from Claude Code v2.1.280 `/login` in a 220-col tmux
			// pane (2026-09-26, #1931): the URL is hard-wrapped at the pane edge
			// with no indent, then an indented prose hint line precedes the paste
			// anchor. The hint must not be glued onto the state param.
			name: "hint line after wrapped URL is not swallowed",
			pane: "   Login\n\n" +
				"   Browser didn't open? Use the url below to sign in (c to copy)\n\n" +
				"https://claude.com/cai/oauth/authorize?code=true&client_id=abc&scope=org%3Acreate_api_key+user%\n" +
				"3Aprofile&code_challenge=CH&code_challenge_method=S256&state=lPJrYB\n" +
				"t1y3SXs8iOUZAqc\n\n" +
				"   Hold Shift (Option in iTerm2, Fn in Terminal.app) while selecting to use your terminal's native copy\n\n" +
				"   Paste code here if prompted >\n",
			want: "https://claude.com/cai/oauth/authorize?code=true&client_id=abc&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=CH&code_challenge_method=S256&state=lPJrYBt1y3SXs8iOUZAqc",
		},
		{
			name: "prose line directly after URL with no blank separator",
			pane: "Use the url below to sign in\nhttps://claude.ai/oauth/authorize?state=xyz\n   Hold Shift (Option in iTerm2) while selecting\nPaste code here if prompted",
			want: "https://claude.ai/oauth/authorize?state=xyz",
		},
		{
			name: "anchors absent",
			pane: "just some text\nwith no anchors",
			want: "",
		},
		{
			name: "sign-in anchor present but no paste anchor yet",
			pane: "Use the url below to sign in\nhttps://claude.ai/x",
			want: "",
		},
		{
			name: "no https in region",
			pane: "Use the url below to sign in\n(loading...)\nPaste code here if prompted",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractLoginURL(tc.pane); got != tc.want {
				t.Errorf("extractLoginURL = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestSliceBetween(t *testing.T) {
	got, ok := sliceBetween("aXXbYYc", "XX", "YY")
	if !ok || got != "b" {
		t.Errorf("sliceBetween = %q,%v; want b,true", got, ok)
	}
	if _, ok := sliceBetween("abc", "XX", "YY"); ok {
		t.Error("missing start anchor should give ok=false")
	}
	if _, ok := sliceBetween("aXXb", "XX", "YY"); ok {
		t.Error("missing end anchor should give ok=false")
	}
}
