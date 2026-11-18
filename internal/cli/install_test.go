package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMCPInstallOpencode runs `kb mcp install opencode` against a fake
// opencode that records its arguments.
func TestMCPInstallOpencode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake opencode is a shell script")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfg := filepath.Join(dir, "cfg")
	argsFile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("KB_DB", "")
	t.Setenv(OpencodeEnv, fake)
	stdout := os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()

	recorded := func() []string {
		t.Helper()
		b, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}

	// Default: global, no pinned database, skill in opencode's config.
	if code := Run([]string{"mcp", "install", "opencode"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := recorded()
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	want := []string{"mcp", "add", "kb", "--global", "--", self, "mcp"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(cfg, "opencode", "skills", "kb", "SKILL.md")); err != nil {
		t.Errorf("skill not installed: %v", err)
	}
	// Running it again is fine (opencode replaces the entry; the skill is unchanged).
	if code := Run([]string{"mcp", "install", "opencode"}); code != 0 {
		t.Fatalf("second install exit %d", code)
	}

	// An explicit -db is pinned as an absolute KB_DB.
	if code := Run([]string{"mcp", "install", "opencode", "-db", "rel.db", "-name", "kb2", "-no-skill"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	abs, _ := filepath.Abs("rel.db")
	got = recorded()
	if strings.Join(got[:6], " ") != "mcp add kb2 --global --env KB_DB="+abs {
		t.Errorf("pinned db args: %q", got)
	}

	// With the Claude Code skill present, opencode already loads it.
	claude := filepath.Join(home, ".claude", "skills", "kb")
	if code := Run([]string{"skill", "install", "-dir", claude}); code != 0 {
		t.Fatal("skill install")
	}
	if d, skip, _ := opencodeSkillDir(false); skip == "" {
		t.Errorf("expected skip, got dir %s", d)
	}

	for _, args := range [][]string{{"mcp", "install"}, {"mcp", "install", "cursor"}} {
		if code := Run(args); code == 0 {
			t.Errorf("kb %v succeeded", args)
		}
	}
	t.Setenv(OpencodeEnv, filepath.Join(dir, "missing"))
	if code := Run([]string{"mcp", "install", "opencode", "-no-skill"}); code == 0 {
		t.Error("missing opencode binary succeeded")
	}
}
