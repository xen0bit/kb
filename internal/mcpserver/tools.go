package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xen0bit/kb/internal/store"
	"github.com/xen0bit/kb/internal/wikitext"
)

type tools struct {
	st        *store.Store
	log       *slog.Logger
	maxFile   int64
	defAuthor string
}

// author picks who a write is attributed to: the call's author argument,
// else the client's declared name, else the server default.
func (t *tools) author(req *mcp.CallToolRequest, given string) string {
	if a := strings.TrimSpace(given); a != "" {
		return a
	}
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil && p.ClientInfo.Name != "" {
			return p.ClientInfo.Name
		}
	}
	if t.defAuthor != "" {
		return t.defAuthor
	}
	return "agent"
}

func text(s string) []mcp.Content { return []mcp.Content{&mcp.TextContent{Text: s}} }

func jsonText(v any) []mcp.Content { return text(toJSON(v)) }

var no = false

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &no}
}

func writes(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &destructive, OpenWorldHint: &no}
}

// addTool registers a handler that returns content blocks, adding the
// cross-cutting parts every tool needs: a log line per call, and a recover so
// a panic becomes a failed call rather than a dead HTTP server.
func addTool[In any](s *mcp.Server, t *tools, tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, In) ([]mcp.Content, error)) {
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out any, err error) {
		started := time.Now()
		defer func() {
			level, attrs := slog.LevelInfo, []any{"tool", tool.Name, "duration", time.Since(started).Round(time.Millisecond).String()}
			if err != nil {
				level, attrs = slog.LevelWarn, append(attrs, "error", err.Error())
			}
			t.log.Log(ctx, level, "tool call", attrs...)
		}()
		defer func() {
			if r := recover(); r != nil {
				res, out, err = nil, nil, fmt.Errorf("%s failed unexpectedly: %v", tool.Name, r)
			}
		}()
		c, err := h(ctx, req, in)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: c}, nil, nil
	})
}

// --- argument types ---------------------------------------------------------

type overviewArgs struct {
	Conventions bool `json:"conventions,omitempty" jsonschema:"include the page-writing conventions (link syntax, frontmatter, redirects); they are included anyway while the kb is empty"`
}

type filterArgs struct {
	Category         string `json:"category,omitempty" jsonschema:"only pages in this category"`
	Status           string `json:"status,omitempty" jsonschema:"only pages with this frontmatter status (e.g. stub, draft, good); 'none' for pages without one"`
	Prefix           string `json:"prefix,omitempty" jsonschema:"only titles starting with this (case-insensitive)"`
	UpdatedSince     string `json:"updated_since,omitempty" jsonschema:"only pages edited at or after this time: RFC 3339, a date, or a duration back from now such as 24h or 7d"`
	UpdatedBefore    string `json:"updated_before,omitempty" jsonschema:"only pages last edited before this time"`
	Author           string `json:"author,omitempty" jsonschema:"only pages last edited by this author"`
	IncludeRedirects bool   `json:"include_redirects,omitempty" jsonschema:"include redirect pages (excluded by default)"`
}

func (f filterArgs) filter() (store.PageFilter, error) {
	now := time.Now()
	since, err := parseTime(f.UpdatedSince, now)
	if err != nil {
		return store.PageFilter{}, err
	}
	before, err := parseTime(f.UpdatedBefore, now)
	if err != nil {
		return store.PageFilter{}, err
	}
	return store.PageFilter{Category: f.Category, Status: f.Status, Prefix: f.Prefix, UpdatedSince: since,
		UpdatedBefore: before, Author: f.Author, Redirects: f.IncludeRedirects}, nil
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"full-text query. Plain words match all of them (stemmed). FTS5 syntax is accepted: \"exact phrase\", prefix*, a OR b, a NOT b, title:word, NEAR(a b, 5)"`
	filterArgs
	Sort   string `json:"sort,omitempty" jsonschema:"relevance (default), updated, created, title, size, words or backlinks"`
	Desc   *bool  `json:"desc,omitempty" jsonschema:"sort descending; defaults to true for everything but title"`
	Limit  int    `json:"limit,omitempty" jsonschema:"results per call, default 20, max 200"`
	Offset int    `json:"offset,omitempty"`
}

type listArgs struct {
	filterArgs
	MinSize int    `json:"min_size,omitempty" jsonschema:"only pages of at least this many bytes"`
	MaxSize int    `json:"max_size,omitempty" jsonschema:"only pages of at most this many bytes"`
	Sort    string `json:"sort,omitempty" jsonschema:"title (default), updated, created, size, words or backlinks"`
	Desc    *bool  `json:"desc,omitempty" jsonschema:"sort descending; defaults to true for everything but title"`
	Limit   int    `json:"limit,omitempty" jsonschema:"results per call, default 50, max 500"`
	Offset  int    `json:"offset,omitempty"`
}

type resolveArgs struct {
	Titles      []string `json:"titles" jsonschema:"titles to look up, up to 200"`
	Suggestions int      `json:"suggestions,omitempty" jsonschema:"near-match suggestions per missing title, default 3, 0-10"`
}

type specialArgs struct {
	Kind   string `json:"kind" jsonschema:"wanted (missing pages ranked by inbound links), most_linked, orphans (nothing links here), dead_ends (no outbound links), stubs, uncategorized, short, long, redirects, broken_redirects, missing_files, unused_files"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 50, max 500"`
	Offset int    `json:"offset,omitempty"`
}

