package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// depthBackend is a Backend started through Start against a recording fake
// opencode server, as a depth leaf or not, with defaultPermission as its
// backend_config default_permission ("" = none).
type depthBackend struct {
	b        *Backend
	mu       sync.Mutex
	reqs     []permRequest
	prompted []string
}

func (d *depthBackend) requests() []permRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]permRequest(nil), d.reqs...)
}

func (d *depthBackend) reset() {
	d.mu.Lock()
	d.reqs, d.prompted = nil, nil
	d.mu.Unlock()
}

func startDepthBackend(t *testing.T, leaf bool, defaultPermission string) *depthBackend {
	t.Helper()
	d := &depthBackend{}
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"sess-leaf"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.reqs = append(d.reqs, permRequest{Path: r.URL.Path, Body: body})
		d.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	b.pendingPerms = make(map[string]*pendingPermission)
	b.cfg = map[string]any{}
	if defaultPermission != "" {
		b.cfg["default_permission"] = defaultPermission
	}
	b.SetPermissionPromptFunc(func(id, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		d.mu.Lock()
		d.prompted = append(d.prompted, id)
		d.mu.Unlock()
	})
	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent", SessionKey: "test-agent/leaf", DepthLeaf: leaf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	d.b = b
	return d
}

// lastTaskAction is the action opencode resolves for a task call from the
// session rulesets foci PATCHed, in order (findLast over every batch); "" if
// no rule matches task.
func lastTaskAction(t *testing.T, reqs []permRequest) string {
	t.Helper()
	action := ""
	for _, r := range reqs {
		if r.Path != "/session/sess-leaf" {
			continue
		}
		var patch sessionPermissionPatch
		if err := json.Unmarshal(r.Body, &patch); err != nil {
			t.Fatalf("decode PATCH body %s: %v", r.Body, err)
		}
		for _, rule := range patch.Permission {
			if rule.Permission == PermTask || rule.Permission == "*" {
				action = rule.Action
			}
		}
	}
	return action
}

// TestDepthLeaf_TaskAskedAfterEveryRuleset (#2625): a depth leaf's session
// rulesets make opencode ask before a task call, whatever the agent's
// default_permission allows and after a later permission-mode change; a
// root session's rulesets are untouched.
func TestDepthLeaf_TaskAskedAfterEveryRuleset(t *testing.T) {
	for _, dp := range []string{"", "allow", "ask"} {
		t.Run("leaf/default_permission="+dp, func(t *testing.T) {
			d := startDepthBackend(t, true, dp)
			if got := lastTaskAction(t, d.requests()); got != "ask" {
				t.Errorf("after Start, task resolves to %q, want ask", got)
			}
			if err := d.b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{Mode: "bypassPermissions"}); err != nil {
				t.Fatalf("SendControl: %v", err)
			}
			if got := lastTaskAction(t, d.requests()); got != "ask" {
				t.Errorf("after a bypassPermissions mode change, task resolves to %q, want ask", got)
			}
		})
	}
	t.Run("root/default_permission=allow", func(t *testing.T) {
		d := startDepthBackend(t, false, "allow")
		if got := lastTaskAction(t, d.requests()); got != "allow" {
			t.Errorf("root session: task resolves to %q, want allow (unchanged)", got)
		}
		for _, r := range d.requests() {
			if strings.Contains(string(r.Body), `"permission":"task"`) {
				t.Errorf("root session got a task rule: %s", r.Body)
			}
		}
	})
}

// TestDepthLeaf_TaskCallRefused (#2625, requirements 3, 4 and 10): in a depth
// leaf a task ask is rejected by foci with the foci_spawn message (the tool
// result the model reads) and never prompted; in a root session it reaches
// the user as before. Other asks in a leaf are surfaced as before.
func TestDepthLeaf_TaskCallRefused(t *testing.T) {
	leaf := startDepthBackend(t, true, "")
	leaf.reset()
	leaf.b.onPermissionAsked(PermissionRequest{ID: "per-task", SessionID: "sess-leaf", Permission: PermTask, Patterns: []string{"general"}})
	var reply *permRequest
	for _, r := range leaf.requests() {
		if r.Path == "/permission/per-task/reply" {
			reply = &r
		}
	}
	if reply == nil {
		t.Fatalf("leaf task ask got no reply; requests %v", leaf.requests())
	}
	var body struct{ Reply, Message string }
	if err := json.Unmarshal(reply.Body, &body); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if body.Reply != "reject" {
		t.Errorf("leaf task reply = %q, want reject", body.Reply)
	}
	for _, want := range []string{"refused in a depth leaf", "foci_spawn", "after this leaf reports"} {
		if !strings.Contains(body.Message, want) {
			t.Errorf("refusal message %q lacks %q", body.Message, want)
		}
	}
	leaf.mu.Lock()
	prompted := leaf.prompted
	leaf.mu.Unlock()
	if len(prompted) != 0 {
		t.Errorf("leaf task ask was prompted: %v", prompted)
	}
	leaf.b.permMu.Lock()
	_, stored := leaf.b.pendingPerms["per-task"]
	leaf.b.permMu.Unlock()
	if stored {
		t.Error("leaf task ask left pending")
	}

	leaf.b.onPermissionAsked(PermissionRequest{ID: "per-edit", SessionID: "sess-leaf", Permission: PermEdit, Patterns: []string{"/tmp/x"}})
	leaf.mu.Lock()
	prompted = append([]string(nil), leaf.prompted...)
	leaf.mu.Unlock()
	if len(prompted) != 1 || prompted[0] != "per-edit" {
		t.Errorf("leaf edit ask: prompted %v, want [per-edit]", prompted)
	}

	root := startDepthBackend(t, false, "")
	root.reset()
	root.b.onPermissionAsked(PermissionRequest{ID: "per-task", SessionID: "sess-leaf", Permission: PermTask, Patterns: []string{"general"}})
	root.mu.Lock()
	prompted = append([]string(nil), root.prompted...)
	root.mu.Unlock()
	if len(prompted) != 1 || prompted[0] != "per-task" {
		t.Errorf("root task ask: prompted %v, want [per-task] (the user decides, as before)", prompted)
	}
	for _, r := range root.requests() {
		if r.Path == "/permission/per-task/reply" {
			t.Errorf("root task ask answered by foci: %s", r.Body)
		}
	}
}
