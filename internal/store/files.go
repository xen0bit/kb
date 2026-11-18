package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/xen0bit/kb/internal/wikitext"
)

// DefaultMaxFileBytes is the default upload limit.
const DefaultMaxFileBytes = 32 << 20

// File is a stored file's metadata.
type File struct {
	ID          int64    `json:"id"`
	Key         string   `json:"-"`
	Name        string   `json:"name"`
	MIME        string   `json:"mime"`
	SHA256      string   `json:"sha256"`
	Size        int64    `json:"size"`
	Description string   `json:"description,omitempty"`
	Rev         int64    `json:"rev"`
	CreatedAt   int64    `json:"created_at"`
	CreatedBy   string   `json:"created_by,omitempty"`
	UpdatedAt   int64    `json:"updated_at"`
	UpdatedBy   string   `json:"updated_by,omitempty"`
	Deleted     bool     `json:"deleted,omitempty"`
	UsedBy      []string `json:"used_by,omitempty"`
	UsedByN     int      `json:"used_by_count"`
}

const fileCols = `f.id, f.key, f.name, f.mime, f.sha256, f.size, f.description, f.current_rev, f.created_at, f.created_by,
	f.updated_at, f.updated_by, f.deleted_at IS NOT NULL,
	(SELECT count(*) FROM file_links fl JOIN pages p ON p.id = fl.src AND p.deleted_at IS NULL WHERE fl.file_key = f.key)`

func scanFile(sc interface{ Scan(...any) error }) (*File, error) {
	var f File
	err := sc.Scan(&f.ID, &f.Key, &f.Name, &f.MIME, &f.SHA256, &f.Size, &f.Description, &f.Rev, &f.CreatedAt, &f.CreatedBy,
		&f.UpdatedAt, &f.UpdatedBy, &f.Deleted, &f.UsedByN)
	return &f, err
}

// PutFileRequest uploads a file.
type PutFileRequest struct {
	Name        string
	MIME        string // detected from the name and content when empty
	Data        []byte
	Description string // kept as is when empty on a replace
	Comment     string
	Author      string
	BaseRev     int64 // required to replace a live file
	MaxBytes    int64 // 0 means DefaultMaxFileBytes
}

// PutFileResult reports an upload.
type PutFileResult struct {
	Name      string `json:"name"`
	Rev       int64  `json:"rev"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MIME      string `json:"mime"`
	Created   bool   `json:"created,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
	Dedup     bool   `json:"deduplicated,omitempty"` // the bytes were already stored
	Markdown  string `json:"markdown"`               // how to embed it
}

// DetectMIME picks a content type from the file name, falling back to
// sniffing the content.
func DetectMIME(name string, data []byte) string {
	if t := mime.TypeByExtension(strings.ToLower(path.Ext(name))); t != "" {
		return t
	}
	return http.DetectContentType(data)
}

// EmbedMarkdown is the markdown that embeds or links a file. The label is
// the description's first sentence when there is one, else the name without
// its extension: alt text that reads as text, not as a file name.
func EmbedMarkdown(name, mimeType, description string) string {
	dest := wikitext.FileScheme + wikitext.LinkForm(name)
	label := strings.TrimSuffix(name, path.Ext(name))
	if d := strings.TrimSpace(description); d != "" {
		if i := strings.IndexAny(d, ".\n"); i > 0 {
			d = d[:i]
		}
		if len(d) <= 100 {
			label = d
		}
	}
	label = strings.NewReplacer("[", "(", "]", ")").Replace(label)
	if strings.HasPrefix(mimeType, "image/") {
		return "![" + label + "](" + dest + ")"
	}
	return "[" + label + "](" + dest + ")"
}

