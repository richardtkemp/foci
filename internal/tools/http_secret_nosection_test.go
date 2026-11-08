package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSecretRefWithoutSection_ErrorsNotPanics covers #2961: a
// {{secret:NAME}} whose NAME has no "section." prefix in a request body
// panicked validateAndResolveSecrets (index out of range on the split name)
// and, through the exec bridge, killed foci-gw. A body or a header reference
// must come back as an ordinary error that names the bad reference.
func TestSecretRefWithoutSection_ErrorsNotPanics(t *testing.T) {
	t.Parallel()
	store := writeTestSecrets(t, `
[custom]
api_key = "sk-secret"
allowed_hosts = ["example.com"]
allowed_in_body = ["api_key"]
`)
	tool := NewHTTPRequestTool(store, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, func() int64 { return 0 }, nil, 0640)

	cases := map[string]map[string]interface{}{
		"body": {
			"url": "https://example.com/", "method": "POST",
			"body": `{"key":"{{secret:nodot}}"}`,
		},
		"header": {
			"url": "https://example.com/", "method": "GET",
			"headers": map[string]string{"Authorization": "Bearer {{secret:nodot}}"},
		},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			params, _ := json.Marshal(p)
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				_, err = tool.Execute(context.Background(), params)
			}()
			if err == nil {
				t.Fatal("expected an error for a secret reference with no section")
			}
			if !strings.Contains(err.Error(), "nodot") || !strings.Contains(err.Error(), "section.key") {
				t.Errorf("error should name the reference and the section.key form: %v", err)
			}
		})
	}
}
