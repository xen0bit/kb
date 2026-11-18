package wikitext

import (
	"strings"
	"testing"
)

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"albert_einstein", "Albert einstein"},
		{"  Albert   Einstein ", "Albert Einstein"},
		{"éclair", "Éclair"},
		{"iPhone", "IPhone"},
		{"C++", "C++"},
	}
	for _, c := range cases {
		got, err := NormalizeTitle(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormalizeTitle(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "   ", "a#b", "x[y]", "a|b", "tab\x01"} {
		if _, err := NormalizeTitle(bad); err == nil {
			t.Errorf("NormalizeTitle(%q) succeeded, want error", bad)
		}
	}
	if Key("albert_EINSTEIN") != Key("Albert Einstein") {
		t.Error("keys differ for titles that should be the same page")
	}
}

func TestParseDestination(t *testing.T) {
	k, tg, f := ParseDestination("kb:Albert_Einstein#Early_life")
	if k != "page" || tg != "Albert_Einstein" || f != "Early life" {
		t.Errorf("got %q %q %q", k, tg, f)
	}
	k, tg, _ = ParseDestination("KB-FILE:Photo%20one.jpg")
	if k != "file" || tg != "Photo one.jpg" {
		t.Errorf("got %q %q", k, tg)
	}
	if k, _, _ := ParseDestination("https://example.com"); k != "" {
		t.Errorf("external link classified as %q", k)
	}
	if got := URLForm("Foo (band)"); got != "Foo_%28band%29" {
		t.Errorf("URLForm = %q", got)
	}
	k, tg, _ = ParseDestination("kb:" + URLForm("Foo (band)"))
	if k != "page" || Key(tg) != Key("Foo (band)") {
		t.Errorf("round trip failed: %q", tg)
	}
}

func TestFileName(t *testing.T) {
	n, err := NormalizeFileName("photo_of a.jpg")
	if err != nil || n != "photo of a.jpg" {
		t.Errorf("got %q %v", n, err)
	}
	if _, err := NormalizeFileName("../etc/passwd"); err == nil {
		t.Error("path separator accepted")
	}
}

const sample = `---
categories: [Physicists, "Nobel laureates", physicists]
aliases: Einstein, A. Einstein
status: Good
born: 1879
spouse:
  - Mileva Marić
---
Albert Einstein was a [physicist](kb:Physicist) known for [relativity](kb:Theory_of_relativity#Special).

![portrait](kb-file:Einstein_1921.jpg) See also [the web](https://example.com) and [again](kb:physicist).

# Life

## Early life

Born in [Ulm](<kb:Ulm Germany>).

` + "```" + `
# not a heading
` + "```" + `

## Later life

Text.

# Legacy

End.
`

func TestAnalyze(t *testing.T) {
	d, err := Analyze(sample)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.Meta.Categories, ","); got != "Physicists,Nobel laureates" {
		t.Errorf("categories = %q", got)
	}
	if got := strings.Join(d.Meta.Aliases, ","); got != "Einstein,A. Einstein" {
		t.Errorf("aliases = %q", got)
	}
	if d.Meta.Status != "good" {
		t.Errorf("status = %q", d.Meta.Status)
	}
	if d.Meta.Props["born"] != 1879 {
		t.Errorf("props = %#v", d.Meta.Props)
	}
	var keys []string
	for _, l := range d.Links {
		keys = append(keys, l.Key)
	}
	if got := strings.Join(keys, ","); got != "physicist,theory of relativity,ulm germany" {
		t.Errorf("links = %q", got)
	}
	if len(d.Files) != 1 || d.Files[0] != "Einstein 1921.jpg" {
		t.Errorf("files = %q", d.Files)
	}
	if d.External != 1 {
		t.Errorf("external = %d", d.External)
	}
	if !strings.HasPrefix(d.Lead, "Albert Einstein was a physicist known for relativity.") {
		t.Errorf("lead = %q", d.Lead)
	}
	if _, err := Analyze("---\n: [bad\n---\nx"); err == nil {
		t.Error("bad YAML accepted")
	}
}

func TestOutlineAndSections(t *testing.T) {
	secs := Outline(sample)
	var paths []string
	for _, s := range secs {
		paths = append(paths, s.Path)
	}
	if got := strings.Join(paths, "|"); got != "|Life|Life/Early life|Life/Later life|Legacy" {
		t.Fatalf("paths = %q", got)
	}
	s, err := FindSection(sample, "early_life")
	if err != nil {
		t.Fatal(err)
	}
	text := SliceLines(sample, s.Start, s.End)
	if !strings.HasPrefix(text, "## Early life") || !strings.Contains(text, "# not a heading") || strings.Contains(text, "Later") {
		t.Errorf("section text = %q", text)
	}
	life, _ := FindSection(sample, "Life")
	if !strings.Contains(SliceLines(sample, life.Start, life.End), "Later life") {
		t.Error("section does not contain its subsections")
	}
	out, _, err := ReplaceSection(sample, "Life/Later life", "## Later life\n\nRewritten.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Rewritten.\n\n# Legacy") || strings.Contains(out, "\nText.\n") {
		t.Errorf("replace produced %q", out)
	}
	if _, err := FindSection(sample, "Nope"); err == nil {
		t.Error("missing section found")
	}
	if lead := secs[0]; lead.Start != 1 || lead.End != secs[1].Start-1 {
		t.Errorf("lead = %+v", lead)
	}
}

func TestApplyEdits(t *testing.T) {
	out, err := ApplyEdits("a b c b", []Edit{{OldText: "a", NewText: "A"}, {OldText: "c b", NewText: "C"}})
	if err != nil || out != "A b C" {
		t.Errorf("got %q %v", out, err)
	}
	if _, err := ApplyEdits("a b c b", []Edit{{OldText: "b", NewText: "x"}}); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Errorf("ambiguous edit: %v", err)
	}
	if _, err := ApplyEdits("Hello  World", []Edit{{OldText: "hello world", NewText: "x"}}); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("near miss: %v", err)
	}
}

func TestRewriteLinks(t *testing.T) {
	doc := "See [P](kb:physicist), [Q](kb:Physicist#Training \"t\"), [R](<kb:Physicist>) and [S](kb:Physics)."
	out, n := RewriteLinks(doc, Key("Physicist"), "Physicist (profession)")
	if n != 3 {
		t.Errorf("rewrote %d links: %q", n, out)
	}
	want := "See [P](kb:Physicist_%28profession%29), [Q](kb:Physicist_%28profession%29#Training \"t\"), [R](<kb:Physicist (profession)>) and [S](kb:Physics)."
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	d, _ := Analyze(out)
	for _, l := range d.Links[:1] {
		if l.Key != Key("Physicist (profession)") {
			t.Errorf("rewritten link resolves to %q", l.Key)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	b := "1\n2\nthree\n4\n5\n6\n7\n8\n9\n10\neleven\n"
	d := UnifiedDiff("a", "b", a, b, 1)
	want := "--- a\n+++ b\n@@ -2,3 +2,3 @@\n 2\n-3\n+three\n 4\n@@ -10,1 +10,2 @@\n 10\n+eleven\n"
	if d != want {
		t.Errorf("diff:\n%s\nwant:\n%s", d, want)
	}
	if UnifiedDiff("a", "b", a, a, 3) != "" {
		t.Error("identical docs diff")
	}
}
