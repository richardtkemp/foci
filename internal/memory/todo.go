package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"foci/internal/sqlite"
)

// TodoItem represents a single todo entry.
type TodoItem struct {
	ID          int64
	Text        string
	Status      string // "open", "in_progress", "done", "dropped"
	Priority    string // "high", "medium", "low"
	Tags        string // comma-separated tags (e.g. "background,daily")
	CloseReason string // reason for completion (set when status="done")
	AgentID     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// TodoStore persists todo items in SQLite.
type TodoStore struct {
	db          *sql.DB
	searchIndex *BleveIndex // optional bleve index for full-text search
}

// NewTodoStore creates or opens the todo database.
func NewTodoStore(dbPath string) (*TodoStore, error) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, err
	}

	closeOnErr := func(msg string, err error) (*TodoStore, error) {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", msg, err)
	}

	// Check if the table already exists and what schema it has.
	var tableDDL string
	err = db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='todos'").Scan(&tableDDL)
	if err != nil && err != sql.ErrNoRows {
		return closeOnErr("check table schema", err)
	}

	if err == sql.ErrNoRows {
		// Fresh database — create with composite primary key (agent_id, id).
		_, err = db.Exec(`CREATE TABLE todos (
			id           INTEGER NOT NULL,
			text         TEXT    NOT NULL,
			status       TEXT    NOT NULL DEFAULT 'open',
			priority     TEXT    NOT NULL DEFAULT 'medium',
			tags         TEXT    NOT NULL DEFAULT '',
			close_reason TEXT    NOT NULL DEFAULT '',
			agent_id     TEXT    NOT NULL,
			created_at   TEXT    NOT NULL,
			completed_at TEXT,
			updated_at   TEXT,
			PRIMARY KEY (agent_id, id)
		)`)
		if err != nil {
			return closeOnErr("create todos table", err)
		}
	} else {
		// Already new schema — ensure updated_at exists (defensive).
		if !columnExists(db, "todos", "updated_at") {
			if _, err := db.Exec("ALTER TABLE todos ADD COLUMN updated_at TEXT"); err != nil {
				return closeOnErr("add updated_at column", err)
			}
		}
	}

	if err := initTodoFTS(db); err != nil {
		return closeOnErr("init todo FTS", err)
	}

	return &TodoStore{db: db}, nil
}

// initTodoFTS creates the FTS5 virtual table and sync triggers for
// full-text search over todo text. Uses an external content table
// backed by todos, so no content is duplicated. Porter stemming
// provides morphological matching (e.g. "running" matches "run").
func initTodoFTS(db *sql.DB) error {
	// Check if FTS table already exists (to know if we need a rebuild).
	var ftsExists bool
	if err := db.QueryRow("SELECT 1 FROM sqlite_master WHERE type='table' AND name='todos_fts'").Scan(new(int)); err == nil {
		ftsExists = true
	}

	_, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS todos_fts USING fts5(
		text,
		content='todos',
		content_rowid='rowid',
		tokenize='porter unicode61'
	)`)
	if err != nil {
		return fmt.Errorf("create todos_fts: %w", err)
	}

	// Triggers keep the FTS index in sync with the todos table.
	for _, stmt := range []string{
		`CREATE TRIGGER IF NOT EXISTS todos_fts_ai AFTER INSERT ON todos BEGIN
			INSERT INTO todos_fts(rowid, text) VALUES (new.rowid, new.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS todos_fts_ad AFTER DELETE ON todos BEGIN
			INSERT INTO todos_fts(todos_fts, rowid, text) VALUES('delete', old.rowid, old.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS todos_fts_au AFTER UPDATE ON todos BEGIN
			INSERT INTO todos_fts(todos_fts, rowid, text) VALUES('delete', old.rowid, old.text);
			INSERT INTO todos_fts(rowid, text) VALUES (new.rowid, new.text);
		END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("create fts trigger: %w", err)
		}
	}

	// Populate FTS index from existing content on first creation.
	if !ftsExists {
		if _, err := db.Exec("INSERT INTO todos_fts(todos_fts) VALUES('rebuild')"); err != nil {
			return fmt.Errorf("rebuild fts index: %w", err)
		}
	}
	return nil
}