type categoriesArgs struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"only categories starting with this"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 100, max 1000"`
	Offset int    `json:"offset,omitempty"`
}

type readArgs struct {
	Title      string `json:"title,omitempty" jsonschema:"page title (underscores or spaces, any case). Give title or rev"`
	Rev        int64  `json:"rev,omitempty" jsonschema:"read this revision instead of the current one"`
	Outline    bool   `json:"outline,omitempty" jsonschema:"return only the section outline (headings, line ranges, sizes), to plan sectioned reads of a long page"`
	Section    string `json:"section,omitempty" jsonschema:"return one section (with its subsections): an index from the outline, a heading path such as History/Early life, or a unique heading title"`
	StartLine  int    `json:"start_line,omitempty" jsonschema:"first line to return (1-based)"`
	EndLine    int    `json:"end_line,omitempty" jsonschema:"last line to return (inclusive)"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"cap on returned text, default 40000; the response says where to continue"`
	NoRedirect bool   `json:"no_redirect,omitempty" jsonschema:"read a redirect page itself instead of following it"`
}

type linksArgs struct {
	Title     string `json:"title"`
	Direction string `json:"direction,omitempty" jsonschema:"out (default: this page's links, with whether each target exists), in (pages linking here) or red (this page's links to missing pages)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"default 100, max 1000"`
	Offset    int    `json:"offset,omitempty"`
}

type historyArgs struct {
	Title     string `json:"title"`
	BeforeRev int64  `json:"before_rev,omitempty" jsonschema:"page back: only revisions older than this"`
	Limit     int    `json:"limit,omitempty" jsonschema:"default 50, max 500"`
}

type diffArgs struct {
	Title   string `json:"title,omitempty" jsonschema:"with no revs: diff the page's current revision against the one before it"`
	FromRev int64  `json:"from_rev,omitempty" jsonschema:"older revision"`
	ToRev   int64  `json:"to_rev,omitempty" jsonschema:"newer revision; defaults to the page's current revision"`
	Context int    `json:"context,omitempty" jsonschema:"lines of context, default 3"`
}

type writeMeta struct {
	Comment string `json:"comment,omitempty" jsonschema:"short edit summary, shown in history and recent_changes"`
	Author  string `json:"author,omitempty" jsonschema:"who to attribute this to; defaults to the client's name"`
}

type createArgs struct {
	Title string `json:"title"`
	Body  string `json:"body" jsonschema:"the whole page as markdown, optionally starting with YAML frontmatter"`
	writeMeta
}

type writeArgs struct {
	Title   string `json:"title"`
	Body    string `json:"body" jsonschema:"the whole new page text"`
	BaseRev int64  `json:"base_rev" jsonschema:"the rev you read; the write fails with a conflict if the page changed since"`
	writeMeta
}

type editArgs struct {
	Title       string          `json:"title"`
	BaseRev     int64           `json:"base_rev,omitempty" jsonschema:"optional: fail if the page is no longer at this rev"`
	Edits       []wikitext.Edit `json:"edits,omitempty" jsonschema:"str_replace edits applied in order; each old_text must occur exactly once at the time it applies"`
	Section     string          `json:"section,omitempty" jsonschema:"replace this whole section (heading line and subsections included) with section_text: an index, heading path, or unique heading title"`
	SectionText string          `json:"section_text,omitempty" jsonschema:"replacement for section, including its heading line; empty deletes the section"`
	Append      string          `json:"append,omitempty" jsonschema:"text to add at the end, after a blank line"`
	Prepend     string          `json:"prepend,omitempty" jsonschema:"text to add at the start, after any frontmatter"`
	writeMeta
}

