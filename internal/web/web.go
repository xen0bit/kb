// Package web is kb's read-only browser viewer: server-rendered pages for
// articles (with red links, infoboxes and a table of contents), history and
// diffs, search, recent changes, maintenance reports, categories, files,
// tasks and notes. The CSS and JS are built by Bun from web/src and embedded
// in release binaries (build tag embed_web).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xen0bit/kb/internal/store"
	"github.com/xen0bit/kb/internal/wikitext"
	webui "github.com/xen0bit/kb/web"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed fallback.css
var fallbackCSS []byte

// Server serves the viewer.
type Server struct {
	st     *store.Store
	log    *slog.Logger
	pages  map[string]*template.Template
	assets http.Handler
	title  string
}

// Options configures the viewer.
type Options struct {
	Logger *slog.Logger
	Title  string // site name in the header, default "kb"
	// AssetsDir serves CSS/JS from disk instead of the embedded copy, for
	// working on web/src without rebuilding the binary.
	AssetsDir string
}

// New builds the viewer.
func New(st *store.Store, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Title == "" {
		opts.Title = "kb"
	}
	s := &Server{st: st, log: opts.Logger, pages: map[string]*template.Template{}, title: opts.Title}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if base == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
		}
		s.pages[base] = t
	}
	s.assets = assetHandler(opts.AssetsDir, opts.Logger)
	return s, nil
}

// assetHandler picks where CSS and JS come from: an explicit directory, the
// copy embedded at build time, web/dist in a checkout, or the small fallback
// stylesheet compiled into every build so a `go install` binary still renders.
func assetHandler(dir string, log *slog.Logger) http.Handler {
	var fsys fs.FS
	switch {
	case dir != "":
		fsys = os.DirFS(dir)
	case webui.Dist != nil:
		fsys = webui.Dist
	default:
		if st, err := os.Stat("web/dist/app.css"); err == nil && !st.IsDir() {
			fsys = os.DirFS("web/dist")
			log.Info("serving web assets from web/dist on disk")
		}
	}
	if fsys == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/static/app.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(fallbackCSS)
			case "/static/app.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write([]byte("// built without web assets (make web.build)\n"))
			default:
				http.NotFound(w, r)
			}
		})
	}
	return http.StripPrefix("/static/", http.FileServerFS(fsys))
}

// Handler is the viewer's routes, with security headers on every response.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /wiki/{title...}", s.page)
	mux.HandleFunc("GET /history/{title...}", s.history)
	mux.HandleFunc("GET /links/{title...}", s.links)
	mux.HandleFunc("GET /diff", s.diff)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /recent", s.recent)
	mux.HandleFunc("GET /random", s.random)
	mux.HandleFunc("GET /pages", s.allPages)
	mux.HandleFunc("GET /special", s.specialIndex)
	mux.HandleFunc("GET /special/{kind}", s.special)
	mux.HandleFunc("GET /categories", s.categories)
	mux.HandleFunc("GET /category/{name...}", s.category)
	mux.HandleFunc("GET /files", s.files)
	mux.HandleFunc("GET /file/{name...}", s.file)
	mux.HandleFunc("GET /tasks", s.tasks)
	mux.HandleFunc("GET /notes", s.notes)
	mux.HandleFunc("GET /feedback", s.feedback)
	mux.HandleFunc("GET /api/suggest", s.suggest)
	mux.Handle("GET /static/", s.assets)
	return secure(mux)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/file/") {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

// --- template plumbing --------------------------------------------------------

