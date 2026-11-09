package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type Task struct {
	ID      int64           `json:"id"`
	Goal    string          `json:"goal"`
	Status  string          `json:"status"`
	Plan    json.RawMessage `json:"plan"`
	Created time.Time       `json:"created_at"`
	Updated time.Time       `json:"updated_at"`
}

func (s *Store) CreateTask(ctx context.Context, goal, status string, plan json.RawMessage) (int64, error) {
	if len(plan) == 0 {
		plan = json.RawMessage("{}")
	}
	now := s.Now().Unix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks(goal, status, plan, created_at, updated_at) VALUES(?,?,?,?,?)`,
		goal, status, string(plan), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateTask(ctx context.Context, id int64, status string, plan json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, plan=?, updated_at=? WHERE id=?`,
		status, string(plan), s.Now().Unix(), id)
	return err
}

func (s *Store) tasks(ctx context.Context, tail string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, goal, status, plan, created_at, updated_at FROM tasks `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var plan string
		var c, u int64
		if err := rows.Scan(&t.ID, &t.Goal, &t.Status, &plan, &c, &u); err != nil {
			return nil, err
		}
		t.Plan, t.Created, t.Updated = json.RawMessage(plan), time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Task(ctx context.Context, id int64) (Task, error) {
	ts, err := s.tasks(ctx, `WHERE id=?`, id)
	if err == nil && len(ts) == 0 {
		err = sql.ErrNoRows
	}
	if err != nil {
		return Task{}, err
	}
	return ts[0], nil
}

// RecentTasks returns the newest tasks first.
func (s *Store) RecentTasks(ctx context.Context, n int) ([]Task, error) {
	return s.tasks(ctx, `ORDER BY id DESC LIMIT ?`, n)
}

// ResumableTask returns the newest interrupted or cancelled task, if any.
func (s *Store) ResumableTask(ctx context.Context) (Task, bool, error) {
	ts, err := s.tasks(ctx, `WHERE status IN ('interrupted','cancelled','failed') ORDER BY id DESC LIMIT 1`)
	if err != nil || len(ts) == 0 {
		return Task{}, false, err
	}
	return ts[0], true, nil
}

// MarkInterrupted is called at startup: a task still "running" was cut off.
func (s *Store) MarkInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='interrupted', updated_at=? WHERE status IN ('running','planned')`, s.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ActionDone returns the recorded result of an action done under key.
func (s *Store) ActionDone(ctx context.Context, key string) (json.RawMessage, bool) {
	var r string
	if err := s.db.QueryRowContext(ctx, `SELECT result FROM actions WHERE key=?`, key).Scan(&r); err != nil {
		return nil, false
	}
	return json.RawMessage(r), true
}

// RecordAction remembers that an action under key happened.
func (s *Store) RecordAction(ctx context.Context, key string, result json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions(key, result, created_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`,
		key, string(result), s.Now().Unix())
	return err
}
