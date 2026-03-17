package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"foci/internal/gemini"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/startup"
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
	// Record clean shutdown immediately
	if err := startup.RecordCleanShutdown(sessionIndex); err != nil {
		log.Warnf("main", "record clean shutdown: %v", err)
	}

	log.Infof("main", "shutting down...")

	// Stop keepalive runners — prevents new timer-triggered branches
	for _, inst := range agents {
		if inst.kaRunner != nil {
			inst.kaRunner.Stop()
		}
	}

	// Close HTTP server — prevents new HTTP-triggered turns
	httpMu.Lock()
	if httpServer != nil {
		_ = httpServer.Close()
	}
	httpMu.Unlock()

	// Wait for in-flight agent turns to complete naturally
	gracefulShutdown(agents, cfg.gracefulTimeout)

	// Close MCP managers
	for _, inst := range agents {
		if inst.mcpManager != nil {
			_ = inst.mcpManager.Close()
		}
	}

	// Stop Anthropic CC token source polling
	if anthropicResolver != nil {
		anthropicResolver.Close()
	}

	// Clean up Gemini cache (delete server-side cached content)
	if gc := clients.PeekClient("gemini", "gemini"); gc != nil {
		if gcTyped, ok := gc.(*gemini.Client); ok {
			gcTyped.Close(cfg.ctx)
		}
	}

	// Cancel context — stops platform bots and cleans up goroutines
	cancel()

	// Wait for platform connections to finish cleanup
	connMgr.Wait()
}

type shutdownConfig struct {
	gracefulTimeout time.Duration
	ctx             context.Context
}

// gracefulShutdown waits for all in-flight agent turns to complete, up to the
// configured timeout.
func gracefulShutdown(agents map[string]*agentInstance, timeout time.Duration) {
	const tickInterval = 100 * time.Millisecond
	deadline := time.After(timeout)

	for {
		var anyBusy bool
		for _, inst := range agents {
			if inst.ag.IsProcessing() {
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
			s := fmt.Sprintf("%s(session=%s", id, d.SessionKey)
			if d.ToolName != "" {
				s += fmt.Sprintf(", tool=%s", d.ToolName)
			}
			if d.Trigger != "" {
				s += fmt.Sprintf(", trigger=%s", d.Trigger)
			}
			s += fmt.Sprintf(", elapsed=%s)", now.Sub(d.StartTime).Truncate(time.Second))
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		log.Warnf("main", "graceful shutdown timed out after %s — agents still processing (no detail available)", timeout)
	} else {
		log.Warnf("main", "graceful shutdown timed out after %s — blocking: %s", timeout, strings.Join(parts, ", "))
	}
}

