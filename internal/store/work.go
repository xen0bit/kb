package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xen0bit/kb/internal/wikitext"
)

// Task statuses.
var taskStatuses = map[string]bool{"todo": true, "doing": true, "blocked": true, "done": true, "dropped": true}

// DefaultLease is how long task_claim_next holds a task for its claimant.
const DefaultLease = 30 * time.Minute

// Task is one item of work.
type Task struct {
	ID         int64  `json:"id"`
	ParentID   int64  `json:"parent_id,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Status     string `json:"status"`
	Priority   int    `json:"priority"`
	Assignee   string `json:"assignee,omitempty"`
	LeaseUntil int64  `json:"lease_until,omitempty"`
	Page       string `json:"page,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	CreatedBy  string `json:"created_by,omitempty"`
	UpdatedAt  int64  `json:"updated_at"`
	Children   int    `json:"children,omitempty"`
	OpenKids   int    `json:"open_children,omitempty"`
}

const taskCols = `t.id, COALESCE(t.parent_id, 0), t.title, t.body, t.status, t.priority, t.assignee, COALESCE(t.lease_until, 0),
	COALESCE((SELECT title FROM pages WHERE key = t.page_key), t.page_key, ''), t.created_at, t.created_by, t.updated_at,
	(SELECT count(*) FROM tasks c WHERE c.parent_id = t.id),
	(SELECT count(*) FROM tasks c WHERE c.parent_id = t.id AND c.status IN ('todo','doing','blocked'))`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := sc.Scan(&t.ID, &t.ParentID, &t.Title, &t.Body, &t.Status, &t.Priority, &t.Assignee, &t.LeaseUntil,
		&t.Page, &t.CreatedAt, &t.CreatedBy, &t.UpdatedAt, &t.Children, &t.OpenKids)
	return &t, err
}

// NewTask describes a task to create.
type NewTask struct {
	ParentID int64
	Title    string
	Body     string
	Priority int
	Page     string // optional page title the task is about
	Author   string
}

func (s *Store) getTask(ctx context.Context, q querier, id int64) (*Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks t WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no task %d", ErrNotFound, id)
	}
	return t, err
}

// GetTask reads one task.
func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) { return s.getTask(ctx, s.r, id) }

