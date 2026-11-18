package web

import (
	"bytes"
	"context"
	"html/template"
	"strings"

	"github.com/xen0bit/kb/internal/store"
	"github.com/xen0bit/kb/internal/wikitext"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// md renders for the browser. Raw HTML in markdown is never passed through
// (goldmark escapes it unless told otherwise): the text was written by agents
// and must not be able to script the viewer. Dangerous URL schemes are
// dropped by goldmark for the same reason.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// anchorIDs gives headings Wikipedia-style ids that kb: fragments can name.
type anchorIDs struct{ used map[string]bool }

func (a *anchorIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	base := wikitext.Anchor(string(value))
	if base == "" {
		base = "section"
	}
	id := base
	for n := 2; a.used[id]; n++ {
		id = base + "_" + itoa(n)
	}
	a.used[id] = true
	return []byte(id)
}

func (a *anchorIDs) Put(value []byte) { a.used[string(value)] = true }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TOCEntry is a heading in a rendered page's table of contents.
type TOCEntry struct {
	Level int
	Text  string
	ID    string
}

// Rendered is a page body as HTML.
type Rendered struct {
	HTML template.HTML
	TOC  []TOCEntry
	Meta wikitext.Meta
}

// render turns a page's markdown into HTML: kb: links become /wiki/ links
// (red when the page does not exist, with the title it resolves to for
// aliases), kb-file: references become /file/ URLs, and external links are
// marked as such.
func render(ctx context.Context, st *store.Store, doc string) (*Rendered, error) {
	meta, _ := wikitext.ParseMeta(doc)
	_, body, _, _ := wikitext.SplitFrontmatter(doc)
	src := []byte(body)
	pctx := parser.NewContext(parser.WithIDs(&anchorIDs{used: map[string]bool{}}))
	root := md.Parser().Parse(text.NewReader(src), parser.WithContext(pctx))

	type ref struct {
		node     ast.Node
		kind     string
		key      string
		fragment string
		title    string
	}
	var refs []ref
	pageKeys, fileKeys := map[string]bool{}, map[string]bool{}
	var toc []TOCEntry
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var dest []byte
		switch t := n.(type) {
		case *ast.Heading:
			id, _ := t.AttributeString("id")
			idb, _ := id.([]byte)
			toc = append(toc, TOCEntry{Level: t.Level, Text: plain(t, src), ID: string(idb)})
			return ast.WalkContinue, nil
		case *ast.Link:
			dest = t.Destination
		case *ast.Image:
			dest = t.Destination
		default:
			return ast.WalkContinue, nil
		}
		kind, target, frag := wikitext.ParseDestination(string(dest))
		switch kind {
		case "page":
			k := wikitext.Key(target)
			refs = append(refs, ref{n, kind, k, frag, target})
			if k != "" {
				pageKeys[k] = true
			}
		case "file":
			k := wikitext.FileKey(target)
			refs = append(refs, ref{n, kind, k, "", target})
			if k != "" {
				fileKeys[k] = true
			}
		default:
			if l, ok := n.(*ast.Link); ok && (bytes.HasPrefix(dest, []byte("http://")) || bytes.HasPrefix(dest, []byte("https://"))) {
				l.SetAttributeString("class", []byte("external"))
				l.SetAttributeString("rel", []byte("nofollow noopener noreferrer"))
			}
		}
		return ast.WalkContinue, nil
	})
	existing, err := st.Existing(ctx, keysOf(pageKeys))
	if err != nil {
		return nil, err
	}
	files, err := st.ExistingFiles(ctx, keysOf(fileKeys))
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		var href, class, title string
		switch r.kind {
		case "page":
			resolved, ok := existing[r.key]
			name := r.title
			if ok {
				name = resolved
			} else {
				class = "new"
				title = r.title + " (page does not exist)"
			}
			href = "/wiki/" + wikitext.URLForm(normalized(name))
			if r.fragment != "" {
				href += "#" + wikitext.Anchor(r.fragment)
			}
		case "file":
			n, _ := wikitext.NormalizeFileName(r.title)
			href = "/file/" + wikitext.URLForm(n)
			if _, ok := files[r.key]; !ok {
				class = "new"
				title = r.title + " (file not uploaded)"
			}
		}
		switch t := r.node.(type) {
		case *ast.Link:
			t.Destination = []byte(href)
			if class != "" {
				t.SetAttributeString("class", []byte(class))
			}
			if title != "" && len(t.Title) == 0 {
				t.Title = []byte(title)
			}
		case *ast.Image:
			t.Destination = []byte(href)
			t.SetAttributeString("loading", []byte("lazy"))
			if class != "" {
				t.SetAttributeString("class", []byte("missing"))
			}
		}
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, root); err != nil {
		return nil, err
	}
	// The HTML is goldmark's output from markdown with raw HTML disabled, so
	// it contains only markup goldmark generated itself.
	return &Rendered{HTML: template.HTML(buf.String()), TOC: toc, Meta: meta}, nil //nolint:gosec
}

func normalized(t string) string {
	if n, err := wikitext.NormalizeTitle(t); err == nil {
		return n
	}
	return t
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if t, ok := c.(*ast.Text); ok {
				b.Write(t.Segment.Value(src))
			}
			if t, ok := c.(*ast.String); ok {
				b.Write(t.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}
