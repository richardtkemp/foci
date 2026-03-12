package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/sqlite"
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
	sharedBleve     *memory.BleveIndex                    // for conversation hook (shared mode)
	agentBleve      map[string]*memory.BleveIndex         // for conversation hook (per-agent mode)
	reminderStores  map[string]*memory.ReminderStore
	scratchpadStores map[string]*memory.Scratchpad
	todoStores      map[string]*memory.TodoStore
	taskListStores  map[string]*memory.TaskListStore
	cleanup         func()
}

// initStandaloneStores creates per-agent standalone memory stores (reminder,
// scratchpad, todo, task list) that should always exist regardless of whether
// memory search sources are configured. Appends to closers for cleanup.
func initStandaloneStores(cfg *config.Config, result memoryResult, closers *[]io.Closer) memoryResult {
	for _, acfg := range cfg.Agents {
		id := acfg.ID

		rs, err := memory.NewReminderStore(sqlite.AgentPath(cfg.DataPath("reminders.db"), id))
		if err != nil {
			log.Fatalf("main", "create reminder store for %s: %v", id, err)
		}
		result.reminderStores[id] = rs
		*closers = append(*closers, rs)

		sp, err := memory.NewScratchpad(sqlite.AgentPath(cfg.DataPath("scratchpad.db"), id))
		if err != nil {
			log.Fatalf("main", "create scratchpad for %s: %v", id, err)
		}
		result.scratchpadStores[id] = sp
		*closers = append(*closers, sp)

		ts, err := memory.NewTodoStore(sqlite.AgentPath(cfg.DataPath("todo.db"), id))
		if err != nil {
			log.Fatalf("main", "create todo store for %s: %v", id, err)
		}
		result.todoStores[id] = ts
		*closers = append(*closers, ts)

		tl, err := memory.NewTaskListStore(sqlite.AgentPath(cfg.DataPath("tasklist.db"), id))
		if err != nil {
			log.Fatalf("main", "create task list store for %s: %v", id, err)
		}
		result.taskListStores[id] = tl
		*closers = append(*closers, tl)
	}

	return result
}

