package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/procx"
)

// Close timeouts.
var (
	closeGracefulWait = 5 * time.Second
)

// Start launches the Codex app-server subprocess and performs the
// initialize handshake. If opts.ResumeSessionID is set, the thread is
// resumed; otherwise a new thread is started.
func (b *Backend) Start(ctx context.Context, opts delegator.StartOptions) error {
	b.startOpts = opts
	b.workDir = opts.WorkDir
	b.agentID = opts.AgentID
	b.label = opts.Label
	if b.label == "" {
		b.label = opts.AgentID
	}
	b.readyCh = make(chan struct{})
	b.readyOnce = sync.Once{}
	b.pendingPerms = make(map[int64]*pendingApproval)
	b.itemCache = make(map[string]itemEnvelope)
	b.autoApproveRules = autoapprove.Compile(opts.AutoApproveRules)
	// Obtaining a connection is the ONLY thing that legitimately differs
	// between a facade attaching to a pooled app-server and the owner that
	// launches one. Everything after it — resolve the model, record the
	// effort, then start or resume the thread — is identical, so it is written
	// once below rather than duplicated per path. Two hand-maintained copies
	// of that sequence is what produced #1573, where the attach path returned
	// before ever resolving the model or recording the effort.
	// Serialise acquire PER AGENT (#1718), the same shape opencode needs — see
	// package keyedmutex. Codex already claims its pool slot under sharedPool
	// BEFORE launching, which is most of the fix; the residual hole is the
	// predicate below. Between `sharedPool.servers[id] = b` and `b.running =
	// true` (set right after cmd.Start()) the owner EXISTS but is not "running",
	// so a second caller read the slot as free and OVERWROTE it with its own
	// facade, then launched a second app-server. That window is an exec plus a
	// few field assignments rather than opencode's 13-second health probe, which
	// is why it has fired once (2026-07-16) against opencode's ten — same defect,
	// ~10^7 less exposure.
	//
	// Scoped to the acquire, NOT deferred to the end of Start: the shared tail
	// below (applyModelAndEffort, thread start/resume) must not hold a per-agent
	// lock, or concurrent sessions on one agent would serialise their turn
	// starts — a behaviour change, not a fix.
	unlockAcquire := acquireLocks.Lock(opts.AgentID)
	sharedPool.Lock()
	owner := sharedPool.servers[opts.AgentID]
	attached := owner != nil && owner.IsRunning()
	if attached {
		sharedPool.refs[opts.AgentID]++
		b.shared = owner
		// Deliberately NO catalogue copy here, and no model/list re-run: the
		// owner already listed on this same connection, and b.catalogue()
		// reads through process() so this facade always sees the owner's
		// CURRENT catalogue. Copying the slice here was a race (this path
		// holds only sharedPool, never owner.mu, which is what guards the
		// field) and went permanently stale the first time the owner
		// refreshed (#1577).
	} else {
		sharedPool.servers[opts.AgentID] = b
		sharedPool.refs[opts.AgentID] = 1
	}
	sharedPool.Unlock()

	if !attached {
		if err := b.launchAppServer(ctx, opts); err != nil {
			unlockAcquire()
			return err
		}
	}
	unlockAcquire()

	// --- shared tail: from here the connection exists, whichever path got us
	// one, and b.Close() is the correct teardown for both. On the attach path
	// it gives back the pool ref taken above (without it a transient failure
	// leaves refs permanently high, closeIdle never reaches zero, and the
	// app-server is never reaped). On the owner path it does everything a bare
	// cancel() did AND removes the pool entry this Start installed, which a
	// cancel() alone left behind. Close is idempotent per facade, so a later
	// Close by the caller is a safe no-op either way.
	if err := b.applyModelAndEffort(ctx, opts); err != nil {
		_ = b.Close()
		return err
	}
	if opts.BatchOnly {
		b.readyOnce.Do(func() { close(b.readyCh) })
		return nil
	}
	if opts.ResumeSessionID != "" {
		var err error
		if opts.Ephemeral {
			err = b.adoptEphemeralFork(opts.ResumeSessionID)
		} else {
			err = b.resumeThread(opts.ResumeSessionID)
		}
		if err != nil {
			_ = b.Close()
			return fmt.Errorf("codex: resume thread %s: %w", opts.ResumeSessionID, err)
		}
		b.logInfof("resumed thread %s", opts.ResumeSessionID)
	} else {
		tid, err := b.startThread()
		if err != nil {
			_ = b.Close()
			return fmt.Errorf("codex: start thread: %w", err)
		}
		b.logInfof("started thread %s", tid)
	}
	return nil
}