// columnExists checks whether a column exists in the given table.
func columnExists(db *sql.DB, table, column string) bool {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

// SetSearchIndex sets the bleve index used for full-text search. When set,
// Add/Edit/Remove operations update the bleve index, and Search queries it
// instead of the embedded FTS5 index.
func (s *TodoStore) SetSearchIndex(idx *BleveIndex) {
	s.searchIndex = idx
}

// IndexAllTodos indexes all existing todos for an agent into the bleve
// search index. Call this once after SetSearchIndex to populate the index
// with pre-existing items.
func (s *TodoStore) IndexAllTodos(agentID string) error {
	if s.searchIndex == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT id, text, updated_at, created_at FROM todos WHERE agent_id = ?`, agentID,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id int64
		var text, createdAt string
		var updatedAt sql.NullString
		if err := rows.Scan(&id, &text, &updatedAt, &createdAt); err != nil {
			return err
		}
		ts := createdAt
		if updatedAt.Valid {
			ts = updatedAt.String
		}
		t, _ := time.Parse(time.RFC3339, ts)
		s.searchIndex.IndexTodo(agentID, id, text, float64(t.Unix()))
	}
	return rows.Err()
}

// Add creates a new todo item and returns its per-agent ID.
// Each agent gets its own sequential ID space (1, 2, 3, ...).
func (s *TodoStore) Add(agentID, text, priority, tags string) (int64, error) {
	if priority == "" {
		priority = "medium"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var nextID int64
	err := s.db.QueryRow(
		`INSERT INTO todos (id, text, status, priority, tags, agent_id, created_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM todos WHERE agent_id = ?), ?, 'open', ?, ?, ?, ?, ?)
		 RETURNING id`,
		agentID, text, priority, tags, agentID, now, now,
	).Scan(&nextID)
	if err != nil {
		return 0, err
	}
	if s.searchIndex != nil {
		t, _ := time.Parse(time.RFC3339, now)
		s.searchIndex.IndexTodo(agentID, nextID, text, float64(t.Unix()))
	}
	return nextID, nil
}

// List returns todo items for an agent, optionally filtered by status, tag, and/or priority.
// sort can be "priority" (default), "created", or "updated".
func (s *TodoStore) List(agentID, status, tag, priority, sort string) ([]TodoItem, error) {
	query := `SELECT id, text, status, priority, tags, close_reason, agent_id, created_at, updated_at, completed_at FROM todos WHERE agent_id = ?`
	args := []any{agentID}

	switch status {
	case "":
		// No filter — return all statuses.
	case "active":
		// Exclude terminal statuses (done, dropped).
		query += ` AND status NOT IN ('done', 'dropped')`
	default:
		query += ` AND status = ?`
		args = append(args, status)
	}
	if tag != "" {
		// Match tag as whole word in comma-separated list
		query += ` AND (',' || tags || ',' LIKE '%,' || ? || ',%')`
		args = append(args, tag)
	}
	if priority != "" {
		query += ` AND priority = ?`
		args = append(args, priority)
	}

	// Apply sort order
	switch sort {
	case "created":
		query += ` ORDER BY created_at ASC, id ASC`
	case "updated":
		query += ` ORDER BY updated_at DESC, id DESC`
	default: // "priority" or empty (default)
		if status != "" && status != "active" {
			// Single-status filter: sort by priority only.
			query += ` ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`
		} else {
			// Multiple statuses visible: group by status, then priority.
			query += ` ORDER BY CASE status WHEN 'in_progress' THEN 0 WHEN 'open' THEN 1 WHEN 'done' THEN 2 WHEN 'dropped' THEN 3 END, CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`
		}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanTodos(rows)
}

// CountOpenByTag counts open todos with the given tag for an agent.
func (s *TodoStore) CountOpenByTag(agentID, tag string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM todos WHERE agent_id = ? AND status = 'open' AND (',' || tags || ',' LIKE '%,' || ? || ',%')`,
		agentID, tag,
	).Scan(&count)
	return count, err
}

