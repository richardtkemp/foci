package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
	if opts.Label == "" {
		b.label = opts.AgentID
	}
	b.autoApproveRules = autoapprove.Compile(opts.AutoApproveRules)

	b.pendingRPC = make(map[int64]chan json.RawMessage)
	b.pendingPerms = make(map[int64]*pendingApproval)
	b.itemCache = make(map[string]itemEnvelope)

	bin := b.codexBinary()
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("codex: binary %q not found: %w", bin, err)
	}

	args := []string{"app-server"}
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

	b.lg.Infof("launching: %s %s (workdir=%s)", bin, strings.Join(args, " "), opts.WorkDir)

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
	b.readyCh = make(chan struct{})
	b.mu.Unlock()

	// Start the reader goroutine.
	go func() {
		b.readStream(cmdCtx, stdout)
		// Reap the process.
		if err := cmd.Wait(); err != nil {
			b.lg.Debugf("process exited: %v", err)
		}
	}()

	// Perform initialize handshake.
	if err := b.initialize(); err != nil {
		cancel()
		return fmt.Errorf("codex: initialize failed: %w", err)
	}

	// Start or resume thread.
	if opts.ResumeSessionID != "" {
		if err := b.resumeThread(opts.ResumeSessionID); err != nil {
			return fmt.Errorf("codex: resume thread %s: %w", opts.ResumeSessionID, err)
		}
		b.lg.Infof("resumed thread %s", opts.ResumeSessionID)
	} else {
		tid, err := b.startThread()
		if err != nil {
			return fmt.Errorf("codex: start thread: %w", err)
		}
		b.lg.Infof("started thread %s", tid)
	}

	return nil
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
	b.mu.Lock()
	b.closing = true
	cancel := b.cancel
	wr := b.writer
	b.mu.Unlock()

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
			b.lg.Warnf("reader goroutine did not exit within 10s")
		}
	}

	return nil
}

// Interrupt cancels the current turn.
func (b *Backend) Interrupt(ctx context.Context) error {
	b.mu.Lock()
	wr := b.writer
	b.mu.Unlock()
	if wr == nil {
		return errors.New("codex: backend not started")
	}
	// turn/interrupt is a notification (no response expected).
	return wr.sendNotification("turn/interrupt", struct {
		ThreadID string `json:"threadId"`
	}{ThreadID: b.SessionID()})
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
	b.rpcMu.Lock()
	defer b.rpcMu.Unlock()
	b.rpcSeq++
	return b.rpcSeq
}

// sendAndWait sends a JSON-RPC request and waits for its response.
func (b *Backend) sendAndWait(method string, params interface{}) (json.RawMessage, error) {
	id := b.nextID()
	ch := make(chan json.RawMessage, 1)

	b.rpcMu.Lock()
	b.pendingRPC[id] = ch
	b.rpcMu.Unlock()

	if err := b.writer.sendRequest(method, params, id); err != nil {
		b.rpcMu.Lock()
		delete(b.pendingRPC, id)
		b.rpcMu.Unlock()
		return nil, err
	}

	select {
	case result := <-ch:
		if result == nil {
			return nil, errors.New("codex: request cancelled (process exited)")
		}
		return result, nil
	case <-time.After(30 * time.Second):
		b.rpcMu.Lock()
		delete(b.pendingRPC, id)
		b.rpcMu.Unlock()
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
	return b.writer.sendNotification("initialized", struct{}{})
}

// startThread creates a new thread and stores the thread ID.
func (b *Backend) startThread() (string, error) {
	params := threadStartParams{
		Cwd:              b.workDir,
		Sandbox:          b.sandboxMode(),
		BaseInstructions: b.startOpts.SystemPrompt,
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
	b.model = tr.Model
	b.mu.Unlock()

	b.readyOnce.Do(func() { close(b.readyCh) })
	if b.onSessionReady != nil {
		b.onSessionReady(tr.Thread.ID)
	}
	return tr.Thread.ID, nil
}

// resumeThread resumes an existing thread.
func (b *Backend) resumeThread(threadID string) error {
	params := threadResumeParams{ThreadID: threadID}
	_, err := b.sendAndWait("thread/resume", params)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.threadID = threadID
	b.mu.Unlock()

	b.readyOnce.Do(func() { close(b.readyCh) })
	if b.onSessionReady != nil {
		b.onSessionReady(threadID)
	}
	return nil
}

// modelFromOpts returns the model from StartOptions or config.
func (b *Backend) modelFromOpts() string {
	if b.startOpts.Model != "" {
		return b.startOpts.Model
	}
	if v, ok := b.cfg["model"].(string); ok {
		return v
	}
	return ""
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
	threadID := b.SessionID()
	if threadID == "" {
		return errors.New("codex: no active thread to compact")
	}

	if b.startOpts.CompactionPromptFunc != nil {
		if prompt := b.startOpts.CompactionPromptFunc(""); prompt != "" {
			if _, err := b.sendAndWait("config/value/write", struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}{Key: "compact_prompt", Value: prompt}); err != nil {
				b.lg.Warnf("config/value/write for compact_prompt failed: %v", err)
			}
		}
	}

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