// launchAppServer spawns the codex app-server subprocess for an agent that has
// no pooled one yet, wires its pipes and reader goroutine, performs the
// initialize handshake, and discovers the model catalogue. It covers exactly
// the "obtain a connection" step that a facade gets for free by attaching —
// nothing that belongs to a session, which is Start's shared tail.
//
// The caller has already installed b as the pool owner, so a failure here must
// tear that down; each error path cancels the process context, and Start's
// tail uses b.Close() thereafter.
//
// ctx is Start's caller context and bounds only the pre-spawn hook-trust probe
// (prepareHookArgs spawns a throwaway app-server to ask codex for the key it
// assigns our hook config). The long-lived subprocess deliberately gets its own
// context.Background()-rooted cmdCtx: it outlives the Start call.
func (b *Backend) launchAppServer(ctx context.Context, opts delegator.StartOptions) error {
	b.pendingRPC = make(map[int64]chan rpcReply)
	b.sessionThreads = make(map[string]string)
	b.threadSessions = make(map[string]string)
	b.threadBackends = make(map[string]*Backend)
	b.autoApproveEnv = autoapprove.EnvironmentFromList(b.buildEnv())

	bin := b.codexBinary()
	if _, err := procx.LookPath(procx.Operator, bin); err != nil {
		return fmt.Errorf("codex: binary %q not found: %w", bin, err)
	}

	// Resolve the compaction prompt ONCE at launch and pass it as a `-c`
	// override (see appServerArgs). Doing this per-process at launch — rather
	// than writing key "compact_prompt" to the shared ~/.codex/config.toml
	// before each compaction — keeps foci's prompt out of the user's own codex
	// CLI config and every other codex agent, and removes the concurrent
	// last-writer-wins race (#1327 sub-issue 2). CompactionPromptFunc is a
	// fresh read of compaction-summary.md, static within a session and
	// re-resolved on every (re)start, so freezing it at launch loses nothing.
	var compactPrompt string
	if opts.CompactionPromptFunc != nil {
		compactPrompt = opts.CompactionPromptFunc("")
	}

	args := b.appServerArgs(compactPrompt)
	// The session-env hook must be configured at spawn: hook config and its
	// trust state are session flags on this argv, and there is no way to add
	// either to a running app-server. See hooks.go.
	args = append(args, b.prepareHookArgs(ctx)...)

	cmdCtx, cancel := context.WithCancel(context.Background())
	// Operator: the codex agent backend.
	cmd := procx.Spawn(cmdCtx, procx.Operator, bin, args...)
	cmd.Dir = b.workDir

	// Build env
	cmd.Env = b.buildEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("codex: stdout pipe: %w", err)
	}

	b.logInfof("launching: %s %s (workdir=%s)", bin, strings.Join(args, " "), opts.WorkDir)

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("codex: start failed: %w", err)
	}

	b.mu.Lock()
	b.cmd = cmd
	b.cancel = cancel
	b.writer = NewWriter(stdin)
	b.running = true
	b.done = make(chan struct{})
	b.mu.Unlock()

	// Start the reader goroutine.
	go func() {
		b.readStream(cmdCtx, stdout)
		// Reap the process.
		if err := cmd.Wait(); err != nil {
			b.logDebugf("process exited: %v", err)
		}
	}()

	// Perform initialize handshake.
	if err := b.initialize(); err != nil {
		cancel()
		return fmt.Errorf("codex: initialize failed: %w", err)
	}

	// model/list is available only after initialize. Catalogue discovery is
	// best-effort: a failure must not prevent the user's coding session from
	// starting, while a success populates foci's backend-scoped live registry.
	if err := b.refreshModelCaps(); err != nil {
		b.logWarnf("model/list failed (using persisted/static model details): %v", err)
	}

	// The usage allowance, for the status block before the first turn.
	// Off the start path: it is informational and must not delay a session.
	go b.seedRateLimits()

	return nil
}

// appServerArgs applies the configured sandbox, defaulting to workspace-write. Thread
// creation and resume both explicitly apply the resolved per-agent sandbox;
// read-only batches remain read-only even on unrestricted agents. Escalations
// use the configured reviewer (automatic review by default, or user prompts).
//
// When compactPrompt is non-empty it is layered in as a per-process
// `-c compact_prompt=<value>` config override (the value parses as TOML, via
// tomlBasicString). This is an in-memory
// override scoped to THIS app-server process; it never touches the shared
// ~/.codex/config.toml, so foci's compaction prompt can't leak into the user's
// own codex CLI or race a concurrent codex agent (#1327 sub-issue 2).
//
// agents.enabled=false removes codex's own spawn_agent tools, for new and resumed
// threads alike (live on 0.166.0). OpenAI encrypts those tools' messages, so foci
// cannot show them (#2839). A codex agent uses foci_spawn instead. It is a
// process flag for the same reason as compact_prompt: the user's own codex keeps
// its subagents.
func appServerArgs(compactPrompt, sandbox, reviewer, approval string) []string {
	if sandbox == "" {
		sandbox = defaultSandboxMode
	}
	if approval == "" {
		approval = defaultApprovalPolicy
	}
	if reviewer == "" {
		reviewer = defaultApprovalsReviewer
	}
	args := []string{
		"app-server", "-c", "sandbox_mode=" + sandbox,
		"-c", "approval_policy=" + approval,
		"-c", "approvals_reviewer=" + reviewer,
		"-c", "agents.enabled=false",
	}
	if compactPrompt != "" {
		args = append(args, "-c", "compact_prompt="+tomlBasicString(compactPrompt))
	}
	return args
}

