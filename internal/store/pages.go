package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xen0bit/kb/internal/wikitext"
)

// MaxPageBytes bounds one revision's body. A page larger than this should be
// split: no agent reads a 4 MiB article in one call either.
const MaxPageBytes = 2 << 20

// Page is a page's current state. Body is set only when asked for.
type Page struct {
	ID         int64          `json:"id"`
	Key        string         `json:"-"`
	Title      string         `json:"title"`
	Rev        int64          `json:"rev"`
	RedirectTo string         `json:"redirect_to,omitempty"`
	Status     string         `json:"status,omitempty"`
	Props      map[string]any `json:"props,omitempty"`
	Lead       string         `json:"lead,omitempty"`
	Size       int            `json:"size"`
	Words      int            `json:"words"`
	CreatedAt  int64          `json:"created_at"`
	CreatedBy  string         `json:"created_by,omitempty"`
	UpdatedAt  int64          `json:"updated_at"`
	UpdatedBy  string         `json:"updated_by,omitempty"`
	Deleted    bool           `json:"deleted,omitempty"`
	Categories []string       `json:"categories,omitempty"`
	Aliases    []string       `json:"aliases,omitempty"`
	Body       string         `json:"-"`
}

const pageCols = `p.id, p.key, p.title, p.current_rev, COALESCE(p.redirect_to, ''), p.status, p.props, p.lead,
	p.size, p.words, p.created_at, p.created_by, p.updated_at, p.updated_by, p.deleted_at IS NOT NULL`

func scanPage(sc interface{ Scan(...any) error }) (*Page, error) {
	var p Page
	var props string
	if err := sc.Scan(&p.ID, &p.Key, &p.Title, &p.Rev, &p.RedirectTo, &p.Status, &props, &p.Lead,
		&p.Size, &p.Words, &p.CreatedAt, &p.CreatedBy, &p.UpdatedAt, &p.UpdatedBy, &p.Deleted); err != nil {
		return nil, err
	}
	if props != "" && props != "{}" {
		_ = json.Unmarshal([]byte(props), &p.Props)
	}
	return &p, nil
}

// normTitle normalizes a title, turning a bad one into ErrInvalid.
func normTitle(title string) (string, string, error) {
	n, err := wikitext.NormalizeTitle(title)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return n, strings.ToLower(n), nil
}