// Complete marks a todo item as done with the given reason.
func (s *TodoStore) Complete(agentID string, id int64, reason string) error {
	return s.Transition(agentID, id, "done", reason)
}

// Transition changes a todo item's status. For "done" and "dropped", sets
// completed_at and close_reason. For "open", clears them.
func (s *TodoStore) Transition(agentID string, id int64, status, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var res sql.Result
	var err error
	switch status {
	case "open", "in_progress":
		res, err = s.db.Exec(
			`UPDATE todos SET status = ?, completed_at = NULL, updated_at = ?, close_reason = '' WHERE id = ? AND agent_id = ?`,
			status, now, id, agentID,
		)
	case "done", "dropped":
		res, err = s.db.Exec(
			`UPDATE todos SET status = ?, completed_at = ?, updated_at = ?, close_reason = ? WHERE id = ? AND agent_id = ?`,
			status, now, now, reason, id, agentID,
		)
	default:
		return fmt.Errorf("invalid status: %s", status)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("todo #%d not found", id)
	}
	return nil
}

// Remove deletes a todo item.
func (s *TodoStore) Remove(agentID string, id int64) error {
	res, err := s.db.Exec(`DELETE FROM todos WHERE id = ? AND agent_id = ?`, id, agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("todo #%d not found", id)
	}
	if s.searchIndex != nil {
		s.searchIndex.RemoveTodo(agentID, id)
	}
	return nil
}

// Edit updates fields on an existing todo item. Only non-empty text and priority
// are applied. Tags are updated only when setTags is true (allowing clearing to "").
// Returns the updated item.
func (s *TodoStore) Edit(agentID string, id int64, text, priority, tags string, setTags bool) (*TodoItem, error) {
	var setClauses []string
	var args []any

	if text != "" {
		setClauses = append(setClauses, "text = ?")
		args = append(args, text)
	}
	if priority != "" {
		setClauses = append(setClauses, "priority = ?")
		args = append(args, priority)
	}
	if setTags {
		setClauses = append(setClauses, "tags = ?")
		args = append(args, tags)
	}

	if len(setClauses) == 0 {
		return nil, fmt.Errorf("nothing to update")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, now)

	// #nosec G202 - setClauses contains only hard-coded column names, not user input
	query := "UPDATE todos SET " + strings.Join(setClauses, ", ") + " WHERE id = ? AND agent_id = ?"
	args = append(args, id, agentID)

	res, err := s.db.Exec(query, args...)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, fmt.Errorf("todo #%d not found", id)
	}

	// Re-read the updated row.
	row := s.db.QueryRow(
		`SELECT id, text, status, priority, tags, close_reason, agent_id, created_at, updated_at, completed_at FROM todos WHERE id = ? AND agent_id = ?`,
		id, agentID,
	)
	var item TodoItem
	var createdAt string
	var updatedAt sql.NullString
	var completedAt sql.NullString
	if err := row.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.Tags, &item.CloseReason, &item.AgentID, &createdAt, &updatedAt, &completedAt); err != nil {
		return nil, fmt.Errorf("re-read after edit: %w", err)
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if updatedAt.Valid {
		item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	}
	if completedAt.Valid {
		ct, _ := time.Parse(time.RFC3339, completedAt.String)
		item.CompletedAt = &ct
	}
	if s.searchIndex != nil {
		s.searchIndex.IndexTodo(agentID, id, item.Text, float64(item.UpdatedAt.Unix()))
	}
	return &item, nil
}

