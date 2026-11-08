package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/gemini"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/startup"
	"foci/internal/tools"
)

// runShutdown performs the graceful shutdown sequence after a signal is received.
func runShutdown(
	agents map[string]*agentInstance,
	httpServer *http.Server,
	httpMu *sync.Mutex,
	connMgr platform.ConnectionManager,
	clients *clientRegistry,
	sessionIndex *session.SessionIndex,
	cfg shutdownConfig,
	cancel func(),
) {
	// Stop the liveness heartbeat first so it can't write a last_alive
	// timestamp after the clean-shutdown record below — which would make the
	// next startup misclassify this clean exit as a crash.
	if cfg.stopHeartbeat != nil {
		cfg.stopHeartbeat()
	}

	// Record clean shutdown immediately
	if err := startup.RecordCleanShutdown(sessionIndex); err != nil {
		mainLog.Warnf("record clean shutdown: %v", err)
	}

	mainLog.Infof("shutting down...")

	// Spawns first (#2589): a background spawn the shutdown cuts short from
	// here on keeps its persisted record, and the next process resumes it,
	// rather than reporting it failed to a parent that can no longer take it.
	tools.Spawns.BeginShutdown()

	// Drain mode first (#2059): from here no agent begins a system turn,
	// post-turn compaction or backend. Turns already running still finish —
	// gracefulShutdown below waits for them — and queued work (a scheduled
	// wake) stays pending for the next process instead of being consumed by a
	// turn that the backend close would cut off.
	// Then stop keepalive runners — prevents new timer-triggered branches.
	for _, inst := range agents {
		drainAgent(inst)
	}

	// Close HTTP server — prevents new HTTP-triggered turns
	httpMu.Lock()
	if httpServer != nil {
		_ = httpServer.Close()
	}
	httpMu.Unlock()

	// Wait for in-flight agent turns to complete naturally
	gracefulShutdown(agents, cfg.gracefulTimeout)

	// Close delegated sessions (kills CC tmux panes, saves resume IDs) and
	// MCP managers.
	for _, inst := range agents {
		closeAgentBackends(inst)
	}

	// Backstop: each backend's process-global shutdown hook
	// (Spec.OnGatewayShutdown). opencode's synchronously reaps its pooled
	// `serve` subprocesses: DelegatedManager.Close above releases each
	// session's Server reference, but the actual Server shutdown is async
	// (releaseServer's `go func`), so the process would exit before those
	// goroutines finish and orphan the subprocess (#948). The hook drains the
	// pool and WAITS for the bounded shutdown. No-op when nothing ran.
	for _, s := range delegator.Specs() {
		if s.OnGatewayShutdown != nil {
			mainLog.Infof("%s: closed %d pooled process(es) on shutdown", s.Name, s.OnGatewayShutdown())
		}
	}

	// Stop Anthropic CC token source polling
	if anthropicResolver != nil {
		anthropicResolver.Close()
	}

	// Clean up Gemini cache (delete server-side cached content)
	if gc := clients.PeekClient("gemini", "gemini"); gc != nil {
		if gcTyped, ok := provider.Pin(gc).(*gemini.Client); ok {
			gcTyped.Close(cfg.ctx)
		}
	}

	// Cancel context — stops platform bots and cleans up goroutines
	cancel()

	// Wait for platform connections to finish cleanup
	connMgr.Wait()
}

// The per-agent halves of a stop, shared by the whole shutdown (runShutdown,
// every agent) and the live stop of one agent (agentEnv.stopLive, #2787), so
// a step added to one cannot be missed by the other. Between them the caller
// waits for the agent's in-flight turns (gracefulShutdown).

// drainAgent stops new work: drain mode (#2059: no new system turn,
// compaction or backend) and the keepalive runner (no timer-triggered
// branch).
func drainAgent(inst *agentInstance) {
	inst.ag.BeginShutdown()
	if inst.kaRunner != nil {
		inst.kaRunner.Stop()
	}
}

// closeAgentBackends ends the agent's backends once its turns are done: the
// delegated sessions (CC panes killed, resume ids saved) and its MCP servers.
func closeAgentBackends(inst *agentInstance) {
	if dm := inst.ag.DelegatedManager; dm != nil {
		dm.Close()
	}
	if inst.mcpManager != nil {
		_ = inst.mcpManager.Close()
	}
}

type shutdownConfig struct {
	gracefulTimeout time.Duration
	ctx             context.Context
	stopHeartbeat   func()
}

// gracefulShutdown waits for all in-flight agent turns to complete, up to the
// configured timeout.
func gracefulShutdown(agents map[string]*agentInstance, timeout time.Duration) {
	const tickInterval = 100 * time.Millisecond
	deadline := time.After(timeout)

	for {
		var anyBusy bool
		for _, inst := range agents {
			if inst.ag.IsAnyTurnInFlight() {
				anyBusy = true
				break
			}
		}
		if !anyBusy {
			return
		}
		select {
		case <-deadline:
			logBusyAgents(agents, timeout)
			return
		default:
			time.Sleep(tickInterval)
		}
	}
}

func logBusyAgents(agents map[string]*agentInstance, timeout time.Duration) {
	var parts []string
	now := time.Now()
	for id, inst := range agents {
		for _, d := range inst.ag.ProcessingDetails() {
			parts = append(parts, describeBusyTurn(id, d, now))
		}
	}
	if len(parts) == 0 {
		mainLog.Warnf("graceful shutdown timed out after %s — agents still processing (no detail available)", timeout)
	} else {
		mainLog.Warnf("graceful shutdown timed out after %s — blocking: %s", timeout, strings.Join(parts, ", "))
	}
}

// describeBusyTurn renders one in-flight turn for the drain-timeout warning.
// elapsed counts from when the turn began on its backend, not from when it was
// registered: a turn can wait minutes behind another before it starts (#2059),
// and folding that wait into elapsed made a seconds-old turn look like the
// one that had been running all along. The wait is reported separately.
func describeBusyTurn(agentID string, d agent.TurnDetail, now time.Time) string {
	s := fmt.Sprintf("%s(session=%s", agentID, d.SessionKey)
	if d.ToolName != "" {
		s += fmt.Sprintf(", tool=%s", d.ToolName)
	}
	if d.Trigger != "" {
		s += fmt.Sprintf(", trigger=%s", d.Trigger)
	}
	if d.DispatchedAt.IsZero() {
		return s + fmt.Sprintf(", not dispatched, waiting=%s)", now.Sub(d.StartTime).Truncate(time.Second))
	}
	s += fmt.Sprintf(", elapsed=%s", now.Sub(d.DispatchedAt).Truncate(time.Second))
	if waited := d.DispatchedAt.Sub(d.StartTime).Truncate(time.Second); waited > 0 {
		s += fmt.Sprintf(", waited=%s before dispatch", waited)
	}
	return s + ")"
}