var funcs = template.FuncMap{
	"wiki":    func(title string) string { return "/wiki/" + wikitext.URLForm(title) },
	"fileurl": func(name string) string { return "/file/" + wikitext.URLForm(name) },
	"history": func(title string) string { return "/history/" + wikitext.URLForm(title) },
	"ago":     ago,
	"iso": func(ms int64) string {
		if ms == 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	},
	"bytes": humanBytes,
	"signed": func(n int) string {
		if n > 0 {
			return "+" + strconv.Itoa(n)
		}
		return strconv.Itoa(n)
	},
	"isimage":  func(m string) bool { return strings.HasPrefix(m, "image/") },
	"add":      func(a, b int) int { return a + b },
	"propkeys": wikitext.SortedPropKeys,
	"prop": func(v any) string {
		switch t := v.(type) {
		case []any:
			parts := make([]string, len(t))
			for i, e := range t {
				parts[i] = fmt.Sprint(e)
			}
			return strings.Join(parts, ", ")
		case map[string]any:
			var parts []string
			for _, k := range wikitext.SortedPropKeys(t) {
				parts = append(parts, k+": "+fmt.Sprint(t[k]))
			}
			return strings.Join(parts, "; ")
		}
		return fmt.Sprint(v)
	},
	"query": func(kv ...string) template.URL {
		v := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] != "" {
				v.Set(kv[i], kv[i+1])
			}
		}
		return template.URL("?" + v.Encode())
	},
	"itoa": strconv.Itoa,
}

func ago(ms int64) string {
	if ms == 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
	return time.UnixMilli(ms).UTC().Format("2 Jan 2006")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func humanBytes(n any) string {
	var v float64
	switch t := n.(type) {
	case int:
		v = float64(t)
	case int64:
		v = float64(t)
	}
	units := []string{"B", "KiB", "MiB", "GiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", int64(v))
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

type view struct {
	Site  string
	Title string
	Query string
	Data  any
}

func (s *Server) show(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.Execute(w, view{Site: s.title, Title: title, Query: r.URL.Query().Get("q"), Data: data}); err != nil {
		s.log.Error("render", "template", name, "error", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrInvalid):
		status = http.StatusBadRequest
	}
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "path", r.URL.Path, "error", err)
	}
	s.show(w, r, status, "error", http.StatusText(status), err.Error())
}

func intParam(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

// pager is the data for previous/next links.
type pager struct {
	Total, Offset, Limit int
	Prev, Next           string
}

func newPager(r *http.Request, total, offset, limit int) pager {
	p := pager{Total: total, Offset: offset, Limit: limit}
	link := func(off int) string {
		q := r.URL.Query()
		q.Set("offset", strconv.Itoa(off))
		return r.URL.Path + "?" + q.Encode()
	}
	if offset > 0 {
		p.Prev = link(max(offset-limit, 0))
	}
	if offset+limit < total {
		p.Next = link(offset + limit)
	}
	return p
}

// --- handlers ----------------------------------------------------------------

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := s.st.GetPage(ctx, "Main Page", store.GetOptions{}); err == nil {
		http.Redirect(w, r, "/wiki/Main_Page", http.StatusFound)
		return
	}
	st, err := s.st.Stats(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	recent, _, _ := s.st.ListPages(ctx, store.PageFilter{}, "updated", nil, 15, 0)
	wanted, _, _ := s.st.Special(ctx, "wanted", 15, 0)
	cats, _, _ := s.st.Categories(ctx, "", 20, 0)
	s.show(w, r, http.StatusOK, "home", s.title, map[string]any{"Stats": st, "Recent": recent, "Wanted": wanted, "Categories": cats})
}

type pageView struct {
	Page      *store.ReadResult
	Rev       *store.Revision
	Rendered  *Rendered
	LinksOut  int
	Backlinks int
	Old       bool
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	title := r.PathValue("title") // already unescaped by the mux
	norm, err := wikitext.NormalizeTitle(title)
	if err != nil {
		s.fail(w, r, fmt.Errorf("%w: %v", store.ErrInvalid, err))
		return
	}
	// Canonical URLs: /wiki/albert einstein → /wiki/Albert_einstein.
	if title != strings.ReplaceAll(norm, " ", "_") {
		canon := wikitext.URLForm(norm)
		u := "/wiki/" + canon
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusMovedPermanently)
		return
	}
	p, err := s.st.GetPage(ctx, norm, store.GetOptions{Body: true, FollowRedirect: r.URL.Query().Get("redirect") != "no"})
	if errors.Is(err, store.ErrNotFound) {
		s.missing(w, r, norm)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := pageView{Page: p}
	body := p.Body
	if rev, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64); rev > 0 && rev != p.Rev {
		rv, err := s.st.GetRevision(ctx, rev)
		if err != nil || rv.PageID != p.ID {
			s.fail(w, r, fmt.Errorf("%w: rev %d is not a revision of %q", store.ErrNotFound, rev, p.Title))
			return
		}
		v.Rev, v.Old, body = rv, true, rv.Body
	}
	if v.Rendered, err = render(ctx, s.st, body); err != nil {
		s.fail(w, r, err)
		return
	}
	v.LinksOut, v.Backlinks, _ = s.st.LinkCounts(ctx, p.ID)
	s.show(w, r, http.StatusOK, "page", p.Title, v)
}