// Get returns a single todo item by ID.
func (s *TodoStore) Get(agentID string, id int64) (*TodoItem, error) {
	row := s.db.QueryRow(
		`SELECT id, text, status, priority, tags, close_reason, agent_id, created_at, updated_at, completed_at FROM todos WHERE id = ? AND agent_id = ?`,
		id, agentID,
	)
	var item TodoItem
	var createdAt string
	var updatedAt sql.NullString
	var completedAt sql.NullString
	if err := row.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.Tags, &item.CloseReason, &item.AgentID, &createdAt, &updatedAt, &completedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("todo #%d not found", id)
		}
		return nil, err
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if updatedAt.Valid {
		item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	}
	if completedAt.Valid {
		t, _ := time.Parse(time.RFC3339, completedAt.String)
		item.CompletedAt = &t
	}
	return &item, nil
}

// Search returns todo items matching the query using full-text search
// with porter stemming (e.g. "running" matches "run"). Results are ranked
// by relevance, with a secondary sort by status and priority for ties.
// Uses the bleve search index when available, falling back to FTS5.
func (s *TodoStore) Search(agentID, query string) ([]TodoItem, error) {
	if s.searchIndex != nil {
		return s.searchBleve(agentID, query)
	}
	return s.searchFTS5(agentID, query)
}

// searchBleve queries the bleve index for matching todos, then loads
// full items from SQLite in the relevance order bleve returned.
func (s *TodoStore) searchBleve(agentID, query string) ([]TodoItem, error) {
	hits, err := s.searchIndex.SearchTodos(agentID, query)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Load full items from SQLite by ID, maintaining bleve's relevance order
	items := make([]TodoItem, 0, len(hits))
	for _, hit := range hits {
		item, err := s.Get(agentID, hit.TodoID)
		if err != nil {
			continue // deleted between search and load
		}
		items = append(items, *item)
	}
	return items, nil
}

// searchFTS5 queries the embedded FTS5 index (fallback when no bleve index).
func (s *TodoStore) searchFTS5(agentID, query string) ([]TodoItem, error) {
	ftsQuery := buildFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT t.id, t.text, t.status, t.priority, t.tags, t.close_reason,
		        t.agent_id, t.created_at, t.updated_at, t.completed_at
		 FROM todos_fts f
		 JOIN todos t ON t.rowid = f.rowid
		 WHERE todos_fts MATCH ? AND t.agent_id = ?
		 ORDER BY f.rank,
		          CASE t.status WHEN 'in_progress' THEN 0 WHEN 'open' THEN 1 WHEN 'done' THEN 2 WHEN 'dropped' THEN 3 END,
		          CASE t.priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END,
		          t.id
		 LIMIT 50`,
		ftsQuery, agentID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanTodos(rows)
}

// buildFTSQuery converts a user query into an FTS5 match expression.
// Each term is quoted to prevent FTS5 syntax errors from special
// characters, while still allowing porter stemming to apply.
func buildFTSQuery(query string) string {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}

// Close closes the underlying database.
func (s *TodoStore) Close() error {
	return s.db.Close()
}

func scanTodos(rows *sql.Rows) ([]TodoItem, error) {
	var items []TodoItem
	for rows.Next() {
		var item TodoItem
		var createdAt string
		var updatedAt sql.NullString
		var completedAt sql.NullString
		if err := rows.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.Tags, &item.CloseReason, &item.AgentID, &createdAt, &updatedAt, &completedAt); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if updatedAt.Valid {
			item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
		}
		if completedAt.Valid {
			t, _ := time.Parse(time.RFC3339, completedAt.String)
			item.CompletedAt = &t
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// FormatTags returns a display string for tags, or empty if none.
func FormatTags(tags string) string {
	if tags == "" {
		return ""
	}
	var parts []string
	for _, t := range strings.Split(tags, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " {" + strings.Join(parts, ",") + "}"
}
