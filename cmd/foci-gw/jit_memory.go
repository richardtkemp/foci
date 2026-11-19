package main

import (
	"context"
	"path/filepath"
	"sync"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/jitmemory"
	"foci/internal/memory"
	"foci/internal/workspace"
)

// jitStore is the one JIT memory database (<data>/jit_memory.db) every agent
// shares (#3175). It is opened at the first run, so an install that never
// turns the feature on never creates it.
var jitStore struct {
	once  sync.Once
	store *jitmemory.Store
}

// jitStoreFor returns the shared store, opening it at the first call; nil
// (logged once) when it cannot be opened: runs are then not recorded.
func jitStoreFor(cfg *config.Config) func() *jitmemory.Store {
	return func() *jitmemory.Store {
		jitStore.once.Do(func() {
			s, err := jitmemory.OpenStore(cfg.DataPath("jit_memory.db"))
			if err != nil {
				mainLog.Warnf("jit memory: %v; runs are not recorded", err)
				return
			}
			jitStore.store = s
		})
		return jitStore.store
	}
}

// jitSourceDirs maps each memory source name the agent's index uses to its
// directory, by the rule buildAgentMemorySources names them.
func jitSourceDirs(cfg *config.Config, acfg config.AgentConfig) map[string]string {
	dirs := map[string]string{}
	for _, src := range cfg.Memory.Sources {
		dirs[src.Name] = src.Dir
	}
	for _, src := range acfg.Memory.Sources {
		dirs[config.AgentMemorySourcePrefix+src.Name] = src.Dir
	}
	return dirs
}

// jitSearch runs one query on the agent's default memory backend (the one
// memory_search uses), else any it has.
func jitSearch(backends map[string]memory.Searcher, rc func() *config.ResolvedAgentConfig) func(string) ([]memory.Result, error) {
	return func(q string) ([]memory.Result, error) {
		s, ok := backends[rc().MemorySearch.SearchBackend]
		if !ok {
			for _, b := range backends {
				s = b
				break
			}
		}
		return s.Search(q, "relevance", nil)
	}
}

// jitFastModel calls the fast summary model directly (app.AskFastModel),
// never through a backend batch: Dick's ruling Q115, the foci_summary path
// runs a Claude Code batch (~4 s).
func jitFastModel(agentID string) jitmemory.ModelFunc {
	return func(ctx context.Context, what, model, system, user string, maxTokens int) (string, jitmemory.CallReport, error) {
		answer, rep, err := app.AskFastModel(ctx, app.FastAsk{What: what, Model: model, System: system, User: user,
			MaxTokens: maxTokens, AgentID: agentID})
		return answer, jitmemory.CallReport{Model: rep.Model, InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens,
			Duration: rep.Duration, CostUSD: rep.CostUSD}, err
	}
}

// wireJITMemory gives ag its JIT memory engine and the delivery of its
// results (#3175). Wired for every agent with a memory index; the feature
// itself is gated per message by [jit_memory] enabled, so turning it on is a
// live config edit.
func wireJITMemory(ag *agent.Agent, p setupParams) {
	if len(p.memBackends) == 0 {
		return
	}
	acfg, cfg := p.acfg, p.cfg
	rc := p.resolvedLive.Load
	dirs := jitSourceDirs(cfg, acfg)
	ag.JIT = jitmemory.New(jitmemory.Deps{
		AgentID:    acfg.ID,
		Search:     jitSearch(p.memBackends, rc),
		SourceDirs: func() map[string]string { return dirs },
		SystemPromptFiles: func() []string {
			if ag.Bootstrap != nil {
				return ag.Bootstrap.FilePaths()
			}
			out := make([]string, 0, len(workspace.DefaultFileOrder))
			for _, name := range workspace.DefaultFileOrder {
				out = append(out, filepath.Join(acfg.Workspace, name))
			}
			return out
		},
		Model: jitFastModel(acfg.ID),
		Store: jitStoreFor(cfg),
	})
	// A result goes to the agent as a message within the agent (#3114): it
	// steers into a running turn, else runs as a work turn that delivers to
	// the session's chat.
	ag.JITDeliverFn = func(sessionKey, text string) {
		deliverToSessionChat(ag, agent.WithSkipHolds(p.ctx), agent.TriggerJITMemory, p.connMgr, acfg.ID, sessionKey, text, nil, "")
	}
}
