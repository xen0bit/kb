package wikitext

import (
	"fmt"
	"strconv"
	"strings"
)

// Section is one heading's span of a document. Index 0 is the lead: whatever
// precedes the first heading (frontmatter included). A section runs from its
// heading line to the next heading of the same or a higher level, so it
// contains its subsections.
//
// Lines are 1-based and End is inclusive, so a section is lines Start..End.
type Section struct {
	Index int    `json:"index"`
	Level int    `json:"level"`
	Title string `json:"title"`
	Path  string `json:"path"` // "History/Early life": the titles of its ancestors and itself
	Start int    `json:"start_line"`
	End   int    `json:"end_line"`
	Bytes int    `json:"bytes"`
}

// Lines splits a document into lines without their terminators. A trailing
// newline does not produce an empty last line.
func Lines(doc string) []string {
	if doc == "" {
		return nil
	}
	ls := strings.Split(doc, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// heading reports whether a line is an ATX heading, and its level and text.
func heading(line string) (int, string, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, "", false
	}
	lvl := 0
	for lvl < len(t) && t[lvl] == '#' {
		lvl++
	}
	if lvl == 0 || lvl > 6 {
		return 0, "", false
	}
	rest := t[lvl:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	rest = strings.TrimSpace(rest)
	// A closing sequence of #s is not part of the text.
	if s := strings.TrimRight(rest, "#"); s != rest && (s == "" || strings.HasSuffix(s, " ")) {
		rest = strings.TrimSpace(s)
	}
	return lvl, rest, true
}

// fence reports whether a line opens or closes a fenced code block.
func fence(line string) (string, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return "", false
	}
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, f) {
			n := 0
			for n < len(t) && t[n] == f[0] {
				n++
			}
			return t[:n], true
		}
	}
	return "", false
}

// Outline returns a document's sections. Headings inside fenced code and
// inside the frontmatter are not headings.
func Outline(doc string) []Section {
	lines := Lines(doc)
	_, _, bodyLine, _ := SplitFrontmatter(doc)
	secs := []Section{{Index: 0, Level: 0, Title: "(lead)", Path: "", Start: 1}}
	var stack []Section // open ancestors, for Path
	inFence := ""
	for i, l := range lines {
		if i < bodyLine {
			continue
		}
		if f, ok := fence(l); ok {
			switch {
			case inFence == "":
				inFence = f
			case strings.HasPrefix(f, inFence) && strings.TrimSpace(strings.TrimLeft(l, " "))[len(f):] == "":
				inFence = ""
			}
			continue
		}
		if inFence != "" {
			continue
		}
		lvl, title, ok := heading(l)
		if !ok {
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].Level >= lvl {
			stack = stack[:len(stack)-1]
		}
		path := title
		if len(stack) > 0 {
			path = stack[len(stack)-1].Path + "/" + title
		}
		s := Section{Index: len(secs), Level: lvl, Title: title, Path: path, Start: i + 1}
		secs = append(secs, s)
		stack = append(stack, s)
	}
	// Close each section: the line before the next heading at its level or
	// above, or the end of the document.
	for i := range secs {
		end := len(lines)
		for j := i + 1; j < len(secs); j++ {
			if secs[i].Level == 0 || secs[j].Level <= secs[i].Level {
				end = secs[j].Start - 1
				break
			}
		}
		secs[i].End = end
		b := 0
		for k := secs[i].Start - 1; k < end && k < len(lines); k++ {
			b += len(lines[k]) + 1
		}
		secs[i].Bytes = b
	}
	if secs[0].End < secs[0].Start { // empty lead
		secs[0].End = secs[0].Start - 1
	}
	return secs
}

// FindSection resolves a selector against a document's outline. The
// selector is a section index ("3"), a heading path ("History/Early life"),
// or a heading title ("Early life") that names exactly one section. Matching
// is case-insensitive and treats underscores as spaces.
func FindSection(doc, selector string) (Section, error) {
	secs := Outline(doc)
	sel := strings.TrimSpace(strings.ReplaceAll(selector, "_", " "))
	sel = strings.TrimPrefix(sel, "#")
	if n, err := strconv.Atoi(sel); err == nil {
		if n < 0 || n >= len(secs) {
			return Section{}, fmt.Errorf("section %d does not exist: the page has sections 0..%d", n, len(secs)-1)
		}
		return secs[n], nil
	}
	var byTitle []Section
	for _, s := range secs[1:] {
		if strings.EqualFold(s.Path, sel) {
			return s, nil
		}
		if strings.EqualFold(s.Title, sel) {
			byTitle = append(byTitle, s)
		}
	}
	switch len(byTitle) {
	case 1:
		return byTitle[0], nil
	case 0:
		return Section{}, fmt.Errorf("no section matches %q; read the page with outline=true to see its sections", selector)
	default:
		paths := make([]string, len(byTitle))
		for i, s := range byTitle {
			paths[i] = fmt.Sprintf("%d %q", s.Index, s.Path)
		}
		return Section{}, fmt.Errorf("%q names %d sections (%s); select one by index or full path", selector, len(byTitle), strings.Join(paths, ", "))
	}
}

// SliceLines returns lines start..end (1-based, inclusive) of a document,
// joined with newlines and with a trailing newline when non-empty.
func SliceLines(doc string, start, end int) string {
	lines := Lines(doc)
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n") + "\n"
}

// ReplaceSection replaces a whole section, its heading line and its
// subsections included, with text. An empty text deletes the section.
func ReplaceSection(doc, selector, text string) (string, Section, error) {
	s, err := FindSection(doc, selector)
	if err != nil {
		return "", s, err
	}
	lines := Lines(doc)
	before := strings.Join(lines[:s.Start-1], "\n")
	after := strings.Join(lines[s.End:], "\n")
	var b strings.Builder
	if before != "" {
		b.WriteString(before)
		b.WriteByte('\n')
	}
	if text != "" {
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	if after != "" {
		if text != "" && !strings.HasSuffix(text, "\n\n") && strings.TrimSpace(lines[s.End]) != "" {
			b.WriteByte('\n')
		}
		b.WriteString(after)
		b.WriteByte('\n')
	}
	return b.String(), s, nil
}
