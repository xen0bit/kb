package exportfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xen0bit/kb/internal/store"
)

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	open := func() *store.Store {
		st, err := store.Open(filepath.Join(t.TempDir(), "kb.db"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	src := open()
	pages := map[string]string{
		"AC/DC":         "A band. See [Foo (band)](kb:Foo_%28band%29#History).\n",
		"Foo (band)":    "---\ncategories: [Bands]\n---\n![logo](kb-file:Logo_1.png)\n\n# History\n",
		"Café 100% Art": "Accents and percent.\n",
	}
	for title, body := range pages {
		if _, err := src.PutPage(ctx, store.PutRequest{Title: title, Body: body, Mode: store.ModeCreate}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.PutFile(ctx, store.PutFileRequest{Name: "Logo_1.png", Data: []byte("\x89PNGdata"), Description: "logo"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	m, err := Export(ctx, src, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Pages) != 3 || len(m.Files) != 1 {
		t.Fatalf("manifest %+v", m)
	}
	if _, err := os.Stat(filepath.Join(dir, "pages", "AC%2FDC.md")); err != nil {
		t.Error(err)
	}

	dst := open()
	res, err := Import(ctx, dst, dir, "importer", nil)
	if err != nil || res.Created != 3 || res.FilesCreated != 1 || len(res.Errors) != 0 {
		t.Fatalf("import %+v %v", res, err)
	}
	for title, body := range pages {
		p, err := dst.GetPage(ctx, title, store.GetOptions{Body: true})
		if err != nil || p.Body != body || p.Title != title {
			t.Errorf("%q: %+v %v", title, p, err)
		}
	}
	f, _, err := dst.GetFile(ctx, "Logo 1.png", false, 0)
	if err != nil || f.Description != "logo" || f.UsedByN != 1 {
		t.Errorf("file %+v %v", f, err)
	}
	again, err := Import(ctx, dst, dir, "importer", nil)
	if err != nil || again.Unchanged != 3 || again.FilesUnchanged != 1 {
		t.Errorf("re-import %+v %v", again, err)
	}

	pdir := filepath.Join(t.TempDir(), "portable")
	if _, err := Export(ctx, src, pdir, Options{Portable: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(pdir, "pages", "AC%2FDC.md"))
	if !strings.Contains(string(b), "](Foo_%28band%29.md#History)") {
		t.Errorf("portable link: %s", b)
	}
	b, _ = os.ReadFile(filepath.Join(pdir, "pages", "Foo_%28band%29.md"))
	if !strings.Contains(string(b), "](../files/Logo_1.png)") {
		t.Errorf("portable file: %s", b)
	}
	if _, err := Export(ctx, src, pdir, Options{}); err == nil {
		t.Error("export into a non-empty dir succeeded")
	}
}
