package slack

// api_test.go: the Web API client's error hygiene — an upload failure must
// never carry Slack's pre-signed upload URL (its sig= query is a credential)
// into the error text callers log or return to tool output.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/log"
)

// TestWebAPI_UploadErrorOmitsSignedURL proves a failed upload's error text
// names the operation and the cause but never the pre-signed upload URL: the
// signature in its sig= query is a credential, and the error reaches WARN
// logs (backend prompt failures) and agent tool output.
func TestWebAPI_UploadErrorOmitsSignedURL(t *testing.T) {
	const signedURL = "http://127.0.0.1:1/upload/SIGNED?sig=SECRETSIG"

	// getUploadURLExternal hands out the signed URL; the POST to it then
	// fails on the refused connection.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/api/files.getUploadURLExternal", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": signedURL, "file_id": "F1"})
	})
	api := newWebAPI(srv.URL+"/api", "xoxb-fake", srv.Client())
	core := &agentCore{agentID: "helen", api: api, logger: func() *log.ComponentLogger {
		return log.NewComponentLogger("slack-test")
	}}

	err := core.upload(context.Background(), Target{Kind: TargetDM, Channel: "D1", User: "U1"}, "a.txt", []byte("x"), "")
	if err == nil {
		t.Fatal("upload to a refused endpoint returned nil; want the transport error")
	}
	if !strings.Contains(err.Error(), "file upload") {
		t.Errorf("upload error = %q, want it to name the operation", err.Error())
	}
	for _, leak := range []string{"SECRETSIG", "sig=", signedURL} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("upload error carries the pre-signed URL (%q): %q", leak, err.Error())
		}
	}

	// The same hygiene on the request-build arm: a URL Slack could not even
	// hand back parseable (a control character) must not be quoted in the
	// parse error either.
	mux2 := http.NewServeMux()
	srv2 := httptest.NewServer(mux2)
	t.Cleanup(srv2.Close)
	mux2.HandleFunc("/api/files.getUploadURLExternal", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": signedURL + "\n", "file_id": "F1"})
	})
	api2 := newWebAPI(srv2.URL+"/api", "xoxb-fake", srv2.Client())
	core2 := &agentCore{agentID: "helen", api: api2, logger: core.logger}
	err = core2.upload(context.Background(), Target{Kind: TargetDM, Channel: "D1", User: "U1"}, "a.txt", []byte("x"), "")
	if err == nil {
		t.Fatal("upload with an unparseable URL returned nil; want the build error")
	}
	if strings.Contains(err.Error(), "SECRETSIG") || strings.Contains(err.Error(), "sig=") {
		t.Errorf("build error carries the pre-signed URL: %q", err.Error())
	}
}
