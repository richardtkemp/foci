package memory

import (
	"database/sql"
	"encoding/json"
	"time"

	"foci/internal/sqlite"
)

// TaskStep is a single step in a task list.
type TaskStep struct {
	Text   string `json:"text"`
	Status string `json:"status"` // "pending", "done", "skipped"
}

// TaskList is an ordered set of steps for the current task.
type TaskList struct {
	Goal    string
	Steps   []TaskStep
	Updated time.Time
}

// TaskListStore persists ephemeral task lists in SQLite.
// One active list per agent (not per-session).
type TaskListStore struct {
	db *sql.DB
}

// NewTaskListStore creates or opens the task list store.
func NewTaskListStore(dbPath string) (*TaskListStore, error) {
	db, err := sqlite.OpenInit(dbPath, `CREATE TABLE IF NOT EXISTS task_list (
		agent_id TEXT PRIMARY KEY,
		goal     TEXT NOT NULL,
		steps    TEXT NOT NULL,
		updated  TEXT NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	return &TaskListStore{db: db}, nil
}

// Set creates or replaces the task list for the given agent.
func (s *TaskListStore) Set(agentID, goal string, steps []TaskStep) error {
	data, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(
		`INSERT INTO task_list (agent_id, goal, steps, updated) VALUES (?, ?, ?, ?)
		 ON CONFLICT(agent_id) DO UPDATE SET goal = excluded.goal, steps = excluded.steps, updated = excluded.updated`,
		agentID, goal, string(data), now,
	)
	return err
}

// Get returns the active task list for the given agent, or nil if none exists.
func (s *TaskListStore) Get(agentID string) (*TaskList, error) {
	var goal, stepsJSON, updated string
	err := s.db.QueryRow(
		"SELECT goal, steps, updated FROM task_list WHERE agent_id = ?", agentID,
	).Scan(&goal, &stepsJSON, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var steps []TaskStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil, err
	}
	t, _ := time.Parse(time.RFC3339, updated)
	return &TaskList{Goal: goal, Steps: steps, Updated: t}, nil
}

// Clear removes the task list for the given agent.
func (s *TaskListStore) Clear(agentID string) error {
	_, err := s.db.Exec("DELETE FROM task_list WHERE agent_id = ?", agentID)
	return err
}

// Close closes the underlying database.
func (s *TaskListStore) Close() error {
	return s.db.Close()
}
