package tools

import (
	"context"
	"fmt"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/turnevent"
)

// An API agent's direct one-shot spawn (raw, character, explore: the
// spawnOneShot loop) survives a foci restart (#2979), with the contract
// background spawns have (#2589): one running when foci stops runs again
// after start, from its saved conversation, and its parent gets its result
// as a [SPAWN RESULT]; one that cannot be resumed is reported lost to its
// parent, never dropped in silence.
//
// What makes that work:
//   - its tracker entry carries a oneShotRecord, and is persisted like a
//     background spawn's (spawnRecord.OneShot), while other direct spawns
//     (a delegated agent's batch) still are not;
//   - its loop appends what each round of tool calls added to its
//     conversation (oneShotSaver) to a session JSONL file of its own: a fresh
//     "spawn" branch of its parent, made before its first model call, whose
//     key the record carries (oneShotRecord.Conversation). Append-only, as
//     every session's history is (#2984): a round writes its own two
//     messages, never the earlier ones again;
//   - its model calls are booked on that session, on a turn of its own whose
//     start the record carries too (oneShotRecord.Started), so a resumed run
//     books on the same session and turn; the spawn link (#2992) names the
//     session, which is how Message info lists it under its parent turn
//     (#2988, Dick 2026-11-09);
//   - endDirect forgets the record, unless foci is shutting down and the run
//     was cut short: then it stays, for RestoreSpawns to resume
//     (resumeOneShotAfterRestart). The conversation file stays either way,
//     as every spawn branch's transcript does.
//
// The parent's tool call that waited for it does not survive the restart (an
// API session's interrupted call reads "Tool call interrupted"), so the
// resumed run's answer reaches the parent as a background spawn's does.

// oneShotRecord is what a new process needs to run an API one-shot again:
// its mode, the model group it was asked for (resolved again on resume),
// and, for a character spawn, the identity it was minted (#2676).
type oneShotRecord struct {
	Mode  string `json:"mode"`
	Group string `json:"group,omitempty"`
	Self  string `json:"self,omitempty"`
	// Conversation is the session key of the JSONL file its conversation is
	// appended to (#2984), and the session its model calls are booked on
	// (#2988); made before its first model call. "" only when that failed
	// (the saver tries again at its first round) or for a record an older
	// process wrote.
	Conversation string `json:"conversation,omitempty"`
	// Started is the start of the ledger turn of its own its calls are booked
	// on (the turn id is minted from it and Conversation), so a resumed run
	// books on the same turn (#2988). Zero for a record an older process
	// wrote: a resumed run then books on a new turn of the same session.
	Started time.Time `json:"started,omitzero"`
}

// SpawnConversations is where an API one-shot's conversation is kept: the
// session store's append-only JSONL files (#2984). cmd/foci-gw's
// sessionBranchAdapter implements it over *session.Store.
type SpawnConversations interface {
	// NewSpawnConversation creates an empty session file for a one-shot
	// spawned by parent (a fresh "spawn" branch) and returns its key.
	NewSpawnConversation(parent string) (key string, err error)
	// AppendSpawnConversation appends msgs to it, in one write.
	AppendSpawnConversation(key string, msgs []provider.Message) error
	// LoadSpawnConversation reads all its messages back.
	LoadSpawnConversation(key string) ([]provider.Message, error)
	// ResetSpawnConversation sets aside the file key (kept as an archive)
	// for an empty one under the same key, so a spawn whose file cannot be
	// read keeps its session (#2988).
	ResetSpawnConversation(key string) error
}

// spawnRawLostReason is why a raw one-shot is not resumed: its sandbox
// directory, its only working space, is wiped at start (tempdir.CleanStale).
const spawnRawLostReason = "a raw spawn's sandbox directory does not survive a restart"

// setOneShot marks parent's direct spawn id as an API one-shot that a restart
// resumes, and persists it.
func (t *SpawnTracker) setOneShot(parent, id string, rec *oneShotRecord) {
	if t == nil || rec == nil {
		return
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || !s.running {
		t.mu.Unlock()
		return
	}
	s.oneShot = rec
	t.mu.Unlock()
	t.persist(parent, id)
}

// conversationKey is the key of the session file parent's one-shot id keeps
// its conversation in, "" when it has none yet (or is not a one-shot).
func (t *SpawnTracker) conversationKey(parent, id string) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || s.oneShot == nil {
		return ""
	}
	return s.oneShot.Conversation
}

