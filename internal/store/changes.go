package store

import (
	"context"
	"database/sql"
	"strings"
)

// Change is one entry of the change feed.
type Change struct {
	ID        int64  `json:"id"`
	At        int64  `json:"at"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	RefID     int64  `json:"ref_id,omitempty"`
	Rev       int64  `json:"rev,omitempty"`
	SizeDelta int    `json:"size_delta,omitempty"`
	Author    string `json:"author,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

func (s *Store) record(ctx context.Context, tx *sql.Tx, kind, target string, ref, rev int64, delta int, author, comment string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO changes (at, kind, target, ref_id, rev, size_delta, author, comment)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, s.ms(), kind, target, ref, rev, delta, author, comment)
	return err
}

// ChangeFilter selects from the change feed.
type ChangeFilter struct {
	AfterID  int64  // only changes with a larger id, oldest first: the resume cursor
	BeforeID int64  // only changes with a smaller id, newest first: paging back
	Since    int64  // unix ms
	Author   string // exact
	Kind     string // prefix: "page", "page.edit", "file", "task", "note"
	Target   string // exact title/name
	Limit    int
}

// Changes lists the feed. With AfterID it returns the oldest changes after
// that id, so an agent can replay everything since its last look; otherwise
// it returns the newest first.
func (s *Store) Changes(ctx context.Context, f ChangeFilter) ([]Change, error) {
	f.Limit = clampLimit(f.Limit, 50, 500)
	var where []string
	var args []any
	if f.AfterID > 0 {
		where = append(where, "id > ?")
		args = append(args, f.AfterID)
	}
	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	if f.Since > 0 {
		where = append(where, "at >= ?")
		args = append(args, f.Since)
	}
	if f.Author != "" {
		where = append(where, "author = ?")
		args = append(args, f.Author)
	}
	if f.Kind != "" {
		where = append(where, "(kind = ? OR kind LIKE ? ESCAPE '\\')")
		args = append(args, f.Kind, escapeLike(strings.TrimSuffix(f.Kind, "."))+".%")
	}
	if f.Target != "" {
		where = append(where, "target = ?")
		args = append(args, f.Target)
	}
	q := `SELECT id, at, kind, target, ref_id, rev, size_delta, author, comment FROM changes`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.AfterID > 0 {
		q += " ORDER BY id ASC"
	} else {
		q += " ORDER BY id DESC"
	}
	q += " LIMIT ?"
	args = append(args, f.Limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.At, &c.Kind, &c.Target, &c.RefID, &c.Rev, &c.SizeDelta, &c.Author, &c.Comment); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