type moveArgs struct {
	From          string `json:"from"`
	To            string `json:"to"`
	LeaveRedirect *bool  `json:"leave_redirect,omitempty" jsonschema:"leave a redirect at the old title (default true)"`
	RewriteLinks  *bool  `json:"rewrite_links,omitempty" jsonschema:"rewrite other pages' links to point at the new title (default true)"`
	BaseRev       int64  `json:"base_rev,omitempty"`
	writeMeta
}

type deleteArgs struct {
	Title   string `json:"title"`
	BaseRev int64  `json:"base_rev,omitempty"`
	writeMeta
}

type restoreArgs struct {
	Title string `json:"title"`
	writeMeta
}

type revertArgs struct {
	Title   string `json:"title"`
	ToRev   int64  `json:"to_rev" jsonschema:"the revision whose text to restore"`
	BaseRev int64  `json:"base_rev,omitempty"`
	writeMeta
}

type bulkItem struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Mode    string `json:"mode,omitempty" jsonschema:"create (default: fails if the page exists), replace (needs base_rev) or upsert"`
	BaseRev int64  `json:"base_rev,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type bulkArgs struct {
	Pages []bulkItem `json:"pages" jsonschema:"up to 50 pages; each succeeds or fails on its own"`
	writeMeta
}

type uploadArgs struct {
	Name        string `json:"name" jsonschema:"file name with extension, e.g. Einstein_1921.jpg; referenced as kb-file:Name"`
	Text        string `json:"text,omitempty" jsonschema:"contents of a text file (SVG, CSV, JSON, plain text, ...) as-is; give text or data_base64"`
	Data        string `json:"data_base64,omitempty" jsonschema:"contents of a binary file (PNG, JPEG, PDF, ...) as base64, standard or URL-safe; a data: URL also works"`
	MIME        string `json:"mime,omitempty" jsonschema:"content type; detected from the name and contents when omitted"`
	Description string `json:"description,omitempty" jsonschema:"what the file shows or contains: caption, source, licence"`
	BaseRev     int64  `json:"base_rev,omitempty" jsonschema:"required to replace an existing file: its current rev"`
	writeMeta
}

type getFileArgs struct {
	Name    string `json:"name"`
	Content bool   `json:"content,omitempty" jsonschema:"also return the contents: images as image content, text as text, anything else as base64"`
	UsedBy  int    `json:"used_by,omitempty" jsonschema:"list up to this many pages embedding the file, default 20"`
}

type listFilesArgs struct {
	Prefix string `json:"prefix,omitempty"`
	MIME   string `json:"mime,omitempty" jsonschema:"content-type prefix, e.g. image/"`
	Sort   string `json:"sort,omitempty" jsonschema:"name (default), updated or size"`
	Desc   bool   `json:"desc,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

type deleteFileArgs struct {
	Name string `json:"name"`
	writeMeta
}

type newTaskArg struct {
	Title    string `json:"title"`
	Body     string `json:"body,omitempty" jsonschema:"details, acceptance criteria"`
	Priority int    `json:"priority,omitempty" jsonschema:"higher is claimed first, default 0"`
	ParentID int64  `json:"parent_id,omitempty" jsonschema:"make this a subtask"`
	Page     string `json:"page,omitempty" jsonschema:"the page this task is about"`
}

type taskCreateArgs struct {
	Tasks []newTaskArg `json:"tasks,omitempty" jsonschema:"tasks to create, up to 500; or give one task's fields (title, body, ...) at the top level instead"`
	newTaskArgOptional
	Author string `json:"author,omitempty"`
}

// newTaskArgOptional is newTaskArg with every field optional, so task_create
// also accepts a single task without the tasks array.
type newTaskArgOptional struct {
	Title    string `json:"title,omitempty" jsonschema:"for a single task: its title"`
	Body     string `json:"body,omitempty"`
	Priority int    `json:"priority,omitempty"`
	ParentID int64  `json:"parent_id,omitempty"`
	Page     string `json:"page,omitempty"`
}