// setConversation records that parent's running one-shot id keeps its
// conversation in the session file key, and persists that. false: it is not
// a running one-shot (any more).
func (t *SpawnTracker) setConversation(parent, id, key string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || !s.running || s.oneShot == nil {
		t.mu.Unlock()
		return false
	}
	s.oneShot.Conversation = key
	t.mu.Unlock()
	t.persist(parent, id)
	return true
}

// oneShotSaver appends a running one-shot's conversation to its session file
// as it grows (#2984): each save writes only the messages after the ones
// already written, so a round costs its own bytes, whatever came before it.
type oneShotSaver struct {
	convs      SpawnConversations
	tr         *SpawnTracker
	parent, id string
	// written is how many messages of the conversation the file holds.
	written int
}

// save is the one-shot loop's checkpoint: msgs is its conversation so far.
// The file is made at the first save. A failed write is retried with the
// next save's messages, so the file never misses one in the middle.
func (w *oneShotSaver) save(msgs []provider.Message) {
	if w == nil || w.convs == nil || len(msgs) <= w.written {
		return
	}
	key := w.tr.conversationKey(w.parent, w.id)
	if key == "" {
		k, err := w.convs.NewSpawnConversation(w.parent)
		if err != nil {
			spawnLog.Warnf("spawn=%s create conversation file: %v", w.id, err)
			return
		}
		if !w.tr.setConversation(w.parent, w.id, k) {
			return
		}
		key = k
	}
	if err := w.convs.AppendSpawnConversation(key, msgs[w.written:]); err != nil {
		spawnLog.Warnf("spawn=%s save conversation to %s: %v", w.id, key, err)
		return
	}
	w.written = len(msgs)
}

// loadOneShotConversation is a one-shot's saved conversation, and the rounds
// of its loop it used (one per assistant message: maxLoops counts across a
// resume). No messages and no error: it has none yet (stopped before it saved
// any). unreadable: the file exists but cannot be read.
func loadOneShotConversation(convs SpawnConversations, id, key string) (msgs []provider.Message, loops int, unreadable bool) {
	if convs == nil || key == "" {
		return nil, 0, false
	}
	msgs, err := convs.LoadSpawnConversation(key)
	if err != nil {
		spawnLog.Errorf("spawn=%s saved conversation %s unreadable: %v", id, key, err)
		return nil, 0, true
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			loops++
		}
	}
	return msgs, loops, false
}

// finishOneShot ends a resumed one-shot's run. cut: its ctx ended before it
// did (a stop, or the shutdown). A run cut after BeginShutdown is
// interrupted: it is left as it is, and so is its persisted record, for the
// next process to resume. Otherwise it is done, and stopped says whether a
// stop ended it; dropped counts the spawn_send messages it could not take
// (a one-shot takes none).
func (t *SpawnTracker) finishOneShot(parent, id string, cut bool) (stopped, interrupted bool, dropped int) {
	if t == nil {
		return false, false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return false, false, 0
	}
	if cut && (t.shuttingDown || t.stopping[session.AgentIDFromAnyKey(parent)]) && !s.stopped {
		return false, true, 0
	}
	dropped = len(s.pending)
	s.pending = nil
	s.running = false
	s.ended = time.Now()
	t.pruneFinished(parent)
	return s.stopped, false, dropped
}

