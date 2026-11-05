package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/BurntSushi/toml"
	"reflect"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/log"
)

// TestThreadLifecycle_WritableRoots proves start and fork send explicit grants,
// including an empty list that removes inherited parent grants on a fork.
func TestThreadLifecycle_WritableRoots(t *testing.T) {
	for _, roots := range [][]string{{"/configured/path with space", "/other"}, {}} {
		for _, method := range []string{"thread/start", "thread/fork"} {
			t.Run(method+fmtRoots(roots), func(t *testing.T) {
				b := newStartableBackend(t, func(gotMethod string, params json.RawMessage, _ int64) (json.RawMessage, error) {
					if gotMethod == "thread/unsubscribe" {
						return json.RawMessage(`{"status":"unsubscribed"}`), nil
					}
					var p struct {
						Config map[string]json.RawMessage `json:"config"`
					}
					if err := json.Unmarshal(params, &p); err != nil {
						t.Fatal(err)
					}
					var got []string
					if err := json.Unmarshal(p.Config["sandbox_workspace_write.writable_roots"], &got); err != nil {
						t.Errorf("missing writable root override: %s (%v)", params, err)
					}
					if gotMethod != method || !reflect.DeepEqual(got, roots) {
						t.Errorf("method=%s roots=%v, want %s %v", gotMethod, got, method, roots)
					}
					return json.RawMessage(`{"approvalsReviewer":"auto_review","thread":{"id":"roots-thread"}}`), nil
				})
				b.cfg = map[string]any{"writable_roots": roots}
				if method == "thread/start" {
					if _, err := b.startThread(); err != nil {
						t.Fatal(err)
					}
				} else if _, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "parent"}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// fmtRoots gives the two list cases stable subtest names.
func fmtRoots(roots []string) string {
	if len(roots) == 0 {
		return "/empty"
	}
	return "/configured"
}

// TestReadOnlyBatch_NoWritableRoots ensures extra roots cannot weaken exploration.
func TestReadOnlyBatch_NoWritableRoots(t *testing.T) {
	b := newStartableBackend(t, func(_ string, params json.RawMessage, _ int64) (json.RawMessage, error) {
		var p struct {
			Sandbox string                     `json:"sandbox"`
			Config  map[string]json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatal(err)
		}
		var roots []string
		if err := json.Unmarshal(p.Config["sandbox_workspace_write.writable_roots"], &roots); err != nil {
			t.Fatal(err)
		}
		if p.Sandbox != "read-only" || len(roots) != 0 {
			t.Errorf("read-only batch widened: %s", params)
		}
		return json.RawMessage(`{"approvalsReviewer":"auto_review","thread":{"id":"roots-thread"}}`), nil
	})
	b.cfg = map[string]any{"writable_roots": []string{"/configured"}}
	b.startOpts.BatchAccess = delegator.BatchAccessReadOnly
	if _, err := b.startThread(); err != nil {
		t.Fatal(err)
	}
}

// TestAppServerArgs_WritableRoots checks launch grants use valid TOML and never
// widen read-only or unrestricted policies, even with awkward path characters.
func TestAppServerArgs_WritableRoots(t *testing.T) {
	roots := []string{`/space and "quote"`, `/back\slash`, "/unicode/é"}
	for _, mode := range []string{"workspace-write", "read-only", "danger-full-access"} {
		b := &Backend{cfg: map[string]any{"writable_roots": roots, "sandbox": mode}}
		args := b.appServerArgs("")
		var decoded struct {
			Sandbox struct {
				Roots []string `toml:"writable_roots"`
			} `toml:"sandbox_workspace_write"`
		}
		value := args[len(args)-1]
		if _, err := toml.Decode(value, &decoded); err != nil {
			t.Fatal(err)
		}
		want := []string{}
		if mode == "workspace-write" {
			want = roots
		}
		if !reflect.DeepEqual(decoded.Sandbox.Roots, want) {
			t.Errorf("mode=%s roots=%v, want %v", mode, decoded.Sandbox.Roots, want)
		}
		if !strings.Contains(strings.Join(args, " "), "approvals_reviewer=auto_review") {
			t.Fatal("reviewer changed")
		}
	}
}

// Not parallel: the log output is process-global. Resume must remain usable,
// but log an actionable INFO once per facade/thread when persisted grants differ.
func TestResumeThread_WritableRootsMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, sandbox     string
		configured, saved []string
		wantLog           bool
	}{
		{"added", "workspaceWrite", []string{"/new"}, nil, true},
		{"removed", "workspaceWrite", nil, []string{"/old"}, true},
		{"same", "workspaceWrite", []string{"/a", "/b"}, []string{"/b", "/a/", "/a"}, false},
		{"empty", "workspaceWrite", []string{}, nil, false},
		{"read-only", "readOnly", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			log.SetOutput(&out)
			defer log.SetOutput(nil)
			b := newStartableBackend(t, func(_ string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
				raw, err := json.Marshal(map[string]any{"approvalsReviewer": "auto_review", "thread": map[string]string{"id": "saved-thread"}, "sandbox": map[string]any{"type": tc.sandbox, "writableRoots": tc.saved}})
				return raw, err
			})
			b.cfg = map[string]any{"writable_roots": tc.configured}
			for i := 0; i < 2; i++ {
				if err := b.resumeThread("saved-thread"); err != nil {
					t.Fatal(err)
				}
			}
			got := out.String()
			count := strings.Count(got, "writable roots differ")
			want := 0
			if tc.wantLog {
				want = 1
			}
			if count != want {
				t.Fatalf("mismatch messages=%d, want %d; log=%s", count, want, got)
			}
			if tc.wantLog && (!strings.Contains(got, "INFO") || !strings.Contains(got, "saved-thread") || !strings.Contains(got, "new thread") || !strings.Contains(got, "configured=") || !strings.Contains(got, "effective=")) {
				t.Fatalf("incomplete mismatch log: %s", got)
			}
		})
	}
}
