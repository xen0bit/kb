package wikitext

import (
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Meta is what a page's YAML frontmatter says about it. The recognised keys
// are lifted out; everything else is kept in Props, which is how a page
// carries infobox-style structured data (born: 1879, field: physics, ...).
//
//	---
//	categories: [Physicists, Nobel laureates]
//	aliases: [Einstein]
//	status: good
//	redirect: Some Other Page
//	born: 1879-03-14
//	---
type Meta struct {
	Categories []string       `json:"categories,omitempty"`
	Aliases    []string       `json:"aliases,omitempty"`
	Status     string         `json:"status,omitempty"`
	Redirect   string         `json:"redirect,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Props      map[string]any `json:"props,omitempty"`
}

// SplitFrontmatter separates a leading "---" YAML block from the markdown
// that follows. bodyLine is the 0-based line the markdown starts on, so line
// numbers reported against the body can be mapped back to the document.
func SplitFrontmatter(doc string) (front, body string, bodyLine int, ok bool) {
	if !strings.HasPrefix(doc, "---\n") && !strings.HasPrefix(doc, "---\r\n") {
		return "", doc, 0, false
	}
	start := strings.IndexByte(doc, '\n') + 1
	rest := doc[start:]
	line := 1
	for off := 0; off <= len(rest); {
		end := strings.IndexByte(rest[off:], '\n')
		var l string
		if end < 0 {
			l = rest[off:]
		} else {
			l = rest[off : off+end]
		}
		if t := strings.TrimRight(l, " \t\r"); t == "---" || t == "..." {
			front = rest[:off]
			if end < 0 {
				return front, "", line + 1, true
			}
			return front, rest[off+end+1:], line + 1, true
		}
		if end < 0 {
			break
		}
		off += end + 1
		line++
	}
	// An opening fence with no closing one is not frontmatter.
	return "", doc, 0, false
}

// ParseMeta reads a document's frontmatter. A document without frontmatter
// has an empty Meta and no error; malformed YAML is an error, because a
// silently ignored category is worse than a rejected edit.
func ParseMeta(doc string) (Meta, error) {
	front, _, _, ok := SplitFrontmatter(doc)
	var m Meta
	if !ok || strings.TrimSpace(front) == "" {
		return m, nil
	}
	raw := map[string]any{}
	if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
		return m, fmt.Errorf("frontmatter is not valid YAML: %w", err)
	}
	take := func(key string) (any, bool) {
		for k, v := range raw {
			if strings.EqualFold(k, key) {
				delete(raw, k)
				return v, true
			}
		}
		return nil, false
	}
	var err error
	if v, ok := take("categories"); ok {
		if m.Categories, err = stringList(v); err != nil {
			return m, fmt.Errorf("frontmatter categories: %w", err)
		}
	} else if v, ok := take("category"); ok {
		if m.Categories, err = stringList(v); err != nil {
			return m, fmt.Errorf("frontmatter category: %w", err)
		}
	}
	if v, ok := take("aliases"); ok {
		if m.Aliases, err = stringList(v); err != nil {
			return m, fmt.Errorf("frontmatter aliases: %w", err)
		}
	}
	if v, ok := take("status"); ok {
		m.Status = strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	}
	if v, ok := take("redirect"); ok {
		m.Redirect = strings.TrimSpace(fmt.Sprint(v))
		if k, t, _ := ParseDestination(m.Redirect); k == "page" {
			m.Redirect = t
		}
	}
	if v, ok := take("summary"); ok {
		m.Summary = strings.TrimSpace(fmt.Sprint(v))
	}
	if len(raw) > 0 {
		m.Props = jsonSafe(raw).(map[string]any)
	}
	m.Categories = dedupeTitles(m.Categories)
	m.Aliases = dedupeTitles(m.Aliases)
	return m, nil
}

func stringList(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		var out []string
		for p := range strings.SplitSeq(t, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			switch e.(type) {
			case map[string]any, []any:
				return nil, fmt.Errorf("expected a list of strings, found a nested %T", e)
			}
			out = append(out, strings.TrimSpace(fmt.Sprint(e)))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected a string or a list of strings, found %T", v)
	}
}

// dedupeTitles normalizes each entry as a title and drops duplicates and
// entries that are not valid titles.
func dedupeTitles(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		n, err := NormalizeTitle(s)
		if err != nil {
			continue
		}
		k := strings.ToLower(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// jsonSafe converts what yaml.v3 decodes into values encoding/json accepts:
// map[any]any never appears with v3's map[string]any target, but nested maps
// with non-string keys and time values can.
func jsonSafe(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = jsonSafe(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = jsonSafe(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jsonSafe(e)
		}
		return out
	case string, bool, int, int64, float64, nil:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// SortedPropKeys returns the keys of props in a stable order, for rendering.
func SortedPropKeys(props map[string]any) []string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
