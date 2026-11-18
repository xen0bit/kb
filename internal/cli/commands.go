package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xen0bit/kb/internal/buildinfo"
	"github.com/xen0bit/kb/internal/exportfs"
	"github.com/xen0bit/kb/internal/mcpserver"
	"github.com/xen0bit/kb/internal/store"
	"github.com/xen0bit/kb/internal/web"
	"github.com/xen0bit/kb/internal/wikitext"
)

func runMCP(ctx context.Context, args []string) error {
	c := newCommand("mcp", "[-http ADDR]")
	httpAddr := c.fs.String("http", "", "serve streamable HTTP on `addr` (e.g. 127.0.0.1:8080) instead of stdio; a non-loopback address requires "+mcpserver.TokenEnv)
	who := c.fs.String("author", "", "default author for writes when the client does not name itself")
	maxFile := c.fs.Int64("max-file-size", store.DefaultMaxFileBytes, "largest upload in `bytes`")
	if _, err := c.parse(args); err != nil {
		return err
	}
	level := slog.LevelWarn
	if *httpAddr != "" {
		level = slog.LevelInfo
	}
	logger := mcpserver.NewLogger(level)
	st, err := store.Open(*c.db, store.Options{Logger: logger})
	if err != nil {
		return err
	}
	defer st.Close()
	opts := mcpserver.Options{Version: buildinfo.Version, Logger: logger, MaxFileBytes: *maxFile, Author: *who}
	if *httpAddr != "" {
		return mcpserver.ServeHTTP(ctx, *httpAddr, st, opts)
	}
	return mcpserver.Serve(ctx, st, opts)
}

func runServe(ctx context.Context, args []string) error {
	c := newCommand("serve", "[-listen ADDR] [-mcp]")
	listen := c.fs.String("listen", "127.0.0.1:8080", "`addr` to serve on; a non-loopback address requires "+mcpserver.TokenEnv)
	withMCP := c.fs.Bool("mcp", false, "also serve MCP (streamable HTTP) at /mcp")
	title := c.fs.String("title", "kb", "site name shown in the header")
	assets := c.fs.String("assets", "", "serve CSS/JS from this `dir` (for working on web/src)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	logger := mcpserver.NewLogger(slog.LevelInfo)
	st, err := store.Open(*c.db, store.Options{Logger: logger})
	if err != nil {
		return err
	}
	defer st.Close()
	v, err := web.New(st, web.Options{Logger: logger, Title: *title, AssetsDir: *assets})
	if err != nil {
		return err
	}
	token := os.Getenv(mcpserver.TokenEnv)
	l, err := mcpserver.Listen(*listen, token, "the kb")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", v.Handler())
	if *withMCP {
		mux.Handle("/mcp", mcpserver.Handler(st, mcpserver.Options{Version: buildinfo.Version, Logger: logger}))
	}
	logger.Info("serving", "url", "http://"+l.Addr().String()+"/", "db", *c.db, "mcp", *withMCP, "auth", token != "")
	fmt.Fprintf(os.Stderr, "kb: viewer at http://%s/", l.Addr())
	if *withMCP {
		fmt.Fprintf(os.Stderr, "  MCP at http://%s/mcp", l.Addr())
	}
	fmt.Fprintln(os.Stderr)
	return mcpserver.RunHTTP(ctx, l, mcpserver.Protect(logger, token, mux))
}

func runGet(ctx context.Context, args []string) error {
	c := newCommand("get", "TITLE [-rev N] [-section S] [-outline]").withJSON()
	rev := c.fs.Int64("rev", 0, "read this revision")
	section := c.fs.String("section", "", "print one section: index, heading path or heading")
	outline := c.fs.Bool("outline", false, "print the section outline")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 && *rev == 0 {
		return usageErr("give one TITLE")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	var body, title string
	var meta any
	if *rev > 0 {
		r, err := st.GetRevision(ctx, *rev)
		if err != nil {
			return err
		}
		body, title, meta = r.Body, r.Title, r
	} else {
		p, err := st.GetPage(ctx, pos[0], store.GetOptions{Body: true, FollowRedirect: true})
		if err != nil {
			return err
		}
		body, title, meta = p.Body, p.Title, p
		if p.RedirectedFrom != "" {
			fmt.Fprintf(os.Stderr, "(redirected from %s)\n", p.RedirectedFrom)
		}
	}
	if *outline {
		secs := wikitext.Outline(body)
		if c.json() {
			return printJSON(secs)
		}
		table(func(w io.Writer) {
			fmt.Fprintln(w, "#\tLINES\tBYTES\tSECTION")
			for _, s := range secs {
				fmt.Fprintf(w, "%d\t%d-%d\t%d\t%s%s\n", s.Index, s.Start, s.End, s.Bytes, strings.Repeat("  ", max(s.Level-1, 0)), s.Title)
			}
		})
		return nil
	}
	if *section != "" {
		s, err := wikitext.FindSection(body, *section)
		if err != nil {
			return err
		}
		body = wikitext.SliceLines(body, s.Start, s.End)
	}
	if c.json() {
		return printJSON(map[string]any{"title": title, "meta": meta, "body": body})
	}
	_, err = io.WriteString(os.Stdout, body)
	return err
}

func readInput(path string) (string, error) {
	var b []byte
	var err error
	if path == "" || path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	return string(b), err
}

func printPut(r *store.PutResult) {
	verb := "updated"
	switch {
	case r.Unchanged:
		verb = "unchanged"
	case r.Created:
		verb = "created"
	}
	fmt.Printf("%s %q rev %d (%d bytes, %+d)\n", verb, r.Title, r.Rev, r.Size, r.SizeDelta)
	if len(r.RedLinks) > 0 {
		fmt.Printf("  red links: %s\n", strings.Join(r.RedLinks, ", "))
	}
	if len(r.NoFiles) > 0 {
		fmt.Printf("  missing files: %s\n", strings.Join(r.NoFiles, ", "))
	}
	for _, w := range r.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func runPut(ctx context.Context, args []string) error {
	c := newCommand("put", "TITLE [-f FILE] [-m COMMENT] [-create] [-base REV]").withJSON()
	file := c.fs.String("f", "-", "read the page from `file` (- for stdin)")
	comment := c.fs.String("m", "", "edit comment")
	who := c.fs.String("author", "", "author (default $KB_AUTHOR, else $USER)")
	create := c.fs.Bool("create", false, "fail if the page exists")
	base := c.fs.Int64("base", 0, "fail unless the page is at this rev")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give one TITLE")
	}
	body, err := readInput(*file)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	mode := store.ModeUpsert
	if *create {
		mode = store.ModeCreate
	}
	r, err := st.PutPage(ctx, store.PutRequest{Title: pos[0], Body: body, Comment: *comment, Author: author(*who), BaseRev: *base, Mode: mode})
	if err != nil {
		return err
	}
	if c.json() {
		return printJSON(r)
	}
	printPut(r)
	return nil
}

func runEdit(ctx context.Context, args []string) error {
	c := newCommand("edit", "TITLE [-m COMMENT]")
	comment := c.fs.String("m", "", "edit comment")
	who := c.fs.String("author", "", "author")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give one TITLE")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	var body string
	var base int64
	title := pos[0]
	p, err := st.GetPage(ctx, title, store.GetOptions{Body: true})
	switch {
	case err == nil:
		body, base, title = p.Body, p.Rev, p.Title
	case store.IsNotFound(err):
		body = "# " + title + "\n\n"
	default:
		return err
	}
	tmp, err := os.CreateTemp("", "kb-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		return err
	}
	tmp.Close()
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", editor+` "$1"`, "sh", tmp.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor: %w", err)
	}
	nb, err := os.ReadFile(tmp.Name())
	if err != nil {
		return err
	}
	if string(nb) == body {
		fmt.Println("no change")
		return nil
	}
	mode := store.ModeReplace
	if base == 0 {
		mode = store.ModeCreate
	}
	r, err := st.PutPage(ctx, store.PutRequest{Title: title, Body: string(nb), BaseRev: base, Comment: *comment, Author: author(*who), Mode: mode})
	if err != nil {
		backup := filepath.Join(os.TempDir(), fmt.Sprintf("kb-edit-%d.md", time.Now().Unix()))
		_ = os.WriteFile(backup, nb, 0o600)
		return fmt.Errorf("%w (your text is saved in %s)", err, backup)
	}
	printPut(r)
	return nil
}

func runMove(ctx context.Context, args []string) error {
	c := newCommand("mv", "FROM TO [-no-redirect] [-no-rewrite]")
	noRedirect := c.fs.Bool("no-redirect", false, "do not leave a redirect at the old title")
	noRewrite := c.fs.Bool("no-rewrite", false, "do not rewrite links on other pages")
	comment := c.fs.String("m", "", "comment")
	who := c.fs.String("author", "", "author")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("give FROM and TO")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := st.MovePage(ctx, store.MoveRequest{From: pos[0], To: pos[1], LeaveRedirect: !*noRedirect, RewriteLinks: !*noRewrite, Comment: *comment, Author: author(*who)})
	if err != nil {
		return err
	}
	fmt.Printf("moved %q → %q (rev %d); redirect left: %v; %d links rewritten on %d pages\n", r.From, r.To, r.Rev, r.Redirect, r.LinksRewritten, len(r.PagesRewritten))
	return nil
}

func runDelete(ctx context.Context, args []string) error {
	c := newCommand("rm", "TITLE [-m COMMENT]")
	comment := c.fs.String("m", "", "comment")
	who := c.fs.String("author", "", "author")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give one TITLE")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeletePage(ctx, pos[0], *comment, author(*who), 0); err != nil {
		return err
	}
	fmt.Printf("deleted %q (kb restore undoes this)\n", pos[0])
	return nil
}

func runRestore(ctx context.Context, args []string) error {
	c := newCommand("restore", "TITLE")
	who := c.fs.String("author", "", "author")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give one TITLE")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := st.RestorePage(ctx, pos[0], "", author(*who))
	if err != nil {
		return err
	}
	printPut(r)
	return nil
}

func runHistory(ctx context.Context, args []string) error {
	c := newCommand("history", "TITLE [-n N]").withJSON()
	n := c.fs.Int("n", 50, "revisions to show")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give one TITLE")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	revs, err := st.History(ctx, pos[0], 0, *n)
	if err != nil {
		return err
	}
	if c.json() {
		return printJSON(revs)
	}
	table(func(w io.Writer) {
		fmt.Fprintln(w, "REV\tWHEN\tAUTHOR\tSIZE\tDELTA\tCOMMENT")
		for _, r := range revs {
			cur := ""
			if r.Current {
				cur = "*"
			}
			fmt.Fprintf(w, "%d%s\t%s\t%s\t%d\t%+d\t%s\n", r.ID, cur, when(r.CreatedAt), r.Author, r.Size, r.SizeDelta, oneLine(r.Comment, 60))
		}
	})
	return nil
}

func runDiff(ctx context.Context, args []string) error {
	c := newCommand("diff", "FROM_REV [TO_REV]")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageErr("give FROM_REV and optionally TO_REV")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	fromID, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return usageErr("FROM_REV must be a number")
	}
	a, err := st.GetRevision(ctx, fromID)
	if err != nil {
		return err
	}
	var b *store.Revision
	if len(pos) == 2 {
		toID, err := strconv.ParseInt(pos[1], 10, 64)
		if err != nil {
			return usageErr("TO_REV must be a number")
		}
		if b, err = st.GetRevision(ctx, toID); err != nil {
			return err
		}
	} else {
		h, err := st.History(ctx, a.Title, 0, 1)
		if err != nil {
			return err
		}
		if b, err = st.GetRevision(ctx, h[0].ID); err != nil {
			return err
		}
	}
	fmt.Print(wikitext.UnifiedDiff(fmt.Sprintf("rev %d", a.ID), fmt.Sprintf("rev %d", b.ID), a.Body, b.Body, 3))
	return nil
}

func listFilters(c *command) func() (store.PageFilter, error) {
	cat := c.fs.String("category", "", "only this category")
	status := c.fs.String("status", "", "only this status")
	prefix := c.fs.String("prefix", "", "only titles with this prefix")
	since := c.fs.String("since", "", "only pages edited since (e.g. 24h, 2026-01-02)")
	return func() (store.PageFilter, error) {
		f := store.PageFilter{Category: *cat, Status: *status, Prefix: *prefix}
		if *since != "" {
			t, err := parseSince(*since)
			if err != nil {
				return f, err
			}
			f.UpdatedSince = t
		}
		return f, nil
	}
}

func parseSince(s string) (int64, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	mult := time.Duration(1)
	if d, ok := strings.CutSuffix(s, "d"); ok {
		s, mult = d+"h", 24
	}
	d, err := time.ParseDuration(strings.TrimPrefix(s, "-"))
	if err != nil {
		return 0, usageErr("cannot read %q as a time (use 24h, 7d or 2026-01-02)", s)
	}
	return time.Now().Add(-d * mult).UnixMilli(), nil
}

func printSummaries(list []store.PageSummary, total int, search bool) {
	table(func(w io.Writer) {
		if search {
			fmt.Fprintln(w, "TITLE\tWORDS\tEDITED\tSNIPPET")
		} else {
			fmt.Fprintln(w, "TITLE\tSTATUS\tWORDS\tLINKS IN\tEDITED\tBY")
		}
		for _, p := range list {
			if search {
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", p.Title, p.Words, when(p.UpdatedAt), oneLine(p.Snippet, 80))
			} else {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n", p.Title, p.Status, p.Words, p.Backlinks, when(p.UpdatedAt), p.UpdatedBy)
			}
		}
	})
	if total > len(list) {
		fmt.Fprintf(os.Stderr, "(%d of %d; use -n and -offset for more)\n", len(list), total)
	}
}

func runList(ctx context.Context, args []string) error {
	c := newCommand("ls", "[-sort title|updated|created|size|words|backlinks] [filters]").withJSON()
	sortBy := c.fs.String("sort", "title", "sort order")
	n := c.fs.Int("n", 100, "max results")
	off := c.fs.Int("offset", 0, "skip this many")
	filters := listFilters(c)
	if _, err := c.parse(args); err != nil {
		return err
	}
	f, err := filters()
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	list, total, err := st.ListPages(ctx, f, *sortBy, nil, *n, *off)
	if err != nil {
		return err
	}
	if c.json() {
		return printJSON(map[string]any{"total": total, "pages": list})
	}
	printSummaries(list, total, false)
	return nil
}

func runSearch(ctx context.Context, args []string) error {
	c := newCommand("search", "QUERY [filters]").withJSON()
	n := c.fs.Int("n", 20, "max results")
	off := c.fs.Int("offset", 0, "skip this many")
	sortBy := c.fs.String("sort", "", "relevance (default), updated, created, title, size, words, backlinks")
	filters := listFilters(c)
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("give a QUERY")
	}
	f, err := filters()
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	hits, total, err := st.Search(ctx, strings.Join(pos, " "), f, *sortBy, nil, *n, *off)
	if err != nil {
		return err
	}
	if c.json() {
		return printJSON(map[string]any{"total": total, "results": hits})
	}
	if total == 0 {
		if sug, _ := st.Suggest(ctx, strings.Join(pos, " "), 5); len(sug) > 0 {
			fmt.Printf("no results; did you mean: %s?\n", strings.Join(sug, ", "))
			return nil
		}
	}
	printSummaries(hits, total, true)
	return nil
}

func runSpecial(ctx context.Context, args []string) error {
	c := newCommand("special", "KIND ("+strings.Join(store.SpecialKinds, ", ")+")").withJSON()
	n := c.fs.Int("n", 50, "max results")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give a KIND: %s", strings.Join(store.SpecialKinds, ", "))
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	rows, total, err := st.Special(ctx, pos[0], *n, 0)
	if err != nil {
		return err
	}
	if c.json() {
		return printJSON(map[string]any{"total": total, "entries": rows})
	}
	table(func(w io.Writer) {
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%d\t%s\n", r.Title, r.Count, r.Note)
		}
	})
	fmt.Fprintf(os.Stderr, "(%d total)\n", total)
	return nil
}

func runRecent(ctx context.Context, args []string) error {
	c := newCommand("recent", "[-since 24h] [-author A] [-kind K]").withJSON()
	since := c.fs.String("since", "", "only changes since")
	who := c.fs.String("author", "", "only by this author")
	kind := c.fs.String("kind", "", "only this kind (page, file, task, note, page.edit, ...)")
	n := c.fs.Int("n", 50, "max results")
	if _, err := c.parse(args); err != nil {
		return err
	}
	var t int64
	if *since != "" {
		var err error
		if t, err = parseSince(*since); err != nil {
			return err
		}
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ch, err := st.Changes(ctx, store.ChangeFilter{Since: t, Author: *who, Kind: *kind, Limit: *n})
	if err != nil {
		return err
	}
	if c.json() {
		return printJSON(ch)
	}
	table(func(w io.Writer) {
		fmt.Fprintln(w, "ID\tWHEN\tKIND\tTARGET\tAUTHOR\tCOMMENT")
		for _, x := range ch {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", x.ID, when(x.At), x.Kind, x.Target, x.Author, oneLine(x.Comment, 50))
		}
	})
	return nil
}

func runFile(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("give put, get, ls or rm")
	}
	sub, args := args[0], args[1:]
	c := newCommand("file "+sub, "...").withJSON()
	name := c.fs.String("name", "", "file name in the kb (put: default the path's base name)")
	desc := c.fs.String("desc", "", "description (put)")
	out := c.fs.String("o", "", "write to `path` instead of stdout (get)")
	who := c.fs.String("author", "", "author")
	replace := c.fs.Bool("replace", false, "replace an existing file (put)")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "put", "add":
		if len(pos) != 1 {
			return usageErr("give one PATH")
		}
		data, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		n := *name
		if n == "" {
			n = filepath.Base(pos[0])
		}
		var base int64
		if *replace {
			if f, _, err := st.GetFile(ctx, n, false, 0); err == nil {
				base = f.Rev
			}
		}
		r, err := st.PutFile(ctx, store.PutFileRequest{Name: n, Data: data, Description: *desc, Author: author(*who), BaseRev: base, MaxBytes: 1 << 40})
		if err != nil {
			return err
		}
		if c.json() {
			return printJSON(r)
		}
		fmt.Printf("%s rev %d, %d bytes, %s\n  embed with: %s\n", r.Name, r.Rev, r.Size, r.MIME, r.Markdown)
	case "get", "cat":
		if len(pos) != 1 {
			return usageErr("give one NAME")
		}
		f, data, err := st.GetFile(ctx, pos[0], !c.json(), 50)
		if err != nil {
			return err
		}
		if c.json() {
			return printJSON(f)
		}
		if *out != "" {
			return os.WriteFile(*out, data, 0o644)
		}
		_, err = os.Stdout.Write(data)
		return err
	case "ls", "list":
		files, total, err := st.ListFiles(ctx, store.FileFilter{Prefix: *name, Limit: 500})
		if err != nil {
			return err
		}
		if c.json() {
			return printJSON(map[string]any{"total": total, "files": files})
		}
		table(func(w io.Writer) {
			fmt.Fprintln(w, "NAME\tTYPE\tSIZE\tUSED BY\tUPDATED\tDESCRIPTION")
			for _, f := range files {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n", f.Name, f.MIME, f.Size, f.UsedByN, when(f.UpdatedAt), oneLine(f.Description, 40))
			}
		})
	case "rm", "delete":
		if len(pos) != 1 {
			return usageErr("give one NAME")
		}
		if err := st.DeleteFile(ctx, pos[0], "", author(*who)); err != nil {
			return err
		}
		fmt.Printf("deleted file %q\n", pos[0])
	default:
		return usageErr("unknown file command %q (put, get, ls, rm)", sub)
	}
	return nil
}

func runTask(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	sub, args := args[0], args[1:]
	c := newCommand("task "+sub, "...").withJSON()
	all := c.fs.Bool("all", false, "include finished tasks (ls)")
	prio := c.fs.Int("p", 0, "priority (add)")
	parent := c.fs.Int64("parent", 0, "parent task id (add, claim)")
	body := c.fs.String("body", "", "details (add)")
	page := c.fs.String("page", "", "page the task is about (add)")
	who := c.fs.String("author", "", "author / assignee")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "ls", "list":
		f := store.TaskFilter{Limit: 500}
		if *all {
			f.Status = []string{"all"}
		}
		list, _, err := st.ListTasks(ctx, f)
		if err != nil {
			return err
		}
		if c.json() {
			return printJSON(list)
		}
		table(func(w io.Writer) {
			fmt.Fprintln(w, "ID\tPARENT\tSTATUS\tPRIO\tASSIGNEE\tTITLE")
			for _, t := range list {
				p := ""
				if t.ParentID != 0 {
					p = strconv.FormatInt(t.ParentID, 10)
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\n", t.ID, p, t.Status, t.Priority, t.Assignee, t.Title)
			}
		})
	case "add":
		if len(pos) == 0 {
			return usageErr("give a TITLE")
		}
		ts, err := st.CreateTasks(ctx, []store.NewTask{{Title: strings.Join(pos, " "), Body: *body, Priority: *prio, ParentID: *parent, Page: *page, Author: author(*who)}})
		if err != nil {
			return err
		}
		fmt.Printf("task %d created\n", ts[0].ID)
	case "done", "drop", "block", "todo", "doing":
		if len(pos) != 1 {
			return usageErr("give a task ID")
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return usageErr("task ID must be a number")
		}
		status := map[string]string{"done": "done", "drop": "dropped", "block": "blocked", "todo": "todo", "doing": "doing"}[sub]
		t, err := st.UpdateTask(ctx, store.TaskUpdate{ID: id, Status: &status, Author: author(*who)})
		if err != nil {
			return err
		}
		fmt.Printf("task %d: %s\n", t.ID, t.Status)
	case "claim":
		t, err := st.ClaimNext(ctx, author(*who), *parent, 0)
		if err != nil {
			return err
		}
		if t == nil {
			fmt.Println("nothing to claim")
			return nil
		}
		if c.json() {
			return printJSON(t)
		}
		fmt.Printf("claimed task %d: %s\n", t.ID, t.Title)
	default:
		return usageErr("unknown task command %q (ls, add, done, drop, block, todo, doing, claim)", sub)
	}
	return nil
}

func runNote(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	sub, args := args[0], args[1:]
	c := newCommand("note "+sub, "...").withJSON()
	topic := c.fs.String("topic", "", "topic")
	page := c.fs.String("page", "", "attach to / filter by page")
	q := c.fs.String("q", "", "full-text query (ls)")
	n := c.fs.Int("n", 20, "max results (ls)")
	who := c.fs.String("author", "", "author")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "add":
		text := strings.Join(pos, " ")
		if text == "" || text == "-" {
			if text, err = readInput("-"); err != nil {
				return err
			}
		}
		nt, err := st.WriteNote(ctx, store.NoteWrite{Topic: *topic, Body: text, Page: *page, Author: author(*who)})
		if err != nil {
			return err
		}
		fmt.Printf("note %d saved\n", nt.ID)
	case "ls", "list":
		notes, _, err := st.ListNotes(ctx, store.NoteFilter{Topic: *topic, Page: *page, Query: *q, Limit: *n})
		if err != nil {
			return err
		}
		if c.json() {
			return printJSON(notes)
		}
		for _, x := range notes {
			fmt.Printf("#%d [%s] %s %s\n%s\n\n", x.ID, x.Topic, x.Author, when(x.UpdatedAt), x.Body)
		}
	default:
		return usageErr("unknown note command %q (add, ls)", sub)
	}
	return nil
}

func runExport(ctx context.Context, args []string) error {
	c := newCommand("export", "DIR [-portable]")
	portable := c.fs.Bool("portable", false, "rewrite kb: links as relative paths, for reading in an editor (not re-importable)")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give a DIR")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	m, err := exportfs.Export(ctx, st, pos[0], exportfs.Options{Portable: *portable})
	if err != nil {
		return err
	}
	fmt.Printf("exported %d pages and %d files to %s\n", len(m.Pages), len(m.Files), pos[0])
	return nil
}

func runImport(ctx context.Context, args []string) error {
	c := newCommand("import", "DIR")
	who := c.fs.String("author", "", "author")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give a DIR")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := exportfs.Import(ctx, st, pos[0], author(*who), nil)
	if err != nil {
		return err
	}
	fmt.Printf("pages: %d created, %d updated, %d unchanged; files: %d created, %d updated, %d unchanged\n",
		r.Created, r.Updated, r.Unchanged, r.FilesCreated, r.FilesUpdated, r.FilesUnchanged)
	for _, e := range r.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("%d items failed", len(r.Errors))
	}
	return nil
}

func runDB(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("give migrate, check, vacuum, stats, version or backup DEST")
	}
	sub, args := args[0], args[1:]
	c := newCommand("db "+sub, "...").withJSON()
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	st, err := c.open() // opening migrates
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "migrate":
		v, _ := st.Version(ctx)
		fmt.Printf("%s is at schema version %d\n", st.Path(), v)
	case "version":
		v, _ := st.Version(ctx)
		fmt.Printf("database %d, binary %d\n", v, store.SchemaVersion())
	case "check":
		probs, err := st.Check(ctx)
		if err != nil {
			return err
		}
		if len(probs) == 0 {
			fmt.Println("ok")
			return nil
		}
		for _, p := range probs {
			fmt.Println(p)
		}
		return fmt.Errorf("%d problems", len(probs))
	case "vacuum":
		n, err := st.Vacuum(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("vacuumed; %d unreferenced blobs removed\n", n)
	case "stats":
		s, err := st.Stats(ctx)
		if err != nil {
			return err
		}
		return printJSON(s)
	case "backup":
		if len(pos) != 1 {
			return usageErr("give a DEST path")
		}
		if err := st.Backup(ctx, pos[0]); err != nil {
			return err
		}
		fmt.Printf("backed up to %s (integrity ok)\n", pos[0])
	default:
		return usageErr("unknown db command %q", sub)
	}
	return nil
}

func runFeedback(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	sub, args := args[0], args[1:]
	c := newCommand("feedback "+sub, "...").withJSON()
	all := c.fs.Bool("all", false, "include resolved feedback (ls)")
	tool := c.fs.String("tool", "", "only about this tool (ls)")
	kind := c.fs.String("kind", "", "only this kind (ls)")
	note := c.fs.String("m", "", "resolution note (resolve)")
	wontfix := c.fs.Bool("wontfix", false, "close as won't fix (resolve)")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "ls", "list":
		f := store.FeedbackFilter{Tool: *tool, Kind: *kind, Limit: 500}
		if *all {
			f.Status = "all"
		}
		list, total, err := st.ListFeedback(ctx, f)
		if err != nil {
			return err
		}
		if c.json() {
			return printJSON(map[string]any{"total": total, "feedback": list})
		}
		for _, x := range list {
			fmt.Printf("#%d %s [%s] %s %s by %s\n  %s\n", x.ID, when(x.At), x.Status, x.Kind, x.Tool, x.Author, x.Message)
			if x.Details != "" {
				fmt.Printf("  details: %s\n", oneLine(x.Details, 300))
			}
			if x.Resolution != "" {
				fmt.Printf("  resolution: %s\n", x.Resolution)
			}
		}
		if total == 0 {
			fmt.Println("no feedback")
		}
	case "resolve":
		if len(pos) != 1 {
			return usageErr("give a feedback ID")
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return usageErr("ID must be a number")
		}
		status := "resolved"
		if *wontfix {
			status = "wontfix"
		}
		if err := st.ResolveFeedback(ctx, id, status, *note); err != nil {
			return err
		}
		fmt.Printf("feedback %d: %s\n", id, status)
	default:
		return usageErr("unknown feedback command %q (ls, resolve)", sub)
	}
	return nil
}
