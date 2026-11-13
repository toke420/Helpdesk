package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------- Activities: agents' to-do lists ----------

type TaskCase struct {
	ID          int64  `json:"id"`
	Subject     string `json:"subject"`
	Status      string `json:"status"`
	ContactName string `json:"contact_name"`
}

type Task struct {
	ID             int64     `json:"id"`
	Title          string    `json:"title"`
	Notes          string    `json:"notes"`
	Kind           string    `json:"kind"`
	High           bool      `json:"high"`
	DueDate        *string   `json:"due_date"` // YYYY-MM-DD in the team's calendar, nil = no date
	DueTime        *string   `json:"due_time"` // HH:MM, only with a date
	AssigneeID     *int64    `json:"assignee_id"`
	CreatedBy      *int64    `json:"created_by"`
	ConversationID *int64    `json:"conversation_id"`
	Position       float64   `json:"position"`
	DoneAt         *int64    `json:"done_at"`
	DoneBy         *int64    `json:"done_by"`
	CreatedAt      int64     `json:"created_at"`
	UpdatedAt      int64     `json:"updated_at"`
	Case           *TaskCase `json:"case"`
}

var taskKinds = []string{"task", "chat", "email", "call", "meeting"}

const taskSelect = `SELECT t.id, t.title, t.notes, t.kind, t.priority, t.due_date, t.due_time, t.assignee_id, t.created_by,
	t.conversation_id, t.position, t.done_at, t.done_by, t.created_at, t.updated_at,
	c.subject, c.status, ct.id, ct.name, ct.email
	FROM tasks t
	LEFT JOIN conversations c ON c.id = t.conversation_id
	LEFT JOIN contacts ct ON ct.id = c.contact_id`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	t := &Task{}
	var subject, status, cname, cemail sql.NullString
	var cid sql.NullInt64
	err := row.Scan(&t.ID, &t.Title, &t.Notes, &t.Kind, &t.High, &t.DueDate, &t.DueTime, &t.AssigneeID, &t.CreatedBy,
		&t.ConversationID, &t.Position, &t.DoneAt, &t.DoneBy, &t.CreatedAt, &t.UpdatedAt,
		&subject, &status, &cid, &cname, &cemail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if t.ConversationID != nil && status.Valid {
		name := cname.String
		if name == "" {
			name = cemail.String
		}
		if name == "" {
			name = fmt.Sprintf("Visitor #%d", cid.Int64)
		}
		t.Case = &TaskCase{ID: *t.ConversationID, Subject: subject.String, Status: status.String, ContactName: name}
	}
	return t, nil
}

type TaskFilter struct {
	// Assignee: "all", "none" (nobody yet) or an agent id.
	All        bool
	Unassigned bool
	AgentID    int64
	// Done lists finished tasks, newest first. Otherwise open tasks, plus any
	// finished at or after DoneSince (so today's ticked-off items stay in view).
	Done           bool
	DoneSince      int64
	ConversationID int64
}

func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]*Task, error) {
	var where []string
	var args []any
	if f.ConversationID > 0 {
		where = append(where, "t.conversation_id = ?")
		args = append(args, f.ConversationID)
	} else if !f.All {
		if f.Unassigned {
			where = append(where, "t.assignee_id IS NULL")
		} else {
			where = append(where, "t.assignee_id = ?")
			args = append(args, f.AgentID)
		}
	}
	order, limit := "t.due_date IS NULL, t.due_date, t.position, t.id", 1000
	switch {
	case f.ConversationID > 0:
		order, limit = "t.done_at IS NOT NULL, t.due_date IS NULL, t.due_date, t.position, t.id", 100
	case f.Done:
		where = append(where, "t.done_at IS NOT NULL")
		order, limit = "t.done_at DESC", 200
	default:
		where = append(where, "(t.done_at IS NULL OR t.done_at >= ?)")
		args = append(args, f.DoneSince)
	}
	q := taskSelect
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY %s LIMIT %d", order, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE t.id = ?`, id))
}

// CreateTask adds a task at the bottom of its owner's list.
func (s *Store) CreateTask(ctx context.Context, t *Task) (*Task, error) {
	now := nowMs()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks (title, notes, kind, priority, due_date, due_time, assignee_id, created_by,
			conversation_id, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?,
			(SELECT COALESCE(MAX(position), 0) + 1 FROM tasks WHERE done_at IS NULL AND assignee_id IS ?), ?, ?)`,
		t.Title, t.Notes, t.Kind, t.High, t.DueDate, t.DueTime, t.AssigneeID, t.CreatedBy, t.ConversationID,
		t.AssigneeID, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetTask(ctx, id)
}

// SaveTask writes every editable field of t back.
func (s *Store) SaveTask(ctx context.Context, t *Task) (*Task, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET title = ?, notes = ?, kind = ?, priority = ?, due_date = ?, due_time = ?,
			assignee_id = ?, conversation_id = ?, position = ?, done_at = ?, done_by = ?, updated_at = ? WHERE id = ?`,
		t.Title, t.Notes, t.Kind, t.High, t.DueDate, t.DueTime, t.AssigneeID, t.ConversationID, t.Position,
		t.DoneAt, t.DoneBy, nowMs(), t.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errNotFound
	}
	return s.GetTask(ctx, t.ID)
}

// TopPosition is just above the first open task of an owner, for "move to top".
func (s *Store) TopPosition(ctx context.Context, assignee *int64) (float64, error) {
	var p float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(position), 1) - 1 FROM tasks WHERE done_at IS NULL AND assignee_id IS ?`, assignee).Scan(&p)
	return p, err
}

// ReorderTasks sets positions to follow the given order. Returns the ids changed.
func (s *Store) ReorderTasks(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowMs()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET position = ?, updated_at = ? WHERE id = ?`, float64(i+1), now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}
