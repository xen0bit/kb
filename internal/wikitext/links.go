package wikitext

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// Link is a kb: link from a page to another page. Target is the display
// title as written in the link; Key is what it resolves against.
type Link struct {
	Target   string `json:"target"`
	Key      string `json:"-"`
	Fragment string `json:"fragment,omitempty"`
}

// Doc is everything the store indexes about a document.
type Doc struct {
	Meta      Meta
	Links     []Link   // kb: page links, deduplicated by key
	Files     []string // kb-file: references (images and plain links), deduplicated by key
	External  int      // count of links to anything else
	Words     int
	Lead      string // the first paragraph as plain text, for search results
	HeadingsN int
	// Malformed are kb: destinations markdown did not parse as links,
	// almost always a title with a bare space: kb:Foo Bar instead of
	// kb:Foo_Bar or <kb:Foo Bar>.
	Malformed []string
}

var md = goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote))

// Parser is the goldmark instance every kb component renders and parses with,
// so the store and the web viewer see the same links.
func Parser() goldmark.Markdown { return md }

// Analyze parses a document. Frontmatter errors are returned; markdown cannot
// fail to parse.
func Analyze(doc string) (Doc, error) {
	var d Doc
	meta, err := ParseMeta(doc)
	if err != nil {
		return d, err
	}
	d.Meta = meta
	_, body, _, _ := SplitFrontmatter(doc)
	src := []byte(body)
	root := md.Parser().Parse(text.NewReader(src))

	seenLink := map[string]bool{}
	seenFile := map[string]bool{}
	addDest := func(dest string) {
		kind, target, frag := ParseDestination(dest)
		switch kind {
		case "page":
			n, err := NormalizeTitle(target)
			if err != nil {
				return
			}
			k := strings.ToLower(n)
			if !seenLink[k] {
				seenLink[k] = true
				d.Links = append(d.Links, Link{Target: n, Key: k, Fragment: frag})
			}
		case "file":
			n, err := NormalizeFileName(target)
			if err != nil {
				return
			}
			k := strings.ToLower(n)
			if !seenFile[k] {
				seenFile[k] = true
				d.Files = append(d.Files, n)
			}
		default:
			d.External++
		}
	}
	var words strings.Builder
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.Link:
			addDest(string(t.Destination))
		case *ast.Image:
			addDest(string(t.Destination))
		case *ast.AutoLink:
			d.External++
		case *ast.Heading:
			d.HeadingsN++
		case *ast.Paragraph:
			if d.Lead == "" {
				d.Lead = plainText(t, src)
			}
		case *ast.Text:
			words.Write(t.Segment.Value(src))
			words.WriteByte(' ')
		}
		return ast.WalkContinue, nil
	})
	for _, m := range spacedDest.FindAllStringSubmatch(body, 20) {
		d.Malformed = append(d.Malformed, m[1])
	}
	d.Words = len(strings.FieldsFunc(words.String(), func(r rune) bool { return unicode.IsSpace(r) }))
	if meta.Summary != "" {
		d.Lead = meta.Summary
	}
	if len(d.Lead) > 400 {
		d.Lead = truncateRunes(d.Lead, 400) + "…"
	}
	return d, nil
}

func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		case *ast.CodeSpan:
			for ch := t.FirstChild(); ch != nil; ch = ch.NextSibling() {
				if tx, ok := ch.(*ast.Text); ok {
					b.Write(tx.Segment.Value(src))
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// spacedDest finds "](kb:Foo Bar)": a destination with a space that is not
// followed by a quoted title, which CommonMark does not accept as a link.
var spacedDest = regexp.MustCompile(`\]\(\s*((?i:kb-file|kb):[^)\s"'<>]+\s+[^)"'\s][^)]*)\)`)

// Dest is a kb: or kb-file: link destination found in a document.
type Dest struct {
	Kind     string // "page" or "file"
	Target   string // decoded title or file name
	Fragment string // decoded, without '#'
	Angle    bool   // written as <kb:...>
}

// MapDestinations calls fn for every inline link or image destination that
// starts with kb: or kb-file:, and replaces the destination (the text inside
// the parentheses, up to any title) with what fn returns when ok is true.
//
// Destinations are scanned the way CommonMark reads them: <…> runs to the
// closing '>', and a bare destination runs to whitespace or the ')' that
// closes it, so balanced parentheses stay inside: kb:Mercury_(planet).
// Links inside code are visited too; telling them apart needs a full parse
// with source positions, and a code sample spelling out a kb: link is rare.
func MapDestinations(doc string, fn func(d Dest) (string, bool)) (string, int) {
	var b strings.Builder
	n := 0
	i := 0
	for {
		j := strings.Index(doc[i:], "](")
		if j < 0 {
			b.WriteString(doc[i:])
			return b.String(), n
		}
		start := i + j + 2
		b.WriteString(doc[i:start])
		k := start
		for k < len(doc) && (doc[k] == ' ' || doc[k] == '\t') {
			k++
		}
		angle := k < len(doc) && doc[k] == '<'
		ds := k
		if angle {
			ds++
		}
		var kind string
		switch {
		case hasPrefixFold(doc[ds:], FileScheme):
			kind = "file"
		case hasPrefixFold(doc[ds:], PageScheme):
			kind = "page"
		default:
			i = start
			continue
		}
		end := ds
		if angle {
			for end < len(doc) && doc[end] != '>' && doc[end] != '\n' {
				end++
			}
			if end >= len(doc) || doc[end] != '>' {
				i = start
				continue
			}
		} else {
			depth := 0
		scan:
			for end < len(doc) {
				switch c := doc[end]; {
				case c == '\\' && end+1 < len(doc):
					end += 2
					continue
				case c == '(':
					depth++
				case c == ')':
					if depth == 0 {
						break scan
					}
					depth--
				case c == ' ' || c == '\t' || c == '\n' || c == '\r':
					break scan
				}
				end++
			}
		}
		raw := doc[ds:end]
		k2, target, frag := ParseDestination(raw)
		if k2 != kind {
			i = start
			continue
		}
		repl, ok := fn(Dest{Kind: kind, Target: target, Fragment: frag, Angle: angle})
		if !ok {
			i = start
			continue
		}
		n++
		b.WriteString(doc[start:k])
		if angle {
			b.WriteString("<" + repl + ">")
			i = end + 1
		} else {
			b.WriteString(repl)
			i = end
		}
	}
}

// RewriteLinks returns doc with every kb: link to oldKey pointed at newTitle,
// keeping each link's fragment, label and title attribute. It reports how many
// links it rewrote.
func RewriteLinks(doc, oldKey, newTitle string) (string, int) {
	return MapDestinations(doc, func(d Dest) (string, bool) {
		if d.Kind != "page" || Key(d.Target) != oldKey {
			return "", false
		}
		frag := ""
		if d.Fragment != "" {
			frag = "#" + Anchor(d.Fragment)
		}
		if d.Angle {
			return PageScheme + newTitle + frag, true
		}
		return PageScheme + URLForm(newTitle) + frag, true
	})
}