// WaitReady blocks until the backend is ready (handshake complete).
func (b *Backend) WaitReady(ctx context.Context) error {
	select {
	case <-b.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CheckReady verifies the codex binary is installed.
func (b *Backend) CheckReady(ctx context.Context) (bool, error) {
	bin := b.codexBinary()
	if _, err := procx.LookPath(procx.Operator, bin); err != nil {
		return false, fmt.Errorf("codex binary %q not found: %w", bin, err)
	}
	return true, nil
}

// Close shuts down the app-server subprocess gracefully.
func (b *Backend) Close() error {
	// Per-facade idempotence, and read threadID under b.mu: the reader
	// goroutine writes it in onThreadStarted, so the old unlocked read at the
	// unregisterThread call below was a genuine data race (caught by -race).
	b.mu.Lock()
	if b.facadeClosed {
		b.mu.Unlock()
		return nil
	}
	b.facadeClosed = true
	threadID := b.threadID
	b.mu.Unlock()

	sharedPool.Lock()
	owner := b.process()
	key := b.agentID
	// `last` must mean "this call took the count to zero", not "the count is
	// zero". refs never goes below zero, so the old form made every repeated
	// Close report last=true and evict a live server from the pool.
	var last bool
	if sharedPool.servers[key] == owner && sharedPool.refs[key] > 0 {
		sharedPool.refs[key]--
		last = sharedPool.refs[key] == 0
	}
	if last {
		delete(sharedPool.refs, key)
		// Evict only OURSELVES. If our app-server died and a later Start
		// installed a new owner under the same agent id, the pooled entry
		// belongs to that live server -- deleting it would orphan a running
		// process and make the next Start spawn a second one.
		if sharedPool.servers[key] == owner {
			delete(sharedPool.servers, key)
		}
	}
	sharedPool.Unlock()
	if !last {
		// The app-server lives on for sibling sessions, so this thread must be
		// handed back to it explicitly, or codex keeps it loaded — with its
		// rollout and writer-lock files open — for the app-server's whole life
		// (release.go).
		//
		// No process exit ends this facade's running turn, as it does for the
		// last close (onReaderStopped) and for Claude Code: stop it in codex
		// first, then complete it here (#3166).
		running := b.interruptOnClose()
		b.releaseThread(threadID)
		if running {
			b.completeClosedTurn()
		}
		return nil
	}
	b = owner
	b.mu.Lock()
	b.closing = true
	cancel := b.cancel
	wr := b.writer
	// Re-read, not re-declare: b is the OWNER now, and its thread is not the
	// facade's thread that was read at the top of this function.
	threadID = b.threadID
	b.mu.Unlock()

	// The owner's own thread never goes through unregisterThread — the whole
	// process and its maps are being torn down — so this is the one place the
	// session-env unbind still stands alone. Every facade thread
	// unbinds via unregisterThread, and refs hitting zero means they all
	// already have.
	b.unbindThreadEnv(threadID)

	if wr != nil {
		_ = wr.Close()
	}
	if cancel != nil {
		// Give the process a moment to exit cleanly, then force.
		go func() {
			time.AfterFunc(closeGracefulWait, func() {
				b.mu.Lock()
				cmd := b.cmd
				b.mu.Unlock()
				if cmd != nil && cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			})
			cancel()
		}()
	}

	if b.done != nil {
		select {
		case <-b.done:
		case <-time.After(10 * time.Second):
			b.logWarnf("reader goroutine did not exit within 10s")
		}
	}

	return nil
}

// interruptOnClose stops this facade's running turn in codex when the facade
// closes on an app-server that lives on, and reports whether a turn was
// running. Without it, codex goes on running (and billing) a turn that nothing
// reads any more: its thread is unsubscribed, so its events are dropped.
func (b *Backend) interruptOnClose() bool {
	b.turnMu.Lock()
	active := b.turnActive
	b.turnMu.Unlock()
	if !active {
		return false
	}
	if err := b.Interrupt(context.Background()); err != nil {
		b.logWarnf("close: interrupt of the running turn failed (codex may run it to its end): %v", err)
	}
	return true
}

// completeClosedTurn completes, as interrupted, a turn still open after its
// facade released its thread (#3166). The agent's turn waits for this
// completion, not for the caller's ctx: before it, a direct spawn's batch
// whose caller had gone (RunBatch closes its backend) never returned, and the
// spawn stayed 'running' in spawn_status. The turn/completed codex sends for
// the interrupt usually arrives first and completes it; once the thread is
// released nothing else can, so this is the last word.
func (b *Backend) completeClosedTurn() {
	b.turnMu.Lock()
	active := b.turnActive
	text := b.turnText.String()
	b.turnMu.Unlock()
	if !active {
		return
	}
	b.completeTurn(&delegator.TurnResult{Text: text,
		Interrupted: delegator.TurnInterrupted("codex session closed mid-turn")})
}

// interruptTimeout bounds the wait for codex's turn/interrupt answer. Live
// (codex 0.167.0) the answer comes in ~10ms; codex never answers one that
// arrives while the thread is idle (it parks it), so the wait must be bounded.
const interruptTimeout = 10 * time.Second

// Interrupt stops the running turn. turn/interrupt is a JSON-RPC REQUEST
// whose params require threadId AND turnId (codex 0.167.0
// generate-json-schema: ClientRequest.json, v2/TurnInterruptParams.json);
// sent as a notification, or without turnId, codex ignores it (#3117, live
// probe on the work Mac). With no running turn nothing is sent: codex would
// not answer an idle-thread interrupt, so it is a clean no-op.
func (b *Backend) Interrupt(ctx context.Context) error {
	p := b.process()
	p.mu.Lock()
	wr := p.writer
	p.mu.Unlock()
	if wr == nil {
		return errors.New("codex: backend not started")
	}
	threadID := b.SessionIDFor(b.startOpts.SessionKey)
	turnID := b.currentTurnID()
	if b.startOpts.SessionKey == "" || threadID == "" || turnID == "" {
		b.logDebugf("interrupt: no running turn (thread=%q turn=%q), nothing to stop", threadID, turnID)
		return nil
	}
	_, err := b.sendAndWaitTimeout(ctx, "turn/interrupt", turnInterruptParams{
		ThreadID: threadID,
		TurnID:   turnID,
	}, interruptTimeout)
	if err == nil {
		b.logInfof("interrupt: turn %s on thread %s stopped", turnID, threadID)
		return nil
	}
	// The turn ended (or a new one began) while the request was in flight:
	// codex's rejection or silence is about a turn that is already gone.
	if b.currentTurnID() != turnID {
		b.logDebugf("interrupt: turn %s ended before codex answered: %v", turnID, err)
		return nil
	}
	b.logWarnf("interrupt: turn/interrupt for turn %s on thread %s failed: %v", turnID, threadID, err)
	return fmt.Errorf("codex: turn/interrupt: %w", err)
}

// currentTurnID is the codex turnId of the turn running on this facade, or "".
func (b *Backend) currentTurnID() string {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnID
}

// --- Protocol handshake ---

// nextID returns the next JSON-RPC request ID.
func (b *Backend) nextID() int64 {
	p := b.process()
	p.rpcMu.Lock()
	defer p.rpcMu.Unlock()
	p.rpcSeq++
	return p.rpcSeq
}

// sendAndWait sends a JSON-RPC request and waits up to 30s for its response.
func (b *Backend) sendAndWait(method string, params interface{}) (json.RawMessage, error) {
	return b.sendAndWaitTimeout(context.Background(), method, params, 30*time.Second)
}

// sendAndWaitTimeout sends a JSON-RPC request and waits for its response, up
// to timeout or until ctx ends.
func (b *Backend) sendAndWaitTimeout(ctx context.Context, method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	id := b.nextID()
	ch := make(chan rpcReply, 1)

	p := b.process()
	p.rpcMu.Lock()
	p.pendingRPC[id] = ch
	p.rpcMu.Unlock()

	if err := p.writer.sendRequest(method, params, id); err != nil {
		p.rpcMu.Lock()
		delete(p.pendingRPC, id)
		p.rpcMu.Unlock()
		return nil, err
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, reply.err
		}
		if reply.result == nil {
			return nil, errors.New("codex: request cancelled (process exited)")
		}
		return reply.result, nil
	case <-time.After(timeout):
		p.rpcMu.Lock()
		delete(p.pendingRPC, id)
		p.rpcMu.Unlock()
		return nil, fmt.Errorf("codex: %s timed out", method)
	case <-ctx.Done():
		p.rpcMu.Lock()
		delete(p.pendingRPC, id)
		p.rpcMu.Unlock()
		return nil, fmt.Errorf("codex: %s: %w", method, ctx.Err())
	}
}

// initialize performs the JSON-RPC initialize handshake.
func (b *Backend) initialize() error {
	v := "dev"
	if fv, ok := b.cfg[delegator.FociVersionConfigKey].(string); ok && fv != "" {
		v = fv
	}
	params := initializeParams{
		ClientInfo: clientInfo{
			Name:    "foci",
			Title:   "Foci",
			Version: v,
		},
	}
	raw, err := b.sendAndWait("initialize", params)
	if err != nil {
		return err
	}
	b.noteInitializeVersion(raw)
	// Acknowledge initialization.
	return b.process().writer.sendNotification("initialized", struct{}{})
}

// startThread creates a new thread and stores the thread ID.
func (b *Backend) startThread() (string, error) {
	params := threadStartParams{
		Config:            b.writableRootsConfig(),
		ApprovalsReviewer: b.approvalsReviewer(),
		Cwd:               b.workDir,
		Sandbox:           b.sandboxMode(),
		BaseInstructions:  b.startOpts.SystemPrompt,
		Ephemeral:         b.startOpts.Ephemeral,
	}
	if m := b.modelFromOpts(); m != "" {
		params.Model = m
	}
	result, err := b.sendAndWait("thread/start", params)
	if err != nil {
		return "", err
	}
	var tr threadResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return "", fmt.Errorf("codex: parse thread/start response: %w", err)
	}
	if err := b.verifyReviewer("thread/start", tr); err != nil {
		return "", err
	}
	b.mu.Lock()
	b.threadID = tr.Thread.ID
	b.sessionFilePath = tr.Thread.Path
	if tr.Thread.Status.Type != "" {
		b.threadStatus = tr.Thread.Status
	}
	b.model = tr.Model
	if b.model == "" {
		b.model = params.Model
	}
	b.mu.Unlock()
	b.registerThread(b.startOpts.SessionKey, tr.Thread.ID)
	b.readyOnce.Do(func() { close(b.readyCh) })
	if b.onSessionReady != nil {
		b.onSessionReady(tr.Thread.ID)
	}
	return tr.Thread.ID, nil
}