// CreateTasks creates tasks in one transaction: a plan of fifty articles is
// one call, not fifty.
func (s *Store) CreateTasks(ctx context.Context, ts []NewTask) ([]Task, error) {
	if len(ts) == 0 {
		return nil, fmt.Errorf("%w: no tasks given", ErrInvalid)
	}
	var out []Task
	err := s.write(ctx, func(tx *sql.Tx) error {
		for i, t := range ts {
			title := strings.TrimSpace(t.Title)
			if title == "" {
				return fmt.Errorf("%w: task %d has no title", ErrInvalid, i+1)
			}
			var page any
			if t.Page != "" {
				k := wikitext.Key(t.Page)
				if k == "" {
					return fmt.Errorf("%w: task %d: %q is not a valid page title", ErrInvalid, i+1, t.Page)
				}
				page = k
			}
			if t.ParentID != 0 {
				if _, err := s.getTask(ctx, tx, t.ParentID); err != nil {
					return fmt.Errorf("task %d: parent: %w", i+1, err)
				}
			}
			now := s.ms()
			r, err := tx.ExecContext(ctx, `INSERT INTO tasks (parent_id, title, body, priority, page_key, created_at, created_by, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, nullZero(t.ParentID), title, t.Body, t.Priority, page, now, t.Author, now)
			if err != nil {
				return err
			}
			id, _ := r.LastInsertId()
			if err := s.record(ctx, tx, "task.create", title, id, 0, 0, t.Author, ""); err != nil {
				return err
			}
			nt, err := s.getTask(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, *nt)
		}
		return nil
	})
	return out, err
}

// TaskUpdate changes a task. Nil fields are left alone.
type TaskUpdate struct {
	ID         int64
	Status     *string
	Priority   *int
	Assignee   *string
	Title      *string
	AppendBody string // appended as a new paragraph: a running log
	Body       *string
	ParentID   *int64
	Page       *string
	Author     string
}

// UpdateTask applies a TaskUpdate. Finishing or dropping a task releases its
// lease; marking it doing without a lease takes one for the author.
func (s *Store) UpdateTask(ctx context.Context, u TaskUpdate) (*Task, error) {
	var out *Task
	err := s.write(ctx, func(tx *sql.Tx) error {
		t, err := s.getTask(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		if u.Status != nil {
			st := strings.ToLower(strings.TrimSpace(*u.Status))
			if !taskStatuses[st] {
				return fmt.Errorf("%w: status must be todo, doing, blocked, done or dropped", ErrInvalid)
			}
			t.Status = st
			switch st {
			case "done", "dropped", "todo":
				t.LeaseUntil = 0
			case "doing":
				if t.LeaseUntil < s.ms() {
					t.LeaseUntil = s.now().Add(DefaultLease).UnixMilli()
				}
				if t.Assignee == "" {
					t.Assignee = u.Author
				}
			}
		}
		if u.Priority != nil {
			t.Priority = *u.Priority
		}
		if u.Assignee != nil {
			t.Assignee = *u.Assignee
		}
		if u.Title != nil {
			if strings.TrimSpace(*u.Title) == "" {
				return fmt.Errorf("%w: title cannot be empty", ErrInvalid)
			}
			t.Title = strings.TrimSpace(*u.Title)
		}
		if u.Body != nil {
			t.Body = *u.Body
		}
		if u.AppendBody != "" {
			t.Body = joinBlocks(t.Body, u.AppendBody)
		}
		if u.ParentID != nil {
			if *u.ParentID == t.ID {
				return fmt.Errorf("%w: a task cannot be its own parent", ErrInvalid)
			}
			if *u.ParentID != 0 {
				// Walk up from the new parent: reaching this task means a cycle.
				for p := *u.ParentID; p != 0; {
					pt, err := s.getTask(ctx, tx, p)
					if err != nil {
						return fmt.Errorf("parent: %w", err)
					}
					if pt.ParentID == t.ID {
						return fmt.Errorf("%w: task %d is a descendant of task %d", ErrInvalid, *u.ParentID, t.ID)
					}
					p = pt.ParentID
				}
			}
			t.ParentID = *u.ParentID
		}
		var page any
		if u.Page != nil {
			t.Page = *u.Page
		}
		if t.Page != "" {
			page = wikitext.Key(t.Page)
		}
		var lease any
		if t.LeaseUntil > 0 {
			lease = t.LeaseUntil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET parent_id = ?, title = ?, body = ?, status = ?, priority = ?, assignee = ?,
			lease_until = ?, page_key = ?, updated_at = ? WHERE id = ?`,
			nullZero(t.ParentID), t.Title, t.Body, t.Status, t.Priority, t.Assignee, lease, page, s.ms(), t.ID); err != nil {
			return err
		}
		comment := ""
		if u.Status != nil {
			comment = "status " + t.Status
		}
		if err := s.record(ctx, tx, "task.update", t.Title, t.ID, 0, 0, u.Author, comment); err != nil {
			return err
		}
		out, err = s.getTask(ctx, tx, t.ID)
		return err
	})
	return out, err
}

// TaskFilter selects tasks.
type TaskFilter struct {
	Status   []string // default: todo, doing, blocked
	Assignee string
	ParentID int64 // -1 for top-level only
	Page     string
	Query    string // substring of title or body
	Sort     string // priority (default), updated, created
	Limit    int
	Offset   int
}

// ListTasks lists tasks, highest priority first by default.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]Task, int, error) {
	f.Limit = clampLimit(f.Limit, 50, 500)
	if len(f.Status) == 0 {
		f.Status = []string{"todo", "doing", "blocked"}
	}
	var where []string
	var args []any
	if len(f.Status) == 1 && f.Status[0] == "all" {
		// no filter
	} else {
		ph := make([]string, len(f.Status))
		for i, st := range f.Status {
			if !taskStatuses[st] {
				return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalid, st)
			}
			ph[i] = "?"
			args = append(args, st)
		}
		where = append(where, "t.status IN ("+strings.Join(ph, ",")+")")
	}
	if f.Assignee != "" {
		where = append(where, "t.assignee = ?")
		args = append(args, f.Assignee)
	}
	switch {
	case f.ParentID > 0:
		where = append(where, "t.parent_id = ?")
		args = append(args, f.ParentID)
	case f.ParentID < 0:
		where = append(where, "t.parent_id IS NULL")
	}
	if f.Page != "" {
		where = append(where, "t.page_key = ?")
		args = append(args, wikitext.Key(f.Page))
	}
	if f.Query != "" {
		where = append(where, `(t.title LIKE ? ESCAPE '\' OR t.body LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(f.Query) + "%"
		args = append(args, like, like)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM tasks t`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "t.priority DESC, t.id ASC"
	switch f.Sort {
	case "updated":
		order = "t.updated_at DESC"
	case "created":
		order = "t.id DESC"
	case "", "priority":
	default:
		return nil, 0, fmt.Errorf("%w: sort must be priority, updated or created", ErrInvalid)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks t`+w+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	return out, total, rows.Err()
}

// ClaimNext atomically gives assignee the highest-priority task that is
// todo, or doing with an expired lease, optionally under a parent. It returns
// nil when there is nothing to claim. Leaf tasks are preferred: a parent with
// open children is a heading, not work.
func (s *Store) ClaimNext(ctx context.Context, assignee string, parentID int64, lease time.Duration) (*Task, error) {
	if assignee == "" {
		return nil, fmt.Errorf("%w: a claim needs an assignee", ErrInvalid)
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	var out *Task
	err := s.write(ctx, func(tx *sql.Tx) error {
		now := s.ms()
		q := `SELECT t.id FROM tasks t WHERE (t.status = 'todo' OR (t.status = 'doing' AND COALESCE(t.lease_until, 0) < ?))
			AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = t.id AND c.status IN ('todo','doing','blocked'))`
		args := []any{now}
		if parentID > 0 {
			// Anywhere under the parent, not only its direct children.
			q = `WITH RECURSIVE sub(id) AS (SELECT id FROM tasks WHERE parent_id = ? UNION ALL
				SELECT t.id FROM tasks t JOIN sub ON t.parent_id = sub.id) ` + q + ` AND t.id IN (SELECT id FROM sub)`
			args = append([]any{parentID}, args...)
		}
		q += ` ORDER BY t.priority DESC, t.id ASC LIMIT 1`
		var id int64
		err := tx.QueryRowContext(ctx, q, args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		until := s.now().Add(lease).UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'doing', assignee = ?, lease_until = ?, updated_at = ? WHERE id = ?`,
			assignee, until, now, id); err != nil {
			return err
		}
		t, err := s.getTask(ctx, tx, id)
		if err != nil {
			return err
		}
		out = t
		return s.record(ctx, tx, "task.update", t.Title, t.ID, 0, 0, assignee, "claimed")
	})
	return out, err
}

