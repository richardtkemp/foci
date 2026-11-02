package main

import (
	"errors"
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

// reindexStaggerStep offsets each index's reindexing so the whole fleet does
// not reindex simultaneously — a full-fleet reindex (startup, or a sweep tick)
// otherwise stacks every agent's peak memory in the same few seconds, which
// is what drove the memory_guard pressure. Each successive index gets an
// extra step of delay on both its initial reindex and its sweep phase.
const reindexStaggerStep = 7 * time.Second

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
		combined[config.AgentMemorySourcePrefix+src.Name] = memory.SourceConfig{
			Dir:    src.Dir,
			Weight: src.Weight + AgentMemoryBoost,
		}
	}

	return combined
}

// agentMemory is one agent's view of the memory system: its search backends
// and its standalone stores.
type agentMemory struct {
	backends   map[string]memory.Searcher // backend name -> searcher (the shared ones in shared mode)
	reminders  *memory.ReminderStore
	scratchpad *memory.Scratchpad
	todos      *memory.TodoStore
	taskList   *memory.TaskListStore
}

// memorySystem holds the memory indices, the per-agent stores and the
// conversation hook. Boot builds it for every configured agent
// (initMemorySystem); `/agents new` adds one agent to it while the process
// runs (addAgent, #963). Both go through the same per-agent steps
// (openAgentStores, buildAgentIndex, wireTodoSearch), so a hot-added agent
// gets exactly what boot would give it.
//
// The MODE is fixed at boot: per-agent indices when any agent overrides its
// memory settings (hasAgentMemoryOverrides), else one shared set. A hot-add
// joins the running mode.
type memorySystem struct {
	globalSources map[string]memory.SourceConfig
	perAgent      bool // per-agent indices (fixed at boot)
	enabled       bool // any memory search at all (fixed at boot)

	sharedBackends map[string]memory.Searcher // shared mode; read-only after boot
	sharedFTS5     *memory.Index              // shared mode
	sharedBleve    *memory.BleveIndex         // shared mode
	convReader     *memory.ConversationReader // nil when memory search is off

	// mu guards everything below. convo.Hook reads agentFTS5/agentBleve on
	// every logged conversation entry, from any goroutine, while a hot-add
	// writes them.
	mu             sync.RWMutex
	agents         map[string]agentMemory
	agentFTS5      map[string]*memory.Index      // per-agent mode
	agentBleve     map[string]*memory.BleveIndex // per-agent mode
	closers        []io.Closer                   // shared indices
	agentClosers   map[string][]io.Closer        // each agent's stores and own indices: removeAgent closes them alone (#2787)
	reindexStagger time.Duration
	closed         bool
	backfillWG     sync.WaitGroup
}

// forAgent returns the agent's memory. An agent with no per-agent index uses
// the shared backends (the boot rule).
func (m *memorySystem) forAgent(id string) agentMemory {
	m.mu.RLock()
	defer m.mu.RUnlock()
	am := m.agents[id]
	if am.backends == nil {
		am.backends = m.sharedBackends
	}
	return am
}

// cleanup waits for the backfills and closes every opened store and index.
func (m *memorySystem) cleanup() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.backfillWG.Wait()
	m.mu.Lock()
	closers := m.closers
	m.closers = nil
	perAgent := m.agentClosers
	m.agentClosers = map[string][]io.Closer{}
	m.mu.Unlock()
	for _, cs := range perAgent {
		closeAll(cs)
	}
	closeAll(closers)
}

// closeAll closes cs in reverse order of opening.
func closeAll(cs []io.Closer) {
	for i := len(cs) - 1; i >= 0; i-- {
		_ = cs[i].Close()
	}
}

// removeAgent takes one agent out of the running memory system and closes
// its stores and own indices: the live stop of that agent (#2787), after its
// turns and backends have ended. Its rows in a SHARED index stay (they are
// rows in a file every agent uses). A background backfill of its index still
// running fails on the closed index and logs it.
func (m *memorySystem) removeAgent(id string) {
	m.mu.Lock()
	delete(m.agents, id)
	delete(m.agentFTS5, id)
	delete(m.agentBleve, id)
	closers := m.agentClosers[id]
	delete(m.agentClosers, id)
	m.mu.Unlock()
	m.convReader.RemoveAgent(id)
	closeAll(closers)
}

