package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/provider"
)

// #2988 (Dick 2026-11-09, Q52 "Own session + link"): a direct-API spawn's
// calls are booked on its own session (its #2984 conversation file) and on a
// turn of its own, like a delegated spawn's; the spawn link names that
// session, so Message info lists it under the parent turn and prices it from
// there. These tests make a ledger the live one, so none is parallel.

// liveLedger makes a fresh ledger the live one for the test.
func liveLedger(t *testing.T) *accounting.Ledger {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })
	return l
}

// parentTurn is the conversation turn of test/imain that makes the spawn
// call: on ctx as the invoking turn, and the session's live turn.
var parentTurn = accounting.Turn{TurnID: "test/imain@42", Session: "test/imain", AgentID: "test",
	Backend: accounting.BackendAPI, Source: accounting.SourceUser, StartedAt: time.Now()}

func withParentTurn(h *persistHarness) context.Context {
	h.spawner.deps.LiveTurn = func(sk string) string {
		if sk == "test/imain" {
			return parentTurn.TurnID
		}
		return ""
	}
	return accounting.WithTurn(h.ctx, parentTurn)
}

// The calls of a spawn a conversation turn made are booked on the spawn's
// own session, from its first call, all on one turn of its own on that
// session; the link names that session and prices the spawn from it, and
// none of it is the parent turn's.
func TestAPIOneShot_BookedOnItsOwnSession(t *testing.T) {
	for _, mode := range []string{spawnModeCharacter, spawnModeExplore, spawnModeRaw} {
		t.Run(mode, func(t *testing.T) {
			l := liveLedger(t)
			var mu sync.Mutex
			calls := 0
			h := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return toolUseResponse("m1")
				}
				return textResponse("done")
			}, probeTool(func(context.Context) {}))
			ctx := withParentTurn(h)

			params, _ := json.Marshal(map[string]string{"name": "own1", "prompt": "look around", "context": mode})
			if res, err := h.spawner.SpawnTool().Execute(ctx, params); err != nil {
				t.Fatalf("spawn = %q, %v", res.Text, err)
			}
			keys := h.convKeys(t)
			if len(keys) != 1 {
				t.Fatalf("conversation files = %v, want one", keys)
			}
			key := keys[0]

			rows, err := l.Calls(time.Time{})
			if err != nil || len(rows) == 0 {
				t.Fatalf("ledger calls = %d (%v)", len(rows), err)
			}
			var cost float64
			for i, r := range rows {
				if r.Kind != accounting.KindSpawn || r.Session != key || r.TurnID != rows[0].TurnID ||
					!strings.HasPrefix(r.TurnID, key+"@") || !strings.HasSuffix(r.TurnID, ":spawn") || r.AgentID != "test" {
					t.Errorf("call %d = %s on session %q turn %q agent %q, want a spawn call on its own session %q, on one turn of its own",
						i+1, r.Kind, r.Session, r.TurnID, r.AgentID, key)
				}
				cost += r.Cost()
			}
			if st, err := l.SessionStats("test/imain"); err != nil || st.TotalCalls != 0 {
				t.Errorf("parent session stats = %+v (%v), want none of the spawn's calls", st, err)
			}
			got, err := l.TurnSpawns(parentTurn.TurnID)
			if err != nil || len(got) != 1 {
				t.Fatalf("spawns of the parent turn = %+v (%v), want the one", got, err)
			}
			if got[0].ChildSession != key || got[0].InParentTurn || !got[0].Priced || got[0].CostUSD != cost || cost == 0 {
				t.Errorf("spawn link = %+v, want its session %q, priced at its calls' $%v, not in the parent turn", got[0], key, cost)
			}
		})
	}
}