func (s *Store) pageByKey(ctx context.Context, q querier, key string) (*Page, error) {
	p, err := scanPage(q.QueryRowContext(ctx, `SELECT `+pageCols+` FROM pages p WHERE p.key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) fillPage(ctx context.Context, q querier, p *Page, body bool) error {
	rows, err := q.QueryContext(ctx, `SELECT category FROM categories WHERE page_id = ? ORDER BY category_key`, p.ID)
	if err != nil {
		return err
	}
	p.Categories, err = scanStrings(rows)
	if err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT alias FROM aliases WHERE page_id = ? ORDER BY alias_key`, p.ID)
	if err != nil {
		return err
	}
	p.Aliases, err = scanStrings(rows)
	if err != nil {
		return err
	}
	if body {
		if err := q.QueryRowContext(ctx, `SELECT body FROM revisions WHERE id = ?`, p.Rev).Scan(&p.Body); err != nil {
			return err
		}
	}
	return nil
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReadResult is a page as read, with how the requested title led to it.
type ReadResult struct {
	*Page
	RequestedTitle string `json:"requested_title,omitempty"`
	RedirectedFrom string `json:"redirected_from,omitempty"`
	ViaAlias       string `json:"via_alias,omitempty"`
}

// GetOptions controls GetPage.
type GetOptions struct {
	Body           bool // load the current revision's text
	FollowRedirect bool // a redirect page resolves to its target (one hop)
	IncludeDeleted bool
}

// GetPage reads a page by title. A title that is no page's key but is an
// alias of exactly one page resolves to that page.
func (s *Store) GetPage(ctx context.Context, title string, opt GetOptions) (*ReadResult, error) {
	_, key, err := normTitle(title)
	if err != nil {
		return nil, err
	}
	res := &ReadResult{}
	p, err := s.pageByKey(ctx, s.r, key)
	if err != nil {
		return nil, err
	}
	if p == nil || (p.Deleted && !opt.IncludeDeleted) {
		var id int64
		err := s.r.QueryRowContext(ctx, `SELECT a.page_id FROM aliases a JOIN pages p ON p.id = a.page_id
			WHERE a.alias_key = ? AND p.deleted_at IS NULL ORDER BY p.updated_at DESC LIMIT 1`, key).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: no page titled %q", ErrNotFound, title)
		}
		if err != nil {
			return nil, err
		}
		p, err = scanPage(s.r.QueryRowContext(ctx, `SELECT `+pageCols+` FROM pages p WHERE p.id = ?`, id))
		if err != nil {
			return nil, err
		}
		res.ViaAlias = title
	}
	if opt.FollowRedirect && p.RedirectTo != "" {
		t, err := s.pageByKey(ctx, s.r, p.RedirectTo)
		if err != nil {
			return nil, err
		}
		if t != nil && !t.Deleted {
			res.RedirectedFrom = p.Title
			p = t
		}
	}
	if err := s.fillPage(ctx, s.r, p, opt.Body); err != nil {
		return nil, err
	}
	res.Page = p
	return res, nil
}

// LinkCounts reports a page's outbound kb: links and the live pages linking
// to it (by title or any alias).
func (s *Store) LinkCounts(ctx context.Context, pageID int64) (out, in int, err error) {
	err = s.r.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM links WHERE src = ?1),
		(SELECT count(DISTINCT l.src) FROM links l JOIN pages s ON s.id = l.src AND s.deleted_at IS NULL
		  WHERE l.target_key IN (SELECT key FROM pages WHERE id = ?1 UNION SELECT alias_key FROM aliases WHERE page_id = ?1)
		  AND l.src != ?1)`, pageID).Scan(&out, &in)
	return
}

// WriteMode says what PutPage may do.
type WriteMode int

const (
	// ModeCreate fails if a live page has the title.
	ModeCreate WriteMode = iota
	// ModeReplace fails unless a live page has the title and BaseRev is its
	// current revision.
	ModeReplace
	// ModeUpsert creates or replaces; BaseRev is checked when non-zero.
	ModeUpsert
)

// PutRequest writes a whole page.
type PutRequest struct {
	Title   string
	Body    string
	Comment string
	Author  string
	BaseRev int64
	Mode    WriteMode
}

// PutResult describes a completed write.
type PutResult struct {
	Title     string   `json:"title"`
	Rev       int64    `json:"rev"`
	Created   bool     `json:"created,omitempty"`
	Unchanged bool     `json:"unchanged,omitempty"`
	Size      int      `json:"size"`
	SizeDelta int      `json:"size_delta"`
	Redirect  string   `json:"redirect_to,omitempty"`
	Links     int      `json:"links"`
	RedLinks  []string `json:"red_links,omitempty"`     // linked titles with no page yet
	NoFiles   []string `json:"missing_files,omitempty"` // embedded files not uploaded yet
	Warnings  []string `json:"warnings,omitempty"`
}

// PutPage creates or replaces a page's text as a new revision.
func (s *Store) PutPage(ctx context.Context, req PutRequest) (*PutResult, error) {
	title, key, err := normTitle(req.Title)
	if err != nil {
		return nil, err
	}
	var res *PutResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		cur, err := s.pageByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		live := cur != nil && !cur.Deleted
		switch req.Mode {
		case ModeCreate:
			if live {
				return fmt.Errorf("%w: page %q exists at rev %d (last edited by %s); read it and use write_page or edit_page with base_rev",
					ErrExists, cur.Title, cur.Rev, orUnknown(cur.UpdatedBy))
			}
		case ModeReplace:
			if !live {
				return fmt.Errorf("%w: no page titled %q; use create_page", ErrNotFound, title)
			}
			if req.BaseRev == 0 {
				return fmt.Errorf("%w: base_rev is required to replace %q (its current rev is %d)", ErrInvalid, cur.Title, cur.Rev)
			}
		}
		if live && req.BaseRev != 0 && req.BaseRev != cur.Rev {
			return conflict(cur, req.BaseRev)
		}
		kind := "page.edit"
		if !live {
			kind = "page.create"
		}
		res, err = s.savePage(ctx, tx, cur, title, req.Body, req.Comment, req.Author, kind)
		return err
	})
	return res, err
}

func conflict(cur *Page, base int64) error {
	return fmt.Errorf("%w: %q is at rev %d (edited by %s), not base_rev %d; re-read the page and apply your change to the current text",
		ErrConflict, cur.Title, cur.Rev, orUnknown(cur.UpdatedBy), base)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// savePage writes body as the newest revision of the page cur (nil for a new
// page), re-indexes it and records the change. A deleted page is revived.
func (s *Store) savePage(ctx context.Context, tx *sql.Tx, cur *Page, title, body, comment, author, kind string) (*PutResult, error) {
	if len(body) > MaxPageBytes {
		return nil, fmt.Errorf("%w: body is %d bytes, over the %d-byte page limit; split the article into linked pages", ErrInvalid, len(body), MaxPageBytes)
	}
	if !strings.HasSuffix(body, "\n") && body != "" {
		body += "\n"
	}
	doc, err := wikitext.Analyze(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	key := strings.ToLower(title)
	redirect := ""
	if doc.Meta.Redirect != "" {
		rt, rk, err := normTitle(doc.Meta.Redirect)
		if err != nil {
			return nil, fmt.Errorf("%w: redirect target: %v", ErrInvalid, err)
		}
		if rk == key {
			return nil, fmt.Errorf("%w: page %q redirects to itself", ErrInvalid, title)
		}
		redirect = rk
		doc.Links = append(doc.Links, wikitext.Link{Target: rt, Key: rk})
	}
	now := s.ms()
	if cur != nil && !cur.Deleted {
		var curBody string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM revisions WHERE id = ?`, cur.Rev).Scan(&curBody); err != nil {
			return nil, err
		}
		if curBody == body && cur.Title == title {
			res := &PutResult{Title: cur.Title, Rev: cur.Rev, Unchanged: true, Size: cur.Size, Redirect: cur.RedirectTo}
			if err := s.linkReport(ctx, tx, cur.ID, res); err != nil {
				return nil, err
			}
			return res, nil
		}
	}
	sum := sha256.Sum256([]byte(body))
	props := "{}"
	if len(doc.Meta.Props) > 0 {
		b, err := json.Marshal(doc.Meta.Props)
		if err != nil {
			return nil, fmt.Errorf("%w: frontmatter: %v", ErrInvalid, err)
		}
		props = string(b)
	}
	var redirectArg any
	if redirect != "" {
		redirectArg = redirect
	}
	res := &PutResult{Title: title, Size: len(body), Redirect: redirect}
	var id, parent int64
	if cur == nil {
		r, err := tx.ExecContext(ctx, `INSERT INTO pages (key, title, created_at, created_by, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?)`, key, title, now, author, now, author)
		if err != nil {
			return nil, err
		}
		id, _ = r.LastInsertId()
		res.Created = true
		res.SizeDelta = len(body)
	} else {
		id, parent = cur.ID, cur.Rev
		res.Created = cur.Deleted
		res.SizeDelta = len(body) - cur.Size
		if cur.Deleted {
			res.SizeDelta = len(body)
		}
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO revisions (page_id, parent_id, title, body, comment, author, sha256, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, nullZero(parent), title, body, comment, author, hex.EncodeToString(sum[:]), len(body), now)
	if err != nil {
		return nil, err
	}
	rev, _ := r.LastInsertId()
	res.Rev = rev
	if _, err := tx.ExecContext(ctx, `UPDATE pages SET key = ?, title = ?, current_rev = ?, redirect_to = ?, status = ?, props = ?,
		lead = ?, size = ?, words = ?, updated_at = ?, updated_by = ?, deleted_at = NULL WHERE id = ?`,
		key, title, rev, redirectArg, doc.Meta.Status, props, doc.Lead, len(body), doc.Words, now, author, id); err != nil {
		return nil, err
	}
	if err := s.index(ctx, tx, id, title, body, doc); err != nil {
		return nil, err
	}
	if res.Created {
		kind = "page.create"
	}
	if err := s.record(ctx, tx, kind, title, id, rev, res.SizeDelta, author, comment); err != nil {
		return nil, err
	}
	if err := s.linkReport(ctx, tx, id, res); err != nil {
		return nil, err
	}
	for _, m := range doc.Malformed {
		res.Warnings = append(res.Warnings, fmt.Sprintf("(%s) is not a link: spaces in a destination must be underscores (kb:Foo_Bar) or the destination wrapped in <> (<kb:Foo Bar>)", m))
	}
	return res, nil
}

func nullZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// index replaces everything derived from a page's text.
func (s *Store) index(ctx context.Context, tx *sql.Tx, id int64, title, body string, doc wikitext.Doc) error {
	if err := s.unindex(ctx, tx, id); err != nil {
		return err
	}
	for _, l := range doc.Links {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO links (src, target_key, target_title) VALUES (?, ?, ?)`, id, l.Key, l.Target); err != nil {
			return err
		}
	}
	for _, c := range doc.Meta.Categories {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO categories (page_id, category_key, category) VALUES (?, ?, ?)`, id, strings.ToLower(c), c); err != nil {
			return err
		}
	}
	for _, a := range doc.Meta.Aliases {
		if strings.EqualFold(a, title) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO aliases (alias_key, page_id, alias) VALUES (?, ?, ?)`, strings.ToLower(a), id, a); err != nil {
			return err
		}
	}
	for _, f := range doc.Files {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO file_links (src, file_key, name) VALUES (?, ?, ?)`, id, strings.ToLower(f), f); err != nil {
			return err
		}
	}
	aliases := strings.Join(doc.Meta.Aliases, "\n")
	if _, err := tx.ExecContext(ctx, `INSERT INTO pages_fts (rowid, title, aliases, body) VALUES (?, ?, ?, ?)`, id, title, aliases, ftsBody(body, doc)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO titles_fts (rowid, title) VALUES (?, ?)`, id, strings.TrimSpace(title+"\n"+aliases))
	return err
}

// ftsBody is what full-text search indexes for a page: the markdown without
// its frontmatter, so snippets show prose rather than YAML, followed by the
// frontmatter's categories, summary and values, so they still match.
func ftsBody(body string, doc wikitext.Doc) string {
	_, md, _, _ := wikitext.SplitFrontmatter(body)
	var extra []string
	extra = append(extra, doc.Meta.Categories...)
	if doc.Meta.Summary != "" {
		extra = append(extra, doc.Meta.Summary)
	}
	for _, k := range wikitext.SortedPropKeys(doc.Meta.Props) {
		extra = append(extra, fmt.Sprintf("%s: %v", k, doc.Meta.Props[k]))
	}
	if len(extra) == 0 {
		return md
	}
	return md + "\n\n" + strings.Join(extra, "\n")
}

func (s *Store) unindex(ctx context.Context, tx *sql.Tx, id int64) error {
	for _, q := range []string{
		`DELETE FROM links WHERE src = ?`,
		`DELETE FROM categories WHERE page_id = ?`,
		`DELETE FROM aliases WHERE page_id = ?`,
		`DELETE FROM file_links WHERE src = ?`,
		`DELETE FROM pages_fts WHERE rowid = ?`,
		`DELETE FROM titles_fts WHERE rowid = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// linkReport fills in which of a page's links and files point at nothing.
func (s *Store) linkReport(ctx context.Context, q querier, id int64, res *PutResult) error {
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM links WHERE src = ?`, id).Scan(&res.Links); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT l.target_title FROM links l WHERE l.src = ?
		AND NOT EXISTS (SELECT 1 FROM pages p WHERE p.key = l.target_key AND p.deleted_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM aliases a JOIN pages p ON p.id = a.page_id WHERE a.alias_key = l.target_key AND p.deleted_at IS NULL)
		ORDER BY l.target_key LIMIT 200`, id)
	if err != nil {
		return err
	}
	if res.RedLinks, err = scanStrings(rows); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT fl.name FROM file_links fl WHERE fl.src = ?
		AND NOT EXISTS (SELECT 1 FROM files f WHERE f.key = fl.file_key AND f.deleted_at IS NULL) ORDER BY fl.file_key LIMIT 200`, id)
	if err != nil {
		return err
	}
	res.NoFiles, err = scanStrings(rows)
	return err
}

// EditRequest changes part of a page. Exactly one of Edits, Section,
// Append or Prepend is used. BaseRev is optional: str_replace edits validate
// themselves against the current text, so an edit made from a stale read
// either still applies exactly or fails.
type EditRequest struct {
	Title       string
	BaseRev     int64
	Edits       []wikitext.Edit
	Section     string // selector, with SectionText replacing it
	SectionText string
	Append      string
	Prepend     string
	Comment     string
	Author      string
}

// EditPage applies an EditRequest as one new revision.
func (s *Store) EditPage(ctx context.Context, req EditRequest) (*PutResult, error) {
	title, key, err := normTitle(req.Title)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, used := range []bool{len(req.Edits) > 0, req.Section != "", req.Append != "", req.Prepend != ""} {
		if used {
			n++
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("%w: give exactly one of edits, section (with section_text), append or prepend", ErrInvalid)
	}
	var res *PutResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		cur, err := s.pageByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return fmt.Errorf("%w: no page titled %q; use create_page", ErrNotFound, title)
		}
		if req.BaseRev != 0 && req.BaseRev != cur.Rev {
			return conflict(cur, req.BaseRev)
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM revisions WHERE id = ?`, cur.Rev).Scan(&body); err != nil {
			return err
		}
		switch {
		case len(req.Edits) > 0:
			body, err = wikitext.ApplyEdits(body, req.Edits)
		case req.Section != "":
			body, _, err = wikitext.ReplaceSection(body, req.Section, req.SectionText)
		case req.Append != "":
			body = joinBlocks(body, req.Append)
		default:
			_, rest, line, ok := wikitext.SplitFrontmatter(body)
			if ok {
				front := wikitext.SliceLines(body, 1, line)
				body = front + joinBlocks(req.Prepend, rest)
			} else {
				body = joinBlocks(req.Prepend, body)
			}
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		res, err = s.savePage(ctx, tx, cur, cur.Title, body, req.Comment, req.Author, "page.edit")
		return err
	})
	return res, err
}