// openAgentStores creates an agent's standalone memory stores (reminder,
// scratchpad, todo, task list), which exist whether or not memory search is
// configured. Databases live in the agent's workspace .data directory. On
// error, the stores it opened are closed again.
func openAgentStores(acfg config.AgentConfig) (agentMemory, []io.Closer, error) {
	var am agentMemory
	var closers []io.Closer
	fail := func(what string, err error) (agentMemory, []io.Closer, error) {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
		return agentMemory{}, nil, fmt.Errorf("create %s for %s: %w", what, acfg.ID, err)
	}

	rs, err := memory.NewReminderStore(config.AgentDataPath(acfg.Workspace, "reminders.db"))
	if err != nil {
		return fail("reminder store", err)
	}
	am.reminders = rs
	closers = append(closers, rs)

	sp, err := memory.NewScratchpad(config.AgentDataPath(acfg.Workspace, "scratchpad.db"))
	if err != nil {
		return fail("scratchpad", err)
	}
	am.scratchpad = sp
	closers = append(closers, sp)

	ts, err := memory.NewTodoStore(config.AgentDataPath(acfg.Workspace, "todo.db"))
	if err != nil {
		return fail("todo store", err)
	}
	am.todos = ts
	closers = append(closers, ts)

	tl, err := memory.NewTaskListStore(config.AgentDataPath(acfg.Workspace, "tasklist.db"))
	if err != nil {
		return fail("task list store", err)
	}
	am.taskList = tl
	closers = append(closers, tl)

	return am, closers, nil
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

// memoryBackend abstracts over FTS5 and bleve for the shared init logic.
type memoryBackend interface {
	memory.Searcher
	io.Closer
	Reindex() error
	Watch() error
	StartSweep(initial, interval time.Duration)
}

// parseMemoryDuration parses a duration string. Returns 0 for empty or "0".
func parseMemoryDuration(s, label string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", label, err)
	}
	return d, nil
}

// startBackend registers b for cleanup, starts watching and sweeping, and
// kicks off an async initial reindex, staggered after the indices before it.
// owner is the agent whose own index b is ("" for a shared one).
func (m *memorySystem) startBackend(owner, label string, b memoryBackend, debounce, sweepInterval time.Duration) {
	m.mu.Lock()
	if owner == "" {
		m.closers = append(m.closers, b)
	} else {
		m.agentClosers[owner] = append(m.agentClosers[owner], b)
	}
	stagger := m.reindexStagger
	m.reindexStagger += reindexStaggerStep
	m.mu.Unlock()
	go func() {
		if stagger > 0 {
			time.Sleep(stagger)
		}
		if err := b.Reindex(); err != nil {
			mainLog.Errorf("initial reindex %s: %v", label, err)
		} else {
			mainLog.Infof("%s: initial reindex complete", label)
		}
	}()
	if debounce > 0 || len(m.globalSources) > 0 || m.perAgent {
		if err := b.Watch(); err != nil {
			mainLog.Errorf("start %s file watching: %v", label, err)
		}
	}
	if sweepInterval > 0 {
		// Offset the sweep phase per-agent too, so periodic sweeps (if
		// re-enabled) also don't cluster.
		b.StartSweep(30*time.Second+stagger, sweepInterval)
	}
}

