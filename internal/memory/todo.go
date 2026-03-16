package memory

import (
	"database/sql"
	"fmt"
	"sort"
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

	return &TodoStore{db: db}, nil
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

// List returns todo items for an agent, optionally filtered by status, tags, and/or priority.
// sort can be "created", "updated", "closed", or "priority".
// Default direction is descending (newest/highest first); reverse=true flips it.
// limit caps the number of results (0 = no limit).
// Multiple tags use AND logic: items must match all specified tag filters.
func (s *TodoStore) List(agentID, status string, tags []string, priority, sort string, reverse bool, limit int) ([]TodoItem, error) {
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
	for _, tag := range tags {
		if negated, val := isNegated(tag); negated {
			query += ` AND (',' || tags || ',' NOT LIKE '%,' || ? || ',%')`
			args = append(args, val)
		} else {
			query += ` AND (',' || tags || ',' LIKE '%,' || ? || ',%')`
			args = append(args, tag)
		}
	}
	if priority != "" {
		if negated, val := isNegated(priority); negated {
			query += ` AND priority != ?`
			args = append(args, val)
		} else {
			query += ` AND priority = ?`
			args = append(args, priority)
		}
	}

	// Apply sort order. Default direction is descending (newest/highest first);
	// reverse=true flips to ascending (oldest/lowest first).
	dir, idDir := "DESC", "DESC"
	if reverse {
		dir, idDir = "ASC", "ASC"
	}
	switch sort {
	case "created":
		query += fmt.Sprintf(` ORDER BY created_at %s, id %s`, dir, idDir)
	case "updated":
		query += fmt.Sprintf(` ORDER BY updated_at %s, id %s`, dir, idDir)
	case "closed":
		query += fmt.Sprintf(` ORDER BY completed_at %s, id %s`, dir, idDir)
	default: // "priority" or empty
		priOrder := `CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END`
		if reverse {
			priOrder = `CASE priority WHEN 'low' THEN 0 WHEN 'medium' THEN 1 WHEN 'high' THEN 2 END`
		}
		if status != "" && status != "active" {
			query += fmt.Sprintf(` ORDER BY %s, id %s`, priOrder, idDir)
		} else {
			statusOrder := `CASE status WHEN 'in_progress' THEN 0 WHEN 'open' THEN 1 WHEN 'done' THEN 2 WHEN 'dropped' THEN 3 END`
			query += fmt.Sprintf(` ORDER BY %s, %s, id %s`, statusOrder, priOrder, idDir)
		}
	}

	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
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

// TodoSearchOpts controls filtering, sorting, and limiting of search results.
type TodoSearchOpts struct {
	Status   string   // "open", "in_progress", "done", "dropped", "active" (excludes done/dropped), "" (no filter)
	Sort     string   // "relevance" (default), "created", "updated", "closed", "priority"
	Reverse  bool     // reverse sort direction (default false = descending/highest first)
	Limit    int      // max results (0 = default 10)
	Tags     []string // filter by tags (AND logic, comma-separated containment; "!tag" negates)
	Priority string   // filter by exact priority ("high", "medium", "low")
}

// Search returns todo items matching the query using full-text search
// with porter stemming (e.g. "running" matches "run"). Results are ranked
// by relevance, with a secondary sort by status and priority for ties.
// Requires a bleve search index (set via SetSearchIndex).
func (s *TodoStore) Search(agentID, query string, opts *TodoSearchOpts) ([]TodoItem, error) {
	if opts == nil {
		opts = &TodoSearchOpts{}
	}
	if s.searchIndex == nil {
		return nil, fmt.Errorf("todo search requires a search index (call SetSearchIndex)")
	}
	return s.searchBleve(agentID, query, opts)
}

// searchBleve queries the bleve index for matching todos, then loads
// full items from SQLite in the relevance order bleve returned.
// Status filtering is applied post-load since bleve doesn't index status.
func (s *TodoStore) searchBleve(agentID, query string, opts *TodoSearchOpts) ([]TodoItem, error) {
	// Request extra results when filtering by status to compensate for post-filter loss.
	bleveLimit := opts.Limit
	if bleveLimit <= 0 {
		bleveLimit = 10
	}
	// For sorts bleve can't do (priority, closed), or when post-filtering,
	// overfetch and post-sort.
	needsPostSort := opts.Sort == "priority" || opts.Sort == "closed"
	needsPostFilter := opts.Status != "" || len(opts.Tags) > 0 || opts.Priority != ""
	if needsPostFilter || needsPostSort {
		bleveLimit *= 5
		if bleveLimit < 50 {
			bleveLimit = 50
		}
	}

	// Translate sort+reverse into bleve sort order.
	bleveSort := opts.Sort
	if opts.Reverse && (bleveSort == "created" || bleveSort == "updated") {
		bleveSort = bleveSort + "_asc"
	}
	// For sorts bleve can't handle, fetch by relevance and post-sort.
	if needsPostSort {
		bleveSort = "relevance"
	}

	hits, err := s.searchIndex.SearchTodos(agentID, query, bleveSort, bleveLimit)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Load full items from SQLite by ID, maintaining bleve's order.
	// Filter by status, tag, and priority during load.
	items := make([]TodoItem, 0, len(hits))
	for _, hit := range hits {
		item, err := s.Get(agentID, hit.TodoID)
		if err != nil {
			continue // deleted between search and load
		}
		if !matchesStatusFilter(item.Status, opts.Status) {
			continue
		}
		if !matchesTagFilters(item.Tags, opts.Tags) {
			continue
		}
		if !matchesPriorityFilter(item.Priority, opts.Priority) {
			continue
		}
		items = append(items, *item)
	}

	// Post-sort for sort orders bleve can't handle.
	if needsPostSort {
		sortTodoItems(items, opts.Sort, opts.Reverse)
	}

	// Apply limit.
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// isNegated checks whether a filter value has a "!" prefix indicating negation.
// Returns the negation flag and the bare value.
func isNegated(v string) (bool, string) {
	if strings.HasPrefix(v, "!") {
		return true, v[1:]
	}
	return false, v
}

// matchesTagFilters checks whether a todo's comma-separated tags satisfy all
// filter tags (AND logic). Each filter may be "!"-prefixed to negate.
func matchesTagFilters(tags string, filters []string) bool {
	for _, filter := range filters {
		negated, val := isNegated(filter)
		found := false
		for _, t := range strings.Split(tags, ",") {
			if strings.TrimSpace(t) == val {
				found = true
				break
			}
		}
		if found == negated {
			return false // positive filter not found, or negated filter found
		}
	}
	return true
}

// matchesPriorityFilter checks whether a todo's priority matches the filter.
// A "!"-prefixed filter negates: the item must NOT have this priority.
func matchesPriorityFilter(priority, filter string) bool {
	if filter == "" {
		return true
	}
	negated, val := isNegated(filter)
	if negated {
		return priority != val
	}
	return priority == val
}

// matchesStatusFilter checks whether a todo's status passes the filter.
func matchesStatusFilter(status, filter string) bool {
	switch filter {
	case "":
		return true
	case "active":
		return status != "done" && status != "dropped"
	default:
		return status == filter
	}
}

// sortTodoItems sorts items in-place by the given sort field and direction.
// Default direction is descending (newest/highest first); reverse=true flips it.
func sortTodoItems(items []TodoItem, sortField string, reverse bool) {
	sort.Slice(items, func(i, j int) bool {
		var less bool
		switch sortField {
		case "created":
			less = items[i].CreatedAt.Before(items[j].CreatedAt)
		case "updated":
			less = items[i].UpdatedAt.Before(items[j].UpdatedAt)
		case "closed":
			ti, tj := items[i].CompletedAt, items[j].CompletedAt
			switch {
			case ti == nil && tj == nil:
				less = items[i].ID < items[j].ID
			case ti == nil:
				less = true // nil sorts before non-nil
			case tj == nil:
				less = false
			default:
				less = ti.Before(*tj)
			}
		case "priority":
			pi := priorityOrd(items[i].Priority)
			pj := priorityOrd(items[j].Priority)
			if pi != pj {
				less = pi < pj
			} else {
				less = items[i].ID < items[j].ID
			}
		default:
			less = items[i].ID < items[j].ID
		}
		// Default is descending (newest/highest first), so invert.
		if !reverse {
			return !less
		}
		return less
	})
}

// priorityOrd returns a numeric rank for priority (high=0, medium=1, low=2).
func priorityOrd(p string) int {
	switch p {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
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