type taskUpdateArgs struct {
	ID       int64   `json:"id,omitempty" jsonschema:"the task to update; or give ids"`
	IDs      []int64 `json:"ids,omitempty" jsonschema:"apply the same update to several tasks, e.g. {\"ids\":[1,2,3],\"status\":\"done\"}"`
	Status   *string `json:"status,omitempty" jsonschema:"todo, doing, blocked, done or dropped"`
	Priority *int    `json:"priority,omitempty"`
	Assignee *string `json:"assignee,omitempty"`
	Title    *string `json:"title,omitempty"`
	Body     *string `json:"body,omitempty" jsonschema:"replace the body"`
	Append   string  `json:"append,omitempty" jsonschema:"append a paragraph to the body: a running log of what was done"`
	ParentID *int64  `json:"parent_id,omitempty" jsonschema:"move under another task; 0 makes it top-level"`
	Page     *string `json:"page,omitempty"`
	Author   string  `json:"author,omitempty"`
}

type taskListArgs struct {
	Status   []string `json:"status,omitempty" jsonschema:"statuses to include, default [todo, doing, blocked]; [all] for every task"`
	Assignee string   `json:"assignee,omitempty"`
	Mine     bool     `json:"mine,omitempty" jsonschema:"only tasks assigned to you (your author name)"`
	ParentID int64    `json:"parent_id,omitempty" jsonschema:"only direct subtasks of this task; -1 for top-level tasks"`
	Page     string   `json:"page,omitempty"`
	Query    string   `json:"query,omitempty" jsonschema:"substring of title or body"`
	Sort     string   `json:"sort,omitempty" jsonschema:"priority (default), updated or created"`
	Limit    int      `json:"limit,omitempty"`
	Offset   int      `json:"offset,omitempty"`
	ID       int64    `json:"id,omitempty" jsonschema:"read just this task, with its full body"`
	Author   string   `json:"author,omitempty"`
}

type claimArgs struct {
	ParentID     int64  `json:"parent_id,omitempty" jsonschema:"only claim tasks anywhere under this task"`
	LeaseMinutes int    `json:"lease_minutes,omitempty" jsonschema:"how long the claim holds before others may take it, default 30; renew by updating the task"`
	Author       string `json:"author,omitempty"`
}

type noteWriteArgs struct {
	ID     int64  `json:"id,omitempty" jsonschema:"update this note instead of creating one"`
	Topic  string `json:"topic,omitempty" jsonschema:"groups notes: e.g. progress, plan, style-guide, questions"`
	Body   string `json:"body"`
	Append bool   `json:"append,omitempty" jsonschema:"with id: append body as a new paragraph"`
	Page   string `json:"page,omitempty" jsonschema:"attach the note to a page (talk-page style)"`
	Author string `json:"author,omitempty"`
}

type noteListArgs struct {
	ID     int64  `json:"id,omitempty" jsonschema:"read just this note"`
	Topic  string `json:"topic,omitempty"`
	Page   string `json:"page,omitempty"`
	Query  string `json:"query,omitempty" jsonschema:"full-text search over notes"`
	Author string `json:"author,omitempty"`
	Topics bool   `json:"topics,omitempty" jsonschema:"list the topics in use instead of notes"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 20, max 200"`
	Offset int    `json:"offset,omitempty"`
}

type changesArgs struct {
	AfterID  int64  `json:"after_id,omitempty" jsonschema:"only changes after this change id, oldest first: pass the last id you saw to resume"`
	BeforeID int64  `json:"before_id,omitempty" jsonschema:"page back: only changes before this id, newest first"`
	Since    string `json:"since,omitempty" jsonschema:"only changes at or after this time: RFC 3339, a date, or a duration back from now such as 24h"`
	Author   string `json:"author,omitempty"`
	Kind     string `json:"kind,omitempty" jsonschema:"prefix: page, page.edit, page.create, page.move, page.delete, file, task, note"`
	Target   string `json:"target,omitempty" jsonschema:"only changes to this exact page title, file name, task title or note topic"`
	Limit    int    `json:"limit,omitempty" jsonschema:"default 50, max 500"`
}

type feedbackArgs struct {
	Kind    string `json:"kind" jsonschema:"bug (a tool misbehaved or errored wrongly), friction (it worked but was confusing, slow or wasteful), idea (something missing), or praise (something that worked well and should stay)"`
	Tool    string `json:"tool,omitempty" jsonschema:"the tool this is about, e.g. edit_page"`
	Message string `json:"message" jsonschema:"one or two sentences: what happened and what you expected"`
	Details string `json:"details,omitempty" jsonschema:"optional: the arguments you sent, the error text, what you did instead"`
	Author  string `json:"author,omitempty"`
}

type checkArgs struct {
	Title string `json:"title" jsonschema:"the page to check"`
}