func (s *Server) missing(w http.ResponseWriter, r *http.Request, title string) {
	ctx := r.Context()
	res, err := s.st.Resolve(ctx, []string{title}, 8)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	linkers, err := s.st.LinkersOf(ctx, title, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hits, _, _ := s.st.Search(ctx, `"`+strings.ReplaceAll(title, `"`, "")+`"`, store.PageFilter{}, "", nil, 10, 0)
	s.show(w, r, http.StatusNotFound, "missing", title, map[string]any{"Title": title, "Res": res[0], "Linkers": linkers, "Mentions": hits})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	title := r.PathValue("title")
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	revs, err := s.st.History(r.Context(), title, before, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var older string
	if n := len(revs); n == 100 {
		older = fmt.Sprintf("?before=%d", revs[n-1].ID)
	}
	t := title
	if len(revs) > 0 {
		t = revs[0].Title
	}
	s.show(w, r, http.StatusOK, "history", "History: "+t, map[string]any{"Title": t, "Revs": revs, "Older": older})
}

func (s *Server) links(w http.ResponseWriter, r *http.Request) {
	title := r.PathValue("title")
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "in"
	}
	off := intParam(r, "offset", 0)
	links, total, err := s.st.PageLinks(r.Context(), title, dir, 200, off)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	label := map[string]string{"in": "Pages linking to", "out": "Links from", "red": "Missing pages linked from"}[dir]
	s.show(w, r, http.StatusOK, "links", label+" "+title, map[string]any{"Title": title, "Dir": dir, "Label": label, "Links": links, "Pager": newPager(r, total, off, 200)})
}

type diffLine struct {
	Class, Text string
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	a, err := s.st.GetRevision(ctx, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, err := s.st.GetRevision(ctx, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if a.PageID != b.PageID {
		s.fail(w, r, fmt.Errorf("%w: revisions of different pages", store.ErrInvalid))
		return
	}
	var lines []diffLine
	for _, l := range wikitext.Lines(wikitext.UnifiedDiff("a", "b", a.Body, b.Body, 3))[2:] {
		c := "ctx"
		switch {
		case strings.HasPrefix(l, "@@"):
			c = "hunk"
		case strings.HasPrefix(l, "+"):
			c = "add"
		case strings.HasPrefix(l, "-"):
			c = "del"
		}
		lines = append(lines, diffLine{c, l})
	}
	s.show(w, r, http.StatusOK, "diff", "Diff: "+b.Title, map[string]any{"A": a, "B": b, "Lines": lines})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/pages", http.StatusFound)
		return
	}
	ctx := r.Context()
	// An exact title goes straight to the page, as Wikipedia's "Go" does.
	if r.URL.Query().Get("fulltext") == "" {
		if res, err := s.st.Resolve(ctx, []string{q}, 0); err == nil && len(res) == 1 && res[0].Status != "missing" && res[0].Status != "invalid" && res[0].Status != "deleted" {
			http.Redirect(w, r, "/wiki/"+wikitext.URLForm(res[0].Title), http.StatusFound)
			return
		}
	}
	off := intParam(r, "offset", 0)
	hits, total, err := s.st.Search(ctx, q, store.PageFilter{Category: r.URL.Query().Get("category")}, r.URL.Query().Get("sort"), nil, 30, off)
	if err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, r, err)
		return
	}
	var sugg []string
	if total == 0 {
		sugg, _ = s.st.Suggest(ctx, q, 8)
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	s.show(w, r, http.StatusOK, "search", "Search: "+q, map[string]any{"Q": q, "Hits": hits, "Pager": newPager(r, total, off, 30), "Suggest": sugg, "Error": errMsg})
}

func (s *Server) recent(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	ch, err := s.st.Changes(r.Context(), store.ChangeFilter{BeforeID: before, Author: r.URL.Query().Get("author"), Kind: r.URL.Query().Get("kind"), Limit: 100})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var older string
	if n := len(ch); n == 100 {
		q := r.URL.Query()
		q.Set("before", strconv.FormatInt(ch[n-1].ID, 10))
		older = "?" + q.Encode()
	}
	s.show(w, r, http.StatusOK, "recent", "Recent changes", map[string]any{"Changes": ch, "Older": older})
}

func (s *Server) random(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, total, err := s.st.ListPages(ctx, store.PageFilter{}, "title", nil, 1, 0)
	if err != nil || total == 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	list, _, err := s.st.ListPages(ctx, store.PageFilter{}, "title", nil, 1, rand.IntN(total)) //nolint:gosec // not security-relevant
	if err != nil || len(list) == 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/wiki/"+wikitext.URLForm(list[0].Title), http.StatusFound)
}

func (s *Server) listPages(w http.ResponseWriter, r *http.Request, heading string, f store.PageFilter, sortBy string) {
	off := intParam(r, "offset", 0)
	if q := r.URL.Query().Get("sort"); q != "" {
		sortBy = q
	}
	f.Prefix = r.URL.Query().Get("prefix")
	f.Status = r.URL.Query().Get("status")
	list, total, err := s.st.ListPages(r.Context(), f, sortBy, nil, 100, off)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.show(w, r, http.StatusOK, "pages", heading, map[string]any{"Heading": heading, "Pages": list, "Sort": sortBy, "Pager": newPager(r, total, off, 100)})
}

func (s *Server) allPages(w http.ResponseWriter, r *http.Request) {
	s.listPages(w, r, "All pages", store.PageFilter{}, "title")
}

func (s *Server) category(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if n, err := wikitext.NormalizeTitle(name); err == nil {
		name = n
	}
	s.listPages(w, r, "Category: "+name, store.PageFilter{Category: name}, "title")
}

var specialLabels = map[string]string{
	"wanted": "Wanted pages", "most_linked": "Most linked-to pages", "orphans": "Orphaned pages", "dead_ends": "Dead-end pages",
	"stubs": "Stubs", "uncategorized": "Uncategorized pages", "short": "Short pages", "long": "Long pages",
	"redirects": "Redirects", "broken_redirects": "Broken redirects", "missing_files": "Missing files", "unused_files": "Unused files",
}

func (s *Server) specialIndex(w http.ResponseWriter, r *http.Request) {
	var kinds [][2]string
	for _, k := range store.SpecialKinds {
		kinds = append(kinds, [2]string{k, specialLabels[k]})
	}
	s.show(w, r, http.StatusOK, "special_index", "Special pages", kinds)
}

func (s *Server) special(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	off := intParam(r, "offset", 0)
	rows, total, err := s.st.Special(r.Context(), kind, 100, off)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	file := kind == "missing_files" || kind == "unused_files"
	s.show(w, r, http.StatusOK, "special", specialLabels[kind], map[string]any{"Kind": kind, "Label": specialLabels[kind], "Rows": rows, "Files": file, "Pager": newPager(r, total, off, 100)})
}

func (s *Server) categories(w http.ResponseWriter, r *http.Request) {
	off := intParam(r, "offset", 0)
	cats, total, err := s.st.Categories(r.Context(), r.URL.Query().Get("prefix"), 200, off)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.show(w, r, http.StatusOK, "categories", "Categories", map[string]any{"Cats": cats, "Pager": newPager(r, total, off, 200)})
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	off := intParam(r, "offset", 0)
	files, total, err := s.st.ListFiles(r.Context(), store.FileFilter{Prefix: r.URL.Query().Get("prefix"), MIME: r.URL.Query().Get("type"), Sort: "updated", Desc: true, Limit: 60, Offset: off})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.show(w, r, http.StatusOK, "files", "Files", map[string]any{"Files": files, "Pager": newPager(r, total, off, 60)})
}

// file serves a blob. Uploaded content is untrusted, so it is served under a
// sandboxing CSP (an SVG cannot run script against the viewer's origin), and
// anything that is not an image, PDF, audio, video or plain text downloads
// instead of rendering.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f, data, err := s.st.GetFile(r.Context(), name, true, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	etag := `"` + f.SHA256 + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; media-src 'self'")
	inline := strings.HasPrefix(f.MIME, "image/") || strings.HasPrefix(f.MIME, "audio/") || strings.HasPrefix(f.MIME, "video/") ||
		f.MIME == "application/pdf" || strings.HasPrefix(f.MIME, "text/plain")
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	h.Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disp, url.PathEscape(f.Name)))
	h.Set("Content-Type", f.MIME)
	http.ServeContent(w, r, "", time.UnixMilli(f.UpdatedAt), strings.NewReader(string(data)))
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query()["status"]
	off := intParam(r, "offset", 0)
	list, total, err := s.st.ListTasks(r.Context(), store.TaskFilter{Status: st, Limit: 200, Offset: off})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.show(w, r, http.StatusOK, "tasks", "Tasks", map[string]any{"Tasks": list, "Pager": newPager(r, total, off, 200), "All": len(st) == 1 && st[0] == "all"})
}

