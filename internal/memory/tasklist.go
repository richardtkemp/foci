package memory

import (
	"database/sql"
	"fmt"
	"time"

	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// Task is a single tracked task.
type Task struct {
	ID          int
	Subject     string
	Description string
	Status      string // "pending", "in_progress", "completed"
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TaskListStore persists ephemeral tasks in SQLite.
// Tasks are scoped per agent with auto-incrementing IDs.
type TaskListStore struct {
	db *sql.DB
}

// NewTaskListStore creates or opens the task list store.
func NewTaskListStore(dbPath string) (*TaskListStore, error) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, err
	}

	// Check if old schema exists (single-row task_list table)
	var oldTable string
	_ = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='task_list'").Scan(&oldTable)
	if oldTable == "task_list" {
		// Drop old table — data model changed completely
		if _, err := db.Exec("DROP TABLE task_list"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("drop old task_list table: %w", err)
		}
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id          INTEGER NOT NULL,
		agent_id    TEXT    NOT NULL,
		subject     TEXT    NOT NULL,
		description TEXT    NOT NULL DEFAULT '',
		status      TEXT    NOT NULL DEFAULT 'pending',
		created_at  TEXT    NOT NULL,
		updated_at  TEXT,
		PRIMARY KEY (agent_id, id)
	)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create tasks table: %w", err)
	}

	return &TaskListStore{db: db}, nil
}

// Create adds a new task and returns its per-agent ID.
func (s *TaskListStore) Create(agentID, subject, description string) (int, error) {
	now := timeutil.FormatNano(timeutil.Now())
	var id int
	err := s.db.QueryRow(
		`INSERT INTO tasks (id, agent_id, subject, description, status, created_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM tasks WHERE agent_id = ?), ?, ?, ?, 'pending', ?, ?)
		 RETURNING id`,
		agentID, agentID, subject, description, now, now,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Get returns a single task by ID, or nil if not found.
func (s *TaskListStore) Get(agentID string, id int) (*Task, error) {
	var t Task
	var createdAt string
	var updatedAt sql.NullString
	err := s.db.QueryRow(
		`SELECT id, subject, description, status, created_at, updated_at
		 FROM tasks WHERE agent_id = ? AND id = ?`,
		agentID, id,
	).Scan(&t.ID, &t.Subject, &t.Description, &t.Status, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if updatedAt.Valid {
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt.String)
	}
	return &t, nil
}

// Update modifies a task. Only non-empty fields are applied.
// status="deleted" removes the task.
func (s *TaskListStore) Update(agentID string, id int, subject, description, status string) error {
	if status == "deleted" {
		res, err := s.db.Exec("DELETE FROM tasks WHERE agent_id = ? AND id = ?", agentID, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("task #%d not found", id)
		}
		return nil
	}

	var sets []string
	var args []any
	if subject != "" {
		sets = append(sets, "subject = ?")
		args = append(args, subject)
	}
	if description != "" {
		sets = append(sets, "description = ?")
		args = append(args, description)
	}
	if status != "" {
		sets = append(sets, "status = ?")
		args = append(args, status)
	}
	if len(sets) == 0 {
		return fmt.Errorf("nothing to update")
	}

	now := timeutil.FormatNano(timeutil.Now())
	sets = append(sets, "updated_at = ?")
	args = append(args, now)
	args = append(args, agentID, id)

	// #nosec G202 - sets contains only hard-coded column names
	query := "UPDATE tasks SET " + joinStrings(sets, ", ") + " WHERE agent_id = ? AND id = ?"
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("task #%d not found", id)
	}
	return nil
}

// List returns all non-deleted tasks for an agent, ordered by ID.
func (s *TaskListStore) List(agentID string) ([]Task, error) {
	rows, err := s.db.Query(
		`SELECT id, subject, description, status, created_at, updated_at
		 FROM tasks WHERE agent_id = ? ORDER BY id`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tasks []Task
	for rows.Next() {
		var t Task
		var createdAt string
		var updatedAt sql.NullString
		if err := rows.Scan(&t.ID, &t.Subject, &t.Description, &t.Status, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		if updatedAt.Valid {
			t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt.String)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Clear removes all tasks for the given agent.
func (s *TaskListStore) Clear(agentID string) error {
	_, err := s.db.Exec("DELETE FROM tasks WHERE agent_id = ?", agentID)
	return err
}

// Close closes the underlying database.
func (s *TaskListStore) Close() error {
	return s.db.Close()
}

// joinStrings joins strings with a separator (avoids importing strings package).
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}
