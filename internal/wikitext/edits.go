package wikitext

import (
	"fmt"
	"strings"
)

// Edit replaces OldText, which must occur exactly once in the document as it
// stands when the edit is applied, with NewText.
type Edit struct {
	OldText string `json:"old_text" jsonschema:"exact text to replace; must occur exactly once at the time this edit applies. Include surrounding context to make it unique"`
	NewText string `json:"new_text" jsonschema:"replacement text; empty deletes old_text"`
}

// ApplyEdits applies edits in order. If any edit cannot apply, the document
// is returned unchanged with an error naming the edit.
func ApplyEdits(doc string, edits []Edit) (string, error) {
	if len(edits) == 0 {
		return doc, fmt.Errorf("no edits given")
	}
	out := doc
	for i, e := range edits {
		if e.OldText == "" {
			return doc, fmt.Errorf("edit %d: old_text is empty (use append or prepend to add text)", i+1)
		}
		n := strings.Count(out, e.OldText)
		switch n {
		case 1:
			out = strings.Replace(out, e.OldText, e.NewText, 1)
		case 0:
			return doc, fmt.Errorf("edit %d: old_text not found%s", i+1, nearMiss(out, e.OldText))
		default:
			return doc, fmt.Errorf("edit %d: old_text occurs %d times; include more surrounding text to make it unique", i+1, n)
		}
	}
	return out, nil
}

// nearMiss helps an agent fix an edit that failed on whitespace or case.
func nearMiss(doc, old string) string {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	if strings.Contains(norm(doc), norm(old)) {
		return " (it does occur if whitespace and case are ignored: re-read the page and copy the text exactly)"
	}
	first, _, _ := strings.Cut(strings.TrimSpace(old), "\n")
	if len(first) > 12 && strings.Contains(doc, first) {
		return fmt.Sprintf(" (its first line %q does occur: the rest differs)", truncateRunes(first, 60))
	}
	return " (re-read the page: it may have changed)"
}