// buildBackends creates the FTS5 and/or bleve backend for a set of sources
// using the resolved memory settings. dbPath and blevePath must be fully
// resolved absolute paths.
func (m *memorySystem) buildBackends(owner, label string, sources map[string]memory.SourceConfig, dbPath, blevePath string, rm config.ResolvedMemorySearch) (map[string]memory.Searcher, *memory.Index, *memory.BleveIndex, error) {
	backends := make(map[string]memory.Searcher)
	var fts5Idx *memory.Index
	var bleveIdx *memory.BleveIndex

	debounce, err := parseMemoryDuration(rm.ReindexDebounce, fmt.Sprintf("reindex_debounce for %s", label))
	if err != nil {
		return nil, nil, nil, err
	}
	sweepInterval, err := parseMemoryDuration(rm.SweepInterval, fmt.Sprintf("sweep_interval for %s", label))
	if err != nil {
		return nil, nil, nil, err
	}

	if rm.SearchBackend == "fts5" {
		idx, err := memory.NewIndex(dbPath, sources, debounce, rm.ConversationWeight)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("create FTS5 index (%s): %w", label, err)
		}
		idx.SetTemporalDecay(rm.TemporalDecay, rm.DecayHalfLife, rm.DecayBoost, rm.EvergreenPatterns) // #352
		idx.SetSearchLimit(rm.SearchLimit)
		m.startBackend(owner, fmt.Sprintf("FTS5 (%s)", label), idx, debounce, sweepInterval)
		backends["fts5"] = idx
		fts5Idx = idx
	}

	if rm.SearchBackend == "bleve" {
		bidx, err := memory.NewBleveIndex(blevePath, sources, debounce, rm.ConversationWeight)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("create bleve index (%s): %w", label, err)
		}
		bidx.SetTemporalDecay(rm.TemporalDecay, rm.DecayHalfLife, rm.DecayBoost, rm.EvergreenPatterns) // #352
		bidx.SetSearchLimit(rm.SearchLimit)
		m.startBackend(owner, fmt.Sprintf("bleve (%s)", label), bidx, debounce, sweepInterval)
		backends["bleve"] = bidx
		bleveIdx = bidx
	}

	return backends, fts5Idx, bleveIdx, nil
}

// buildAgentIndex builds one agent's own indices (per-agent mode): global +
// agent-specific sources, with the agent's resolved settings (backend,
// debounce, weight, sweep). An agent with no sources at all gets none.
func (m *memorySystem) buildAgentIndex(cfg *config.Config, acfg config.AgentConfig) error {
	combined := buildAgentMemorySources(m.globalSources, acfg.Memory.Sources)
	if len(combined) == 0 {
		return nil
	}
	rm := resolvedMemorySettings(cfg, acfg)
	backends, fts5Idx, bleveIdx, err := m.buildBackends(
		acfg.ID,
		fmt.Sprintf("agent %s", acfg.ID),
		combined,
		config.AgentDataPath(acfg.Workspace, "memory.db"),
		config.AgentDataPath(acfg.Workspace, "search.bleve"),
		rm,
	)
	if err != nil {
		return err
	}
	m.mu.Lock()
	am := m.agents[acfg.ID]
	am.backends = backends
	m.agents[acfg.ID] = am
	if fts5Idx != nil {
		m.agentFTS5[acfg.ID] = fts5Idx
	}
	if bleveIdx != nil {
		m.agentBleve[acfg.ID] = bleveIdx
	}
	m.mu.Unlock()
	mainLog.Infof("agent %q: memory backend %s with %d sources", acfg.ID, rm.SearchBackend, len(combined))
	return nil
}

// indexAgentConversation is convo.Hook in per-agent mode: it routes a logged
// entry to the indices of the agent its session key names.
func (m *memorySystem) indexAgentConversation(text, session string, rowID int64) {
	m.mu.RLock()
	var fts5Idx *memory.Index
	var bleveIdx *memory.BleveIndex
	for agentID, idx := range m.agentFTS5 {
		if strings.HasPrefix(session, agentID+"/") {
			fts5Idx = idx
			break
		}
	}
	for agentID, idx := range m.agentBleve {
		if strings.HasPrefix(session, agentID+"/") {
			bleveIdx = idx
			break
		}
	}
	m.mu.RUnlock()
	if fts5Idx != nil {
		fts5Idx.IndexConversation(text, session, rowID)
	}
	if bleveIdx != nil {
		bleveIdx.IndexConversation(text, session, rowID)
	}
}

// wireTodoSearch wires an agent's todo store to its bleve index (its own in
// per-agent mode, else the shared one) for full-text search, and indexes its
// existing todos in the background.
func (m *memorySystem) wireTodoSearch(agentID string) {
	m.mu.RLock()
	ts := m.agents[agentID].todos
	idx, ok := m.agentBleve[agentID]
	m.mu.RUnlock()
	if !ok {
		idx = m.sharedBleve
	}
	if ts == nil || idx == nil {
		return
	}
	ts.SetSearchIndex(idx)
	go func() {
		if err := ts.IndexAllTodos(agentID); err != nil {
			mainLog.Errorf("index todos for agent %s: %v", agentID, err)
		}
	}()
}