// trackOneShot registers the API one-shot ctx runs as with its direct spawn's
// tracker entry (the spawnLink runDirect put on ctx) and has p append its
// conversation to a session file (oneShotSaver). The file is made here,
// before the first model call, and p books every call on it, on a turn of its
// own (#2988); the spawn's link records it as where its spend is. A one-shot
// run outside a tracked direct spawn (no parent session), or with no
// conversation store, is not given one and books as before: on the invoking
// turn, else on a turn of its own on the session ctx names.
func trackOneShot(ctx context.Context, rec *oneShotRecord, p *oneShotParams) {
	link, ok := spawnLinkFrom(ctx)
	if !ok || link.s == nil {
		return
	}
	tr, convs := link.s.deps.Tracker, link.s.deps.Conversations
	if convs != nil {
		if key, err := convs.NewSpawnConversation(link.parent); err != nil {
			spawnLog.Warnf("spawn=%s create conversation file: %v (its calls book on %s)", link.id, err, SessionKeyFromContext(ctx))
		} else {
			rec.Conversation, rec.Started = key, time.Now()
			p.ownSession, p.ownTurnStart = key, rec.Started
			noteSpawnBooking(ctx, key, "", false)
		}
	}
	tr.setOneShot(link.parent, link.id, rec)
	w := &oneShotSaver{convs: convs, tr: tr, parent: link.parent, id: link.id}
	p.checkpoint = w.save
}

// apiOneShotRun is the ctx and parameters an API character or explore
// one-shot runs with. A character spawn's ctx carries its identity
// (rec.Self) and the character marker.
func (s *Spawner) apiOneShotRun(ctx context.Context, rec *oneShotRecord, prompt string) (context.Context, oneShotParams, error) {
	deps := s.deps
	client, model, format := s.resolveAPISpawn(rec.Mode, rec.Group)
	p := oneShotParams{
		client:         client,
		model:          model,
		format:         format,
		prompt:         prompt,
		sessions:       deps.Sessions,
		fallbackFn:     deps.FallbackFunc,
		clientProvider: deps.ClientProvider,
		routingFor:     deps.ProviderRoutingFor,
	}
	switch rec.Mode {
	case spawnModeCharacter:
		// The main session's prompt for the spawn's own key (#2981).
		if deps.SystemPrompt != nil {
			p.system = deps.SystemPrompt(rec.Self)
		}
		// ctx carries the one-shot's own spawnLink (runDirect, or the
		// restart's resume), so this is its own depth (#3061).
		p.toolDefs, p.tools = spawnToolSet(deps.Registry, spawnCharacterBlacklist, s.callerSpawnDepth(ctx) < MaxSpawnDepth())
		p.maxResultChars = spawnMaxResultChars
		p.maxLoops = deps.maxToolLoops()
		ctx = withSpawnSelf(WithSpawnCharacter(ctx), rec.Self, model)
	case spawnModeExplore:
		p.system = []provider.SystemBlock{{Type: "text", Text: exploreSystemPrompt}}
		p.toolDefs, p.tools = spawnExploreToolSet(deps.Registry)
		p.maxResultChars = spawnExploreMaxResultChars
		p.maxLoops = deps.exploreMaxDepth()
	default:
		return ctx, p, fmt.Errorf("spawn: %q is not a resumable one-shot mode", rec.Mode)
	}
	return ctx, p, nil
}

// cannotResumeOneShot says why a persisted API one-shot cannot run again, or
// "" when it can. host is the Spawner of the agent it runs as (nil: gone).
func cannotResumeOneShot(host *Spawner, rec spawnRecord) string {
	switch {
	case host == nil:
		return "its agent is not available"
	case host.deps.Delegated != nil:
		return "its agent no longer runs on the API"
	case rec.OneShot.Mode == spawnModeRaw:
		return spawnRawLostReason
	case rec.OneShot.Mode != spawnModeCharacter && rec.OneShot.Mode != spawnModeExplore:
		return fmt.Sprintf("%q is not a resumable one-shot mode", rec.OneShot.Mode)
	case rec.Resumes >= maxSpawnRestartResumes:
		return fmt.Sprintf("restarts had already interrupted it %d times", rec.Resumes)
	}
	return ""
}

// withRestartNotice is a one-shot's saved conversation with the restart
// notice added to its last message (always the user's: the task, or the
// results of its last tool calls), so the model knows tool calls of the round
// the restart cut may not have run, or may have run twice.
func withRestartNotice(msgs []provider.Message) []provider.Message {
	notice := provider.ContentBlock{Type: "text", Text: spawnRestartNotice("")}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	last := &out[len(out)-1]
	if last.Role != "user" {
		return append(out, provider.Message{Role: "user", Content: []provider.ContentBlock{notice}})
	}
	last.Content = append(append([]provider.ContentBlock(nil), last.Content...), notice)
	return out
}

