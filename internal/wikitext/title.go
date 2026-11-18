// Package wikitext holds the pure, storage-free rules of a kb document: how a
// title is normalized into the key it is stored under, which links and file
// embeds a markdown body carries, how a body splits into sections, what its
// frontmatter says, and how str_replace edits apply to it.
//
// Nothing here touches the database, so every rule is cheap to test on its
// own, and the store, the MCP server and the web viewer all agree on them.
package wikitext

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitleBytes bounds a title. Wikipedia's limit is 255 bytes too.
const MaxTitleBytes = 255

// forbidden are the characters a title may not contain. '#' separates a
// fragment in a link, '|' and the brackets are markdown/link syntax, and '<'
// '>' would need escaping in every link destination.
const forbidden = "#<>[]{}|"

// ErrEmptyTitle is returned for a title that is empty after normalization.
var ErrEmptyTitle = errors.New("title is empty")

// NormalizeTitle returns the display form of a title: underscores become
// spaces, runs of whitespace collapse to one space, the ends are trimmed and
// the first letter is upper-cased, as Wikipedia does. It rejects titles that
// are empty, too long, or contain control characters or link syntax.
func NormalizeTitle(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("title is not valid UTF-8")
	}
	s = strings.ReplaceAll(s, "_", " ")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r):
			return "", fmt.Errorf("title contains a control character (%U)", r)
		case strings.ContainsRune(forbidden, r):
			return "", fmt.Errorf("title contains %q, which is not allowed in a title (forbidden: %s)", r, forbidden)
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if out == "" {
		return "", ErrEmptyTitle
	}
	if len(out) > MaxTitleBytes {
		return "", fmt.Errorf("title is %d bytes, longer than the %d allowed", len(out), MaxTitleBytes)
	}
	r, n := utf8.DecodeRuneInString(out)
	if up := unicode.ToUpper(r); up != r {
		out = string(up) + out[n:]
	}
	return out, nil
}

// Key is the identity a title is stored and looked up under: the normalized
// title, case-folded. Two titles with the same key are the same page. A title
// that does not normalize has an empty key.
func Key(title string) string {
	n, err := NormalizeTitle(title)
	if err != nil {
		return ""
	}
	return strings.ToLower(n)
}

// URLForm is the title as it is written in a kb: link destination or a web
// path: spaces as underscores, and anything a link destination cannot carry
// bare percent-encoded.
func URLForm(title string) string {
	t := strings.ReplaceAll(title, " ", "_")
	return escapePath(t)
}

// LinkForm is a title as written in a markdown link destination: like
// URLForm, but parentheses stay readable when they balance, since CommonMark
// keeps balanced parentheses inside a destination: kb:Mercury_(planet).
// Unbalanced ones are percent-encoded, as they would end the link.
func LinkForm(title string) string {
	t := strings.ReplaceAll(title, " ", "_")
	if !balanced(t) {
		return escapePath(t)
	}
	var b strings.Builder
	for _, r := range t {
		switch r {
		case '(', ')':
			b.WriteRune(r)
		default:
			b.WriteString(escapePath(string(r)))
		}
	}
	return b.String()
}

func balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return false
			}
			depth--
		}
	}
	return depth == 0
}

// escapePath percent-encodes what would break a markdown link destination or
// a URL path, and leaves everything else (including non-ASCII) readable.
func escapePath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '(' || c == ')' || c == '%' || c == '?' || c == '"' || c == '\'' || c == '\\' || c == '/' || c <= ' ' || c == 0x7f:
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// PageScheme and FileScheme are the link destination prefixes that make a
// markdown link point inside the kb.
const (
	PageScheme = "kb:"
	FileScheme = "kb-file:"
)

// PageLink renders a markdown link to a page.
func PageLink(label, title string) string {
	return "[" + label + "](" + PageScheme + LinkForm(title) + ")"
}

// ParseDestination splits a link destination into its kind and target.
// kind is "page" for kb:Title#Fragment, "file" for kb-file:Name, and "" for
// anything else (an external or relative link). Percent-encoding is decoded.
func ParseDestination(dest string) (kind, target, fragment string) {
	dest = strings.TrimSpace(dest)
	var rest string
	switch {
	case hasPrefixFold(dest, FileScheme):
		kind, rest = "file", dest[len(FileScheme):]
	case hasPrefixFold(dest, PageScheme):
		kind, rest = "page", dest[len(PageScheme):]
	default:
		return "", "", ""
	}
	if kind == "page" {
		if i := strings.IndexByte(rest, '#'); i >= 0 {
			rest, fragment = rest[:i], rest[i+1:]
			if f, err := url.PathUnescape(fragment); err == nil {
				fragment = f
			}
			fragment = strings.ReplaceAll(fragment, "_", " ")
		}
	}
	if u, err := url.PathUnescape(rest); err == nil {
		rest = u
	}
	return kind, rest, fragment
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// NormalizeFileName applies title rules to a file name, except that the case
// of the first letter is kept: "photo.jpg" stays as written.
func NormalizeFileName(name string) (string, error) {
	if strings.ContainsAny(name, "/\\") {
		return "", errors.New("file name may not contain a path separator")
	}
	n, err := NormalizeTitle(name)
	if err != nil {
		return "", fmt.Errorf("file name: %w", err)
	}
	// Restore the original first rune's case: NormalizeTitle capitalized it.
	trimmed := strings.TrimLeftFunc(strings.ReplaceAll(name, "_", " "), unicode.IsSpace)
	orig, _ := utf8.DecodeRuneInString(trimmed)
	_, sz := utf8.DecodeRuneInString(n)
	return string(orig) + n[sz:], nil
}

// FileKey is the identity a file is stored under.
func FileKey(name string) string {
	n, err := NormalizeFileName(name)
	if err != nil {
		return ""
	}
	return strings.ToLower(n)
}

// Anchor is the HTML id of a heading, and the fragment a kb: link uses to
// reach it: Wikipedia's form, the heading text with underscores for spaces.
// Anchor("Early life") == Anchor("Early_life") == "Early_life".
func Anchor(heading string) string {
	f := strings.Join(strings.Fields(strings.ReplaceAll(heading, "_", " ")), "_")
	var b strings.Builder
	for _, r := range f {
		switch {
		case r == '"' || r == '\'' || r == '<' || r == '>' || r == '&' || r == '#' || r == '%' || unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