// verifyReviewer fails closed if a server ignores the explicit posture. In
// particular, re-resuming an already-loaded thread can retain its old reviewer.
func (b *Backend) verifyReviewer(method string, result threadResult) error {
	want := b.approvalsReviewer()
	if result.ApprovalsReviewer != want {
		return fmt.Errorf("codex: %s approval reviewer %q, want %q", method, result.ApprovalsReviewer, want)
	}
	return nil
}

// resumeThread resumes an existing thread.
func (b *Backend) resumeThread(threadID string) error {
	params := threadResumeParams{
		Config:            b.writableRootsConfig(),
		ApprovalsReviewer: b.approvalsReviewer(),
		ThreadID:          threadID,
		Sandbox:           b.sandboxMode(),
		BaseInstructions:  b.startOpts.SystemPrompt,
	}
	result, err := b.sendAndWait("thread/resume", params)
	// A thread foci released (release.go) unloads 60s later; a resume that
	// lands in that instant is refused until the unload finishes. Wait it out
	// rather than fall back to a fresh thread and lose the history.
	for deadline := time.Now().Add(resumeClosingRetryFor); isThreadClosing(err) && time.Now().Before(deadline); {
		b.logInfof("thread/resume %s: codex is still unloading it; retrying", threadID)
		time.Sleep(resumeClosingRetryEvery)
		result, err = b.sendAndWait("thread/resume", params)
	}
	if err != nil {
		return err
	}
	var tr threadResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return fmt.Errorf("codex: parse thread/resume response: %w", err)
	}
	if err := b.verifyReviewer("thread/resume", tr); err != nil {
		return err
	}
	b.noteResumedWritableRoots(threadID, tr.Sandbox)
	b.mu.Lock()
	b.threadID = threadID
	b.sessionFilePath = tr.Thread.Path
	if tr.Thread.Status.Type != "" {
		b.threadStatus = tr.Thread.Status
	}
	// thread/resume returns the thread's model (required in codex 0.164.0's
	// ThreadResumeResponse), exactly as thread/start does. Without it a resumed
	// session booked every call with model "" — no rate group, NULL cost.
	if tr.Model != "" {
		b.model = tr.Model
	}
	b.mu.Unlock()
	b.registerThread(b.startOpts.SessionKey, threadID)
	b.readyOnce.Do(func() { close(b.readyCh) })
	if b.onSessionReady != nil {
		b.onSessionReady(threadID)
	}
	return nil
}

