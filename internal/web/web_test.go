package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xen0bit/kb/internal/store"
)

func TestViewer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "kb.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	put := func(title, body string) {
		if _, err := st.PutPage(ctx, store.PutRequest{Title: title, Body: body, Mode: store.ModeUpsert, Author: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	put("Foo (band)", "---\ncategories: [Bands]\naliases: [Foo]\nformed: 1999\n---\nA band. See [Bar](kb:Bar), [missing](kb:Nope), [section](kb:Bar#Early_life), [ext](https://example.com) and <script>alert(1)</script>.\n\n![logo](kb-file:logo.svg)\n\n# History\n\n## Early life\n\nx\n\n# Later\n\ny\n")
	put("Bar", "# Early life\n\nLinked from [Foo](kb:Foo).\n")
	put("Bar", "# Early life\n\nLinked from [Foo](kb:Foo). Edited.\n")
	if _, err := st.PutFile(ctx, store.PutFileRequest{Name: "logo.svg", Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTasks(ctx, []store.NewTask{{Title: "Write Nope"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteNote(ctx, store.NoteWrite{Topic: "progress", Body: "Did [Foo](kb:Foo)."}); err != nil {
		t.Fatal(err)
	}
	w, err := New(st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, http.Header, string) {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}

	code, h, body := get("/wiki/Foo_%28band%29")
	if code != 200 {
		t.Fatalf("page: %d %s", code, body)
	}
	for _, want := range []string{
		`href="/wiki/Bar"`, `class="new"`, `href="/wiki/Bar#Early_life"`, `id="Early_life"`, `class="external"`,
		`src="/file/logo.svg"`, `<th>formed</th>`, `href="/category/Bands"`, `raw HTML omitted`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("raw HTML passed through")
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Error("no CSP")
	}
	if code, h, _ := get("/wiki/foo_(band)"); code != http.StatusMovedPermanently || h.Get("Location") != "/wiki/Foo_%28band%29" {
		t.Errorf("canonical redirect: %d %q", code, h.Get("Location"))
	}
	if code, _, body := get("/wiki/Foo"); code != 200 || !strings.Contains(body, "another name") {
		t.Errorf("alias: %d", code)
	}
	if code, _, body := get("/wiki/Nope"); code != 404 || !strings.Contains(body, "Foo (band)") {
		t.Errorf("missing: %d %s", code, body)
	}
	if code, h, _ := get("/search?q=bar"); code != http.StatusFound || h.Get("Location") != "/wiki/Bar" {
		t.Errorf("search go: %d %q", code, h.Get("Location"))
	}
	if code, _, body := get("/search?q=band&fulltext=1"); code != 200 || !strings.Contains(body, "Foo (band)") {
		t.Errorf("search: %d", code)
	}
	code, h, body = get("/file/logo.svg")
	if code != 200 || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") || !strings.Contains(body, "<svg") {
		t.Errorf("file: %d %v", code, h)
	}
	if code, _, body := get("/api/suggest?q=fo"); code != 200 || !strings.Contains(body, `"Foo (band)"`) {
		t.Errorf("suggest: %d %s", code, body)
	}
	_, _, hist := get("/history/Bar")
	if !strings.Contains(hist, "/diff?from=") {
		t.Errorf("history lacks diff link: %s", hist)
	}
	for _, p := range []string{"/", "/recent", "/pages", "/pages?sort=updated", "/special", "/categories", "/category/Bands",
		"/files", "/tasks", "/tasks?status=all", "/notes", "/links/Bar?dir=in", "/static/app.css"} {
		if code, _, body := get(p); code != 200 {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	for _, k := range store.SpecialKinds {
		if code, _, body := get("/special/" + k); code != 200 {
			t.Errorf("special %s: %d %s", k, code, body)
		}
	}
	revs, _ := st.History(ctx, "Bar", 0, 2)
	if code, _, body := get("/diff?from=" + itoa(int(revs[1].ID)) + "&to=" + itoa(int(revs[0].ID))); code != 200 || !strings.Contains(body, `class="add"`) {
		t.Errorf("diff: %d %s", code, body)
	}
	if code, _, _ := get("/random"); code != http.StatusFound {
		t.Errorf("random: %d", code)
	}
}