// backfillAgent backfills one agent's historical conversations into its own
// indices (per-agent mode), in the background.
func (m *memorySystem) backfillAgent(acfg config.AgentConfig) {
	m.mu.RLock()
	bleveIdx := m.agentBleve[acfg.ID]
	fts5Idx := m.agentFTS5[acfg.ID]
	m.mu.RUnlock()
	dbPath := config.AgentDataPath(acfg.Workspace, "conversation.db")
	if bleveIdx != nil {
		if n, err := bleveIdx.BackfillConversations(dbPath); err != nil {
			mainLog.Errorf("backfill conversations for agent %s: %v", acfg.ID, err)
		} else if n > 0 {
			mainLog.Infof("backfilled %d conversation messages into bleve for agent %s", n, acfg.ID)
		}
	}
	if fts5Idx != nil {
		if n, err := fts5Idx.BackfillConversations(dbPath); err != nil {
			mainLog.Errorf("backfill conversations (fts5) for agent %s: %v", acfg.ID, err)
		} else if n > 0 {
			mainLog.Infof("backfilled %d conversation messages into FTS5 for agent %s", n, acfg.ID)
		}
	}
}

// errMemoryClosed is returned by addAgent once shutdown has begun.
var errMemoryClosed = errors.New("memory system is shutting down")

// addAgent adds an agent to the running memory system (a hot-add from
// `/agents new`, #963) through the same steps boot runs for each agent. cfg
// carries the global memory settings. On error nothing is left registered or
// open. The returned note, when non-empty, tells the user about a memory
// setting the running process cannot honour without a restart.
func (m *memorySystem) addAgent(cfg *config.Config, acfg config.AgentConfig) (note string, err error) {
	am, closers, err := openAgentStores(acfg)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
		return "", errMemoryClosed
	}
	if _, dup := m.agents[acfg.ID]; dup {
		m.mu.Unlock()
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
		return "", fmt.Errorf("memory stores for %s already open", acfg.ID)
	}
	m.agents[acfg.ID] = am
	m.agentClosers[acfg.ID] = append(m.agentClosers[acfg.ID], closers...)
	m.mu.Unlock()

	if !m.enabled {
		if hasAgentMemoryOverrides([]config.AgentConfig{acfg}) {
			note = "Its [agents.memory] settings take effect after /restart (memory search is off in the running process)."
		}
		return note, nil
	}

	if m.perAgent {
		if err := m.buildAgentIndex(cfg, acfg); err != nil {
			m.mu.Lock()
			delete(m.agents, acfg.ID)
			m.mu.Unlock()
			// The stores stay in agentClosers; they close at shutdown with the rest.
			return "", err
		}
		// Add under mu: cleanup sets closed under mu before it Waits, so an
		// Add here never races a Wait that has already begun.
		m.mu.Lock()
		if !m.closed {
			m.backfillWG.Add(1)
			go func() {
				defer m.backfillWG.Done()
				m.backfillAgent(acfg)
			}()
		}
		m.mu.Unlock()
	} else if hasAgentMemoryOverrides([]config.AgentConfig{acfg}) {
		// Boot with this agent would switch EVERY agent to per-agent
		// indices; the running agents cannot change mode live.
		note = "Its [agents.memory] settings take effect after /restart (the running process uses the shared memory index)."
	}

	m.wireTodoSearch(acfg.ID)
	if m.convReader != nil {
		m.convReader.AddAgent(acfg.ID, config.AgentDataPath(acfg.Workspace, "conversation.db"))
	}
	return note, nil
}