// joinBlocks joins two markdown fragments with a blank line between them.
func joinBlocks(a, b string) string {
	a = strings.TrimRight(a, "\n")
	b = strings.TrimLeft(b, "\n")
	switch {
	case a == "":
		return b
	case b == "":
		return a + "\n"
	}
	return a + "\n\n" + b
}

// MoveRequest renames a page.
type MoveRequest struct {
	From, To       string
	LeaveRedirect  bool
	RewriteLinks   bool // point every live page's kb: links at the new title
	Comment        string
	Author         string
	BaseRev        int64
	MaxLinkRewrite int // bound on pages rewritten; default 500
}

// MoveResult reports a move.
type MoveResult struct {
	From           string   `json:"from"`
	To             string   `json:"to"`
	Rev            int64    `json:"rev"`
	Redirect       bool     `json:"redirect_left"`
	PagesRewritten []string `json:"pages_rewritten,omitempty"`
	LinksRewritten int      `json:"links_rewritten"`
	NotRewritten   int      `json:"pages_not_rewritten,omitempty"`
}

// MovePage renames a page, keeping its history, and optionally leaves a
// redirect behind and rewrites the links that pointed at the old title.
func (s *Store) MovePage(ctx context.Context, req MoveRequest) (*MoveResult, error) {
	_, fromKey, err := normTitle(req.From)
	if err != nil {
		return nil, err
	}
	to, toKey, err := normTitle(req.To)
	if err != nil {
		return nil, err
	}
	maxRewrite := clampLimit(req.MaxLinkRewrite, 500, 5000)
	res := &MoveResult{To: to}
	err = s.write(ctx, func(tx *sql.Tx) error {
		cur, err := s.pageByKey(ctx, tx, fromKey)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return fmt.Errorf("%w: no page titled %q", ErrNotFound, req.From)
		}
		if req.BaseRev != 0 && req.BaseRev != cur.Rev {
			return conflict(cur, req.BaseRev)
		}
		res.From = cur.Title
		if fromKey == toKey && cur.Title == to {
			return fmt.Errorf("%w: %q is already the page's title", ErrInvalid, to)
		}
		if fromKey != toKey {
			dst, err := s.pageByKey(ctx, tx, toKey)
			if err != nil {
				return err
			}
			if dst != nil {
				if !dst.Deleted && dst.RedirectTo != fromKey {
					return fmt.Errorf("%w: page %q already exists; move or delete it first", ErrExists, dst.Title)
				}
				// A deleted page, or a redirect back to the page being moved,
				// is in the way only by name: retire it so the key is free.
				if err := s.retireKey(ctx, tx, dst); err != nil {
					return err
				}
			}
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM revisions WHERE id = ?`, cur.Rev).Scan(&body); err != nil {
			return err
		}
		comment := fmt.Sprintf("moved from %q", cur.Title)
		if req.Comment != "" {
			comment += ": " + req.Comment
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pages SET key = ?, title = ? WHERE id = ?`, toKey, to, cur.ID); err != nil {
			return err
		}
		// The move is a revision of its own (same text, new title), so the
		// history says when and by whom the page was renamed.
		r, err := tx.ExecContext(ctx, `INSERT INTO revisions (page_id, parent_id, title, body, comment, author, sha256, size, created_at)
			SELECT page_id, id, ?, body, ?, ?, sha256, size, ? FROM revisions WHERE id = ?`, to, comment, req.Author, s.ms(), cur.Rev)
		if err != nil {
			return err
		}
		rev, _ := r.LastInsertId()
		res.Rev = rev
		if _, err := tx.ExecContext(ctx, `UPDATE pages SET current_rev = ?, updated_at = ?, updated_by = ? WHERE id = ?`, rev, s.ms(), req.Author, cur.ID); err != nil {
			return err
		}
		doc, err := wikitext.Analyze(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if err := s.index(ctx, tx, cur.ID, to, body, doc); err != nil {
			return err
		}
		if err := s.record(ctx, tx, "page.move", to, cur.ID, rev, 0, req.Author, comment); err != nil {
			return err
		}
		if fromKey != toKey && req.LeaveRedirect {
			rbody := "---\nredirect: " + to + "\n---\n"
			if _, err := s.savePage(ctx, tx, nil, cur.Title, rbody, "redirect after move to "+fmt.Sprintf("%q", to), req.Author, "page.create"); err != nil {
				return err
			}
			res.Redirect = true
		}
		if req.RewriteLinks && fromKey != toKey {
			rows, err := tx.QueryContext(ctx, `SELECT p.id FROM links l JOIN pages p ON p.id = l.src
				WHERE l.target_key = ? AND p.deleted_at IS NULL AND p.redirect_to IS NULL ORDER BY p.id`, fromKey)
			if err != nil {
				return err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			rows.Close()
			for i, id := range ids {
				if i >= maxRewrite {
					res.NotRewritten = len(ids) - i
					break
				}
				p, err := scanPage(tx.QueryRowContext(ctx, `SELECT `+pageCols+` FROM pages p WHERE p.id = ?`, id))
				if err != nil {
					return err
				}
				var pb string
				if err := tx.QueryRowContext(ctx, `SELECT body FROM revisions WHERE id = ?`, p.Rev).Scan(&pb); err != nil {
					return err
				}
				nb, n := wikitext.RewriteLinks(pb, fromKey, to)
				if n == 0 {
					continue
				}
				if _, err := s.savePage(ctx, tx, p, p.Title, nb, fmt.Sprintf("links: %q → %q", cur.Title, to), req.Author, "page.edit"); err != nil {
					return err
				}
				res.PagesRewritten = append(res.PagesRewritten, p.Title)
				res.LinksRewritten += n
			}
		}
		return nil
	})
	return res, err
}

// retireKey frees a page's key by renaming the row to a tombstone key that no
// title can normalize to, keeping its history reachable by id.
func (s *Store) retireKey(ctx context.Context, tx *sql.Tx, p *Page) error {
	tomb := fmt.Sprintf("\x00retired:%d:%s", p.ID, p.Key)
	if _, err := tx.ExecContext(ctx, `UPDATE pages SET key = ?, deleted_at = COALESCE(deleted_at, ?) WHERE id = ?`, tomb, s.ms(), p.ID); err != nil {
		return err
	}
	return s.unindex(ctx, tx, p.ID)
}

// DeletePage soft-deletes a page: it leaves search, link counts and listings
// but keeps its history, and RestorePage brings it back.
func (s *Store) DeletePage(ctx context.Context, title, comment, author string, baseRev int64) error {
	_, key, err := normTitle(title)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		cur, err := s.pageByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return fmt.Errorf("%w: no page titled %q", ErrNotFound, title)
		}
		if baseRev != 0 && baseRev != cur.Rev {
			return conflict(cur, baseRev)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pages SET deleted_at = ?, updated_at = ?, updated_by = ? WHERE id = ?`, s.ms(), s.ms(), author, cur.ID); err != nil {
			return err
		}
		if err := s.unindex(ctx, tx, cur.ID); err != nil {
			return err
		}
		return s.record(ctx, tx, "page.delete", cur.Title, cur.ID, cur.Rev, -cur.Size, author, comment)
	})
}

// RestorePage undoes DeletePage.
func (s *Store) RestorePage(ctx context.Context, title, comment, author string) (*PutResult, error) {
	_, key, err := normTitle(title)
	if err != nil {
		return nil, err
	}
	var res *PutResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		cur, err := s.pageByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if cur == nil || !cur.Deleted {
			return fmt.Errorf("%w: no deleted page titled %q", ErrNotFound, title)
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM revisions WHERE id = ?`, cur.Rev).Scan(&body); err != nil {
			return err
		}
		doc, err := wikitext.Analyze(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pages SET deleted_at = NULL, updated_at = ?, updated_by = ? WHERE id = ?`, s.ms(), author, cur.ID); err != nil {
			return err
		}
		if err := s.index(ctx, tx, cur.ID, cur.Title, body, doc); err != nil {
			return err
		}
		if err := s.record(ctx, tx, "page.restore", cur.Title, cur.ID, cur.Rev, cur.Size, author, comment); err != nil {
			return err
		}
		res = &PutResult{Title: cur.Title, Rev: cur.Rev, Size: cur.Size, Redirect: cur.RedirectTo}
		return s.linkReport(ctx, tx, cur.ID, res)
	})
	return res, err
}

// RevertPage makes the text of an earlier revision the page's newest one.
func (s *Store) RevertPage(ctx context.Context, title string, toRev, baseRev int64, comment, author string) (*PutResult, error) {
	_, key, err := normTitle(title)
	if err != nil {
		return nil, err
	}
	var res *PutResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		cur, err := s.pageByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return fmt.Errorf("%w: no page titled %q", ErrNotFound, title)
		}
		if baseRev != 0 && baseRev != cur.Rev {
			return conflict(cur, baseRev)
		}
		var body string
		var pid int64
		err = tx.QueryRowContext(ctx, `SELECT page_id, body FROM revisions WHERE id = ?`, toRev).Scan(&pid, &body)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && pid != cur.ID) {
			return fmt.Errorf("%w: rev %d is not a revision of %q", ErrNotFound, toRev, cur.Title)
		}
		if err != nil {
			return err
		}
		c := fmt.Sprintf("revert to rev %d", toRev)
		if comment != "" {
			c += ": " + comment
		}
		res, err = s.savePage(ctx, tx, cur, cur.Title, body, c, author, "page.revert")
		return err
	})
	return res, err
}

// Revision is one stored revision.
type Revision struct {
	ID        int64  `json:"rev"`
	PageID    int64  `json:"page_id"`
	ParentID  int64  `json:"parent_rev,omitempty"`
	Title     string `json:"title"`
	Comment   string `json:"comment,omitempty"`
	Author    string `json:"author,omitempty"`
	Size      int    `json:"size"`
	SizeDelta int    `json:"size_delta"`
	SHA256    string `json:"sha256"`
	CreatedAt int64  `json:"created_at"`
	Current   bool   `json:"current,omitempty"`
	Body      string `json:"-"`
}

// GetRevision reads one revision, text included.
func (s *Store) GetRevision(ctx context.Context, id int64) (*Revision, error) {
	var r Revision
	var parent sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT r.id, r.page_id, r.parent_id, r.title, r.comment, r.author, r.size, r.sha256, r.created_at, r.body,
		r.size - COALESCE((SELECT size FROM revisions WHERE id = r.parent_id), 0), p.current_rev = r.id
		FROM revisions r JOIN pages p ON p.id = r.page_id WHERE r.id = ?`, id).Scan(
		&r.ID, &r.PageID, &parent, &r.Title, &r.Comment, &r.Author, &r.Size, &r.SHA256, &r.CreatedAt, &r.Body, &r.SizeDelta, &r.Current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no revision %d", ErrNotFound, id)
	}
	r.ParentID = parent.Int64
	return &r, err
}