func (s *Server) notes(w http.ResponseWriter, r *http.Request) {
	off := intParam(r, "offset", 0)
	q := r.URL.Query()
	notes, total, err := s.st.ListNotes(r.Context(), store.NoteFilter{Topic: q.Get("topic"), Page: q.Get("page"), Query: q.Get("nq"), Limit: 50, Offset: off})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	topics, _ := s.st.NoteTopics(r.Context(), 50)
	type rn struct {
		store.Note
		HTML template.HTML
	}
	out := make([]rn, len(notes))
	for i, n := range notes {
		out[i].Note = n
		if rd, err := render(r.Context(), s.st, n.Body); err == nil {
			out[i].HTML = rd.HTML
		}
	}
	s.show(w, r, http.StatusOK, "notes", "Notes", map[string]any{"Notes": out, "Topics": topics, "Topic": q.Get("topic"), "Pager": newPager(r, total, off, 50)})
}

// suggest backs the search box's type-ahead: title-prefix matches first,
// then fuzzy matches.
func (s *Server) suggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if q == "" {
		_, _ = w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	seen := map[string]bool{}
	var out []string
	list, _, _ := s.st.ListPages(ctx, store.PageFilter{Prefix: q, Redirects: true}, "backlinks", nil, 8, 0)
	for _, p := range list {
		if !seen[p.Title] {
			seen[p.Title] = true
			out = append(out, p.Title)
		}
	}
	if len(out) < 8 {
		fz, _ := s.st.Suggest(ctx, q, 8-len(out))
		for _, t := range fz {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	_, _ = w.Write([]byte(jsonStrings(out)))
}

func jsonStrings(ss []string) string {
	if ss == nil {
		ss = []string{}
	}
	b, _ := json.Marshal(ss)
	return string(b)
}

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	off := intParam(r, "offset", 0)
	status := r.URL.Query().Get("status")
	list, total, err := s.st.ListFeedback(r.Context(), store.FeedbackFilter{Status: status, Tool: r.URL.Query().Get("tool"), Limit: 100, Offset: off})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.show(w, r, http.StatusOK, "feedback", "Feedback", map[string]any{"Items": list, "All": status == "all", "Pager": newPager(r, total, off, 100)})
}
