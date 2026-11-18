# AGENTS.md

Guidance for working in this repository. For usage see [README.md](README.md).

## What kb is

One CGO-free Go binary that keeps a Wikipedia-style markdown knowledge base
in a single SQLite file. It serves the kb over MCP (stdio, or streamable
HTTP) for agents, offers a CLI, and has a read-only web viewer for people.
The tools are designed for an agent whose kb is far larger than its context.
Keep every result bounded, paginated and explicit about how to continue.

## Layout

| Path | What lives there |
|---|---|
| `cmd/kb` | entry point |
| `internal/cli` | subcommands (stdlib `flag`, flags allowed after positionals) |
| `internal/wikitext` | pure document rules: title normalization and keys, `kb:`/`kb-file:` links, frontmatter, sections/outline, str_replace edits, diff, anchors |
| `internal/store` | **all SQL**: migrations, pages and revisions, indexing, search, special pages, files and blobs, tasks, notes, change feed |
| `internal/store/migrations` | `NNNN_description.sql`, embedded; numbered 1..N with no gaps |
| `internal/mcpserver` | MCP server, HTTP transport and auth, tool definitions (`tools.go` args, `register.go` handlers), resources |
| `internal/web` | viewer handlers, `html/template` templates, goldmark rendering with link rewriting |
| `web/src` | Bun project for the viewer's CSS and JS (`bun test`, `bun run build` → `web/dist`) |
| `web/` (Go) | package `webui`: embeds `web/dist` under `-tags embed_web` |
| `internal/exportfs` | export/import a folder of markdown and files |
| `internal/skill` | `SKILL.md`, the agent skill, embedded and installed by `kb skill install` |

## Rules every change follows

- **Schema changes go in a new migration file.** Never edit a migration that
  has shipped. Add `000N_what.sql`. The store applies it inside a transaction,
  sets `PRAGMA user_version`, and writes a `.pre-vN.bak` copy first. Add a
  test that upgrades a v(N-1) database.
- **Writes go through `store.savePage`**, so revisions, link, category and
  alias indexes, FTS and the change feed stay in one transaction. Never write
  `pages`, `links` or `pages_fts` directly from elsewhere.
- **Every write is attributable and conflict-checked.** It carries an author
  and comment and honours `base_rev`. Conflict messages name the current rev
  and its editor, so an agent can recover.
- **Errors wrap the store sentinels** (`ErrNotFound`, `ErrExists`,
  `ErrConflict`, `ErrInvalid`). Write messages that tell an agent what to do
  next ("re-read and reapply", "use create_page", "did you mean").
- **MCP results are text.** Several clients drop `structuredContent`, so the
  text has to carry the whole answer. Return JSON through `toJSON`, which
  rewrites `*_at` timestamps as RFC 3339. A tool that can grow must take
  `limit`/`offset` or `max_bytes` and report where to continue.
- **Tool descriptions are the documentation agents read.** When behaviour
  changes, update the description, `instructions.go`, `internal/skill/SKILL.md`
  and the README table together. Tools taking nested arrays carry a one-line
  JSON example in their description: agents guess shapes otherwise.
- **The viewer never trusts page text.** goldmark runs without
  `WithUnsafe`. Files are served under a sandboxing CSP. Templates use
  `html/template` only.
- **No tool touches the filesystem or the network.** That is the security
  model. HTTP on a non-loopback address requires `KB_MCP_TOKEN`.

## Build and test

```bash
make build            # ./kb; serves web/dist from disk, or a fallback stylesheet
make build-with-web   # ./kb with embedded assets (what releases ship)
make test             # go test -race ./... and bun test
make lint             # vet + golangci-lint (both tag sets)
make dev              # serve ./dev.db with live assets and MCP
make snapshot         # goreleaser dry run (8 architectures, deb/rpm/apk)
```

The most useful focused runs:

```bash
go test ./internal/wikitext/          # document rules
go test ./internal/store/ -run Move   # one store behaviour
go test ./internal/mcpserver/         # scripted agent session over in-memory MCP
go test ./internal/web/               # every viewer route
```

## CI and release

`ci.yml` runs:

- tests with `-race` on Go 1.27 and the latest Go, plus a check that
  `go mod tidy` changes nothing
- gofmt, vet and golangci-lint (pinned to v2.13.1)
- a cross-build for all 8 Debian architectures
- the bun tests and build
- govulncheck
- a full GoReleaser snapshot, which builds the apt repository with a
  throwaway key and has apt consume it on riscv64

`release.yml` runs on a `v*` tag. It runs GoReleaser (cosign keyless on the
checksums, SBOMs), checks `APT_SIGNING_KEY` against
`packaging/apt-signing-key.asc`, and publishes the signed apt repository to
GitHub Pages.