// resumeOneShotAfterRestart runs a persisted API one-shot again (#2979): from
// its saved conversation, or from its task when it had saved none, on the
// agent it runs as. Its progress shows as a direct spawn's does; its answer
// goes to its parent as a [SPAWN RESULT]. stop (base's cancel) is its stop
// handle from the start.
func (s *Spawner) resumeOneShotAfterRestart(host *Spawner, rec spawnRecord, base context.Context, stop context.CancelFunc) {
	tr, obs := s.deps.Tracker, s.deps.Observer
	parent, id := rec.Parent, rec.ID
	running := recInfo(rec)

	runCtx, cancelRun := context.WithCancel(base)
	defer cancelRun()
	tr.setCancel(parent, id, func() { cancelRun(); stop() })
	ctx := WithSessionKey(runCtx, parent)
	if host != s {
		ctx = crossAgentContext(ctx, host.deps.AgentID, parent)
	}
	ctx = withSpawnLink(ctx, spawnLink{s: s, parent: parent, id: id, info: running})

	tr.appendLog(parent, id, "message", "(resumed after a foci restart)")
	if obs != nil {
		shown := running
		shown.Name = "(resumed after restart) " + running.Label()
		obs.SpawnStarted(parent, shown, spawnRestartNotice(rec.Prompt))
	}
	sink := &spawnTextSink{BufferSink: turnevent.NewBufferSink(), obs: obs, tracker: tr, parent: parent, id: id}

	ctx, p, err := host.apiOneShotRun(ctx, rec.OneShot, rec.Prompt)
	var answer string
	if err == nil {
		w := &oneShotSaver{convs: s.deps.Conversations, tr: tr, parent: parent, id: id}
		key := rec.OneShot.Conversation
		saved, loops, unreadable := loadOneShotConversation(w.convs, id, key)
		switch {
		case len(saved) > 0:
			// The file holds saved; the notice goes on in memory only, and
			// the next round appends after it.
			p.history, p.startLoop = withRestartNotice(saved), loops
			w.written = len(saved)
		default:
			p.prompt = spawnRestartNotice(rec.Prompt)
			if unreadable {
				// Set the file aside for an empty one under the same key,
				// rather than append to it: its session stays the spawn's
				// (#2988). Failing that, the next round starts a new file.
				if err := w.convs.ResetSpawnConversation(key); err != nil {
					spawnLog.Errorf("spawn=%s reset unreadable conversation %s: %v", id, key, err)
					tr.setConversation(parent, id, "")
					key = ""
				}
			}
		}
		if key != "" && w.convs != nil {
			// Its calls book on the session and turn they booked on before
			// the restart (#2988).
			p.ownSession, p.ownTurnStart = key, rec.OneShot.Started
		}
		p.checkpoint = w.save
		spawnLog.Infof("resume one-shot spawn=%s mode=%s parent=%s after restart (resume %d) saved_messages=%d",
			id, rec.OneShot.Mode, parent, rec.Resumes+1, len(p.history))
		answer, err = spawnOneShot(turnevent.WithTee(ctx, sink), p)
	}

	stopped, interrupted, dropped := tr.finishOneShot(parent, id, runCtx.Err() != nil)
	if interrupted {
		spawnLog.Infof("spawn=%s one-shot cut short by shutdown — left for the next process to resume", id)
		return
	}
	switch {
	case stopped:
		tr.setOutcome(parent, id, "stopped")
	case err != nil:
		tr.setOutcome(parent, id, "failed")
		tr.appendLog(parent, id, "error", err.Error())
	default:
		tr.setOutcome(parent, id, "completed")
	}
	if obs != nil {
		obs.SpawnEnded(parent, id)
	}
	s.deliver(parent, id, s.resultMessage(parent, id, "", stopped, err, answer, dropped))
}
