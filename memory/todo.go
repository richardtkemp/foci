package memory

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// TodoItem represents a single todo entry.
type TodoItem struct {
	ID          int64
	Text        string
	Status      string // "open" or "done"
	Priority    string // "high", "medium", "low"
	AgentID     string
	CreatedAt   time.Time
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
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS todos (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		text         TEXT    NOT NULL,
		status       TEXT    NOT NULL DEFAULT 'open',
		priority     TEXT    NOT NULL DEFAULT 'medium',
		agent_id     TEXT    NOT NULL,
		created_at   TEXT    NOT NULL,
		completed_at TEXT
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create todos table: %w", err)
	}

	return &TodoStore{db: db}, nil
}

// Add creates a new todo item and returns its ID.
func (s *TodoStore) Add(agentID, text, priority string) (int64, error) {
	if priority == "" {
		priority = "medium"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO todos (text, status, priority, agent_id, created_at) VALUES (?, 'open', ?, ?, ?)`,
		text, priority, agentID, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// List returns todo items for an agent, optionally filtered by status.
// If status is empty, returns all items.
func (s *TodoStore) List(agentID, status string) ([]TodoItem, error) {
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.db.Query(
			`SELECT id, text, status, priority, agent_id, created_at, completed_at FROM todos WHERE agent_id = ? AND status = ? ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`,
			agentID, status,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, text, status, priority, agent_id, created_at, completed_at FROM todos WHERE agent_id = ? ORDER BY status ASC, CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 END, id`,
			agentID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanTodos(rows)
}

// Complete marks a todo item as done.
func (s *TodoStore) Complete(agentID string, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`UPDATE todos SET status = 'done', completed_at = ? WHERE id = ? AND agent_id = ?`,
		now, id, agentID,
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

// Close closes the underlying database.
func (s *TodoStore) Close() error {
	return s.db.Close()
}

func scanTodos(rows *sql.Rows) ([]TodoItem, error) {
	var items []TodoItem
	for rows.Next() {
		var item TodoItem
		var createdAt string
		var completedAt sql.NullString
		if err := rows.Scan(&item.ID, &item.Text, &item.Status, &item.Priority, &item.AgentID, &createdAt, &completedAt); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if completedAt.Valid {
			t, _ := time.Parse(time.RFC3339, completedAt.String)
			item.CompletedAt = &t
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
