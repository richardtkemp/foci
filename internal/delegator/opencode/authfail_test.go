//go:build ignore
// Content below is fully disabled (no kept tests); Step 9+ replaces with fresh tests.
package opencode

import "testing"

// TODO(opencode): rewrite — opencode auth detection via ProviderAuthError events, not claude subprocess; see plan section 11
// func TestIsAuthFailure(t *testing.T) {
// 	cases := []struct {
// 		in   string
// 		want bool
// 	}{
// 		{"Failed to authenticate. API Error: 401 Invalid authentication credentials", true},
// 		{"failed to authenticate", true},
// 		{"Invalid authentication credentials", true},
// 		{"API Error: 401 authentication failed", true},
// 		{"", false},
// 		{"Error: CC process exited unexpectedly: exit status 1", false},
// 		{"API Error: 429 rate limited", false},
// 		{"401 not found", false}, // 401 without an authentication mention
// 		{"some normal assistant reply", false},
// 	}
// 	for _, tc := range cases {
// 		if got := isAuthFailure(tc.in); got != tc.want {
// 			t.Errorf("isAuthFailure(%q) = %v; want %v", tc.in, got, tc.want)
// 		}
// 	}
// }

// TODO(opencode): rewrite — opencode auth detection via ProviderAuthError events, not claude subprocess; see plan section 11
// func TestFireAuthFailure(t *testing.T) {
// 	b := &Backend{}
// 	// No hook set → no panic.
// 	b.fireAuthFailure("x")
//
// 	var got string
// 	b.onAuthFailure = func(detail string) { got = detail }
// 	b.fireAuthFailure("Failed to authenticate")
// 	if got != "Failed to authenticate" {
// 		t.Errorf("hook got %q", got)
// 	}
// }
