package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
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
	b.subagents = newSubagentTracker()
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
		if err := b.resumeThread(opts.ResumeSessionID); err != nil {
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
	b.batchRuns = make(map[string]*batchRun)
	b.autoApproveEnv = autoapprove.EnvironmentFromList(b.buildEnv())

	bin := b.codexBinary()
	if _, err := exec.LookPath(bin); err != nil {
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

	args := appServerArgs(compactPrompt)
	// The session-env hook must be configured at spawn: hook config and its
	// trust state are session flags on this argv, and there is no way to add
	// either to a running app-server. See hooks.go.
	args = append(args, b.prepareHookArgs(ctx)...)

	cmdCtx, cancel := context.WithCancel(context.Background())
	cmd := procx.Spawn(cmdCtx, bin, args...)
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

	return nil
}

// appServerArgs keeps Codex from starting its nested bwrap sandbox: foci already
// runs inside an outer sandbox that deliberately removes setuid/capability assumptions.
//
// When compactPrompt is non-empty it is layered in as a per-process
// `-c compact_prompt=<value>` config override (the value parses as TOML — same
// escaping the batch runner's `-c instructions=` uses). This is an in-memory
// override scoped to THIS app-server process; it never touches the shared
// ~/.codex/config.toml, so foci's compaction prompt can't leak into the user's
// own codex CLI or race a concurrent codex agent (#1327 sub-issue 2).
func appServerArgs(compactPrompt string) []string {
	args := []string{"app-server", "-c", "sandbox_policy.mode=danger-full-access"}
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
	if _, err := exec.LookPath(bin); err != nil {
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
	if sharedPool.refs[key] > 0 {
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
		// Drop this facade's open subagent runs. The shared connection outlives
		// us while siblings keep the app-server alive, so a child's late
		// notifications would otherwise still resolve to a facade whose UI is
		// gone.
		if b.subagents != nil {
			b.subagents.stopAll()
		}
		if threadID != "" {
			owner.unregisterThread(threadID)
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
	// session-env unbind still stands alone. Every facade and batch thread
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

// Interrupt cancels the current turn.
func (b *Backend) Interrupt(ctx context.Context) error {
	p := b.process()
	p.mu.Lock()
	wr := p.writer
	p.mu.Unlock()
	if wr == nil {
		return errors.New("codex: backend not started")
	}
	// turn/interrupt is a notification (no response expected).
	threadID := b.SessionIDFor(b.startOpts.SessionKey)
	if b.startOpts.SessionKey == "" || threadID == "" {
		return errors.New("codex: no active thread")
	}
	return wr.sendNotification("turn/interrupt", struct {
		ThreadID string `json:"threadId"`
	}{ThreadID: threadID})
}

// SendKeystroke/SendSpecialKey — no TUI in app-server mode.
func (b *Backend) SendKeystroke(ctx context.Context, key string) error {
	return errNoTUI
}
func (b *Backend) SendSpecialKey(ctx context.Context, key string) error {
	return errNoTUI
}

var errNoTUI = errors.New("codex: app-server mode does not support keystroke input")

// Capabilities advertises what the Codex backend supports.
func (b *Backend) Capabilities() delegator.Capabilities {
	return delegator.Capabilities{
		Streaming:      true,
		PostToolNudge:  false,
		PreAnswerNudge: false,
	}
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

// sendAndWait sends a JSON-RPC request and waits for its response.
func (b *Backend) sendAndWait(method string, params interface{}) (json.RawMessage, error) {
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
	case <-time.After(30 * time.Second):
		p.rpcMu.Lock()
		delete(p.pendingRPC, id)
		p.rpcMu.Unlock()
		return nil, fmt.Errorf("codex: %s timed out", method)
	}
}

// initialize performs the JSON-RPC initialize handshake.
func (b *Backend) initialize() error {
	v := "dev"
	if fv, ok := b.cfg["foci_version"].(string); ok && fv != "" {
		v = fv
	}
	params := initializeParams{
		ClientInfo: clientInfo{
			Name:    "foci",
			Title:   "Foci",
			Version: v,
		},
	}
	_, err := b.sendAndWait("initialize", params)
	if err != nil {
		return err
	}
	// Acknowledge initialization.
	return b.process().writer.sendNotification("initialized", struct{}{})
}

// startThread creates a new thread and stores the thread ID.
func (b *Backend) startThread() (string, error) {
	params := threadStartParams{
		Cwd:              b.workDir,
		Sandbox:          b.sandboxMode(),
		BaseInstructions: b.startOpts.SystemPrompt,
		Ephemeral:        false,
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

// resumeThread resumes an existing thread.
func (b *Backend) resumeThread(threadID string) error {
	params := threadResumeParams{
		ThreadID:         threadID,
		BaseInstructions: b.startOpts.SystemPrompt,
	}
	result, err := b.sendAndWait("thread/resume", params)
	if err != nil {
		return err
	}
	var tr threadResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return fmt.Errorf("codex: parse thread/resume response: %w", err)
	}
	b.mu.Lock()
	b.threadID = threadID
	b.sessionFilePath = tr.Thread.Path
	if tr.Thread.Status.Type != "" {
		b.threadStatus = tr.Thread.Status
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

// prepareConfiguredModel resolves backend_config.model (or a per-session
// ModelFunc override) after model/list has populated the catalogue. Resumed
// threads receive it on their next turn/start because thread/resume has no
// model field; fresh threads receive it directly in thread/start.
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
	b.compactDoneCh = make(chan struct{})
}

// WaitForCompaction blocks until the contextCompaction item lifecycle
// completes (item/completed with type "contextCompaction"), or ctx expires.
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.compactMu.Lock()
	ch := b.compactDoneCh
	b.compactMu.Unlock()
	if ch == nil {
		return nil // not armed
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
