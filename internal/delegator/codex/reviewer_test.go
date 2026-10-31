package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestThreadLifecycle_PassesResolvedReviewer reproduces a persisted user
// reviewer overriding the server default unless resume explicitly replaces it.
func TestThreadLifecycle_PassesResolvedReviewer(t *testing.T) {
	for _, mode := range []string{"", "auto_review", "user"} {
		for _, method := range []string{"thread/start", "thread/resume"} {
			t.Run(mode+"/"+method, func(t *testing.T) {
				want := mode
				if want == "" {
					want = "auto_review"
				}
				b := newStartableBackend(t, func(gotMethod string, params json.RawMessage, _ int64) (json.RawMessage, error) {
					var p map[string]any
					if err := json.Unmarshal(params, &p); err != nil {
						t.Fatal(err)
					}
					if gotMethod != method {
						t.Fatalf("method=%s, want %s", gotMethod, method)
					}
					got, _ := p["approvalsReviewer"].(string)
					if got != want {
						t.Errorf("%s reviewer=%q, want %q", method, got, want)
					}
					// A resumed user thread stays user when no override is sent.
					if got == "" {
						got = "user"
					}
					return json.Marshal(map[string]any{"thread": map[string]string{"id": "reviewer-thread"}, "approvalsReviewer": got})
				})
				b.cfg = map[string]any{"approvals_reviewer": mode}
				var err error
				if method == "thread/start" {
					_, err = b.startThread()
				} else {
					err = b.resumeThread("reviewer-thread")
				}
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestThreadLifecycle_RejectsUnconfirmedReviewer prevents an already-loaded
// thread or an incompatible server silently ignoring the requested posture.
func TestThreadLifecycle_RejectsUnconfirmedReviewer(t *testing.T) {
	for _, requested := range []string{"auto_review", "user"} {
		for _, returned := range []string{"", "user", "auto_review"} {
			if returned == requested {
				continue
			}
			for _, method := range []string{"thread/start", "thread/resume"} {
				t.Run(requested+"/"+returned+"/"+method, func(t *testing.T) {
					b := newStartableBackend(t, func(_ string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
						return json.Marshal(map[string]any{"thread": map[string]string{"id": "reviewer-thread"}, "approvalsReviewer": returned})
					})
					b.cfg = map[string]any{"approvals_reviewer": requested}
					var err error
					if method == "thread/start" {
						_, err = b.startThread()
					} else {
						err = b.resumeThread("reviewer-thread")
					}
					if err == nil || !strings.Contains(err.Error(), "reviewer") {
						t.Fatalf("error=%v, want reviewer mismatch", err)
					}
				})
			}
		}
	}
}

// TestAppServerArgs_ResolvedReviewer keeps server launch on exactly the same
// policy resolver as new and resumed threads, including a human override.
func TestAppServerArgs_ResolvedReviewer(t *testing.T) {
	for _, mode := range []string{"", "user", "auto_review"} {
		b := &Backend{cfg: map[string]any{"approvals_reviewer": mode}}
		want := mode
		if want == "" {
			want = "auto_review"
		}
		args := appServerArgs("", b.sandboxMode(), b.approvalsReviewer())
		if !reflect.DeepEqual(args, []string{"app-server", "-c", "sandbox_mode=workspace-write", "-c", "approval_policy=on-request", "-c", "approvals_reviewer=" + want}) {
			t.Fatalf("mode=%q args=%v", mode, args)
		}
	}
}