// A spawn cut by a restart, then resumed by the next process, books its
// later calls on the same session and turn as its first ones, and Message
// info still lists it under its parent turn with all its spend.
func TestAPIOneShot_ResumedRunBooksOnTheSameSessionAndTurn(t *testing.T) {
	l := liveLedger(t)

	// The first process: one round, then foci shuts down.
	var h1 *persistHarness
	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	var id string
	h1 = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return toolUseResponse("m1")
	}, probeTool(func(context.Context) {
		if r := h1.tracker.Running("test/imain"); len(r) == 1 {
			id = r[0].ID
		}
		h1.tracker.BeginShutdown()
		cancelCall()
	}))
	h1.ctx = WithSessionKey(callCtx, "test/imain")
	ctx := withParentTurn(h1)
	params, _ := json.Marshal(map[string]string{"name": "own2", "prompt": "long task", "context": "character"})
	if _, err := h1.spawner.SpawnTool().Execute(ctx, params); err == nil {
		t.Fatal("a cut spawn returned no error")
	}
	rec, ok := h1.store.record(t, id)
	if !ok || rec.OneShot == nil || rec.OneShot.Conversation == "" || rec.OneShot.Started.IsZero() {
		t.Fatalf("record after the cut = %+v (ok %v), want its session and turn start", rec, ok)
	}
	key := rec.OneShot.Conversation

	// The next process resumes it from the same record and files.
	h2 := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return textResponse("resumed answer")
	}, probeTool(func(context.Context) {}))
	h2.convs = h1.convs
	h2.spawner.deps.Conversations = h1.convs
	h2.store.put(t, rec)
	h2.spawner.RestoreSpawns()
	d := h2.nextDelivery(t)
	if !strings.Contains(d.msg, "resumed answer") {
		t.Fatalf("delivered %q, want its answer", d.msg)
	}
	d.done(true)

	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger calls = %d (%v), want the call before the restart and the one after", len(rows), err)
	}
	wantTurn := accounting.MintTurnID(key, accounting.KindSpawn, rec.OneShot.Started)
	var cost float64
	for i, r := range rows {
		if r.Session != key || r.TurnID != wantTurn {
			t.Errorf("call %d on session %q turn %q, want %q turn %q", i+1, r.Session, r.TurnID, key, wantTurn)
		}
		cost += r.Cost()
	}
	got, err := l.TurnSpawns(parentTurn.TurnID)
	if err != nil || len(got) != 1 {
		t.Fatalf("spawns of the parent turn = %+v (%v), want the one", got, err)
	}
	if got[0].ChildSession != key || got[0].CostUSD != cost || got[0].InParentTurn {
		t.Errorf("spawn link after the restart = %+v, want its session %q priced at both calls' $%v", got[0], key, cost)
	}
}

// A resumed spawn whose file cannot be read keeps its session: the file is
// set aside, the run starts again from its task in an empty file under the
// same key, and its calls book there.
func TestAPIOneShot_UnreadableConversationKeepsItsSession(t *testing.T) {
	l := liveLedger(t)
	h := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return textResponse("fresh answer")
	}, probeTool(func(context.Context) {}))
	key := h.savedConversation(t, []provider.Message{{Role: "user", Content: provider.TextContent("count the stars")}})
	path, err := h.convs.store.SessionPath(key)
	if err != nil {
		t.Fatal(err)
	}
	// A write torn by a power cut.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"assistant","content":[{"ty`)
	_ = f.Close()
	if _, err := h.convs.LoadSpawnConversation(key); err == nil {
		t.Fatal("the torn file reads cleanly: the test sets up nothing")
	}
	started := time.Now().Add(-time.Minute)
	h.store.put(t, spawnRecord{
		ID: "spawn-os8", Parent: "test/imain", Name: "stars", Prompt: "count the stars",
		Started: started, Running: true,
		OneShot: &oneShotRecord{Mode: spawnModeCharacter, Self: "test/imain/b1", Conversation: key, Started: started},
	})
	h.spawner.RestoreSpawns()
	d := h.nextDelivery(t)
	if !strings.Contains(d.msg, "fresh answer") {
		t.Fatalf("delivered %q, want its answer", d.msg)
	}
	d.done(true)

	got := h.conv(t, key)
	if len(got) != 2 || !strings.Contains(provider.TextOf(got[0].Content), "count the stars") || provider.TextOf(got[1].Content) != "fresh answer" {
		t.Fatalf("file %s after the run = %+v, want its task again and its answer", key, got)
	}
	var live []string // convKeys lists archives too (<key>.<stamp>)
	for _, k := range h.convKeys(t) {
		if !strings.Contains(k, ".") {
			live = append(live, k)
		}
	}
	if len(live) != 1 || live[0] != key {
		t.Errorf("live conversation files = %v, want only %s", live, key)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	archived := false
	for _, e := range ents {
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if e.Name() != filepath.Base(path) && strings.HasPrefix(e.Name(), base+".") {
			archived = true
		}
	}
	if !archived {
		t.Errorf("no archive of the unreadable file beside %s", path)
	}
	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 1 || rows[0].Session != key ||
		rows[0].TurnID != accounting.MintTurnID(key, accounting.KindSpawn, started) {
		t.Fatalf("ledger calls = %+v (%v), want its one call on %s, its turn from before", rows, err, key)
	}
}

// With no conversation store, a spawn books as before: on the turn that
// invoked it (TestSpawnBooksOntoTheInvokingTurn covers the plain Spawner);
// here the same through the tracked direct path.
func TestAPIOneShot_NoConversationStoreBooksOnTheInvokingTurn(t *testing.T) {
	l := liveLedger(t)
	h := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return textResponse("done")
	}, nil)
	h.spawner.deps.Conversations = nil
	ctx := withParentTurn(h)
	params, _ := json.Marshal(map[string]string{"name": "own3", "prompt": "t", "context": "character"})
	if _, err := h.spawner.SpawnTool().Execute(ctx, params); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 1 || rows[0].Session != "test/imain" || rows[0].TurnID != parentTurn.TurnID {
		t.Fatalf("ledger calls = %+v (%v), want its call on the parent turn", rows, err)
	}
	if got, _ := l.TurnSpawns(parentTurn.TurnID); len(got) != 1 || !got[0].InParentTurn || got[0].ChildSession != "" {
		t.Errorf("spawn link = %+v, want it in the parent turn", got)
	}
}
