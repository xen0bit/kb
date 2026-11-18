// Command kb is a Wikipedia-style knowledge base for agents: markdown pages,
// files, history, search and red links in one SQLite file, served over MCP
// (stdio or HTTP) and browsable in a read-only web viewer.
package main

import (
	"os"

	"github.com/xen0bit/kb/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
