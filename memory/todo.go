package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// TodoItem represents a single todo entry.
type TodoItem struct {
	ID          int64
	Text        string
	Status      string // "open" or "done"
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
	db *sql.DB
}

// NewTodoStore creates or opens the todo database.
func NewTodoStore(dbPath string) (*TodoStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open todo db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS todos (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		text         TEXT    NOT NULL,
		status       TEXT    NOT NULL DEFAULT 'open',
		priority     TEXT    NOT NULL DEFAULT 'medium',
		tags         TEXT    NOT NULL DEFAULT '',
		close_reason TEXT    NOT NULL DEFAULT '',
		agent_id     TEXT    NOT NULL,
		created_at   TEXT    NOT NULL,
		completed_at TEXT
	)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create todos table: %w", err)
	}

	var hasUpdatedAt bool
	rows, err := db.Query("PRAGMA table_info(todos)")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check table info: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return nil, fmt.Errorf("scan table info: %w", err)
		}
		if name == "updated_at" {
			hasUpdatedAt = true
		}
	}
	_ = rows.Close()

	if !hasUpdatedAt {
		if _, err := db.Exec("ALTER TABLE todos ADD COLUMN updated_at TEXT"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("add updated_at column: %w", err)
		}
	}

	return &TodoStore{db: db}, nil
}

// Add creates a new todo item and returns its ID.
func (s *TodoStore) Add(agentID, text, priority, tags string) (int64, error) {
	if priority == "" {
		priority = "medium"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO todos (text, status, priority, tags, agent_id, created_at, updated_at) VALUES (?, 'open', ?, ?, ?, ?, ?)`,
		text, priority, tags, agentID, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// List returns todo items for an agent, optionally filtered by status and/or tag.
func (s *TodoStore) List(agentID, status, tag string) ([]TodoItem, error) {
	query := `SELECT id, text, status, priority, tags, close_reason, agent_id, created_at, updated_at, completed_at FROM todos WHERE agent_id = ?`
	args := []any{agentID}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if tag != "" {
		// Match tag as whole word in comma-separated list
		query += ` AND (',' || tags || ',' LIKE '%,' || ? || ',%')`
		args = append(args, tag)
	}

	if status != "" {
		query += ` ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`
	} else {
		query += ` ORDER BY status ASC, CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`
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
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`UPDATE todos SET status = 'done', completed_at = ?, updated_at = ?, close_reason = ? WHERE id = ? AND agent_id = ?`,
		now, now, reason, id, agentID,
	)
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
	var updatedAt string
	var completedAt sql.NullString
	if err := row.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.Tags, &item.CloseReason, &item.AgentID, &createdAt, &updatedAt, &completedAt); err != nil {
		return nil, fmt.Errorf("re-read after edit: %w", err)
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if completedAt.Valid {
		t, _ := time.Parse(time.RFC3339, completedAt.String)
		item.CompletedAt = &t
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
	var updatedAt string
	var completedAt sql.NullString
	if err := row.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.Tags, &item.CloseReason, &item.AgentID, &createdAt, &updatedAt, &completedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("todo #%d not found", id)
		}
		return nil, err
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if completedAt.Valid {
		t, _ := time.Parse(time.RFC3339, completedAt.String)
		item.CompletedAt = &t
	}
	return &item, nil
}

// Search returns todo items matching a case-insensitive substring query.
func (s *TodoStore) Search(agentID, query string) ([]TodoItem, error) {
	rows, err := s.db.Query(
		`SELECT id, text, status, priority, tags, close_reason, agent_id, created_at, updated_at, completed_at FROM todos WHERE agent_id = ? AND text LIKE '%' || ? || '%' COLLATE NOCASE ORDER BY status ASC, CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`,
		agentID, query,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanTodos(rows)
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
		var updatedAt string
		var completedAt sql.NullString
		if err := rows.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.Tags, &item.CloseReason, &item.AgentID, &createdAt, &updatedAt, &completedAt); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
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