// requestedModelFromOpts returns the unresolved model requested by this
// session's StartOptions or backend configuration.
func (b *Backend) requestedModelFromOpts() string {
	if b.startOpts.Model != "" {
		return b.startOpts.Model
	}
	if v, ok := b.cfg["model"].(string); ok {
		return v
	}
	return ""
}

// modelFromOpts returns the catalogue-resolved launch model when Start has
// prepared one, falling back to the raw option only for direct unit callers.
func (b *Backend) modelFromOpts() string {
	b.mu.Lock()
	resolved := b.launchModel
	b.mu.Unlock()
	if resolved != "" {
		return resolved
	}
	return b.requestedModelFromOpts()
}

// applyModelAndEffort performs the per-session setup that must happen before a
// thread exists: resolve the requested model against the catalogue, and record
// the requested reasoning effort for the first turn to apply.
//
// Both of Start's paths need this — the owner that launches the app-server and
// every facade that attaches to an already-running one — so it lives in one
// place. Splitting it across two launch sequences is exactly how #1573 arose.
// It does NOT call refreshModelCaps: a facade inherits the owner's
// catalogueModels, so re-listing would be redundant work on a shared
// connection.
func (b *Backend) applyModelAndEffort(ctx context.Context, opts delegator.StartOptions) error {
	if err := b.prepareConfiguredModel(ctx, opts.ResumeSessionID != ""); err != nil {
		return fmt.Errorf("codex: resolve configured model: %w", err)
	}
	b.mu.Lock()
	if opts.Effort != "" && opts.Effort != "off" {
		b.pendingEffort = opts.Effort
	}
	b.mu.Unlock()
	return nil
}

// prepareConfiguredModel resolves backend_config.model (or a per-session
// ModelFunc override) after model/list has populated the catalogue. Resumed
// threads receive it on their next turn/start because thread/resume has no
// model field; fresh threads receive it directly in thread/start.
func (b *Backend) prepareConfiguredModel(ctx context.Context, resumed bool) error {
	requested := b.requestedModelFromOpts()
	if requested == "" {
		return nil
	}
	resolution, err := b.ResolveModel(ctx, requested)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.launchModel = resolution.BackendModel
	if resumed {
		b.pendingModel = resolution.BackendModel
		b.modelSwitchPending = true
	}
	b.mu.Unlock()
	return nil
}

// --- CompactionWaiter ---

// ArmCompactionWait arms the compaction completion signal. Must be called
// before triggering compaction (via the /compact slash command path) so
// that the contextCompaction item/completed event is not missed.
func (b *Backend) ArmCompactionWait() {
	b.compactMu.Lock()
	defer b.compactMu.Unlock()
	b.compactDoneCh = make(chan error, 1)
	b.compactSent = false
	b.compactTurnID = ""
}

