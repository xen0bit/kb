package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xen0bit/kb/internal/wikitext"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kb.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *Store, title, body string) *PutResult {
	t.Helper()
	r, err := s.PutPage(context.Background(), PutRequest{Title: title, Body: body, Author: "test", Mode: ModeUpsert})
	if err != nil {
		t.Fatalf("put %q: %v", title, err)
	}
	return r
}

func TestMigrations(t *testing.T) {
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || SchemaVersion() != len(files) {
		t.Fatalf("schema version %d with %d files", SchemaVersion(), len(files))
	}
	path := filepath.Join(t.TempDir(), "kb.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "A", "x")
	s.Close()
	// Reopening an up-to-date kb is a no-op, and read-only works.
	s, err = Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPage(context.Background(), PutRequest{Title: "B", Body: "y"}); err == nil {
		t.Error("write on a read-only store succeeded")
	}
	s.Close()
	// A kb from a newer binary is refused, not guessed at.
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion()+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path, Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer schema: %v", err)
	}
}

// TestUpgradeFromV1 builds a kb at schema version 1, with data in it, and
// opens it with this binary: the upgrade must keep a backup, reach the
// current version, and keep the data readable.
func TestUpgradeFromV1(t *testing.T) {
	if SchemaVersion() < 2 {
		t.Skip("needs a second migration")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.db")
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	old := &Store{path: path, w: w, log: slog.New(slog.DiscardHandler), now: time.Now}
	if err := old.migrateTo(ctx, false, 1); err != nil {
		t.Fatal(err)
	}
	// What a v1 binary would have written for one page.
	for _, q := range []string{
		`INSERT INTO pages (id, key, title, current_rev, created_at, updated_at) VALUES (1, 'old page', 'Old page', 1, 1, 1)`,
		`INSERT INTO revisions (id, page_id, title, body, sha256, size, created_at) VALUES (1, 1, 'Old page', 'Kept.' || char(10), 'x', 6, 1)`,
		`INSERT INTO pages_fts (rowid, title, aliases, body) VALUES (1, 'Old page', '', 'Kept.')`,
	} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Version(ctx); v != SchemaVersion() {
		t.Errorf("version %d after upgrade, want %d", v, SchemaVersion())
	}
	if _, err := os.Stat(fmt.Sprintf("%s.pre-v%d.bak", path, SchemaVersion())); err != nil {
		t.Errorf("no backup before migrating: %v", err)
	}
	p, err := s.GetPage(ctx, "old page", GetOptions{Body: true})
	if err != nil || p.Body != "Kept.\n" {
		t.Errorf("page after upgrade: %+v %v", p, err)
	}
	if hits, _, _ := s.Search(ctx, "kept", PageFilter{}, "", nil, 10, 0); len(hits) != 1 {
		t.Errorf("search after upgrade: %v", hits)
	}
	// A fresh kb at v1 has no data and needs no backup.
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	f, err := Open(fresh, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(fresh + ".pre-v2.bak"); err == nil {
		t.Error("backup written for a new kb")
	}
}

func TestPageLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	r, err := s.PutPage(ctx, PutRequest{Title: "albert_einstein", Body: "---\ncategories: [Physicists]\naliases: [Einstein]\nstatus: stub\n---\nA [physicist](kb:Physicist) from [Ulm](kb:Ulm).\n\n![p](kb-file:Einstein.jpg)", Author: "a", Mode: ModeCreate})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Created || r.Title != "Albert einstein" || r.Links != 2 || len(r.RedLinks) != 2 || len(r.NoFiles) != 1 {
		t.Fatalf("create result %+v", r)
	}
	if _, err := s.PutPage(ctx, PutRequest{Title: "Albert Einstein", Body: "x", Mode: ModeCreate}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate create: %v", err)
	}
	// Stale base_rev is a conflict; the right one is an edit.
	if _, err := s.PutPage(ctx, PutRequest{Title: "Albert Einstein", Body: "x", BaseRev: r.Rev + 99, Mode: ModeReplace}); !errors.Is(err, ErrConflict) {
		t.Errorf("stale write: %v", err)
	}
	if _, err := s.PutPage(ctx, PutRequest{Title: "Albert Einstein", Body: "x", Mode: ModeReplace}); !errors.Is(err, ErrInvalid) {
		t.Errorf("replace without base_rev: %v", err)
	}
	// Writing the red-linked page turns the link blue.
	put(t, s, "Physicist", "A scientist. Not to be confused with [physics](kb:Physics).")
	got, err := s.GetPage(ctx, "einstein", GetOptions{Body: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ViaAlias != "einstein" || got.Title != "Albert einstein" || got.Status != "stub" || len(got.Categories) != 1 {
		t.Errorf("read via alias: %+v", got)
	}
	out, in, err := s.LinkCounts(ctx, got.ID)
	if err != nil || out != 2 || in != 0 {
		t.Errorf("link counts %d %d %v", out, in, err)
	}
	wanted, total, err := s.Special(ctx, "wanted", 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("wanted %v %d %v", wanted, total, err)
	}
	// Unchanged writes do not make revisions.
	again, err := s.PutPage(ctx, PutRequest{Title: "Physicist", Body: "A scientist. Not to be confused with [physics](kb:Physics).\n", Mode: ModeUpsert})
	if err != nil || !again.Unchanged {
		t.Errorf("unchanged write: %+v %v", again, err)
	}
	// Edits.
	e, err := s.EditPage(ctx, EditRequest{Title: "Albert Einstein", Edits: []wikitext.Edit{{OldText: "from [Ulm](kb:Ulm)", NewText: "born in [Ulm](kb:Ulm)"}}, Author: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditPage(ctx, EditRequest{Title: "Albert Einstein", BaseRev: r.Rev, Append: "more"}); !errors.Is(err, ErrConflict) {
		t.Errorf("stale append: %v", err)
	}
	if _, err := s.EditPage(ctx, EditRequest{Title: "Albert Einstein", BaseRev: e.Rev, Append: "## Later\n\nMore."}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPage(ctx, "Albert Einstein", GetOptions{Body: true})
	if !strings.Contains(p.Body, "born in [Ulm]") || !strings.HasSuffix(p.Body, "\n\n## Later\n\nMore.\n") {
		t.Errorf("body after edits: %q", p.Body)
	}
	if _, err := s.EditPage(ctx, EditRequest{Title: "Albert Einstein", Prepend: "Lead line."}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetPage(ctx, "Albert Einstein", GetOptions{Body: true})
	if !strings.Contains(p.Body, "---\nLead line.\n\nA [physicist]") {
		t.Errorf("prepend did not go after frontmatter: %q", p.Body)
	}
	hist, err := s.History(ctx, "Albert Einstein", 0, 10)
	if err != nil || len(hist) != 4 || !hist[0].Current {
		t.Fatalf("history %+v %v", hist, err)
	}
	rv, err := s.RevertPage(ctx, "Albert Einstein", r.Rev, 0, "", "c")
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := s.GetRevision(ctx, r.Rev)
	now, _ := s.GetRevision(ctx, rv.Rev)
	if orig.Body != now.Body || now.ParentID != hist[0].ID {
		t.Error("revert did not restore the text")
	}
	// Delete takes the page out of search and listings; restore brings it back.
	if err := s.DeletePage(ctx, "Albert Einstein", "", "c", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPage(ctx, "Albert Einstein", GetOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted page read: %v", err)
	}
	if hits, _, _ := s.Search(ctx, "Ulm", PageFilter{}, "", nil, 10, 0); len(hits) != 0 {
		t.Errorf("deleted page searchable: %v", hits)
	}
	if _, err := s.RestorePage(ctx, "Albert Einstein", "", "c"); err != nil {
		t.Fatal(err)
	}
	if hits, _, _ := s.Search(ctx, "Ulm", PageFilter{}, "", nil, 10, 0); len(hits) != 1 {
		t.Errorf("restored page not searchable: %v", hits)
	}
}

func TestSections(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	put(t, s, "Doc", "Lead.\n\n# A\n\nAlpha.\n\n# B\n\nBeta.\n")
	if _, err := s.EditPage(ctx, EditRequest{Title: "Doc", Section: "A", SectionText: "# A\n\nAleph."}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPage(ctx, "Doc", GetOptions{Body: true})
	if p.Body != "Lead.\n\n# A\n\nAleph.\n\n# B\n\nBeta.\n" {
		t.Errorf("section replace: %q", p.Body)
	}
}

func TestMoveRewritesLinksAndRedirects(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	put(t, s, "Mercury", "A planet.")
	put(t, s, "Solar System", "Has [Mercury](kb:Mercury) and [also](kb:mercury#Orbit).")
	put(t, s, "Venus", "Next to [Mercury](kb:Mercury).")
	m, err := s.MovePage(ctx, MoveRequest{From: "Mercury", To: "Mercury (planet)", LeaveRedirect: true, RewriteLinks: true, Author: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Redirect || m.LinksRewritten != 3 || len(m.PagesRewritten) != 2 {
		t.Errorf("move %+v", m)
	}
	ss, _ := s.GetPage(ctx, "Solar System", GetOptions{Body: true})
	if !strings.Contains(ss.Body, "kb:Mercury_%28planet%29#Orbit") {
		t.Errorf("links not rewritten: %q", ss.Body)
	}
	r, err := s.GetPage(ctx, "Mercury", GetOptions{FollowRedirect: true, Body: true})
	if err != nil || r.RedirectedFrom != "Mercury" || r.Title != "Mercury (planet)" || r.Body != "A planet.\n" {
		t.Errorf("redirect read %+v %v", r, err)
	}
	h, _ := s.History(ctx, "Mercury (planet)", 0, 10)
	if len(h) != 2 || !strings.Contains(h[0].Comment, "moved from") {
		t.Errorf("history after move %+v", h)
	}
	if _, err := s.MovePage(ctx, MoveRequest{From: "Venus", To: "Solar System"}); !errors.Is(err, ErrExists) {
		t.Errorf("move onto existing: %v", err)
	}
	// Moving back over its own redirect is allowed.
	if _, err := s.MovePage(ctx, MoveRequest{From: "Mercury (planet)", To: "Mercury"}); err != nil {
		t.Errorf("move back over redirect: %v", err)
	}
	put(t, s, "Mars (planet)", "x")
	put(t, s, "Mars (mythology)", "y")
	if res, _ := s.Resolve(ctx, []string{"mars"}, 3); res[0].Status != "missing" || len(res[0].Suggest) != 2 || res[0].Suggest[0] != "Mars (mythology)" {
		t.Errorf("disambiguation suggestions %+v", res)
	}
	if res, _ := s.Resolve(ctx, []string{"Mercury", "Mercury (planet)", "Mercuri", "Pluto", "a#b"}, 3); res[0].Status != "exists" ||
		res[1].Status != "missing" || res[2].Status != "missing" || len(res[2].Suggest) == 0 || res[2].Suggest[0] != "Mercury" || res[4].Status != "invalid" {
		t.Errorf("resolve %+v", res)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	put(t, s, "Quantum mechanics", "---\ncategories: [Physics]\n---\nThe study of very small things; see [entanglement](kb:Quantum_entanglement).")
	put(t, s, "Classical mechanics", "---\ncategories: [Physics]\nstatus: stub\n---\nNewton's laws. Unlike quantum theory, it is deterministic.")
	if w := put(t, s, "Cooking", "Mechanics of the kitchen? See [x](kb:Not a link)."); len(w.Warnings) != 1 {
		t.Errorf("no warning for a spaced destination: %+v", w)
	}
	hits, total, err := s.Search(ctx, "quantum", PageFilter{}, "", nil, 10, 0)
	if err != nil || total != 2 || hits[0].Title != "Quantum mechanics" {
		t.Fatalf("hits %+v %d %v", hits, total, err)
	}
	if !strings.Contains(hits[1].Snippet, "**quantum**") {
		t.Errorf("snippet %q", hits[1].Snippet)
	}
	if hits, _, _ := s.Search(ctx, "mechanics", PageFilter{Category: "physics", Status: "stub"}, "", nil, 10, 0); len(hits) != 1 {
		t.Errorf("filtered search %+v", hits)
	}
	if _, _, err := s.Search(ctx, "C++ (programming language)", PageFilter{}, "", nil, 10, 0); err != nil {
		t.Errorf("plain query with punctuation: %v", err)
	}
	if _, _, err := s.Search(ctx, `"unclosed`, PageFilter{}, "", nil, 10, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad FTS syntax: %v", err)
	}
	if hits, _, _ := s.Search(ctx, "mech*", PageFilter{}, "updated", nil, 10, 0); len(hits) != 3 || hits[0].Title != "Cooking" {
		t.Errorf("prefix + sort by updated %+v", hits)
	}
	list, total, err := s.ListPages(ctx, PageFilter{Prefix: "c"}, "title", nil, 10, 0)
	if err != nil || total != 2 || list[0].Title != "Classical mechanics" {
		t.Errorf("list %+v %d %v", list, total, err)
	}
	cats, _, _ := s.Categories(ctx, "", 10, 0)
	if len(cats) != 1 || cats[0].Count != 2 {
		t.Errorf("categories %+v", cats)
	}
	for _, k := range SpecialKinds {
		if _, _, err := s.Special(ctx, k, 10, 0); err != nil {
			t.Errorf("special %s: %v", k, err)
		}
	}
	orph, _, _ := s.Special(ctx, "orphans", 10, 0)
	if len(orph) != 3 {
		t.Errorf("orphans %+v", orph)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Pages != 3 || st.WantedPages != 1 || st.Categories != 1 {
		t.Errorf("stats %+v %v", st, err)
	}
	if probs, err := s.Check(ctx); err != nil || len(probs) != 0 {
		t.Errorf("check %v %v", probs, err)
	}
}

func TestFiles(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	png := []byte("\x89PNG\r\n\x1a\nfakepng")
	r, err := s.PutFile(ctx, PutFileRequest{Name: "dot.png", Data: png, Author: "a", Description: "a dot"})
	if err != nil || !r.Created || r.MIME != "image/png" || r.Markdown != "![dot.png](kb-file:dot.png)" {
		t.Fatalf("put %+v %v", r, err)
	}
	if _, err := s.PutFile(ctx, PutFileRequest{Name: "Dot.png", Data: png}); !errors.Is(err, ErrExists) {
		t.Errorf("replace without base_rev: %v", err)
	}
	r2, err := s.PutFile(ctx, PutFileRequest{Name: "copy.png", Data: png})
	if err != nil || !r2.Dedup {
		t.Errorf("dedup %+v %v", r2, err)
	}
	put(t, s, "Page", "![x](kb-file:dot.png)")
	f, data, err := s.GetFile(ctx, "DOT.png", true, 10)
	if err != nil || string(data) != string(png) || f.UsedByN != 1 || f.UsedBy[0] != "Page" || f.Description != "a dot" {
		t.Errorf("get %+v %v", f, err)
	}
	list, total, err := s.ListFiles(ctx, FileFilter{MIME: "image/"})
	if err != nil || total != 2 || len(list) != 2 {
		t.Errorf("list %v %d %v", list, total, err)
	}
	if err := s.DeleteFile(ctx, "copy.png", "", "a"); err != nil {
		t.Fatal(err)
	}
	if mf, _, _ := s.Special(ctx, "unused_files", 10, 0); len(mf) != 0 {
		t.Errorf("unused files %+v", mf)
	}
	if _, err := s.PutFile(ctx, PutFileRequest{Name: "big.bin", Data: make([]byte, 11), MaxBytes: 10}); !errors.Is(err, ErrInvalid) {
		t.Errorf("size limit: %v", err)
	}
}

func TestTasksAndClaims(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	root, err := s.CreateTasks(ctx, []NewTask{{Title: "Write physics", Priority: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var kids []NewTask
	for i := range 20 {
		kids = append(kids, NewTask{ParentID: root[0].ID, Title: fmt.Sprintf("Article %d", i), Priority: i % 3})
	}
	if _, err := s.CreateTasks(ctx, kids); err != nil {
		t.Fatal(err)
	}
	// Twenty agents claim at once: each gets a different task, and the root
	// (which has open children) is never handed out.
	var mu sync.Mutex
	got := map[int64]string{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			who := fmt.Sprintf("agent-%d", i)
			task, err := s.ClaimNext(ctx, who, 0, time.Minute)
			if err != nil || task == nil {
				t.Errorf("claim: %v %v", task, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if prev, dup := got[task.ID]; dup {
				t.Errorf("task %d claimed by %s and %s", task.ID, prev, who)
			}
			got[task.ID] = who
		})
	}
	wg.Wait()
	if _, ok := got[root[0].ID]; ok {
		t.Error("parent with open children was claimed")
	}
	if next, _ := s.ClaimNext(ctx, "late", 0, time.Minute); next != nil {
		t.Errorf("claimed %+v with everything leased", next)
	}
	// Expired leases are reclaimable.
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if next, _ := s.ClaimNext(ctx, "late", root[0].ID, time.Minute); next == nil || next.Assignee != "late" {
		t.Errorf("expired lease not reclaimed: %+v", next)
	}
	done := "done"
	tk, err := s.UpdateTask(ctx, TaskUpdate{ID: root[0].ID + 1, Status: &done, AppendBody: "wrote it", Author: "x"})
	if err != nil || tk.Status != "done" || tk.LeaseUntil != 0 || tk.Body != "wrote it" {
		t.Errorf("update %+v %v", tk, err)
	}
	p := root[0].ID + 2
	if _, err := s.UpdateTask(ctx, TaskUpdate{ID: root[0].ID, ParentID: &p}); !errors.Is(err, ErrInvalid) {
		t.Errorf("cycle: %v", err)
	}
	list, total, err := s.ListTasks(ctx, TaskFilter{ParentID: -1})
	if err != nil || total != 1 || list[0].OpenKids != 19 {
		t.Errorf("top-level %+v %d %v", list, total, err)
	}
}

func TestNotesAndChanges(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	n, err := s.WriteNote(ctx, NoteWrite{Topic: "progress", Body: "Did physics A-C.", Author: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote(ctx, NoteWrite{ID: n.ID, Body: "Did D-F.", Append: true}); err != nil {
		t.Fatal(err)
	}
	ns, total, err := s.ListNotes(ctx, NoteFilter{Query: "physics"})
	if err != nil || total != 1 || ns[0].Body != "Did physics A-C.\n\nDid D-F." {
		t.Errorf("notes %+v %v", ns, err)
	}
	put(t, s, "X", "x")
	ch, err := s.Changes(ctx, ChangeFilter{})
	if err != nil || len(ch) != 3 || ch[0].Kind != "page.create" {
		t.Fatalf("changes %+v %v", ch, err)
	}
	after, _ := s.Changes(ctx, ChangeFilter{AfterID: ch[2].ID})
	if len(after) != 2 || after[0].Kind != "note.write" {
		t.Errorf("after %+v", after)
	}
	if pg, _ := s.Changes(ctx, ChangeFilter{Kind: "page"}); len(pg) != 1 {
		t.Errorf("kind filter %+v", pg)
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	put(t, s, "A", "a")
	dst := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	b, err := Open(dst, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.GetPage(ctx, "A", GetOptions{}); err != nil {
		t.Error(err)
	}
	if _, err := s.Vacuum(ctx); err != nil {
		t.Error(err)
	}
}

func TestFeedback(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	f, err := s.AddFeedback(ctx, Feedback{Kind: "Friction", Tool: "read_page", Message: "outline was unclear", Author: "a"})
	if err != nil || f.ID == 0 || f.Kind != "friction" || f.Status != "open" {
		t.Fatalf("add %+v %v", f, err)
	}
	if _, err := s.AddFeedback(ctx, Feedback{Kind: "rant", Message: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad kind: %v", err)
	}
	if err := s.ResolveFeedback(ctx, f.ID, "resolved", "clarified the description"); err != nil {
		t.Fatal(err)
	}
	if open, n, _ := s.ListFeedback(ctx, FeedbackFilter{}); n != 0 || len(open) != 0 {
		t.Errorf("open after resolve: %v", open)
	}
	all, n, err := s.ListFeedback(ctx, FeedbackFilter{Status: "all", Tool: "read_page"})
	if err != nil || n != 1 || all[0].Resolution != "clarified the description" {
		t.Errorf("all %+v %v", all, err)
	}
	if err := s.ResolveFeedback(ctx, 999, "resolved", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}