// History lists a page's revisions newest first. before, when non-zero,
// pages backwards from that revision id.
func (s *Store) History(ctx context.Context, title string, before int64, limit int) ([]Revision, error) {
	rp, err := s.GetPage(ctx, title, GetOptions{IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 50, 500)
	q := `SELECT r.id, r.parent_id, r.title, r.comment, r.author, r.size, r.sha256, r.created_at,
		r.size - COALESCE((SELECT size FROM revisions WHERE id = r.parent_id), 0)
		FROM revisions r WHERE r.page_id = ?`
	args := []any{rp.ID}
	if before > 0 {
		q += ` AND r.id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r := Revision{PageID: rp.ID}
		var parent sql.NullInt64
		if err := rows.Scan(&r.ID, &parent, &r.Title, &r.Comment, &r.Author, &r.Size, &r.SHA256, &r.CreatedAt, &r.SizeDelta); err != nil {
			return nil, err
		}
		r.ParentID = parent.Int64
		r.Current = r.ID == rp.Rev
		out = append(out, r)
	}
	return out, rows.Err()
}

// LinkReport reports a live page's links to missing pages and its embeds of
// files not uploaded, as they stand now: after a batch of writes, links to
// pages created later in the batch are no longer red.
func (s *Store) LinkReport(ctx context.Context, title string) (red, missingFiles []string, err error) {
	rp, err := s.GetPage(ctx, title, GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	var r PutResult
	if err := s.linkReport(ctx, s.r, rp.ID, &r); err != nil {
		return nil, nil, err
	}
	return r.RedLinks, r.NoFiles, nil
}
