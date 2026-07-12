package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/convo"
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
		combined["agent/"+src.Name] = memory.SourceConfig{
			Dir:    src.Dir,
			Weight: src.Weight + AgentMemoryBoost,
		}
	}

	return combined
}

// memoryResult holds the outputs of initMemorySystem.
type memoryResult struct {
	sharedBackends   map[string]memory.Searcher            // backend name -> searcher (shared mode)
	agentBackends    map[string]map[string]memory.Searcher // agentID -> backend name -> searcher
	sharedFTS5       *memory.Index                         // for conversation hook (shared mode)
	agentFTS5        map[string]*memory.Index              // for conversation hook (per-agent mode)
	sharedBleve      *memory.BleveIndex                    // for conversation hook (shared mode)
	agentBleve       map[string]*memory.BleveIndex         // for conversation hook (per-agent mode)
	convReader       *memory.ConversationReader            // for conversation context lookup
	reminderStores   map[string]*memory.ReminderStore
	scratchpadStores map[string]*memory.Scratchpad
	todoStores       map[string]*memory.TodoStore
	taskListStores   map[string]*memory.TaskListStore
	cleanup          func()
}

// initStandaloneStores creates per-agent standalone memory stores (reminder,
// scratchpad, todo, task list) that should always exist regardless of whether
// memory search sources are configured. Databases are stored in each agent's
// workspace .data directory. Appends to closers for cleanup.
func initStandaloneStores(cfg *config.Config, result memoryResult, closers *[]io.Closer) memoryResult {
	for _, acfg := range cfg.Agents {
		id := acfg.ID

		rs, err := memory.NewReminderStore(config.AgentDataPath(acfg.Workspace, "reminders.db"))
		if err != nil {
			log.Fatalf("main", "create reminder store for %s: %v", id, err)
		}
		result.reminderStores[id] = rs
		*closers = append(*closers, rs)

		sp, err := memory.NewScratchpad(config.AgentDataPath(acfg.Workspace, "scratchpad.db"))
		if err != nil {
			log.Fatalf("main", "create scratchpad for %s: %v", id, err)
		}
		result.scratchpadStores[id] = sp
		*closers = append(*closers, sp)

		ts, err := memory.NewTodoStore(config.AgentDataPath(acfg.Workspace, "todo.db"))
		if err != nil {
			log.Fatalf("main", "create todo store for %s: %v", id, err)
		}
		result.todoStores[id] = ts
		*closers = append(*closers, ts)

		tl, err := memory.NewTaskListStore(config.AgentDataPath(acfg.Workspace, "tasklist.db"))
		if err != nil {
			log.Fatalf("main", "create task list store for %s: %v", id, err)
		}
		result.taskListStores[id] = tl
		*closers = append(*closers, tl)
	}

	return result
}

// hasAgentMemoryOverrides returns true if any agent has per-agent memory
// sources or overrides index-creation settings (search_backend,
// reindex_debounce, conversation_weight, sweep_interval).
func hasAgentMemoryOverrides(agents []config.AgentConfig) bool {
	for _, acfg := range agents {
		m := acfg.Memory
		if len(m.Sources) > 0 || m.SearchBackend != nil || m.ReindexDebounce != nil || m.ConversationWeight != nil || m.SweepInterval != nil {
			return true
		}
	}
	return false
}

