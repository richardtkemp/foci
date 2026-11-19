package main

import (
	"context"
	"path/filepath"
	"sync"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/jitmemory"
	"foci/internal/memsearch"
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

// newMemSearcher is the agent's shared memory search (#3185): the one
// memory_search and JIT memory both call, with the own-material rules. Its
// window is [jit_memory] dedupe_compactions, read live.
func newMemSearcher(p setupParams) *memsearch.Searcher {
	rc := p.resolvedLive
	dirs := jitSourceDirs(p.cfg, p.acfg)
	return &memsearch.Searcher{
		Backends:       p.memBackends,
		DefaultBackend: func() string { return rc.Load().MemorySearch.SearchBackend },
		Conv:           p.convReader,
		Own:            p.ownIndex,
		SourceDirs:     func() map[string]string { return dirs },
		Window:         func() int { return rc.Load().JITMemory.DedupeCompactions },
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
// results (#3175), and the own-material index its tool events feed (#3185). Wired for every agent with a memory index; the feature
// itself is gated per message by [jit_memory] enabled, so turning it on is a
// live config edit.
func wireJITMemory(ag *agent.Agent, p setupParams) {
	if len(p.memBackends) == 0 {
		return
	}
	acfg, cfg := p.acfg, p.cfg
	ag.OwnMemory = p.ownIndex
	ag.JIT = jitmemory.New(jitmemory.Deps{
		AgentID: acfg.ID,
		Search:  newMemSearcher(p).Search,
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
