// Package cli is the kb command line: the MCP server, the web viewer, and
// the same operations the MCP tools offer, for people and scripts.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/xen0bit/kb/internal/buildinfo"
	"github.com/xen0bit/kb/internal/mcpserver"
	"github.com/xen0bit/kb/internal/store"
)

const usage = `kb - a Wikipedia-style knowledge base for agents, in one SQLite file

Serve:
  kb mcp [-http ADDR]               MCP over stdio, or streamable HTTP on ADDR
  kb serve [-listen ADDR] [-mcp]    read-only web viewer; -mcp also serves MCP at /mcp

Pages:
  kb get TITLE [-rev N] [-section S] [-outline]
  kb put TITLE [-f FILE] [-m COMMENT] [-create]   text from FILE or stdin
  kb edit TITLE                     edit in $EDITOR
  kb mv FROM TO [-no-redirect] [-no-rewrite]
  kb rm TITLE                       soft delete (kb restore TITLE undoes it)
  kb restore TITLE
  kb history TITLE
  kb diff FROM_REV [TO_REV]
  kb ls [-sort updated] [-category C] [-status S] [-prefix P]
  kb search QUERY
  kb special KIND                   wanted, orphans, dead_ends, stubs, ...
  kb recent [-since 24h]

Files, work, notes:
  kb file put PATH [-name N] [-desc D] | get NAME [-o PATH] | ls | rm NAME
  kb task ls [-all] | add TITLE [-p N] [-parent ID] | done ID | claim
  kb note add [-topic T] TEXT | ls [-topic T] [-q QUERY]

Data:
  kb export DIR [-portable]         pages/*.md, files/*, manifest.json
  kb import DIR                     the reverse; re-importing is a no-op
  kb db migrate|check|vacuum|stats|version|backup DEST

Every command takes -db PATH (default $KB_DB, else $XDG_DATA_HOME/kb/kb.db).
Listing commands take -json. kb version prints the build.
`

// Run executes the command line and returns the exit code.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(buildinfo.String())
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return 0
	case "mcp":
		err = runMCP(ctx, rest)
	case "serve":
		err = runServe(ctx, rest)
	case "get", "cat":
		err = runGet(ctx, rest)
	case "put":
		err = runPut(ctx, rest)
	case "edit":
		err = runEdit(ctx, rest)
	case "mv", "move":
		err = runMove(ctx, rest)
	case "rm", "delete":
		err = runDelete(ctx, rest)
	case "restore":
		err = runRestore(ctx, rest)
	case "history", "log":
		err = runHistory(ctx, rest)
	case "diff":
		err = runDiff(ctx, rest)
	case "ls", "list":
		err = runList(ctx, rest)
	case "search", "find":
		err = runSearch(ctx, rest)
	case "special":
		err = runSpecial(ctx, rest)
	case "recent":
		err = runRecent(ctx, rest)
	case "file":
		err = runFile(ctx, rest)
	case "task":
		err = runTask(ctx, rest)
	case "note":
		err = runNote(ctx, rest)
	case "export":
		err = runExport(ctx, rest)
	case "import":
		err = runImport(ctx, rest)
	case "db":
		err = runDB(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "kb: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(os.Stderr, "kb %s: %v\n", cmd, err)
		return 2
	default:
		fmt.Fprintf(os.Stderr, "kb %s: %v\n", cmd, err)
		return 1
	}
}

var errUsage = errors.New("usage")

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

// DefaultDB is where the kb lives when neither -db nor $KB_DB says.
func DefaultDB() string {
	if p := os.Getenv("KB_DB"); p != "" {
		return p
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "kb.db"
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "kb", "kb.db")
}

// command is a subcommand's flag set plus the flags every command shares.
type command struct {
	fs     *flag.FlagSet
	db     *string
	asJSON *bool
}

func newCommand(name, synopsis string) *command {
	fs := flag.NewFlagSet("kb "+name, flag.ContinueOnError)
	c := &command{fs: fs}
	c.db = fs.String("db", DefaultDB(), "kb database `path`")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: kb %s %s\n", name, synopsis)
		fs.PrintDefaults()
	}
	return c
}

func (c *command) withJSON() *command {
	c.asJSON = c.fs.Bool("json", false, "print JSON")
	return c
}

// parse accepts flags before and after positional arguments, so
// `kb get Title -rev 3` works as well as `kb get -rev 3 Title`.
func (c *command) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := c.fs.Parse(args); err != nil {
			return nil, err
		}
		rest := c.fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (c *command) open() (*store.Store, error) {
	logger := mcpserver.NewLogger(slog.LevelWarn)
	return store.Open(*c.db, store.Options{Logger: logger})
}

func (c *command) json() bool { return c.asJSON != nil && *c.asJSON }

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(fn func(w io.Writer)) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fn(tw)
	_ = tw.Flush()
}

func when(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

func author(given string) string {
	if given != "" {
		return given
	}
	if a := os.Getenv("KB_AUTHOR"); a != "" {
		return a
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "cli"
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
