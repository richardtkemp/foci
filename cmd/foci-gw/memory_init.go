package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
)

// AgentMemoryBoost is the weight added to agent-specific memory sources.
// With a boost of 1.0, an agent-specific source with weight 0.5 gets an
// effective weight of 1.5 (multiplier = 1.0 + 1.5 = 2.5), making it rank
// higher than global sources with the same base weight.
const AgentMemoryBoost = 1.0

// buildAgentMemorySources combines global memory sources with agent-specific
// sources. Agent-specific sources get a weight boost to rank higher.
func buildAgentMemorySources(globalSources map[string]memory.SourceConfig, agentSources []config.MemorySource) map[string]memory.SourceConfig {
	combined := make(map[string]memory.SourceConfig, len(globalSources)+len(agentSources))

	// Add global sources as-is
	for name, src := range globalSources {
		combined[name] = src
	}

	// Add agent-specific sources with weight boost
	for _, src := range agentSources {
		combined["agent:"+src.Name] = memory.SourceConfig{
			Dir:    src.Dir,
			Weight: src.Weight + AgentMemoryBoost,
		}
	}

	return combined
}

// memoryResult holds the outputs of initMemorySystem.
type memoryResult struct {
	sharedBackends  map[string]memory.Searcher            // backend name -> searcher (shared mode)
	agentBackends   map[string]map[string]memory.Searcher // agentID -> backend name -> searcher
	sharedFTS5      *memory.Index                         // for conversation hook (shared mode)
	agentFTS5       map[string]*memory.Index              // for conversation hook (per-agent mode)
	reminderStore   *memory.ReminderStore
	scratchpadStore *memory.Scratchpad
	todoStore       *memory.TodoStore
	cleanup         func()
}