// WaitForCompaction blocks until the armed compaction settles or ctx
// expires. It returns nil when the contextCompaction item completes,
// delegator.ErrCompactionStopped when codex reports the compaction turn
// interrupted (a /stop), and another error when that turn fails, ends
// without compacting, or the connection drops (#3172). Before #3172 only the
// item released it, so an interrupted compaction held the session's input
// for the whole delegatedCompactTimeout.
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.compactMu.Lock()
	ch := b.compactDoneCh
	b.compactMu.Unlock()
	if ch == nil {
		return nil // not armed
	}
	select {
	case err := <-ch:
		b.compactMu.Lock()
		if b.compactDoneCh == ch {
			b.compactDoneCh = nil
			b.compactSent = false
			b.compactTurnID = ""
		}
		b.compactMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resolveCompactionLocked records the armed wait's outcome. The first
// outcome wins: a contextCompaction item that completed before its turn's
// turn/completed keeps its nil. Caller holds compactMu.
func (b *Backend) resolveCompactionLocked(err error) {
	if b.compactDoneCh == nil {
		return
	}
	select {
	case b.compactDoneCh <- err:
	default:
	}
}

// noteCompactionTurn records turnID as the armed compaction's turn: the
// first turn to start after thread/compact/start went out. An explicit
// contextCompaction item/started (override=true) names it authoritatively.
func (b *Backend) noteCompactionTurn(turnID string, override bool) {
	if turnID == "" {
		return
	}
	b.compactMu.Lock()
	defer b.compactMu.Unlock()
	if b.compactDoneCh == nil || !b.compactSent {
		return
	}
	if b.compactTurnID == "" || override {
		b.compactTurnID = turnID
	}
}

// onCompactionTurnEnded releases the armed wait when the compaction's own
// turn ends. A contextCompaction item that completed first has already
// resolved it with success, so this changes nothing then. Otherwise the turn
// ended without compacting: interrupted means stopped (codex's turn_aborted
// on a /stop), failed carries codex's error, and a completed turn with no
// compaction item is reported as such rather than waited on.
func (b *Backend) onCompactionTurnEnded(turnID, status, failMsg string) {
	b.compactMu.Lock()
	defer b.compactMu.Unlock()
	if b.compactDoneCh == nil || b.compactTurnID == "" || turnID != b.compactTurnID {
		return
	}
	var err error
	switch status {
	case "interrupted":
		err = delegator.ErrCompactionStopped
	case "failed":
		err = fmt.Errorf("codex compaction turn failed: %s", failMsg)
	default:
		err = fmt.Errorf("codex compaction turn ended (%s) without a contextCompaction item", status)
	}
	b.resolveCompactionLocked(err)
}

// failCompactionWait releases an armed wait that can no longer complete
// (the app-server connection ended).
func (b *Backend) failCompactionWait(err error) {
	b.compactMu.Lock()
	defer b.compactMu.Unlock()
	b.resolveCompactionLocked(err)
}

// triggerCompaction sends thread/compact/start to the app-server. The
// request returns immediately; progress streams as contextCompaction item
// notifications. WaitForCompaction blocks on the completion signal.
func (b *Backend) triggerCompaction() error {
	threadID := b.SessionIDFor(b.startOpts.SessionKey)
	if b.startOpts.SessionKey == "" || threadID == "" {
		return errors.New("codex: no active thread to compact")
	}

	// The compaction prompt is set once at launch via `-c compact_prompt=`
	// (see Start / appServerArgs), NOT written to the shared config here —
	// that global-config write leaked foci's prompt into the user's own codex
	// CLI and raced concurrent agents (#1327 sub-issue 2). thread/compact/start
	// consumes the process-local override.
	//
	// compactSent is set before the request: codex can send the compaction
	// turn's turn/started before this request's reply arrives.
	b.compactMu.Lock()
	if b.compactDoneCh != nil {
		b.compactSent = true
		b.compactTurnID = ""
	}
	b.compactMu.Unlock()
	_, err := b.sendAndWait("thread/compact/start", compactStartParams{ThreadID: threadID})
	return err
}

// buildEnv constructs the environment for the app-server subprocess.
func (b *Backend) buildEnv() []string {
	env := environ()
	if key, ok := b.cfg["api_key"].(string); ok && key != "" {
		env = append(env, "CODEX_API_KEY="+key)
	}
	for k, v := range b.startOpts.Env {
		env = append(env, k+"="+v)
	}
	return env
}