// PutFile stores a file, creating it or adding a revision.
func (s *Store) PutFile(ctx context.Context, req PutFileRequest) (*PutFileResult, error) {
	name, err := wikitext.NormalizeFileName(req.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	limit := req.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxFileBytes
	}
	if int64(len(req.Data)) > limit {
		return nil, fmt.Errorf("%w: %d bytes is over the %d-byte file limit", ErrInvalid, len(req.Data), limit)
	}
	if len(req.Data) == 0 {
		return nil, fmt.Errorf("%w: file is empty", ErrInvalid)
	}
	key := strings.ToLower(name)
	mt := req.MIME
	if mt == "" {
		mt = DetectMIME(name, req.Data)
	}
	sumb := sha256.Sum256(req.Data)
	sum := hex.EncodeToString(sumb[:])
	res := &PutFileResult{Name: name, SHA256: sum, Size: int64(len(req.Data)), MIME: mt, Markdown: EmbedMarkdown(name, mt, req.Description)}
	err = s.write(ctx, func(tx *sql.Tx) error {
		cur, err := scanFile(tx.QueryRowContext(ctx, `SELECT `+fileCols+` FROM files f WHERE f.key = ?`, key))
		if errors.Is(err, sql.ErrNoRows) {
			cur = nil
		} else if err != nil {
			return err
		}
		live := cur != nil && !cur.Deleted
		if live {
			if req.BaseRev == 0 {
				return fmt.Errorf("%w: file %q exists at rev %d; pass base_rev to replace it", ErrExists, cur.Name, cur.Rev)
			}
			if req.BaseRev != cur.Rev {
				return fmt.Errorf("%w: file %q is at rev %d, not base_rev %d", ErrConflict, cur.Name, cur.Rev, req.BaseRev)
			}
			if cur.SHA256 == sum && cur.MIME == mt && (req.Description == "" || req.Description == cur.Description) {
				res.Rev, res.Unchanged = cur.Rev, true
				return nil
			}
		}
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO blobs (sha256, size, data) VALUES (?, ?, ?)`, sum, len(req.Data), req.Data)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			res.Dedup = true
		}
		now := s.ms()
		var id int64
		if cur == nil {
			r, err := tx.ExecContext(ctx, `INSERT INTO files (key, name, mime, sha256, size, description, created_at, created_by, updated_at, updated_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, key, name, mt, sum, len(req.Data), req.Description, now, req.Author, now, req.Author)
			if err != nil {
				return err
			}
			id, _ = r.LastInsertId()
		} else {
			id = cur.ID
		}
		res.Created = !live
		r, err = tx.ExecContext(ctx, `INSERT INTO file_revisions (file_id, sha256, size, mime, comment, author, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, sum, len(req.Data), mt, req.Comment, req.Author, now)
		if err != nil {
			return err
		}
		res.Rev, _ = r.LastInsertId()
		desc := req.Description
		if desc == "" && cur != nil {
			desc = cur.Description
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET name = ?, mime = ?, sha256 = ?, size = ?, description = ?, current_rev = ?,
			updated_at = ?, updated_by = ?, deleted_at = NULL WHERE id = ?`, name, mt, sum, len(req.Data), desc, res.Rev, now, req.Author, id); err != nil {
			return err
		}
		return s.record(ctx, tx, "file.upload", name, id, res.Rev, len(req.Data), req.Author, req.Comment)
	})
	return res, err
}

// GetFile reads a file's metadata, and its bytes when data is true. usedBy
// lists up to that many pages embedding it.
func (s *Store) GetFile(ctx context.Context, name string, data bool, usedBy int) (*File, []byte, error) {
	key := wikitext.FileKey(name)
	if key == "" {
		return nil, nil, fmt.Errorf("%w: %q is not a valid file name", ErrInvalid, name)
	}
	f, err := scanFile(s.r.QueryRowContext(ctx, `SELECT `+fileCols+` FROM files f WHERE f.key = ? AND f.deleted_at IS NULL`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: no file named %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, nil, err
	}
	if usedBy > 0 {
		rows, err := s.r.QueryContext(ctx, `SELECT p.title FROM file_links fl JOIN pages p ON p.id = fl.src AND p.deleted_at IS NULL
			WHERE fl.file_key = ? ORDER BY p.key LIMIT ?`, key, usedBy)
		if err != nil {
			return nil, nil, err
		}
		if f.UsedBy, err = scanStrings(rows); err != nil {
			return nil, nil, err
		}
	}
	var b []byte
	if data {
		if err := s.r.QueryRowContext(ctx, `SELECT data FROM blobs WHERE sha256 = ?`, f.SHA256).Scan(&b); err != nil {
			return nil, nil, fmt.Errorf("file %q: blob %s: %w", f.Name, f.SHA256, err)
		}
	}
	return f, b, nil
}

// FileFilter selects files.
type FileFilter struct {
	Prefix string
	MIME   string // prefix, e.g. "image/"
	Sort   string // name (default), updated, size
	Desc   bool
	Limit  int
	Offset int
}

// ListFiles lists live files.
func (s *Store) ListFiles(ctx context.Context, f FileFilter) ([]File, int, error) {
	f.Limit = clampLimit(f.Limit, 50, 500)
	where := []string{"f.deleted_at IS NULL"}
	var args []any
	if f.Prefix != "" {
		where = append(where, `f.key LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(strings.ToLower(f.Prefix))+"%")
	}
	if f.MIME != "" {
		where = append(where, `f.mime LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(f.MIME)+"%")
	}
	w := strings.Join(where, " AND ")
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM files f WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "f.key"
	switch f.Sort {
	case "updated":
		order = "f.updated_at"
	case "size":
		order = "f.size"
	case "", "name":
	default:
		return nil, 0, fmt.Errorf("%w: sort must be name, updated or size", ErrInvalid)
	}
	if f.Desc {
		order += " DESC"
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+fileCols+` FROM files f WHERE `+w+` ORDER BY `+order+`, f.id LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		fl, err := scanFile(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *fl)
	}
	return out, total, rows.Err()
}

// DeleteFile soft-deletes a file. Its bytes stay until `kb db vacuum` finds
// no revision referring to them.
func (s *Store) DeleteFile(ctx context.Context, name, comment, author string) error {
	key := wikitext.FileKey(name)
	if key == "" {
		return fmt.Errorf("%w: %q is not a valid file name", ErrInvalid, name)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var id, size int64
		var n string
		err := tx.QueryRowContext(ctx, `SELECT id, name, size FROM files WHERE key = ? AND deleted_at IS NULL`, key).Scan(&id, &n, &size)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no file named %q", ErrNotFound, name)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET deleted_at = ?, updated_at = ?, updated_by = ? WHERE id = ?`, s.ms(), s.ms(), author, id); err != nil {
			return err
		}
		return s.record(ctx, tx, "file.delete", n, id, 0, -int(size), author, comment)
	})
}
