package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xen0bit/kb/internal/wikitext"
)

// ftsQuery passes a query through when it uses FTS5 syntax deliberately
// (quotes, operators, column filters, prefix stars), and otherwise quotes each
// word, so "C++ (programming language)" is a search rather than a syntax error.
func ftsQuery(q string) string {
	q = strings.TrimSpace(q)
	if strings.ContainsAny(q, `"*:^`) || strings.Contains(q, " AND ") || strings.Contains(q, " OR ") || strings.Contains(q, " NOT ") || strings.Contains(q, "NEAR(") {
		return q
	}
	var parts []string
	for _, w := range strings.FieldsFunc(q, func(r rune) bool { return unicode.IsSpace(r) }) {
		parts = append(parts, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " ")
}

// ftsErr turns an FTS5 syntax error into ErrInvalid with a hint.
func ftsErr(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	if strings.Contains(m, "fts5") || strings.Contains(m, "syntax error") || strings.Contains(m, "no such column") ||
		strings.Contains(m, "unterminated") || strings.Contains(m, "malformed MATCH") {
		return fmt.Errorf("%w: search query: %v (FTS5 syntax: words, \"exact phrase\", prefix*, a OR b, a NOT b, title:word, NEAR(a b, 5))", ErrInvalid, err)
	}
	return err
}

// PageFilter narrows listings and searches.
type PageFilter struct {
	Category      string
	Status        string // exact; "none" for pages without one
	Prefix        string // title prefix
	UpdatedSince  int64
	UpdatedBefore int64
	Author        string // last editor
	Redirects     bool   // include redirect pages
	MinSize       int
	MaxSize       int
}

func (f PageFilter) where(args *[]any) []string {
	w := []string{"p.deleted_at IS NULL"}
	if !f.Redirects {
		w = append(w, "p.redirect_to IS NULL")
	}
	if f.Category != "" {
		w = append(w, "EXISTS (SELECT 1 FROM categories c WHERE c.page_id = p.id AND c.category_key = ?)")
		*args = append(*args, wikitext.Key(f.Category))
	}
	switch f.Status {
	case "":
	case "none":
		w = append(w, "p.status = ''")
	default:
		w = append(w, "p.status = ?")
		*args = append(*args, strings.ToLower(f.Status))
	}
	if f.Prefix != "" {
		w = append(w, `p.key LIKE ? ESCAPE '\'`)
		*args = append(*args, escapeLike(strings.ToLower(strings.ReplaceAll(f.Prefix, "_", " ")))+"%")
	}
	if f.UpdatedSince > 0 {
		w = append(w, "p.updated_at >= ?")
		*args = append(*args, f.UpdatedSince)
	}
	if f.UpdatedBefore > 0 {
		w = append(w, "p.updated_at < ?")
		*args = append(*args, f.UpdatedBefore)
	}
	if f.Author != "" {
		w = append(w, "p.updated_by = ?")
		*args = append(*args, f.Author)
	}
	if f.MinSize > 0 {
		w = append(w, "p.size >= ?")
		*args = append(*args, f.MinSize)
	}
	if f.MaxSize > 0 {
		w = append(w, "p.size <= ?")
		*args = append(*args, f.MaxSize)
	}
	return w
}

// backlinksExpr counts live pages linking to p by title or alias.
const backlinksExpr = `(SELECT count(DISTINCT l.src) FROM links l JOIN pages s ON s.id = l.src AND s.deleted_at IS NULL
	WHERE l.src != p.id AND (l.target_key = p.key OR l.target_key IN (SELECT alias_key FROM aliases WHERE page_id = p.id)))`

// PageSummary is a page in a listing or search result.
type PageSummary struct {
	Title      string  `json:"title"`
	Rev        int64   `json:"rev"`
	Status     string  `json:"status,omitempty"`
	Size       int     `json:"size"`
	Words      int     `json:"words"`
	UpdatedAt  int64   `json:"updated_at"`
	UpdatedBy  string  `json:"updated_by,omitempty"`
	CreatedAt  int64   `json:"created_at"`
	RedirectTo string  `json:"redirect_to,omitempty"`
	Backlinks  int     `json:"backlinks"`
	Lead       string  `json:"lead,omitempty"`
	Snippet    string  `json:"snippet,omitempty"`
	Score      float64 `json:"score,omitempty"`
}

var sortCols = map[string]string{
	"title":     "p.key",
	"updated":   "p.updated_at",
	"created":   "p.created_at",
	"size":      "p.size",
	"words":     "p.words",
	"backlinks": "backlinks",
}

// sortClause builds ORDER BY for a sort name; desc defaults by kind (newest,
// largest and most-linked first; titles A→Z).
func sortClause(name string, desc *bool) (string, error) {
	col, ok := sortCols[name]
	if !ok {
		return "", fmt.Errorf("%w: sort must be one of title, updated, created, size, words, backlinks", ErrInvalid)
	}
	d := name != "title"
	if desc != nil {
		d = *desc
	}
	if d {
		return col + " DESC, p.id DESC", nil
	}
	return col + " ASC, p.id ASC", nil
}

// ListPages lists live pages. sort is one of title (default), updated,
// created, size, words, backlinks.
func (s *Store) ListPages(ctx context.Context, f PageFilter, sortBy string, desc *bool, limit, offset int) ([]PageSummary, int, error) {
	if sortBy == "" {
		sortBy = "title"
	}
	order, err := sortClause(sortBy, desc)
	if err != nil {
		return nil, 0, err
	}
	limit = clampLimit(limit, 50, 500)
	var args []any
	w := strings.Join(f.where(&args), " AND ")
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM pages p WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT p.title, p.current_rev, p.status, p.size, p.words, p.updated_at, p.updated_by, p.created_at,
		COALESCE(p.redirect_to, ''), `+backlinksExpr+` AS backlinks, p.lead FROM pages p WHERE `+w+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanSummaries(rows, false)
	return out, total, err
}

func scanSummaries(rows *sql.Rows, scored bool) ([]PageSummary, error) {
	defer rows.Close()
	var out []PageSummary
	for rows.Next() {
		var p PageSummary
		dest := []any{&p.Title, &p.Rev, &p.Status, &p.Size, &p.Words, &p.UpdatedAt, &p.UpdatedBy, &p.CreatedAt, &p.RedirectTo, &p.Backlinks, &p.Lead}
		if scored {
			dest = append(dest, &p.Snippet, &p.Score)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Search runs a full-text query. sort "relevance" (default) ranks by BM25
// with title and alias matches weighted above body matches.
func (s *Store) Search(ctx context.Context, query string, f PageFilter, sortBy string, desc *bool, limit, offset int) ([]PageSummary, int, error) {
	if strings.TrimSpace(query) == "" {
		return nil, 0, fmt.Errorf("%w: search needs a query (use list_pages to browse)", ErrInvalid)
	}
	limit = clampLimit(limit, 20, 200)
	order := "score ASC"
	if sortBy != "" && sortBy != "relevance" {
		o, err := sortClause(sortBy, desc)
		if err != nil {
			return nil, 0, err
		}
		order = o
	}
	args := []any{ftsQuery(query)}
	w := append([]string{"pages_fts MATCH ?"}, f.where(&args)...)
	from := `pages_fts JOIN pages p ON p.id = pages_fts.rowid`
	ws := strings.Join(w, " AND ")
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM `+from+` WHERE `+ws, args...).Scan(&total); err != nil {
		return nil, 0, ftsErr(err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT p.title, p.current_rev, p.status, p.size, p.words, p.updated_at, p.updated_by, p.created_at,
		COALESCE(p.redirect_to, ''), `+backlinksExpr+` AS backlinks, p.lead,
		snippet(pages_fts, 2, '**', '**', '…', 24), bm25(pages_fts, 10.0, 5.0, 1.0) AS score
		FROM `+from+` WHERE `+ws+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, ftsErr(err)
	}
	out, err := scanSummaries(rows, true)
	for i := range out {
		out[i].Score = -out[i].Score // bm25 is lower-is-better; report higher-is-better
		out[i].Lead = ""             // the snippet is the useful part of a hit
	}
	return out, total, err
}

// Suggest returns up to n live titles that look like title: by trigram
// overlap, re-ranked by edit distance. It is the "did you mean" behind a
// missing page.
func (s *Store) Suggest(ctx context.Context, title string, n int) ([]string, error) {
	t := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(title, "_", " ")))
	if utf8.RuneCountInString(t) < 3 || n <= 0 {
		return nil, nil
	}
	runes := []rune(t)
	seen := map[string]bool{}
	var grams []string
	for i := 0; i+3 <= len(runes); i++ {
		g := string(runes[i : i+3])
		if !seen[g] {
			seen[g] = true
			grams = append(grams, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
		}
		if len(grams) == 64 {
			break
		}
	}
	rows, err := s.r.QueryContext(ctx, `SELECT p.title, titles_fts.title FROM titles_fts JOIN pages p ON p.id = titles_fts.rowid
		WHERE titles_fts MATCH ? AND p.deleted_at IS NULL ORDER BY bm25(titles_fts) LIMIT 50`, strings.Join(grams, " OR "))
	if err != nil {
		return nil, ftsErr(err)
	}
	defer rows.Close()
	type cand struct {
		title string
		d     int
	}
	var cs []cand
	for rows.Next() {
		var pt, names string
		if err := rows.Scan(&pt, &names); err != nil {
			return nil, err
		}
		best := 1 << 30
		for name := range strings.SplitSeq(names, "\n") {
			best = min(best, levenshtein(t, strings.ToLower(name)))
		}
		cs = append(cs, cand{pt, best})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].d < cs[j].d })
	var out []string
	for _, c := range cs {
		if len(out) == n {
			break
		}
		// Reject what is barely related: more edits than half the title.
		if c.d > max(3, len(runes)/2) {
			continue
		}
		out = append(out, c.title)
	}
	return out, nil
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Resolution says what a title refers to.
type Resolution struct {
	Query     string   `json:"query"`
	Status    string   `json:"status"` // exists, redirect, alias, deleted, missing, invalid
	Title     string   `json:"title,omitempty"`
	Rev       int64    `json:"rev,omitempty"`
	Target    string   `json:"target,omitempty"` // for redirect and alias: the page it reaches
	Link      string   `json:"link,omitempty"`   // a markdown link destination to use
	Suggest   []string `json:"did_you_mean,omitempty"`
	Error     string   `json:"error,omitempty"`
	Backlinks int      `json:"inbound_links,omitempty"` // for missing: how many pages already want it
}

// Resolve resolves many titles at once.
func (s *Store) Resolve(ctx context.Context, titles []string, suggest int) ([]Resolution, error) {
	out := make([]Resolution, 0, len(titles))
	for _, q := range titles {
		r := Resolution{Query: q}
		n, err := wikitext.NormalizeTitle(q)
		if err != nil {
			r.Status, r.Error = "invalid", err.Error()
			out = append(out, r)
			continue
		}
		key := strings.ToLower(n)
		p, err := s.pageByKey(ctx, s.r, key)
		if err != nil {
			return nil, err
		}
		switch {
		case p != nil && !p.Deleted && p.RedirectTo != "":
			r.Status, r.Title, r.Rev = "redirect", p.Title, p.Rev
			if t, err := s.pageByKey(ctx, s.r, p.RedirectTo); err == nil && t != nil && !t.Deleted {
				r.Target = t.Title
			}
			r.Link = wikitext.PageScheme + wikitext.URLForm(p.Title)
		case p != nil && !p.Deleted:
			r.Status, r.Title, r.Rev = "exists", p.Title, p.Rev
			r.Link = wikitext.PageScheme + wikitext.URLForm(p.Title)
		default:
			var target string
			err := s.r.QueryRowContext(ctx, `SELECT p.title FROM aliases a JOIN pages p ON p.id = a.page_id
				WHERE a.alias_key = ? AND p.deleted_at IS NULL ORDER BY p.updated_at DESC LIMIT 1`, key).Scan(&target)
			if err == nil {
				r.Status, r.Title, r.Target = "alias", n, target
				r.Link = wikitext.PageScheme + wikitext.URLForm(target)
				break
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			r.Status, r.Title = "missing", n
			if p != nil {
				r.Status = "deleted"
			}
			r.Link = wikitext.PageScheme + wikitext.URLForm(n)
			if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM links l JOIN pages s ON s.id = l.src AND s.deleted_at IS NULL
				WHERE l.target_key = ?`, key).Scan(&r.Backlinks); err != nil {
				return nil, err
			}
			if r.Suggest, err = s.Suggest(ctx, n, suggest); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// LinkEntry is one link in a page's link report.
type LinkEntry struct {
	Title  string `json:"title"`
	Exists bool   `json:"exists"`
	Count  int    `json:"count,omitempty"` // for wanted pages: inbound links
}

// PageLinks lists a page's outbound links (with whether each target
// exists) or its backlinks.
func (s *Store) PageLinks(ctx context.Context, title, direction string, limit, offset int) ([]LinkEntry, int, error) {
	rp, err := s.GetPage(ctx, title, GetOptions{})
	if err != nil {
		return nil, 0, err
	}
	limit = clampLimit(limit, 100, 1000)
	var q, cq string
	switch direction {
	case "out", "":
		cq = `SELECT count(*) FROM links WHERE src = ?1`
		q = `SELECT l.target_title, EXISTS (SELECT 1 FROM pages p WHERE p.key = l.target_key AND p.deleted_at IS NULL)
			OR EXISTS (SELECT 1 FROM aliases a JOIN pages p ON p.id = a.page_id WHERE a.alias_key = l.target_key AND p.deleted_at IS NULL)
			FROM links l WHERE l.src = ?1 ORDER BY l.target_key LIMIT ?2 OFFSET ?3`
	case "red":
		red := `FROM links l WHERE l.src = ?1
			AND NOT EXISTS (SELECT 1 FROM pages p WHERE p.key = l.target_key AND p.deleted_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM aliases a JOIN pages p ON p.id = a.page_id WHERE a.alias_key = l.target_key AND p.deleted_at IS NULL)`
		cq = `SELECT count(*) ` + red
		q = `SELECT l.target_title, 0 ` + red + ` ORDER BY l.target_key LIMIT ?2 OFFSET ?3`
	case "in":
		in := `FROM pages s WHERE s.deleted_at IS NULL AND s.id != ?1 AND EXISTS (SELECT 1 FROM links l WHERE l.src = s.id AND
			(l.target_key IN (SELECT key FROM pages WHERE id = ?1) OR l.target_key IN (SELECT alias_key FROM aliases WHERE page_id = ?1)))`
		cq = `SELECT count(*) ` + in
		q = `SELECT s.title, 1 ` + in + ` ORDER BY s.key LIMIT ?2 OFFSET ?3`
	default:
		return nil, 0, fmt.Errorf("%w: direction must be out, in or red", ErrInvalid)
	}
	var total int
	if err := s.r.QueryRowContext(ctx, cq, rp.ID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, q, rp.ID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []LinkEntry
	for rows.Next() {
		var e LinkEntry
		if err := rows.Scan(&e.Title, &e.Exists); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// SpecialKinds are the maintenance reports Special returns.
var SpecialKinds = []string{"wanted", "orphans", "dead_ends", "stubs", "uncategorized", "short", "long", "redirects", "broken_redirects", "missing_files", "unused_files", "most_linked"}

// SpecialEntry is one row of a maintenance report.
type SpecialEntry struct {
	Title string `json:"title"`
	Count int    `json:"count,omitempty"` // inbound links, size, etc., as the report says
	Note  string `json:"note,omitempty"`
}

const redLinkCond = `NOT EXISTS (SELECT 1 FROM pages p WHERE p.key = l.target_key AND p.deleted_at IS NULL)
	AND NOT EXISTS (SELECT 1 FROM aliases a JOIN pages p ON p.id = a.page_id WHERE a.alias_key = l.target_key AND p.deleted_at IS NULL)`

// Special runs a maintenance report, the way Wikipedia's special pages do.
// It is how an agent finds what to write next (wanted), what nothing reaches
// (orphans), and what is thin (stubs, short).
func (s *Store) Special(ctx context.Context, kind string, limit, offset int) ([]SpecialEntry, int, error) {
	limit = clampLimit(limit, 50, 500)
	live := `p.deleted_at IS NULL AND p.redirect_to IS NULL`
	var body, sel string
	switch kind {
	case "wanted":
		body = `FROM links l JOIN pages s ON s.id = l.src AND s.deleted_at IS NULL WHERE ` + redLinkCond + ` GROUP BY l.target_key`
		sel = `SELECT min(l.target_title), count(*) AS n, '' ` + body + ` ORDER BY n DESC, l.target_key`
	case "most_linked":
		body = `FROM pages p WHERE ` + live + ` AND ` + backlinksExpr + ` > 0`
		sel = `SELECT p.title, ` + backlinksExpr + ` AS n, '' ` + body + ` ORDER BY n DESC, p.key`
	case "orphans":
		body = `FROM pages p WHERE ` + live + ` AND ` + backlinksExpr + ` = 0`
		sel = `SELECT p.title, p.size, '' ` + body + ` ORDER BY p.updated_at DESC`
	case "dead_ends":
		body = `FROM pages p WHERE ` + live + ` AND NOT EXISTS (SELECT 1 FROM links l WHERE l.src = p.id)`
		sel = `SELECT p.title, p.size, '' ` + body + ` ORDER BY p.updated_at DESC`
	case "stubs":
		body = `FROM pages p WHERE ` + live + ` AND p.status = 'stub'`
		sel = `SELECT p.title, ` + backlinksExpr + ` AS n, 'inbound links' ` + body + ` ORDER BY n DESC, p.key`
	case "uncategorized":
		body = `FROM pages p WHERE ` + live + ` AND NOT EXISTS (SELECT 1 FROM categories c WHERE c.page_id = p.id)`
		sel = `SELECT p.title, p.size, '' ` + body + ` ORDER BY p.updated_at DESC`
	case "short":
		body = `FROM pages p WHERE ` + live
		sel = `SELECT p.title, p.size, '' ` + body + ` ORDER BY p.size ASC, p.key`
	case "long":
		body = `FROM pages p WHERE ` + live
		sel = `SELECT p.title, p.size, '' ` + body + ` ORDER BY p.size DESC, p.key`
	case "redirects":
		body = `FROM pages p WHERE p.deleted_at IS NULL AND p.redirect_to IS NOT NULL`
		sel = `SELECT p.title, 0, COALESCE((SELECT t.title FROM pages t WHERE t.key = p.redirect_to), p.redirect_to) ` + body + ` ORDER BY p.key`
	case "broken_redirects":
		body = `FROM pages p WHERE p.deleted_at IS NULL AND p.redirect_to IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM pages t WHERE t.key = p.redirect_to AND t.deleted_at IS NULL)`
		sel = `SELECT p.title, 0, p.redirect_to ` + body + ` ORDER BY p.key`
	case "missing_files":
		body = `FROM file_links fl JOIN pages p ON p.id = fl.src AND p.deleted_at IS NULL
			WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.key = fl.file_key AND f.deleted_at IS NULL) GROUP BY fl.file_key`
		sel = `SELECT min(fl.name), count(*) AS n, 'pages embedding it' ` + body + ` ORDER BY n DESC, fl.file_key`
	case "unused_files":
		body = `FROM files f WHERE f.deleted_at IS NULL AND NOT EXISTS (SELECT 1 FROM file_links fl JOIN pages p ON p.id = fl.src
			AND p.deleted_at IS NULL WHERE fl.file_key = f.key)`
		sel = `SELECT f.name, f.size, '' ` + body + ` ORDER BY f.updated_at DESC`
	default:
		return nil, 0, fmt.Errorf("%w: kind must be one of %s", ErrInvalid, strings.Join(SpecialKinds, ", "))
	}
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 `+body+`)`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, sel+` LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []SpecialEntry
	for rows.Next() {
		var e SpecialEntry
		if err := rows.Scan(&e.Title, &e.Count, &e.Note); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Categories lists categories with their live member counts. prefix filters
// by name; a category exists as soon as one page names it.
func (s *Store) Categories(ctx context.Context, prefix string, limit, offset int) ([]Count, int, error) {
	limit = clampLimit(limit, 100, 1000)
	w := `FROM categories c JOIN pages p ON p.id = c.page_id AND p.deleted_at IS NULL`
	var args []any
	if prefix != "" {
		w += ` WHERE c.category_key LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(strings.ToLower(prefix))+"%")
	}
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT count(DISTINCT c.category_key) `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT min(c.category), count(*) AS n `+w+` GROUP BY c.category_key ORDER BY n DESC, c.category_key LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanCounts(rows)
	return out, total, err
}

// Stats are the kb's headline numbers.
type Stats struct {
	SchemaVersion int            `json:"schema_version"`
	Pages         int            `json:"pages"`
	Redirects     int            `json:"redirects"`
	Revisions     int            `json:"revisions"`
	Words         int64          `json:"words"`
	Bytes         int64          `json:"bytes"`
	Files         int            `json:"files"`
	FileBytes     int64          `json:"file_bytes"`
	Categories    int            `json:"categories"`
	Links         int            `json:"links"`
	WantedPages   int            `json:"wanted_pages"`
	Tasks         map[string]int `json:"tasks"`
	Notes         int            `json:"notes"`
	LastChangeID  int64          `json:"last_change_id"`
	LastChangeAt  int64          `json:"last_change_at,omitempty"`
	ByStatus      []Count        `json:"pages_by_status,omitempty"`
}

// Stats computes the headline numbers.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	st := &Stats{Tasks: map[string]int{}}
	var err error
	if st.SchemaVersion, err = s.Version(ctx); err != nil {
		return nil, err
	}
	err = s.r.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM pages WHERE deleted_at IS NULL AND redirect_to IS NULL),
		(SELECT count(*) FROM pages WHERE deleted_at IS NULL AND redirect_to IS NOT NULL),
		(SELECT count(*) FROM revisions),
		(SELECT COALESCE(sum(words), 0) FROM pages WHERE deleted_at IS NULL),
		(SELECT COALESCE(sum(size), 0) FROM pages WHERE deleted_at IS NULL),
		(SELECT count(*) FROM files WHERE deleted_at IS NULL),
		(SELECT COALESCE(sum(size), 0) FROM blobs),
		(SELECT count(DISTINCT c.category_key) FROM categories c JOIN pages p ON p.id = c.page_id AND p.deleted_at IS NULL),
		(SELECT count(*) FROM links),
		(SELECT count(DISTINCT l.target_key) FROM links l JOIN pages s ON s.id = l.src AND s.deleted_at IS NULL WHERE `+redLinkCond+`),
		(SELECT count(*) FROM notes),
		(SELECT COALESCE(max(id), 0) FROM changes),
		(SELECT COALESCE(max(at), 0) FROM changes)`).Scan(
		&st.Pages, &st.Redirects, &st.Revisions, &st.Words, &st.Bytes, &st.Files, &st.FileBytes,
		&st.Categories, &st.Links, &st.WantedPages, &st.Notes, &st.LastChangeID, &st.LastChangeAt)
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT status, count(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	cs, err := scanCounts(rows)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		st.Tasks[c.Name] = c.Count
	}
	rows, err = s.r.QueryContext(ctx, `SELECT CASE status WHEN '' THEN '(none)' ELSE status END, count(*) FROM pages
		WHERE deleted_at IS NULL AND redirect_to IS NULL GROUP BY status ORDER BY count(*) DESC`)
	if err != nil {
		return nil, err
	}
	st.ByStatus, err = scanCounts(rows)
	return st, err
}

// Existing reports which of the given page keys resolve to a live page,
// directly or through an alias, mapping each to the title it reaches. Keys
// missing from the result are red links.
func (s *Store) Existing(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		ph := strings.Repeat("?,", len(chunk))
		ph = ph[:len(ph)-1]
		args := make([]any, 0, 2*len(chunk))
		for _, k := range chunk {
			args = append(args, k)
		}
		args = append(args, args...)
		rows, err := s.r.QueryContext(ctx, `SELECT key, title FROM pages WHERE deleted_at IS NULL AND key IN (`+ph+`)
			UNION ALL SELECT a.alias_key, p.title FROM aliases a JOIN pages p ON p.id = a.page_id
			WHERE p.deleted_at IS NULL AND a.alias_key IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k, t string
			if err := rows.Scan(&k, &t); err != nil {
				rows.Close()
				return nil, err
			}
			if _, seen := out[k]; !seen {
				out[k] = t
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ExistingFiles reports which of the given file keys are live files, mapped
// to their content type.
func (s *Store) ExistingFiles(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	rows, err := s.r.QueryContext(ctx, `SELECT key, mime FROM files WHERE deleted_at IS NULL AND key IN (`+ph[:len(ph)-1]+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, m string
		if err := rows.Scan(&k, &m); err != nil {
			return nil, err
		}
		out[k] = m
	}
	return out, rows.Err()
}

// LinkersOf lists live pages linking to a title, whether or not it exists:
// for a missing page, who wants it.
func (s *Store) LinkersOf(ctx context.Context, title string, limit int) ([]string, error) {
	key := wikitext.Key(title)
	if key == "" {
		return nil, fmt.Errorf("%w: %q is not a valid title", ErrInvalid, title)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT p.title FROM links l JOIN pages p ON p.id = l.src AND p.deleted_at IS NULL
		WHERE l.target_key = ? ORDER BY p.key LIMIT ?`, key, clampLimit(limit, 50, 1000))
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}