// initMemorySystem sets up memory indices, reminder/scratchpad/todo stores,
// and conversation hooks. Returns a memoryResult with a cleanup function
// that closes all opened resources.
func initMemorySystem(cfg *config.Config) memoryResult {
	var closers []io.Closer
	result := memoryResult{
		sharedBackends: make(map[string]memory.Searcher),
		agentBackends:  make(map[string]map[string]memory.Searcher),
		agentFTS5:      make(map[string]*memory.Index),
		cleanup:        func() {},
	}

	// Build global source map from [memory] config
	globalMemSources := make(map[string]memory.SourceConfig)
	for _, src := range cfg.Memory.Sources {
		globalMemSources[src.Name] = memory.SourceConfig{Dir: src.Dir, Weight: src.Weight}
	}

	// Parse debounce delay
	var memDebounce time.Duration
	if cfg.Memory.ReindexDebounce != "" {
		var err error
		memDebounce, err = time.ParseDuration(cfg.Memory.ReindexDebounce)
		if err != nil {
			log.Fatalf("main", "invalid reindex_debounce: %v", err)
		}
	}

	// Check if any agent has per-agent memory sources
	hasPerAgentMemory := false
	for _, acfg := range cfg.Agents {
		if len(acfg.Memory.Sources) > 0 {
			hasPerAgentMemory = true
			break
		}
	}

	memoryEnabled := len(globalMemSources) > 0 || hasPerAgentMemory
	if !memoryEnabled {
		return result
	}

	// Parse sweep interval ("0" disables)
	var sweepInterval time.Duration
	if cfg.Memory.SweepInterval != "" && cfg.Memory.SweepInterval != "0" {
		var err error
		sweepInterval, err = time.ParseDuration(cfg.Memory.SweepInterval)
		if err != nil {
			log.Fatalf("main", "invalid sweep_interval: %v", err)
		}
	}

	wantFTS5 := cfg.Memory.HasBackend("fts5")
	wantBleve := cfg.Memory.HasBackend("bleve")

	// initBackends creates FTS5 and/or bleve backends for a given set of sources,
	// returning the backend map and (optionally) the FTS5 index for conversation hooks.
	initBackends := func(label string, sources map[string]memory.SourceConfig, dbPrefix string, blevePrefix string) (map[string]memory.Searcher, *memory.Index) {
		backends := make(map[string]memory.Searcher)
		var fts5Idx *memory.Index

		if wantFTS5 {
			dbPath := cfg.DataPath(dbPrefix)
			idx, err := memory.NewIndex(dbPath, sources, memDebounce, cfg.Memory.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create FTS5 index (%s): %v", label, err)
			}
			closers = append(closers, idx)
			if err := idx.Reindex(); err != nil {
				log.Errorf("main", "reindex FTS5 (%s): %v", label, err)
			}
			if memDebounce > 0 || len(sources) > 0 {
				if err := idx.Watch(); err != nil {
					log.Errorf("main", "start FTS5 file watching (%s): %v", label, err)
				}
			}
			if sweepInterval > 0 {
				idx.StartSweep(30*time.Second, sweepInterval)
			}
			backends["fts5"] = idx
			fts5Idx = idx
		}

		if wantBleve {
			blevePath := cfg.DataPath(blevePrefix)
			bidx, err := memory.NewBleveIndex(blevePath, sources, memDebounce)
			if err != nil {
				log.Fatalf("main", "create bleve index (%s): %v", label, err)
			}
			closers = append(closers, bidx)
			if err := bidx.Reindex(); err != nil {
				log.Errorf("main", "reindex bleve (%s): %v", label, err)
			}
			if memDebounce > 0 || len(sources) > 0 {
				if err := bidx.Watch(); err != nil {
					log.Errorf("main", "start bleve file watching (%s): %v", label, err)
				}
			}
			if sweepInterval > 0 {
				bidx.StartSweep(30*time.Second, sweepInterval)
			}
			backends["bleve"] = bidx
		}

		return backends, fts5Idx
	}

	if hasPerAgentMemory {
		// Per-agent indices: each agent gets global + agent-specific sources
		for _, acfg := range cfg.Agents {
			combined := buildAgentMemorySources(globalMemSources, acfg.Memory.Sources)
			if len(combined) == 0 {
				continue
			}
			backends, fts5Idx := initBackends(
				fmt.Sprintf("agent %s", acfg.ID),
				combined,
				fmt.Sprintf("memory-%s.db", acfg.ID),
				fmt.Sprintf("memory-%s.bleve", acfg.ID),
			)
			result.agentBackends[acfg.ID] = backends
			if fts5Idx != nil {
				result.agentFTS5[acfg.ID] = fts5Idx
			}
			log.Infof("main", "agent %q: memory backends %v with %d sources", acfg.ID, cfg.Memory.SearchBackends, len(combined))
		}

		// Conversation hook: route to agent's FTS5 index by session key prefix
		if wantFTS5 {
			log.ConversationHook = func(text, session string) {
				for agentID, idx := range result.agentFTS5 {
					if strings.HasPrefix(session, "agent:"+agentID+":") {
						idx.IndexConversation(text, session)
						return
					}
				}
			}
		}
	} else {
		// Shared indices (backward compat — no agent has per-agent memory)
		backends, fts5Idx := initBackends("shared", globalMemSources, "memory.db", "memory.bleve")
		result.sharedBackends = backends
		result.sharedFTS5 = fts5Idx

		if fts5Idx != nil {
			log.ConversationHook = fts5Idx.IndexConversation
		}
	}

	// Reminder store (shared across agents)
	reminderDbPath := cfg.DataPath("reminders.db")
	var err error
	result.reminderStore, err = memory.NewReminderStore(reminderDbPath)
	if err != nil {
		log.Fatalf("main", "create reminder store: %v", err)
	}
	closers = append(closers, result.reminderStore)

	// Scratchpad (shared across agents)
	scratchpadDbPath := cfg.DataPath("scratchpad.db")
	result.scratchpadStore, err = memory.NewScratchpad(scratchpadDbPath)
	if err != nil {
		log.Fatalf("main", "create scratchpad: %v", err)
	}
	closers = append(closers, result.scratchpadStore)

	// Todo list (shared across agents, agent_id scoped per-agent)
	todoDbPath := cfg.DataPath("todo.db")
	result.todoStore, err = memory.NewTodoStore(todoDbPath)
	if err != nil {
		log.Fatalf("main", "create todo store: %v", err)
	}
	closers = append(closers, result.todoStore)

	result.cleanup = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
	}
	return result
}
