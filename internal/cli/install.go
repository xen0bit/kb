package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// OpencodeEnv names the opencode binary to use, for when it is neither on
// PATH nor in the installer's ~/.opencode/bin.
const OpencodeEnv = "OPENCODE"

// runMCPInstall registers this kb binary as an MCP server with a client.
// It delegates to the client's own CLI so the config is always in the shape
// that client version expects, and installs the agent skill next to it.
func runMCPInstall(args []string) error {
	c := newCommand("mcp install", "opencode [-project] [-db PATH] [-name NAME] [-no-skill] [-force]")
	project := c.fs.Bool("project", false, "register in ./opencode.json (this project) instead of the global config")
	name := c.fs.String("name", "kb", "MCP server `name`")
	noSkill := c.fs.Bool("no-skill", false, "do not install the agent skill")
	force := c.fs.Bool("force", false, "replace an installed SKILL.md that differs from this version's")
	dryRun := c.fs.Bool("n", false, "print what would be done, change nothing")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give the client: kb mcp install opencode")
	}
	switch pos[0] {
	case "opencode":
	default:
		return usageErr("unknown client %q (supported: opencode)", pos[0])
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}

	// Pin the database only when the user chose one; otherwise the server
	// follows the same default as every other kb command.
	pinDB := os.Getenv("KB_DB") != ""
	c.fs.Visit(func(f *flag.Flag) {
		if f.Name == "db" {
			pinDB = true
		}
	})
	db := ""
	if pinDB {
		if db, err = filepath.Abs(*c.db); err != nil {
			return err
		}
	}

	oc, err := findOpencode()
	if err != nil {
		return fmt.Errorf("%w\n\nAdd this to opencode.json by hand instead:\n%s", err, opencodeSnippet(*name, self, db))
	}
	ocArgs := []string{"mcp", "add", *name}
	if !*project {
		ocArgs = append(ocArgs, "--global")
	}
	if db != "" {
		ocArgs = append(ocArgs, "--env", "KB_DB="+db)
	}
	ocArgs = append(ocArgs, "--", self, "mcp")
	if *dryRun {
		fmt.Printf("would run: %s %q\n", oc, ocArgs)
	} else {
		cmd := exec.Command(oc, ocArgs...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s mcp add: %w", oc, err)
		}
		fmt.Println("if opencode is already running, `opencode reload` picks this up")
	}

	if *noSkill {
		return nil
	}
	dir, skip, err := opencodeSkillDir(*project)
	if err != nil {
		return err
	}
	if skip != "" {
		fmt.Printf("skill: opencode already loads %s\n", skip)
		return nil
	}
	if *dryRun {
		fmt.Printf("would install %s\n", filepath.Join(dir, "SKILL.md"))
		return nil
	}
	return installSkill(dir, *force)
}

// findOpencode looks in $OPENCODE, then PATH, then where the opencode
// installer puts it (~/.opencode/bin), which is often not on PATH for
// non-interactive shells.
func findOpencode() (string, error) {
	if p := os.Getenv(OpencodeEnv); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".opencode", "bin", "opencode")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("opencode not found on PATH or in ~/.opencode/bin (set " + OpencodeEnv + " to its path)")
}

// opencodeSkillDir is where opencode looks for skills: the project's
// .opencode/skills, or the global config's skills directory. opencode also
// reads ~/.claude/skills, so an existing `kb skill install` there is enough
// and a second copy would only duplicate the skill.
func opencodeSkillDir(project bool) (dir, alreadyLoaded string, err error) {
	if project {
		return filepath.Join(".opencode", "skills", "kb"), "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	claude := filepath.Join(home, ".claude", "skills", "kb", "SKILL.md")
	if _, err := os.Stat(claude); err == nil {
		return "", claude, nil
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "opencode", "skills", "kb"), "", nil
}

func opencodeSnippet(name, self, db string) string {
	env := ""
	if db != "" {
		env = fmt.Sprintf(",\n        \"environment\": {\"KB_DB\": %q}", db)
	}
	return fmt.Sprintf(`{
  "mcp": {
    "servers": {
      %q: {
        "type": "local",
        "command": [%q, "mcp"]%s
      }
    }
  }
}`, name, self, env)
}
