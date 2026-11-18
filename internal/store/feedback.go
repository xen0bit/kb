package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// FeedbackKinds are the kinds of feedback accepted.
var FeedbackKinds = []string{"bug", "friction", "idea", "praise"}

// Feedback is one report about kb itself.
type Feedback struct {
	ID         int64  `json:"id"`
	At         int64  `json:"at"`
	Kind       string `json:"kind"`
	Tool       string `json:"tool,omitempty"`
	Message    string `json:"message"`
	Details    string `json:"details,omitempty"`
	Author     string `json:"author,omitempty"`
	Status     string `json:"status"`
	Resolution string `json:"resolution,omitempty"`
}

// MaxFeedbackBytes bounds one report.
const MaxFeedbackBytes = 16 << 10

// AddFeedback records a report.
func (s *Store) AddFeedback(ctx context.Context, f Feedback) (*Feedback, error) {
	f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
	if !contains(FeedbackKinds, f.Kind) {
		return nil, fmt.Errorf("%w: kind must be one of %s", ErrInvalid, strings.Join(FeedbackKinds, ", "))
	}
	f.Message = strings.TrimSpace(f.Message)
	if f.Message == "" {
		return nil, fmt.Errorf("%w: message is empty", ErrInvalid)
	}
	if len(f.Message)+len(f.Details) > MaxFeedbackBytes {
		return nil, fmt.Errorf("%w: feedback is over %d bytes; summarize it", ErrInvalid, MaxFeedbackBytes)
	}
	f.Tool = strings.TrimSpace(f.Tool)
	f.At, f.Status = s.ms(), "open"
	err := s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO feedback (at, kind, tool, message, details, author) VALUES (?, ?, ?, ?, ?, ?)`,
			f.At, f.Kind, f.Tool, f.Message, f.Details, f.Author)
		if err != nil {
			return err
		}
		f.ID, _ = r.LastInsertId()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// FeedbackFilter selects reports.
type FeedbackFilter struct {
	Status string // open (default), resolved, wontfix, all
	Kind   string
	Tool   string
	Limit  int
	Offset int
}

// ListFeedback lists reports, newest first.
func (s *Store) ListFeedback(ctx context.Context, f FeedbackFilter) ([]Feedback, int, error) {
	f.Limit = clampLimit(f.Limit, 50, 500)
	var where []string
	var args []any
	switch f.Status {
	case "":
		where = append(where, "status = 'open'")
	case "all":
	default:
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Tool != "" {
		where = append(where, "tool = ?")
		args = append(args, f.Tool)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM feedback`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, at, kind, tool, message, details, author, status, resolution FROM feedback`+w+
		` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Feedback
	for rows.Next() {
		var x Feedback
		if err := rows.Scan(&x.ID, &x.At, &x.Kind, &x.Tool, &x.Message, &x.Details, &x.Author, &x.Status, &x.Resolution); err != nil {
			return nil, 0, err
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// ResolveFeedback closes a report as resolved or wontfix, with a note.
func (s *Store) ResolveFeedback(ctx context.Context, id int64, status, resolution string) error {
	if status != "resolved" && status != "wontfix" && status != "open" {
		return fmt.Errorf("%w: status must be resolved, wontfix or open", ErrInvalid)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE feedback SET status = ?, resolution = ? WHERE id = ?`, status, resolution, id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: no feedback %d", ErrNotFound, id)
		}
		return nil
	})
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }
