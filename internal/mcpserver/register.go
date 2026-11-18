package mcpserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xen0bit/kb/internal/store"
	"github.com/xen0bit/kb/internal/wikitext"
)

const (
	defaultReadBytes = 40000
	maxReadBytes     = 400000
	maxBulkPages     = 50
	maxBulkBytes     = 8 << 20
	maxInlineFile    = 8 << 20 // largest file get_file returns inline
)

func (t *tools) register(s *mcp.Server) {
	t.registerFind(s)
	t.registerRead(s)
	t.registerWrite(s)
	t.registerFiles(s)
	t.registerWork(s)
}

// --- orient and find --------------------------------------------------------

func (t *tools) registerFind(s *mcp.Server) {
	addTool(s, t, &mcp.Tool{
		Name:        "kb_overview",
		Description: "Start here. The state of the kb in one call: page, file, link and revision counts; pages by status; the latest changes (with the id to resume recent_changes from); the most-wanted missing pages; open tasks; recent notes and note topics; the biggest categories; and the conventions for writing pages (link syntax, frontmatter, redirects).",
		Annotations: readOnly("Overview"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ overviewArgs) ([]mcp.Content, error) {
		st, err := t.st.Stats(ctx)
		if err != nil {
			return nil, err
		}
		changes, err := t.st.Changes(ctx, store.ChangeFilter{Limit: 10})
		if err != nil {
			return nil, err
		}
		wanted, _, err := t.st.Special(ctx, "wanted", 10, 0)
		if err != nil {
			return nil, err
		}
		tasks, openTasks, err := t.st.ListTasks(ctx, store.TaskFilter{Limit: 10})
		if err != nil {
			return nil, err
		}
		for i := range tasks {
			tasks[i].Body = firstLine(tasks[i].Body, 160)
		}
		notes, _, err := t.st.ListNotes(ctx, store.NoteFilter{Limit: 5})
		if err != nil {
			return nil, err
		}
		for i := range notes {
			notes[i].Body = firstLine(notes[i].Body, 300)
		}
		topics, err := t.st.NoteTopics(ctx, 10)
		if err != nil {
			return nil, err
		}
		cats, _, err := t.st.Categories(ctx, "", 10, 0)
		if err != nil {
			return nil, err
		}
		out := map[string]any{
			"stats": st, "recent_changes": changes, "wanted_pages": wanted, "open_tasks": tasks,
			"open_task_count": openTasks, "recent_notes": notes, "note_topics": topics, "top_categories": cats,
		}
		if st.Pages == 0 {
			out["hint"] = "The kb is empty. Plan the work as tasks (task_create), then write pages; red links you leave become the wanted list."
		}
		return text(toJSON(out) + "\n\n" + conventions), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "search",
		Description: "Full-text search over page titles, aliases and text, ranked by BM25 with title and alias matches weighted highest. Returns titles with highlighted snippets, revs, sizes, backlink counts and edit times. Combine with filters (category, status, prefix, edit time, last author) and sort by relevance, updated, created, title, size, words or backlinks. Paginate with offset.",
		Annotations: readOnly("Search pages"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a searchArgs) ([]mcp.Content, error) {
		f, err := a.filter()
		if err != nil {
			return nil, err
		}
		hits, total, err := t.st.Search(ctx, a.Query, f, a.Sort, a.Desc, a.Limit, a.Offset)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"results": hits, "page": paging(total, a.Offset, len(hits))}
		if total == 0 {
			if sug, _ := t.st.Suggest(ctx, a.Query, 5); len(sug) > 0 {
				res["did_you_mean"] = sug
			}
		}
		return jsonText(res), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "list_pages",
		Description: "List pages, by default A→Z. Sort by updated to see the latest edits, created for newest pages, size/words for the longest or shortest, backlinks for the most linked-to. Filter by category, status, title prefix, edit time, last author and size. Redirects are excluded unless include_redirects is set. Each entry has the page's lead (first paragraph) so a listing doubles as a summary.",
		Annotations: readOnly("List pages"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a listArgs) ([]mcp.Content, error) {
		f, err := a.filter()
		if err != nil {
			return nil, err
		}
		f.MinSize, f.MaxSize = a.MinSize, a.MaxSize
		list, total, err := t.st.ListPages(ctx, f, a.Sort, a.Desc, a.Limit, a.Offset)
		if err != nil {
			return nil, err
		}
		for i := range list {
			list[i].Lead = firstLine(list[i].Lead, 200)
		}
		return jsonText(map[string]any{"pages": list, "page": paging(total, a.Offset, len(list))}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "resolve_titles",
		Description: "Look up many titles at once (up to 200): for each, whether it exists, is a redirect (and to what), is an alias of another page, was deleted, or is missing (with how many pages already link to it and near-match suggestions). Each result carries the kb: link destination to use. Use it before linking, and to check whether articles you plan to write already exist.",
		Annotations: readOnly("Resolve titles"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a resolveArgs) ([]mcp.Content, error) {
		if len(a.Titles) == 0 || len(a.Titles) > 200 {
			return nil, fmt.Errorf("%w: give 1 to 200 titles", store.ErrInvalid)
		}
		n := a.Suggestions
		if n == 0 {
			n = 3
		}
		res, err := t.st.Resolve(ctx, a.Titles, min(max(n, 0), 10))
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"titles": res}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "special_pages",
		Description: "Maintenance reports, like Wikipedia's special pages. wanted: titles that pages link to but nobody has written, ranked by inbound links (the natural to-do list for growing the kb). most_linked; orphans: pages nothing links to; dead_ends: pages that link nowhere; stubs: pages with status stub, most-linked first; uncategorized; short/long: by size; redirects; broken_redirects; missing_files: embedded but never uploaded; unused_files.",
		Annotations: readOnly("Special pages"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a specialArgs) ([]mcp.Content, error) {
		rows, total, err := t.st.Special(ctx, a.Kind, a.Limit, a.Offset)
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"kind": a.Kind, "entries": rows, "page": paging(total, a.Offset, len(rows))}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "list_categories",
		Description: "List categories with how many pages are in each, largest first. A category exists once a page's frontmatter names it; list its pages with list_pages category=...",
		Annotations: readOnly("List categories"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a categoriesArgs) ([]mcp.Content, error) {
		cats, total, err := t.st.Categories(ctx, a.Prefix, a.Limit, a.Offset)
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"categories": cats, "page": paging(total, a.Offset, len(cats))}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "recent_changes",
		Description: "The change feed: every page create/edit/move/delete/revert, file upload, task change and note, with author, comment, rev and size change. Newest first by default. To resume where you left off, pass after_id (the last change id you saw; kb_overview reports the latest) and get everything after it oldest first. Filter by time, author, kind or target.",
		Annotations: readOnly("Recent changes"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a changesArgs) ([]mcp.Content, error) {
		since, err := parseTime(a.Since, time.Now())
		if err != nil {
			return nil, err
		}
		ch, err := t.st.Changes(ctx, store.ChangeFilter{AfterID: a.AfterID, BeforeID: a.BeforeID, Since: since, Author: a.Author, Kind: a.Kind, Target: a.Target, Limit: a.Limit})
		if err != nil {
			return nil, err
		}
		res := map[string]any{"changes": ch}
		if n := len(ch); n > 0 {
			if a.AfterID > 0 {
				res["resume_after_id"] = ch[n-1].ID
			} else {
				res["older_before_id"] = ch[n-1].ID
			}
		}
		return jsonText(res), nil
	})
}

// --- read --------------------------------------------------------------------

type pageHeader struct {
	Title          string         `json:"title"`
	Rev            int64          `json:"rev"`
	Current        bool           `json:"current"`
	RedirectedFrom string         `json:"redirected_from,omitempty"`
	ViaAlias       string         `json:"via_alias,omitempty"`
	RedirectTo     string         `json:"redirect_to,omitempty"`
	Status         string         `json:"status,omitempty"`
	Categories     []string       `json:"categories,omitempty"`
	Aliases        []string       `json:"aliases,omitempty"`
	Props          map[string]any `json:"props,omitempty"`
	UpdatedAt      int64          `json:"updated_at"`
	UpdatedBy      string         `json:"updated_by,omitempty"`
	Size           int            `json:"size"`
	Words          int            `json:"words,omitempty"`
	LinksOut       int            `json:"links_out"`
	Backlinks      int            `json:"backlinks"`
	TotalLines     int            `json:"total_lines"`
	Showing        string         `json:"showing,omitempty"`
	Continue       string         `json:"continue,omitempty"`
}

func (t *tools) registerRead(s *mcp.Server) {
	addTool(s, t, &mcp.Tool{
		Name:        "read_page",
		Description: "Read a page: a one-line JSON header (title, rev, status, categories, aliases, frontmatter props, last edit, size, link counts, line count) followed by the markdown. Follows redirects and aliases and says so. Long pages: read outline=true first, then one section at a time (section=...) or a line range; output is capped at max_bytes and the header says where to continue. Pass rev to read an old revision.",
		Annotations: readOnly("Read page"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a readArgs) ([]mcp.Content, error) {
		return t.readPage(ctx, a)
	})

	addTool(s, t, &mcp.Tool{
		Name:        "page_links",
		Description: "A page's links. direction=out (default): every page it links to, with whether the target exists. in: every page linking to it (by title or alias), i.e. backlinks / what links here. red: only its links to pages that do not exist yet.",
		Annotations: readOnly("Page links"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a linksArgs) ([]mcp.Content, error) {
		links, total, err := t.st.PageLinks(ctx, a.Title, a.Direction, a.Limit, a.Offset)
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"title": a.Title, "direction": orDefault(a.Direction, "out"), "links": links, "page": paging(total, a.Offset, len(links))}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "page_history",
		Description: "A page's revisions, newest first: rev, author, comment, time, size and size change. Read one with read_page rev=..., compare with diff_revisions, restore with revert_page.",
		Annotations: readOnly("Page history"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a historyArgs) ([]mcp.Content, error) {
		revs, err := t.st.History(ctx, a.Title, a.BeforeRev, a.Limit)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"title": a.Title, "revisions": revs}
		if len(revs) > 0 && revs[len(revs)-1].ParentID != 0 {
			res["older_before_rev"] = revs[len(revs)-1].ID
		}
		return jsonText(res), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "diff_revisions",
		Description: "Unified diff between two revisions of a page. Give title alone to see what the latest edit changed, from_rev alone to compare it with the current text, or both revs.",
		Annotations: readOnly("Diff revisions"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a diffArgs) ([]mcp.Content, error) {
		return t.diff(ctx, a)
	})
}

func (t *tools) readPage(ctx context.Context, a readArgs) ([]mcp.Content, error) {
	var h pageHeader
	var body string
	if a.Rev > 0 {
		r, err := t.st.GetRevision(ctx, a.Rev)
		if err != nil {
			return nil, err
		}
		if a.Title != "" && wikitext.Key(a.Title) != wikitext.Key(r.Title) {
			cur, err := t.st.GetPage(ctx, a.Title, store.GetOptions{IncludeDeleted: true})
			if err != nil || cur.ID != r.PageID {
				return nil, fmt.Errorf("%w: rev %d belongs to %q, not %q", store.ErrInvalid, a.Rev, r.Title, a.Title)
			}
		}
		body = r.Body
		h = pageHeader{Title: r.Title, Rev: r.ID, Current: r.Current, UpdatedAt: r.CreatedAt, UpdatedBy: r.Author, Size: r.Size}
		if m, err := wikitext.ParseMeta(body); err == nil {
			h.Status, h.Categories, h.Aliases, h.Props, h.RedirectTo = m.Status, m.Categories, m.Aliases, m.Props, m.Redirect
		}
		out, in, _ := t.st.LinkCounts(ctx, r.PageID)
		h.LinksOut, h.Backlinks = out, in
	} else {
		if a.Title == "" {
			return nil, fmt.Errorf("%w: give title or rev", store.ErrInvalid)
		}
		p, err := t.st.GetPage(ctx, a.Title, store.GetOptions{Body: true, FollowRedirect: !a.NoRedirect})
		if err != nil {
			if sug, _ := t.st.Suggest(ctx, a.Title, 5); len(sug) > 0 {
				return nil, fmt.Errorf("%w; did you mean: %s?", err, strings.Join(sug, ", "))
			}
			return nil, err
		}
		body = p.Body
		h = pageHeader{Title: p.Title, Rev: p.Rev, Current: true, RedirectedFrom: p.RedirectedFrom, ViaAlias: p.ViaAlias,
			RedirectTo: p.RedirectTo, Status: p.Status, Categories: p.Categories, Aliases: p.Aliases, Props: p.Props,
			UpdatedAt: p.UpdatedAt, UpdatedBy: p.UpdatedBy, Size: p.Size, Words: p.Words}
		out, in, _ := t.st.LinkCounts(ctx, p.ID)
		h.LinksOut, h.Backlinks = out, in
	}
	lines := wikitext.Lines(body)
	h.TotalLines = len(lines)
	if a.Outline {
		return text(toJSON(h) + "\n\n" + toJSON(map[string]any{"sections": wikitext.Outline(body)})), nil
	}
	start, end := 1, len(lines)
	switch {
	case a.Section != "":
		sec, err := wikitext.FindSection(body, a.Section)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
		}
		start, end = sec.Start, sec.End
		h.Showing = fmt.Sprintf("section %d %q", sec.Index, sec.Path)
	case a.StartLine > 0 || a.EndLine > 0:
		if a.StartLine > 0 {
			start = a.StartLine
		}
		if a.EndLine > 0 {
			end = min(a.EndLine, len(lines))
		}
	}
	maxB := a.MaxBytes
	if maxB <= 0 {
		maxB = defaultReadBytes
	}
	maxB = min(maxB, maxReadBytes)
	chunk := wikitext.SliceLines(body, start, end)
	kept, n, truncated := truncateBytes(chunk, maxB)
	shownEnd := end
	if truncated {
		shownEnd = start + n - 1
		h.Continue = fmt.Sprintf("truncated at max_bytes=%d: continue with start_line=%d", maxB, shownEnd+1)
		if a.Section == "" && a.StartLine == 0 {
			h.Continue += " (or read outline=true and fetch sections)"
		}
	}
	if start != 1 || shownEnd != len(lines) || h.Showing != "" {
		r := fmt.Sprintf("lines %d-%d of %d", start, shownEnd, len(lines))
		if h.Showing != "" {
			r = h.Showing + ", " + r
		}
		h.Showing = r
	}
	return text(toJSON(h) + "\n\n" + kept), nil
}

func (t *tools) diff(ctx context.Context, a diffArgs) ([]mcp.Content, error) {
	ctxLines := a.Context
	if ctxLines <= 0 {
		ctxLines = 3
	}
	var from, to *store.Revision
	var err error
	switch {
	case a.FromRev > 0:
		if from, err = t.st.GetRevision(ctx, a.FromRev); err != nil {
			return nil, err
		}
		if a.ToRev > 0 {
			to, err = t.st.GetRevision(ctx, a.ToRev)
		} else {
			hist, herr := t.st.History(ctx, from.Title, 0, 1)
			if herr != nil {
				// The page may have been renamed since; fall back to its id.
				return nil, herr
			}
			to, err = t.st.GetRevision(ctx, hist[0].ID)
		}
	case a.Title != "":
		hist, herr := t.st.History(ctx, a.Title, 0, 2)
		if herr != nil {
			return nil, herr
		}
		if len(hist) < 2 {
			return text(fmt.Sprintf("%q has only one revision (rev %d): nothing to compare", a.Title, hist[0].ID)), nil
		}
		if to, err = t.st.GetRevision(ctx, hist[0].ID); err == nil {
			from, err = t.st.GetRevision(ctx, hist[1].ID)
		}
	default:
		return nil, fmt.Errorf("%w: give title, or from_rev (and optionally to_rev)", store.ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	if from.PageID != to.PageID {
		return nil, fmt.Errorf("%w: rev %d and rev %d belong to different pages", store.ErrInvalid, from.ID, to.ID)
	}
	d := wikitext.UnifiedDiff(fmt.Sprintf("rev %d (%s, %s)", from.ID, orDefault(from.Author, "?"), time.UnixMilli(from.CreatedAt).UTC().Format(time.RFC3339)),
		fmt.Sprintf("rev %d (%s, %s)", to.ID, orDefault(to.Author, "?"), time.UnixMilli(to.CreatedAt).UTC().Format(time.RFC3339)),
		from.Body, to.Body, ctxLines)
	if d == "" {
		d = fmt.Sprintf("rev %d and rev %d have identical text", from.ID, to.ID)
		if from.Title != to.Title {
			d += fmt.Sprintf(" (title changed: %q → %q)", from.Title, to.Title)
		}
	}
	kept, _, cut := truncateBytes(d, maxReadBytes)
	if cut {
		kept += "\n[diff truncated]\n"
	}
	return text(kept), nil
}

// --- write -------------------------------------------------------------------

func (t *tools) registerWrite(s *mcp.Server) {
	addTool(s, t, &mcp.Tool{
		Name:        "create_page",
		Description: "Create a new page from markdown (optionally with YAML frontmatter: categories, aliases, status, summary, redirect, or any infobox fields). Fails if the title exists, reporting its rev. The result lists the page's red_links (linked titles with no page yet) and missing_files, and warnings for links that did not parse.",
		Annotations: writes("Create page", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a createArgs) ([]mcp.Content, error) {
		r, err := t.st.PutPage(ctx, store.PutRequest{Title: a.Title, Body: a.Body, Comment: a.Comment, Author: t.author(req, a.Author), Mode: store.ModeCreate})
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "write_page",
		Description: "Replace a page's whole text. base_rev must be the rev you read: if the page changed since, this fails with a conflict naming the current rev and editor, and you should re-read and reapply. Prefer edit_page for partial changes. Writing identical text is a no-op.",
		Annotations: writes("Write page", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a writeArgs) ([]mcp.Content, error) {
		r, err := t.st.PutPage(ctx, store.PutRequest{Title: a.Title, Body: a.Body, BaseRev: a.BaseRev, Comment: a.Comment, Author: t.author(req, a.Author), Mode: store.ModeReplace})
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "edit_page",
		Description: "Change part of a page as one new revision, using exactly one of: edits (str_replace pairs, applied in order; each old_text must occur exactly once, so include enough context), section + section_text (replace a whole section, heading and subsections included; empty text deletes it), append (add at the end) or prepend (add at the top, after frontmatter). base_rev is optional: str_replace edits check themselves against the current text.",
		Annotations: writes("Edit page", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a editArgs) ([]mcp.Content, error) {
		r, err := t.st.EditPage(ctx, store.EditRequest{Title: a.Title, BaseRev: a.BaseRev, Edits: a.Edits, Section: a.Section, SectionText: a.SectionText,
			Append: a.Append, Prepend: a.Prepend, Comment: a.Comment, Author: t.author(req, a.Author)})
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "move_page",
		Description: "Rename a page, keeping its history. By default leaves a redirect at the old title and rewrites every other page's kb: links to the new title (each rewritten page gets a revision). Fails if the new title is taken by a real page.",
		Annotations: writes("Move page", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a moveArgs) ([]mcp.Content, error) {
		r, err := t.st.MovePage(ctx, store.MoveRequest{From: a.From, To: a.To, LeaveRedirect: boolOr(a.LeaveRedirect, true),
			RewriteLinks: boolOr(a.RewriteLinks, true), BaseRev: a.BaseRev, Comment: a.Comment, Author: t.author(req, a.Author)})
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "delete_page",
		Description: "Delete a page. Its history is kept and restore_page brings it back; while deleted it is gone from search, listings and link counts, and links to it are red again.",
		Annotations: writes("Delete page", true),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a deleteArgs) ([]mcp.Content, error) {
		if err := t.st.DeletePage(ctx, a.Title, a.Comment, t.author(req, a.Author), a.BaseRev); err != nil {
			return nil, err
		}
		return text(fmt.Sprintf("deleted %q (restore_page undoes this)", a.Title)), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "restore_page",
		Description: "Undelete a page deleted with delete_page, at the revision it had.",
		Annotations: writes("Restore page", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a restoreArgs) ([]mcp.Content, error) {
		r, err := t.st.RestorePage(ctx, a.Title, a.Comment, t.author(req, a.Author))
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "revert_page",
		Description: "Make an earlier revision's text the page's newest revision (history is kept; nothing is erased).",
		Annotations: writes("Revert page", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a revertArgs) ([]mcp.Content, error) {
		r, err := t.st.RevertPage(ctx, a.Title, a.ToRev, a.BaseRev, a.Comment, t.author(req, a.Author))
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "bulk_write",
		Description: fmt.Sprintf("Create or replace up to %d pages in one call; each page succeeds or fails on its own and the result reports each (rev, red_links, or error). mode per page: create (default), replace (needs base_rev) or upsert. The fastest way to write many short articles or stubs.", maxBulkPages),
		Annotations: writes("Bulk write", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a bulkArgs) ([]mcp.Content, error) {
		if len(a.Pages) == 0 || len(a.Pages) > maxBulkPages {
			return nil, fmt.Errorf("%w: give 1 to %d pages", store.ErrInvalid, maxBulkPages)
		}
		total := 0
		for _, p := range a.Pages {
			total += len(p.Body)
		}
		if total > maxBulkBytes {
			return nil, fmt.Errorf("%w: %d bytes in one call is over the %d-byte bulk limit; split it", store.ErrInvalid, total, maxBulkBytes)
		}
		author := t.author(req, a.Author)
		type item struct {
			Title  string           `json:"title"`
			OK     bool             `json:"ok"`
			Result *store.PutResult `json:"result,omitempty"`
			Error  string           `json:"error,omitempty"`
		}
		var out []item
		okN := 0
		for _, p := range a.Pages {
			mode := store.ModeCreate
			switch strings.ToLower(p.Mode) {
			case "", "create":
			case "replace":
				mode = store.ModeReplace
			case "upsert":
				mode = store.ModeUpsert
			default:
				out = append(out, item{Title: p.Title, Error: "mode must be create, replace or upsert"})
				continue
			}
			c := p.Comment
			if c == "" {
				c = a.Comment
			}
			r, err := t.st.PutPage(ctx, store.PutRequest{Title: p.Title, Body: p.Body, BaseRev: p.BaseRev, Comment: c, Author: author, Mode: mode})
			if err != nil {
				out = append(out, item{Title: p.Title, Error: err.Error()})
				continue
			}
			okN++
			out = append(out, item{Title: r.Title, OK: true, Result: r})
		}
		return jsonText(map[string]any{"written": okN, "failed": len(a.Pages) - okN, "pages": out}), nil
	})
}

// --- files -------------------------------------------------------------------

func (t *tools) registerFiles(s *mcp.Server) {
	addTool(s, t, &mcp.Tool{
		Name:        "upload_file",
		Description: "Upload an image or other file (base64). Embed it in a page with the returned markdown: ![alt](kb-file:Name.png) for images, [label](kb-file:Name.pdf) otherwise. Identical bytes are stored once. Replacing an existing file needs its base_rev; old versions are kept.",
		Annotations: writes("Upload file", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a uploadArgs) ([]mcp.Content, error) {
		data, mt, err := decodeBase64(a.Data)
		if err != nil {
			return nil, fmt.Errorf("%w: data_base64 is not valid base64: %v", store.ErrInvalid, err)
		}
		if a.MIME != "" {
			mt = a.MIME
		}
		r, err := t.st.PutFile(ctx, store.PutFileRequest{Name: a.Name, MIME: mt, Data: data, Description: a.Description,
			Comment: a.Comment, Author: t.author(req, a.Author), BaseRev: a.BaseRev, MaxBytes: t.maxFile})
		if err != nil {
			return nil, err
		}
		return jsonText(r), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "get_file",
		Description: "A file's metadata (type, size, hash, description, rev, which pages embed it). With content=true, also its contents: images as image content you can see, text formats as text, anything else as base64.",
		Annotations: readOnly("Get file"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a getFileArgs) ([]mcp.Content, error) {
		used := a.UsedBy
		if used == 0 {
			used = 20
		}
		f, data, err := t.st.GetFile(ctx, a.Name, a.Content, min(used, 500))
		if err != nil {
			return nil, err
		}
		meta := map[string]any{"file": f, "markdown": store.EmbedMarkdown(f.Name, f.MIME)}
		c := jsonText(meta)
		if !a.Content {
			return c, nil
		}
		if f.Size > maxInlineFile {
			return append(c, &mcp.TextContent{Text: fmt.Sprintf("[%d bytes is too large to return inline; the limit is %d]", f.Size, maxInlineFile)}), nil
		}
		switch {
		case strings.HasPrefix(f.MIME, "image/") && f.MIME != "image/svg+xml":
			c = append(c, &mcp.ImageContent{Data: data, MIMEType: f.MIME})
		case isTextMIME(f.MIME) && utf8.Valid(data):
			c = append(c, &mcp.TextContent{Text: string(data)})
		default:
			c = append(c, &mcp.TextContent{Text: "base64:" + base64.StdEncoding.EncodeToString(data)})
		}
		return c, nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "list_files",
		Description: "List uploaded files with type, size, description and how many pages use each. Filter by name prefix or content type (e.g. image/); sort by name, updated or size.",
		Annotations: readOnly("List files"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a listFilesArgs) ([]mcp.Content, error) {
		files, total, err := t.st.ListFiles(ctx, store.FileFilter{Prefix: a.Prefix, MIME: a.MIME, Sort: a.Sort, Desc: a.Desc, Limit: a.Limit, Offset: a.Offset})
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"files": files, "page": paging(total, a.Offset, len(files))}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file. Pages embedding it will show it as missing (special_pages kind=missing_files). Its revisions are kept.",
		Annotations: writes("Delete file", true),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a deleteFileArgs) ([]mcp.Content, error) {
		if err := t.st.DeleteFile(ctx, a.Name, a.Comment, t.author(req, a.Author)); err != nil {
			return nil, err
		}
		return text(fmt.Sprintf("deleted file %q", a.Name)), nil
	})
}

func isTextMIME(m string) bool {
	m, _, _ = strings.Cut(m, ";")
	return strings.HasPrefix(m, "text/") || m == "application/json" || m == "image/svg+xml" ||
		strings.HasSuffix(m, "+xml") || strings.HasSuffix(m, "+json") || m == "application/xml" ||
		m == "application/yaml" || m == "application/x-yaml" || m == "application/javascript"
}

// --- tasks and notes --------------------------------------------------------

func (t *tools) registerWork(s *mcp.Server) {
	addTool(s, t, &mcp.Tool{
		Name:        "task_create",
		Description: "Create tasks, up to 500 per call: a plan of work that outlives your context. Tasks nest (parent_id), carry a priority (higher first) and can name the page they are about. Several agents can share one backlog with task_claim_next.",
		Annotations: writes("Create tasks", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a taskCreateArgs) ([]mcp.Content, error) {
		if len(a.Tasks) > 500 {
			return nil, fmt.Errorf("%w: at most 500 tasks per call", store.ErrInvalid)
		}
		author := t.author(req, a.Author)
		in := make([]store.NewTask, len(a.Tasks))
		for i, nt := range a.Tasks {
			in[i] = store.NewTask{ParentID: nt.ParentID, Title: nt.Title, Body: nt.Body, Priority: nt.Priority, Page: nt.Page, Author: author}
		}
		out, err := t.st.CreateTasks(ctx, in)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, len(out))
		for i, tk := range out {
			ids[i] = tk.ID
		}
		if len(out) > 20 {
			return jsonText(map[string]any{"created": len(out), "ids": ids}), nil
		}
		return jsonText(map[string]any{"created": len(out), "tasks": out}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "task_update",
		Description: "Update a task: status (todo, doing, blocked, done, dropped), priority, assignee, title, body, parent, page; or append a line to its body as a work log. Marking it done or dropped releases its lease; updating it while doing renews nothing, so claim again or set status doing to extend.",
		Annotations: writes("Update task", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a taskUpdateArgs) ([]mcp.Content, error) {
		tk, err := t.st.UpdateTask(ctx, store.TaskUpdate{ID: a.ID, Status: a.Status, Priority: a.Priority, Assignee: a.Assignee, Title: a.Title,
			Body: a.Body, AppendBody: a.Append, ParentID: a.ParentID, Page: a.Page, Author: t.author(req, a.Author)})
		if err != nil {
			return nil, err
		}
		return jsonText(tk), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "task_list",
		Description: "List tasks, highest priority first; by default only open ones (todo, doing, blocked). Filter by status, assignee (or mine=true), parent, page or text. Each task shows its open subtask count. Pass id to read one task in full.",
		Annotations: readOnly("List tasks"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a taskListArgs) ([]mcp.Content, error) {
		if a.ID > 0 {
			tk, err := t.st.GetTask(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			return jsonText(tk), nil
		}
		assignee := a.Assignee
		if a.Mine {
			assignee = t.author(req, a.Author)
		}
		list, total, err := t.st.ListTasks(ctx, store.TaskFilter{Status: a.Status, Assignee: assignee, ParentID: a.ParentID, Page: a.Page,
			Query: a.Query, Sort: a.Sort, Limit: a.Limit, Offset: a.Offset})
		if err != nil {
			return nil, err
		}
		for i := range list {
			list[i].Body = firstLine(list[i].Body, 200)
		}
		return jsonText(map[string]any{"tasks": list, "page": paging(total, a.Offset, len(list))}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "task_claim_next",
		Description: "Atomically take the highest-priority open leaf task (todo, or doing with an expired lease), optionally anywhere under parent_id, marking it doing and assigned to you with a lease. Safe with many agents at once: no two get the same task. Returns nothing to do when the queue is empty.",
		Annotations: writes("Claim next task", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a claimArgs) ([]mcp.Content, error) {
		lease := time.Duration(a.LeaseMinutes) * time.Minute
		tk, err := t.st.ClaimNext(ctx, t.author(req, a.Author), a.ParentID, lease)
		if err != nil {
			return nil, err
		}
		if tk == nil {
			return text(`{"task":null,"note":"nothing to claim: no open leaf tasks (try special_pages kind=wanted for work)"}`), nil
		}
		return jsonText(map[string]any{"task": tk}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "note_write",
		Description: "Write a note: a progress log, plan, decisions, style guide, open questions, or a talk-page note on a page (page=...). Notes are searchable and survive across sessions. With id, replace a note's body or append to it (append=true): keep one running 'progress' note rather than many.",
		Annotations: writes("Write note", false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a noteWriteArgs) ([]mcp.Content, error) {
		n, err := t.st.WriteNote(ctx, store.NoteWrite{ID: a.ID, Topic: a.Topic, Body: a.Body, Append: a.Append, Page: a.Page, Author: t.author(req, a.Author)})
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"id": n.ID, "topic": n.Topic, "page": n.Page, "updated_at": n.UpdatedAt, "bytes": len(n.Body)}), nil
	})

	addTool(s, t, &mcp.Tool{
		Name:        "note_list",
		Description: "Read notes, newest first: filter by topic, page or author, or full-text search with query. topics=true lists the topics in use. Pass id to read one note.",
		Annotations: readOnly("List notes"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a noteListArgs) ([]mcp.Content, error) {
		if a.ID > 0 {
			n, err := t.st.GetNote(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			return jsonText(n), nil
		}
		if a.Topics {
			ts, err := t.st.NoteTopics(ctx, a.Limit)
			if err != nil {
				return nil, err
			}
			return jsonText(map[string]any{"topics": ts}), nil
		}
		notes, total, err := t.st.ListNotes(ctx, store.NoteFilter{Topic: a.Topic, Page: a.Page, Query: a.Query, Author: a.Author, Limit: a.Limit, Offset: a.Offset})
		if err != nil {
			return nil, err
		}
		return jsonText(map[string]any{"notes": notes, "page": paging(total, a.Offset, len(notes))}), nil
	})
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if utf8.RuneCountInString(s) > n {
		r := []rune(s)
		s = string(r[:n]) + "…"
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func boolOr(b *bool, d bool) bool {
	if b == nil {
		return d
	}
	return *b
}
