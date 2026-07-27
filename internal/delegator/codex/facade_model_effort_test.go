package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/delegator"
)

// stubAppServerWithCatalogue is stubAppServer plus a non-empty model/list, so
// b.catalogueModels is actually populated and model resolution has something
// to resolve AGAINST. With the empty catalogue the other stubs return,
// ResolveModel falls back to modelcaps' runtime store — cold in tests — and
// the assertion below would pass or fail for reasons unrelated to the bug.
func stubAppServerWithCatalogue(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys
next_thread = 0
for line in sys.stdin:
  try: msg=json.loads(line)
  except Exception: continue
  method=msg.get("method"); ident=msg.get("id")
  def send(x): print(json.dumps(x), flush=True)
  if method == "initialize":
    send({"id":ident,"result":{}})
  elif method == "model/list":
    send({"id":ident,"result":{"data":[{"model":"gpt-5-codex"}]}})
  elif method == "thread/start":
    next_thread += 1; tid="thread-%d" % next_thread
    thread={"id":tid,"path":None,"status":{"type":"idle"}}
    send({"id":ident,"result":{"thread":thread,"model":"gpt-5-codex"}})
    send({"method":"thread/started","params":{"thread":thread}})
  elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// stubAppServerGrowingCatalogue serves a one-model catalogue on the first
// model/list and a two-model one thereafter, so a test can refresh the owner's
// catalogue and see whether a facade observes the change.
func stubAppServerGrowingCatalogue(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys
listed = 0
next_thread = 0
for line in sys.stdin:
  try: msg=json.loads(line)
  except Exception: continue
  method=msg.get("method"); ident=msg.get("id")
  def send(x): print(json.dumps(x), flush=True)
  if method == "initialize":
    send({"id":ident,"result":{}})
  elif method == "model/list":
    listed += 1
    if listed == 1:
      send({"id":ident,"result":{"data":[{"model":"gpt-5-codex"}]}})
    else:
      send({"id":ident,"result":{"data":[{"model":"gpt-5-codex"},{"model":"o3-mini"}]}})
  elif method == "thread/start":
    next_thread += 1; tid="thread-%d" % next_thread
    thread={"id":tid,"path":None,"status":{"type":"idle"}}
    send({"id":ident,"result":{"thread":thread,"model":"gpt-5-codex"}})
    send({"method":"thread/started","params":{"thread":thread}})
  elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// A facade must read the OWNER's live catalogue, not a copy taken when it
// attached (#1577). The copy was both a data race — the attach path holds only
// sharedPool, never owner.mu, which is what guards the field — and permanently
// stale, because refreshModelCaps rebinds the owner's field to a freshly
// allocated slice that an existing copy never observes.
//
// Staleness is the assertable half: refresh the owner AFTER the facade has
// attached, and the facade must see the new model. A copy cannot.
func TestFacadeSeesOwnerCatalogueAfterRefresh(t *testing.T) {
	stub := stubAppServerGrowingCatalogue(t)
	cfg := map[string]any{"binary": stub}
	const agent = "catalogue-agent"
	ctx := context.Background()

	ownerBE, _ := newFromConfig(cfg)
	owner := ownerBE.(*Backend)
	if err := owner.Start(ctx, delegator.StartOptions{
		AgentID: agent, SessionKey: "owner/session", WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	defer func() { _ = owner.Close() }()

	facadeBE, _ := newFromConfig(cfg)
	facade := facadeBE.(*Backend)
	if err := facade.Start(ctx, delegator.StartOptions{
		AgentID: agent, SessionKey: "facade/session", WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("start facade: %v", err)
	}
	defer func() { _ = facade.Close() }()
	if facade.process() != owner.process() {
		t.Fatal("facade did not attach to the owner's app-server")
	}

	// Asserted through ResolveModel rather than an accessor: it is the real
	// consumer of the catalogue, and it exists in both the pre- and post-fix
	// code, so this test can be run against the unfixed version.
	if _, err := facade.ResolveModel(ctx, "gpt-5-codex"); err != nil {
		t.Fatalf("facade could not resolve the owner's initial model: %v", err)
	}

	// The owner re-lists; the stub now also offers o3-mini.
	if err := owner.refreshModelCaps(); err != nil {
		t.Fatalf("refreshModelCaps: %v", err)
	}
	if _, err := owner.ResolveModel(ctx, "o3-mini"); err != nil {
		t.Fatalf("owner cannot resolve the model it just listed — stub did not grow the catalogue: %v", err)
	}

	if _, err := facade.ResolveModel(ctx, "o3-mini"); err != nil {
		t.Errorf("facade cannot resolve a model the owner listed after the facade attached: %v — the facade is resolving against a stale snapshot instead of the owner's live catalogue", err)
	}
}

// Start has two paths — the owner that launches the app-server, and a facade
// that attaches to an already-running one — and both need the same pre-thread
// setup. The facade path used to return before ever resolving the model or
// recording the effort (#1573), so every session after the first on a shared
// app-server silently passed a raw foci alias to thread/start, never set
// pendingModel on resume (running codex's DEFAULT model), and dropped
// reasoning effort entirely, with no error.
//
// Needs TWO live sessions sharing a pool, which is why the existing package
// tests never caught it.
func TestFacadeResolvesModelAndKeepsEffort(t *testing.T) {
	stub := stubAppServerWithCatalogue(t)
	cfg := map[string]any{"binary": stub}
	const agent = "facade-model-agent"
	ctx := context.Background()

	ownerBE, _ := newFromConfig(cfg)
	owner := ownerBE.(*Backend)
	if err := owner.Start(ctx, delegator.StartOptions{
		AgentID: agent, SessionKey: "owner/session", WorkDir: t.TempDir(),
		Model: "gpt-5-codex",
	}); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	defer func() { _ = owner.Close() }()

	// The facade must inherit the owner's catalogue, or resolution below is
	// testing the fallback rather than the real path.
	owner.mu.Lock()
	ownerCatalogue := len(owner.catalogueModels)
	owner.mu.Unlock()
	if ownerCatalogue == 0 {
		t.Fatal("owner catalogue empty — stub model/list did not populate catalogueModels")
	}

	facadeBE, _ := newFromConfig(cfg)
	facade := facadeBE.(*Backend)
	// A "codex/"-prefixed alias: resolution must strip the prefix, so a
	// resolved value is distinguishable from the raw request.
	if err := facade.Start(ctx, delegator.StartOptions{
		AgentID: agent, SessionKey: "facade/session", WorkDir: t.TempDir(),
		Model: "codex/gpt-5-codex", Effort: "high",
	}); err != nil {
		t.Fatalf("start facade: %v", err)
	}
	defer func() { _ = facade.Close() }()

	if facade.process() != owner.process() {
		t.Fatal("facade did not attach to the owner's app-server — not exercising the attach path")
	}

	facade.mu.Lock()
	launch := facade.launchModel
	effort := facade.pendingEffort
	facade.mu.Unlock()

	if launch != "gpt-5-codex" {
		t.Errorf("facade launchModel = %q, want %q — the attach path skipped model resolution, so thread/start gets the raw alias", launch, "gpt-5-codex")
	}
	if effort != "high" {
		t.Errorf("facade pendingEffort = %q, want %q — reasoning effort is dropped for facade sessions", effort, "high")
	}
}
