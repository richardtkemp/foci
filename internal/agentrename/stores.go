package agentrename

import (
	"path/filepath"
	"sort"

	"foci/internal/config"
)

// paths are the filesystem locations the rename reads and rewrites, resolved
// once from the config (and kept in the journal, so a resumed run uses the
// same places even after foci.toml has changed).
type paths struct {
	DataDir     string   `json:"data_dir"`
	SessionsDir string   `json:"sessions_dir"`
	OldWS       string   `json:"old_workspace"`
	NewWS       string   `json:"new_workspace"` // == OldWS in keep mode
	CCProjects  string   `json:"cc_projects"`   // <CC config dir>/projects
	StateDB     string   `json:"state_db"`
	APIDB       string   `json:"api_db"`
	APIShadowDB string   `json:"api_shadow_db"`
	FramesDB    string   `json:"frames_db"`
	DeferDB     string   `json:"defer_db"`
	OpencodeDB  string   `json:"opencode_db"`
	CodexDBs    []string `json:"codex_dbs"`
	// DefaultMemorySource is set when the agent uses the default memory
	// source, whose name is the agent id (and so follows the rename).
	DefaultMemorySource bool `json:"default_memory_source"`
}

// dbSpec is one database step: a file and the rules that rewrite it.
type dbSpec struct {
	id    string // step id in the journal
	path  string
	rules []rule
	// fk: the file has foreign keys (turns/api_calls) — checked, deferred, at commit.
	fk bool
	// ws: the file lives in the agent's workspace (moves with it).
	ws bool
}

// workspaceDBs are the per-agent databases under <workspace>/.data.
var workspaceDBs = []string{"todo.db", "reminders.db", "scratchpad.db", "tasklist.db", "conversation.db", "memory.db"}

// dbSpecs returns every database step for renaming r, in order. wsDir is
// where the workspace databases are now (the old workspace, or the new one
// when a resumed run has already moved it). move adds the cwd/path rules of
// a workspace move.
func dbSpecs(p paths, r ids, wsDir string, move bool) []dbSpec {
	key := r.old + "/"
	eq := func(t, c string) rule {
		return rule{table: t, col: c, kind: kindEq, from: r.old, to: r.new, idRule: true, r: r}
	}
	pre := func(t, c string) rule {
		return rule{table: t, col: c, kind: kindPrefix, from: key, to: r.new + "/", idRule: true, r: r}
	}
	path := func(t, c, from, to string) rule {
		return rule{table: t, col: c, kind: kindPrefix, from: from, to: to, r: r}
	}
	val := func(t, c, where string, args ...any) rule {
		return rule{table: t, col: c, kind: kindValue, where: where, whereArgs: args, r: r}
	}
	sessFrom := filepath.Join(p.SessionsDir, r.old) + "/"
	sessTo := filepath.Join(p.SessionsDir, r.new) + "/"
	// Paths that point into the moved workspace and the CC project folder of
	// its cwd (transcripts), in a move only.
	movePaths := func(t, c string) []rule {
		if !move {
			return nil
		}
		return []rule{
			path(t, c, p.OldWS+"/", p.NewWS+"/"),
			path(t, c, filepath.Join(p.CCProjects, ccSlug(p.OldWS))+"/", filepath.Join(p.CCProjects, ccSlug(p.NewWS))+"/"),
		}
	}

	// state.db. Value and key rewrites select the agent's rows by its OLD id,
	// so they run before the agent_id rewrite.
	state := []rule{
		val("agent_metadata", "value", "agent_id = ?", r.old),
		{table: "agent_metadata", col: "key", kind: kindMetaKey, where: "agent_id = ?", whereArgs: []any{r.old}, r: r},
		// Facet bindings live under the "_system" pseudo-agent, valued by session key.
		val("agent_metadata", "value", "agent_id = '_system'"),
		eq("agent_metadata", "agent_id"),
		val("chat_metadata", "value", "agent_id = ? AND key = 'session_key'", r.old),
		eq("chat_metadata", "agent_id"),
		val("session_metadata", "value", "substr(session_key, 1, length(?1)) = ?1", key),
		pre("session_metadata", "session_key"),
		pre("session_index", "session_key"),
		pre("session_index", "parent_session_key"),
		eq("session_index", "agent_id"),
		path("session_index", "file_path", sessFrom, sessTo),
		pre("session_archives", "session_key"),
		path("session_archives", "file_path", sessFrom, sessTo),
		pre("backend_resume_history", "session_key"),
	}
	state = append(state, movePaths("session_index", "file_path")...)
	state = append(state, movePaths("session_archives", "file_path")...)

	api := func() []rule {
		rs := []rule{
			pre("turns", "turn_id"),
			pre("turns", "session"),
			eq("turns", "agent_id"),
			pre("api_calls", "session"),
			pre("api_calls", "turn_id"),
			pre("api_calls", "call_key"),
			eq("api_calls", "agent_id"),
			val("api_calls", "detail", "instr(detail, ?) > 0", `"`+key),
			pre("backend_reports", "session"),
			pre("backend_reports", "scope_key"),
		}
		return append(rs, movePaths("api_calls", "session_file")...)
	}

	specs := []dbSpec{
		{id: "db:state", path: p.StateDB, rules: state},
		{id: "db:api", path: p.APIDB, rules: api(), fk: true},
		{id: "db:api-shadow", path: p.APIShadowDB, rules: api(), fk: true},
		{id: "db:app-frames", path: p.FramesDB, rules: []rule{eq("app_frames", "agent_id"), eq("app_prompts", "agent_id")}},
		{id: "db:deferred-sends", path: p.DeferDB, rules: []rule{eq("deferred_sends", "agent_id"), pre("deferred_sends", "session_key")}},
	}
	wsRules := map[string][]rule{
		"todo.db":         {eq("todos", "agent_id")},
		"reminders.db":    {eq("reminders", "agent_id"), pre("reminders", "session_key")},
		"scratchpad.db":   {eq("scratchpad", "agent_id")},
		"tasklist.db":     {eq("tasks", "agent_id")},
		"conversation.db": {pre("messages", "session")},
	}
	if p.DefaultMemorySource {
		// The default source is named after the agent and indexed under
		// config.AgentMemorySourcePrefix ("agent/<id>"). Legacy "agent:<id>" rows
		// are no longer any source's and are left alone.
		src := func(t string) rule {
			return rule{table: t, col: "source", kind: kindEq, from: config.AgentMemorySourcePrefix + r.old, to: config.AgentMemorySourcePrefix + r.new, idRule: true, r: r}
		}
		wsRules["memory.db"] = []rule{src("memory_meta"), src("memory_fts")}
	}
	for _, f := range workspaceDBs {
		if rs, ok := wsRules[f]; ok {
			specs = append(specs, dbSpec{id: "ws:" + f, path: filepath.Join(wsDir, ".data", f), rules: rs, ws: true})
		}
	}
	if move {
		cwd := func(t, c string) []rule {
			return []rule{
				{table: t, col: c, kind: kindEq, from: p.OldWS, to: p.NewWS, r: r},
				path(t, c, p.OldWS+"/", p.NewWS+"/"),
			}
		}
		if p.OpencodeDB != "" {
			specs = append(specs, dbSpec{id: "ext:opencode", path: p.OpencodeDB, rules: cwd("session", "directory")})
		}
		codex := append([]string(nil), p.CodexDBs...)
		sort.Strings(codex)
		for _, f := range codex {
			specs = append(specs, dbSpec{id: "ext:codex:" + filepath.Base(f), path: f, rules: cwd("threads", "cwd")})
		}
	}
	return specs
}