// Note is a free-form note.
type Note struct {
	ID        int64  `json:"id"`
	Topic     string `json:"topic,omitempty"`
	Body      string `json:"body"`
	Author    string `json:"author,omitempty"`
	Page      string `json:"page,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// NoteWrite creates a note, or with ID replaces or appends to one.
type NoteWrite struct {
	ID     int64
	Topic  string
	Body   string
	Append bool // with ID: append Body as a new paragraph instead of replacing
	Page   string
	Author string
}

// WriteNote saves a note.
func (s *Store) WriteNote(ctx context.Context, n NoteWrite) (*Note, error) {
	if strings.TrimSpace(n.Body) == "" {
		return nil, fmt.Errorf("%w: note body is empty", ErrInvalid)
	}
	var page any
	if n.Page != "" {
		k := wikitext.Key(n.Page)
		if k == "" {
			return nil, fmt.Errorf("%w: %q is not a valid page title", ErrInvalid, n.Page)
		}
		page = k
	}
	var out *Note
	err := s.write(ctx, func(tx *sql.Tx) error {
		now := s.ms()
		id := n.ID
		if id == 0 {
			r, err := tx.ExecContext(ctx, `INSERT INTO notes (topic, body, author, page_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
				strings.TrimSpace(n.Topic), n.Body, n.Author, page, now, now)
			if err != nil {
				return err
			}
			id, _ = r.LastInsertId()
		} else {
			cur, err := s.getNote(ctx, tx, id)
			if err != nil {
				return err
			}
			body := n.Body
			if n.Append {
				body = joinBlocks(cur.Body, n.Body)
			}
			topic := cur.Topic
			if n.Topic != "" {
				topic = strings.TrimSpace(n.Topic)
			}
			if page == nil && cur.Page != "" {
				page = wikitext.Key(cur.Page)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET topic = ?, body = ?, page_key = ?, updated_at = ? WHERE id = ?`, topic, body, page, now, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM notes_fts WHERE rowid = ?`, id); err != nil {
				return err
			}
		}
		nt, err := s.getNote(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes_fts (rowid, topic, body) VALUES (?, ?, ?)`, id, nt.Topic, nt.Body); err != nil {
			return err
		}
		out = nt
		target := nt.Topic
		if target == "" {
			target = fmt.Sprintf("note %d", id)
		}
		return s.record(ctx, tx, "note.write", target, id, 0, 0, n.Author, "")
	})
	return out, err
}

func (s *Store) getNote(ctx context.Context, q querier, id int64) (*Note, error) {
	var n Note
	err := q.QueryRowContext(ctx, `SELECT id, topic, body, author, COALESCE((SELECT title FROM pages p WHERE p.key = notes.page_key), page_key, ''), created_at, updated_at FROM notes WHERE id = ?`, id).Scan(
		&n.ID, &n.Topic, &n.Body, &n.Author, &n.Page, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no note %d", ErrNotFound, id)
	}
	return &n, err
}

// GetNote reads one note.
func (s *Store) GetNote(ctx context.Context, id int64) (*Note, error) { return s.getNote(ctx, s.r, id) }

// NoteFilter selects notes.
type NoteFilter struct {
	Topic  string // exact (case-insensitive)
	Page   string
	Query  string // full-text
	Author string
	Limit  int
	Offset int
}

// ListNotes lists notes, newest first, or by relevance with a query.
func (s *Store) ListNotes(ctx context.Context, f NoteFilter) ([]Note, int, error) {
	f.Limit = clampLimit(f.Limit, 20, 200)
	var where []string
	var args []any
	from := "notes n"
	order := "n.updated_at DESC, n.id DESC"
	if f.Query != "" {
		from = "notes_fts JOIN notes n ON n.id = notes_fts.rowid"
		where = append(where, "notes_fts MATCH ?")
		args = append(args, ftsQuery(f.Query))
		order = "bm25(notes_fts)"
	}
	if f.Topic != "" {
		where = append(where, "n.topic = ? COLLATE NOCASE")
		args = append(args, f.Topic)
	}
	if f.Page != "" {
		where = append(where, "n.page_key = ?")
		args = append(args, wikitext.Key(f.Page))
	}
	if f.Author != "" {
		where = append(where, "n.author = ?")
		args = append(args, f.Author)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM `+from+w, args...).Scan(&total); err != nil {
		return nil, 0, ftsErr(err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT n.id, n.topic, n.body, n.author, COALESCE((SELECT title FROM pages p WHERE p.key = n.page_key), n.page_key, ''), n.created_at, n.updated_at FROM `+
		from+w+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, ftsErr(err)
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Topic, &n.Body, &n.Author, &n.Page, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	return out, total, rows.Err()
}

// NoteTopics lists topics with their note counts, most recently used first.
func (s *Store) NoteTopics(ctx context.Context, limit int) ([]Count, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT topic, count(*) FROM notes WHERE topic != '' GROUP BY topic COLLATE NOCASE
		ORDER BY max(updated_at) DESC LIMIT ?`, clampLimit(limit, 50, 500))
	if err != nil {
		return nil, err
	}
	return scanCounts(rows)
}

// Count is a name with a number.
type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func scanCounts(rows *sql.Rows) ([]Count, error) {
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