// initMemorySystem sets up memory indices, reminder/scratchpad/todo stores,
// and conversation hooks. Returns a memoryResult with a cleanup function
// that closes all opened resources.
func initMemorySystem(cfg *config.Config) memoryResult {
	var closers []io.Closer
	result := memoryResult{
		sharedBackends:   make(map[string]memory.Searcher),
		agentBackends:    make(map[string]map[string]memory.Searcher),
		agentFTS5:        make(map[string]*memory.Index),
		agentBleve:       make(map[string]*memory.BleveIndex),
		reminderStores:   make(map[string]*memory.ReminderStore),
		scratchpadStores: make(map[string]*memory.Scratchpad),
		todoStores:       make(map[string]*memory.TodoStore),
		taskListStores:   make(map[string]*memory.TaskListStore),
		cleanup:          func() {},
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

	// Always create standalone stores (scratchpad, todo, reminders, task list)
	// even when no memory search sources are configured.
	result = initStandaloneStores(cfg, result, &closers)

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

	// migrateBlevePath renames legacy memory-*.bleve paths to search-*.bleve.
	migrateBlevePath := func(oldName, newName string) {
		oldPath := cfg.DataPath(oldName)
		newPath := cfg.DataPath(newName)
		if _, err := os.Stat(oldPath); err == nil {
			if _, err := os.Stat(newPath); os.IsNotExist(err) {
				if err := os.Rename(oldPath, newPath); err != nil {
					log.Errorf("main", "migrate bleve path %s → %s: %v", oldPath, newPath, err)
				} else {
					log.Infof("main", "migrated bleve index %s → %s", oldName, newName)
				}
			}
		}
	}

	// memoryBackend abstracts over FTS5 and bleve for shared init logic.
	type memoryBackend interface {
		memory.Searcher
		io.Closer
		Reindex() error
		Watch() error
		StartSweep(initial, interval time.Duration)
	}

	// initOne creates, reindexes, watches, and registers a single backend.
	initOne := func(label string, b memoryBackend) {
		closers = append(closers, b)
		if err := b.Reindex(); err != nil {
			log.Errorf("main", "reindex %s: %v", label, err)
		}
		if memDebounce > 0 || len(globalMemSources) > 0 || hasPerAgentMemory {
			if err := b.Watch(); err != nil {
				log.Errorf("main", "start %s file watching: %v", label, err)
			}
		}
		if sweepInterval > 0 {
			b.StartSweep(30*time.Second, sweepInterval)
		}
	}

	// initBackends creates FTS5 and/or bleve backends for a given set of sources,
	// returning the backend map and (optionally) the typed indices for conversation hooks.
	initBackends := func(label string, sources map[string]memory.SourceConfig, dbPrefix string, blevePrefix string) (map[string]memory.Searcher, *memory.Index, *memory.BleveIndex) {
		backends := make(map[string]memory.Searcher)
		var fts5Idx *memory.Index
		var bleveIdx *memory.BleveIndex

		if wantFTS5 {
			idx, err := memory.NewIndex(cfg.DataPath(dbPrefix), sources, memDebounce, cfg.Memory.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create FTS5 index (%s): %v", label, err)
			}
			initOne(fmt.Sprintf("FTS5 (%s)", label), idx)
			backends["fts5"] = idx
			fts5Idx = idx
		}

		if wantBleve {
			bidx, err := memory.NewBleveIndex(cfg.DataPath(blevePrefix), sources, memDebounce, cfg.Memory.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create bleve index (%s): %v", label, err)
			}
			initOne(fmt.Sprintf("bleve (%s)", label), bidx)
			backends["bleve"] = bidx
			bleveIdx = bidx
		}

		return backends, fts5Idx, bleveIdx
	}

	if hasPerAgentMemory {
		// Per-agent indices: each agent gets global + agent-specific sources
		for _, acfg := range cfg.Agents {
			combined := buildAgentMemorySources(globalMemSources, acfg.Memory.Sources)
			if len(combined) == 0 {
				continue
			}
			bleveName := fmt.Sprintf("search-%s.bleve", acfg.ID)
			migrateBlevePath(fmt.Sprintf("memory-%s.bleve", acfg.ID), bleveName)
			backends, fts5Idx, bleveIdx := initBackends(
				fmt.Sprintf("agent %s", acfg.ID),
				combined,
				fmt.Sprintf("memory-%s.db", acfg.ID),
				bleveName,
			)
			result.agentBackends[acfg.ID] = backends
			if fts5Idx != nil {
				result.agentFTS5[acfg.ID] = fts5Idx
			}
			if bleveIdx != nil {
				result.agentBleve[acfg.ID] = bleveIdx
			}
			log.Infof("main", "agent %q: memory backends %v with %d sources", acfg.ID, cfg.Memory.SearchBackends, len(combined))
		}

		// Conversation hook: route to agent's indices by session key prefix
		if wantFTS5 || wantBleve {
			log.ConversationHook = func(text, session string) {
				for agentID, idx := range result.agentFTS5 {
					if strings.HasPrefix(session, "agent:"+agentID+":") {
						idx.IndexConversation(text, session)
						break
					}
				}
				for agentID, idx := range result.agentBleve {
					if strings.HasPrefix(session, "agent:"+agentID+":") {
						idx.IndexConversation(text, session)
						break
					}
				}
			}
		}
	} else {
		// Shared indices (backward compat — no agent has per-agent memory)
		migrateBlevePath("memory.bleve", "search.bleve")
		backends, fts5Idx, bleveIdx := initBackends("shared", globalMemSources, "memory.db", "search.bleve")
		result.sharedBackends = backends
		result.sharedFTS5 = fts5Idx
		result.sharedBleve = bleveIdx

		// Wire conversation hook to all active backends
		switch {
		case fts5Idx != nil && bleveIdx != nil:
			log.ConversationHook = func(text, session string) {
				fts5Idx.IndexConversation(text, session)
				bleveIdx.IndexConversation(text, session)
			}
		case fts5Idx != nil:
			log.ConversationHook = fts5Idx.IndexConversation
		case bleveIdx != nil:
			log.ConversationHook = bleveIdx.IndexConversation
		}
	}

	// Wire todo stores to bleve indices for full-text search
	if wantBleve {
		for agentID, ts := range result.todoStores {
			var idx *memory.BleveIndex
			if bleveIdx, ok := result.agentBleve[agentID]; ok {
				idx = bleveIdx
			} else if result.sharedBleve != nil {
				idx = result.sharedBleve
			}
			if idx != nil {
				ts.SetSearchIndex(idx)
				if err := ts.IndexAllTodos(agentID); err != nil {
					log.Errorf("main", "index todos for agent %s: %v", agentID, err)
				}
			}
		}
	}

	result.cleanup = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
	}
	return result
}
