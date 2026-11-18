package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCommands runs the CLI in-process against a scratch kb, so flag
// parsing and wiring are covered, not only the store underneath.
func TestCommands(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "kb.db")
	page := filepath.Join(dir, "p.md")
	if err := os.WriteFile(page, []byte("Links to [B](kb:B).\n\n# Part\n\nText.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\nxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	ok := [][]string{
		{"put", "A", "-f", page, "-db", db, "-m", "first"},
		{"get", "a", "-section", "Part", "-db", db},
		{"get", "A", "-outline", "-json", "-db", db},
		{"ls", "-db", db, "-sort", "updated"},
		{"search", "text", "-db", db},
		{"special", "wanted", "-db", db},
		{"mv", "A", "A2", "-db", db},
		{"history", "A2", "-db", db},
		{"diff", "1", "-db", db},
		{"file", "put", img, "-db", db},
		{"file", "ls", "-db", db},
		{"task", "add", "Write", "B", "-db", db},
		{"task", "claim", "-db", db},
		{"note", "add", "-topic", "progress", "hello", "-db", db},
		{"note", "ls", "-db", db},
		{"recent", "-since", "1h", "-db", db},
		{"export", filepath.Join(dir, "out"), "-db", db},
		{"import", filepath.Join(dir, "out"), "-db", filepath.Join(dir, "copy.db")},
		{"rm", "A2", "-db", db},
		{"restore", "A2", "-db", db},
		{"db", "check", "-db", db},
		{"db", "backup", filepath.Join(dir, "bak.db"), "-db", db},
		{"feedback", "ls", "-db", db},
		{"feedback", "ls", "-all", "-json", "-db", db},
		{"version"},
	}
	for _, args := range ok {
		if code := Run(args); code != 0 {
			t.Errorf("kb %v exited %d", args, code)
		}
	}
	for _, args := range [][]string{{"nope"}, {"get", "-db", db}, {"put", "A2", "-create", "-f", page, "-db", db}} {
		if code := Run(args); code == 0 {
			t.Errorf("kb %v succeeded", args)
		}
	}
}