// resolvedMemorySettings resolves per-agent memory settings by merging
// the agent's memory config with the global memory config.
func resolvedMemorySettings(cfg *config.Config, acfg config.AgentConfig) config.ResolvedMemorySearch {
	return config.Resolve(cfg, acfg).MemorySearch
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

	hasPerAgentMemory := hasAgentMemoryOverrides(cfg.Agents)
	memoryEnabled := len(globalMemSources) > 0 || hasPerAgentMemory

	// Always create standalone stores (scratchpad, todo, reminders, task list)
	// even when no memory search sources are configured.
	result = initStandaloneStores(cfg, result, &closers)

	if !memoryEnabled {
		return result
	}

	// memoryBackend abstracts over FTS5 and bleve for shared init logic.
	type memoryBackend interface {
		memory.Searcher
		io.Closer
		Reindex() error
		Watch() error
		StartSweep(initial, interval time.Duration)
	}

	// reindexStagger offsets each agent's reindexing so the whole fleet does not
	// reindex simultaneously — a full-fleet reindex (startup, or a sweep tick)
	// otherwise stacks every agent's peak memory in the same few seconds, which
	// is what drove the memory_guard pressure. Each successive index gets an
	// extra reindexStaggerStep of delay on both its initial reindex and its
	// sweep phase.
	const reindexStaggerStep = 7 * time.Second
	reindexStagger := time.Duration(0)

	// initOne watches, starts sweep, and kicks off an async initial reindex.
	initOne := func(label string, b memoryBackend, debounce, sweepInterval time.Duration) {
		closers = append(closers, b)
		stagger := reindexStagger
		reindexStagger += reindexStaggerStep
		go func() {
			if stagger > 0 {
				time.Sleep(stagger)
			}
			if err := b.Reindex(); err != nil {
				log.Errorf("main", "initial reindex %s: %v", label, err)
			} else {
				log.Infof("main", "%s: initial reindex complete", label)
			}
		}()
		if debounce > 0 || len(globalMemSources) > 0 || hasPerAgentMemory {
			if err := b.Watch(); err != nil {
				log.Errorf("main", "start %s file watching: %v", label, err)
			}
		}
		if sweepInterval > 0 {
			// Offset the sweep phase per-agent too, so periodic sweeps (if
			// re-enabled) also don't cluster.
			b.StartSweep(30*time.Second+stagger, sweepInterval)
		}
	}

	// parseDurationOr parses a duration string, fataling on invalid input.
	// Returns 0 for empty or "0" values.
	parseDurationOr := func(s, label string) time.Duration {
		if s == "" || s == "0" {
			return 0
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("main", "invalid %s: %v", label, err)
		}
		return d
	}

	// initBackends creates FTS5 and/or bleve backends for a given set of sources
	// using the provided resolved memory settings.
	// dbPath and blevePath must be fully resolved absolute paths.
	initBackends := func(label string, sources map[string]memory.SourceConfig, dbPath, blevePath string, rm config.ResolvedMemorySearch) (map[string]memory.Searcher, *memory.Index, *memory.BleveIndex) {
		backends := make(map[string]memory.Searcher)
		var fts5Idx *memory.Index
		var bleveIdx *memory.BleveIndex

		debounce := parseDurationOr(rm.ReindexDebounce, fmt.Sprintf("reindex_debounce for %s", label))
		sweepInterval := parseDurationOr(rm.SweepInterval, fmt.Sprintf("sweep_interval for %s", label))

		if rm.SearchBackend == "fts5" {
			idx, err := memory.NewIndex(dbPath, sources, debounce, rm.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create FTS5 index (%s): %v", label, err)
			}
			idx.SetTemporalDecay(rm.TemporalDecay, rm.DecayHalfLife, rm.DecayBoost, rm.EvergreenPatterns) // #352
			idx.SetSearchLimit(rm.SearchLimit)
			initOne(fmt.Sprintf("FTS5 (%s)", label), idx, debounce, sweepInterval)
			backends["fts5"] = idx
			fts5Idx = idx
		}

		if rm.SearchBackend == "bleve" {
			bidx, err := memory.NewBleveIndex(blevePath, sources, debounce, rm.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create bleve index (%s): %v", label, err)
			}
			bidx.SetTemporalDecay(rm.TemporalDecay, rm.DecayHalfLife, rm.DecayBoost, rm.EvergreenPatterns) // #352
			bidx.SetSearchLimit(rm.SearchLimit)
			initOne(fmt.Sprintf("bleve (%s)", label), bidx, debounce, sweepInterval)
			backends["bleve"] = bidx
			bleveIdx = bidx
		}

		return backends, fts5Idx, bleveIdx
	}

	if hasPerAgentMemory {
		// Per-agent indices: each agent gets global + agent-specific sources,
		// with per-agent resolved settings (backend, debounce, weight, sweep).
		for _, acfg := range cfg.Agents {
			combined := buildAgentMemorySources(globalMemSources, acfg.Memory.Sources)
			if len(combined) == 0 {
				continue
			}

			rm := resolvedMemorySettings(cfg, acfg)

			fts5Path := config.AgentDataPath(acfg.Workspace, "memory.db")
			blevePath := config.AgentDataPath(acfg.Workspace, "search.bleve")
			backends, fts5Idx, bleveIdx := initBackends(
				fmt.Sprintf("agent %s", acfg.ID),
				combined,
				fts5Path,
				blevePath,
				rm,
			)
			result.agentBackends[acfg.ID] = backends
			if fts5Idx != nil {
				result.agentFTS5[acfg.ID] = fts5Idx
			}
			if bleveIdx != nil {
				result.agentBleve[acfg.ID] = bleveIdx
			}
			log.Infof("main", "agent %q: memory backend %s with %d sources", acfg.ID, rm.SearchBackend, len(combined))
		}

		// Conversation hook: route to agent's indices by session key prefix
		if len(result.agentFTS5) > 0 || len(result.agentBleve) > 0 {
			convo.Hook = func(text, session string, rowID int64) {
				for agentID, idx := range result.agentFTS5 {
					if strings.HasPrefix(session, agentID+"/") {
						idx.IndexConversation(text, session, rowID)
						break
					}
				}
				for agentID, idx := range result.agentBleve {
					if strings.HasPrefix(session, agentID+"/") {
						idx.IndexConversation(text, session, rowID)
						break
					}
				}
			}
		}
	} else {
		// Shared indices — no agent overrides memory settings.
		// Resolve from global config (empty agent = no overrides).
		rm := resolvedMemorySettings(cfg, config.AgentConfig{})
		backends, fts5Idx, bleveIdx := initBackends("shared", globalMemSources, cfg.DataPath("memory.db"), cfg.DataPath("search.bleve"), rm)
		result.sharedBackends = backends
		result.sharedFTS5 = fts5Idx
		result.sharedBleve = bleveIdx

		// Wire conversation hook to all active backends
		switch {
		case fts5Idx != nil && bleveIdx != nil:
			convo.Hook = func(text, session string, rowID int64) {
				fts5Idx.IndexConversation(text, session, rowID)
				bleveIdx.IndexConversation(text, session, rowID)
			}
		case fts5Idx != nil:
			convo.Hook = fts5Idx.IndexConversation
		case bleveIdx != nil:
			convo.Hook = bleveIdx.IndexConversation
		}
	}

	// Wire todo stores to bleve indices for full-text search
	for agentID, ts := range result.todoStores {
		var idx *memory.BleveIndex
		if bleveIdx, ok := result.agentBleve[agentID]; ok {
			idx = bleveIdx
		} else if result.sharedBleve != nil {
			idx = result.sharedBleve
		}
		if idx != nil {
			ts.SetSearchIndex(idx)
			go func(aid string) {
				if err := ts.IndexAllTodos(aid); err != nil {
					log.Errorf("main", "index todos for agent %s: %v", aid, err)
				}
			}(agentID)
		}
	}

	// Build ConversationReader for context lookup around search results.
	convDBPaths := make(map[string]string)
	for _, acfg := range cfg.Agents {
		convDBPaths[acfg.ID] = config.AgentDataPath(acfg.Workspace, "conversation.db")
	}
	result.convReader = memory.NewConversationReader(convDBPaths)

	// Backfill historical conversations into bleve indices.
	// Runs in a goroutine to avoid blocking startup.
	hasAnyBleve := result.sharedBleve != nil || len(result.agentBleve) > 0
	var backfillWG sync.WaitGroup
	if hasAnyBleve {
		backfillWG.Add(1)
		go func() {
			defer backfillWG.Done()
			if hasPerAgentMemory {
				for _, acfg := range cfg.Agents {
					idx, ok := result.agentBleve[acfg.ID]
					if !ok {
						continue
					}
					dbPath := config.AgentDataPath(acfg.Workspace, "conversation.db")
					n, err := idx.BackfillConversations(dbPath)
					if err != nil {
						log.Errorf("main", "backfill conversations for agent %s: %v", acfg.ID, err)
					} else if n > 0 {
						log.Infof("main", "backfilled %d conversation messages into bleve for agent %s", n, acfg.ID)
					}
				}
			} else if result.sharedBleve != nil {
				// Shared mode: conversation DBs are still per-agent, iterate all.
				for _, acfg := range cfg.Agents {
					dbPath := config.AgentDataPath(acfg.Workspace, "conversation.db")
					n, err := result.sharedBleve.BackfillConversations(dbPath)
					if err != nil {
						log.Errorf("main", "backfill conversations for agent %s: %v", acfg.ID, err)
					} else if n > 0 {
						log.Infof("main", "backfilled %d conversation messages into bleve for agent %s", n, acfg.ID)
					}
				}
			}
		}()
	}

	// Backfill historical conversations into FTS5 indices (one-time wipe+rebuild;
	// #352 / backend parity). Per-agent: each agent's own index rebuilds from its
	// own conversation DB. Shared: one index serves all agents, so ALL agents'
	// conversation DBs are rebuilt in a single call (the wipe is index-wide).
	hasAnyFTS5 := result.sharedFTS5 != nil || len(result.agentFTS5) > 0
	if hasAnyFTS5 {
		backfillWG.Add(1)
		go func() {
			defer backfillWG.Done()
			if hasPerAgentMemory {
				for _, acfg := range cfg.Agents {
					idx, ok := result.agentFTS5[acfg.ID]
					if !ok {
						continue
					}
					dbPath := config.AgentDataPath(acfg.Workspace, "conversation.db")
					n, err := idx.BackfillConversations(dbPath)
					if err != nil {
						log.Errorf("main", "backfill conversations (fts5) for agent %s: %v", acfg.ID, err)
					} else if n > 0 {
						log.Infof("main", "backfilled %d conversation messages into FTS5 for agent %s", n, acfg.ID)
					}
				}
			} else if result.sharedFTS5 != nil {
				paths := make([]string, 0, len(cfg.Agents))
				for _, acfg := range cfg.Agents {
					paths = append(paths, config.AgentDataPath(acfg.Workspace, "conversation.db"))
				}
				n, err := result.sharedFTS5.BackfillConversations(paths...)
				if err != nil {
					log.Errorf("main", "backfill conversations (fts5, shared): %v", err)
				} else if n > 0 {
					log.Infof("main", "backfilled %d conversation messages into shared FTS5", n)
				}
			}
		}()
	}

	result.cleanup = func() {
		backfillWG.Wait()
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
	}
	return result
}