// initMemorySystem sets up memory indices, the per-agent stores and the
// conversation hook for every configured agent. Any error is fatal at boot.
func initMemorySystem(cfg *config.Config) *memorySystem {
	m := &memorySystem{
		globalSources:  make(map[string]memory.SourceConfig),
		sharedBackends: make(map[string]memory.Searcher),
		agents:         make(map[string]agentMemory),
		agentFTS5:      make(map[string]*memory.Index),
		agentBleve:     make(map[string]*memory.BleveIndex),
		agentClosers:   make(map[string][]io.Closer),
	}

	// Build global source map from [memory] config
	for _, src := range cfg.Memory.Sources {
		m.globalSources[src.Name] = memory.SourceConfig{Dir: src.Dir, Weight: src.Weight}
	}

	m.perAgent = hasAgentMemoryOverrides(cfg.Agents)
	m.enabled = len(m.globalSources) > 0 || m.perAgent

	// Always create standalone stores (scratchpad, todo, reminders, task list)
	// even when no memory search sources are configured.
	for _, acfg := range cfg.Agents {
		am, closers, err := openAgentStores(acfg)
		if err != nil {
			log.Fatalf("main", "%v", err)
		}
		m.agents[acfg.ID] = am
		m.agentClosers[acfg.ID] = closers
	}

	if !m.enabled {
		return m
	}

	if m.perAgent {
		for _, acfg := range cfg.Agents {
			if err := m.buildAgentIndex(cfg, acfg); err != nil {
				log.Fatalf("main", "%v", err)
			}
		}
		// Conversation hook: route to the agent's indices by session key
		// prefix. Installed even with no index yet, so an agent hot-added
		// later is routed without reassigning convo.Hook under its readers.
		convo.Hook = m.indexAgentConversation
	} else {
		// Shared indices — no agent overrides memory settings.
		// Resolve from global config (empty agent = no overrides).
		rm := resolvedMemorySettings(cfg, config.AgentConfig{})
		backends, fts5Idx, bleveIdx, err := m.buildBackends("", "shared", m.globalSources, cfg.DataPath("memory.db"), cfg.DataPath("search.bleve"), rm)
		if err != nil {
			log.Fatalf("main", "%v", err)
		}
		m.sharedBackends = backends
		m.sharedFTS5 = fts5Idx
		m.sharedBleve = bleveIdx

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
	for _, acfg := range cfg.Agents {
		m.wireTodoSearch(acfg.ID)
	}

	// Build ConversationReader for context lookup around search results.
	convDBPaths := make(map[string]string)
	for _, acfg := range cfg.Agents {
		convDBPaths[acfg.ID] = config.AgentDataPath(acfg.Workspace, "conversation.db")
	}
	m.convReader = memory.NewConversationReader(convDBPaths)

	// Backfill historical conversations into the indices, in the background.
	// Per-agent: each agent's own indices rebuild from its own conversation
	// DB. Shared bleve: every agent's DB into the one index. Shared FTS5
	// (#352 / backend parity): the wipe+rebuild is index-wide, so ALL agents'
	// conversation DBs are rebuilt in a single call.
	if m.perAgent {
		m.backfillWG.Add(1)
		go func() {
			defer m.backfillWG.Done()
			for _, acfg := range cfg.Agents {
				m.backfillAgent(acfg)
			}
		}()
		return m
	}
	if m.sharedBleve != nil {
		m.backfillWG.Add(1)
		go func() {
			defer m.backfillWG.Done()
			for _, acfg := range cfg.Agents {
				dbPath := config.AgentDataPath(acfg.Workspace, "conversation.db")
				n, err := m.sharedBleve.BackfillConversations(dbPath)
				if err != nil {
					mainLog.Errorf("backfill conversations for agent %s: %v", acfg.ID, err)
				} else if n > 0 {
					mainLog.Infof("backfilled %d conversation messages into bleve for agent %s", n, acfg.ID)
				}
			}
		}()
	}
	if m.sharedFTS5 != nil {
		m.backfillWG.Add(1)
		go func() {
			defer m.backfillWG.Done()
			paths := make([]string, 0, len(cfg.Agents))
			for _, acfg := range cfg.Agents {
				paths = append(paths, config.AgentDataPath(acfg.Workspace, "conversation.db"))
			}
			n, err := m.sharedFTS5.BackfillConversations(paths...)
			if err != nil {
				mainLog.Errorf("backfill conversations (fts5, shared): %v", err)
			} else if n > 0 {
				mainLog.Infof("backfilled %d conversation messages into shared FTS5", n)
			}
		}()
	}
	return m
}
