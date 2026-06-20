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
