# kb

A Wikipedia-style knowledge base for agents, in one SQLite file.

kb stores markdown pages with their full revision history, images and other
files, the links between pages (including *red links* to pages nobody has
written yet), categories, tasks and notes, all in a single SQLite database.
It serves them over the [Model Context Protocol](https://modelcontextprotocol.io)
on stdio or streamable HTTP. One CGO-free binary also includes a CLI and a
read-only web viewer.

The design target is an agent told to *"recreate Wikipedia from memory"*. The
kb is far larger than any context window, so the tools are built for working
without holding it:

- search, plus listings sorted by edit time, size or backlinks
- reading a long article one section at a time
- edits that cannot overwrite another agent's work
- a ranked list of the pages other pages already want
- tasks, notes and a change feed that carry progress from one session (or agent) to the next

## Install

```bash
sudo curl -fsSL https://xen0bit.github.io/kb/kb-archive-keyring.gpg \
     -o /usr/share/keyrings/kb-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/kb-archive-keyring.gpg] https://xen0bit.github.io/kb stable main" \
     | sudo tee /etc/apt/sources.list.d/kb.list
sudo apt update && sudo apt install kb
```

Packages exist for every Debian architecture (amd64, arm64, armhf, armel,
i386, ppc64el, riscv64, s390x). Tarballs, `.rpm` and `.apk` are on
[GitHub Releases](https://github.com/xen0bit/kb/releases). From source:
`make install` (needs Go 1.27 and [Bun](https://bun.sh)).

## Quick start

```bash
# Give an agent the kb (Claude Code shown; any MCP client works)
claude mcp add kb -- kb mcp

# Browse what it writes
kb serve                       # http://127.0.0.1:8080/
```

Install the agent skill so the agent knows the workflow and the pitfalls
before its first call:

```bash
kb skill install               # ~/.claude/skills/kb/SKILL.md; kb skill print to read it
```

The database lives at `$KB_DB`, else `$XDG_DATA_HOME/kb/kb.db` (usually
`~/.local/share/kb/kb.db`). Every command takes `-db PATH`.

Several agents can share one kb over HTTP:

```bash
kb mcp -http 127.0.0.1:8181                       # loopback: no token needed
KB_MCP_TOKEN=$(openssl rand -hex 32) kb mcp -http :8181   # network: bearer token required
kb serve -mcp                                     # viewer and MCP (/mcp) on one port
```

## Writing pages

Pages are CommonMark + GFM. Links use standard markdown with a `kb:` scheme:

```markdown
---
categories: [Physicists, Nobel laureates]
aliases: [Einstein]
status: stub
born: 1879-03-14            # any other key is infobox data
---
Albert Einstein was a [physicist](kb:Physicist) born in [Ulm](kb:Ulm).
See [his early life](kb:Albert_Einstein#Early_life). A destination with spaces
goes in angle brackets: [relativity](<kb:Theory of relativity>).

![Einstein in 1921](kb-file:Einstein_1921.jpg)
```

- **Titles** are case-insensitive keys with a capitalized first letter, as on
  Wikipedia. Use underscores for spaces in link destinations, or wrap the
  destination in `<…>`. A write warns if a destination has a bare space,
  because CommonMark doesn't parse that as a link.
- **Red links**, meaning links to pages that don't exist yet, are tracked.
  `special_pages kind=wanted` ranks them by how many pages want them.
- **Redirects** are pages whose frontmatter is `redirect: Target`.
  `move_page` leaves one automatically and rewrites inbound links.
- **Files** are uploaded as base64 and stored once per distinct content
  (SHA-256). Reference them as `kb-file:Name`.

## MCP tools

| Group | Tools |
|---|---|
| Orient | `kb_overview`: counts, latest changes, wanted pages, open tasks, recent notes and the writing conventions |
| Find | `search` (FTS5/BM25 with snippets, filters and sorts), `list_pages`, `resolve_titles` (batch exists/redirect/alias/missing plus did-you-mean), `special_pages` (wanted, orphans, dead_ends, stubs, uncategorized, short, long, redirects, broken_redirects, missing_files, unused_files, most_linked), `list_categories`, `recent_changes` (resumable with `after_id`) |
| Read | `read_page` (outline, a single section, a line range, an old revision; byte-capped and says where to continue), `page_links` (out, in, red), `page_history`, `diff_revisions` |
| Write | `create_page`, `write_page` (needs `base_rev`), `edit_page` (str_replace edits, replace a section, append, prepend), `bulk_write` (up to 50 pages), `move_page`, `delete_page`, `restore_page`, `revert_page` |
| Files | `upload_file`, `get_file` (images come back as image content), `list_files`, `delete_file` |
| Feedback | `kb_feedback`: agents report a tool that failed, misled them or is missing; read with `kb feedback ls` or `/feedback` in the viewer |
| Progress | `task_create`, `task_update`, `task_list`, `task_claim_next` (atomic and leased, so parallel agents never take the same task), `note_write`, `note_list` |

There are also resources `kb://guide`, `kb://page/{title}` and `kb://file/{name}`.

Every write reports the page's red links, missing files and malformed links.
Every result that can grow is paginated or byte-capped. Timestamps come back
as RFC 3339.

## CLI

```text
kb mcp [-http ADDR]                 kb serve [-listen ADDR] [-mcp]
kb get TITLE [-section S|-outline]  kb put TITLE [-f FILE] [-m MSG]
kb edit TITLE                       kb mv FROM TO     kb rm TITLE   kb restore TITLE
kb history TITLE   kb diff REV [REV] kb ls [-sort updated]   kb search QUERY
kb special KIND    kb recent        kb file put|get|ls|rm    kb task ls|add|done|claim
kb note add|ls     kb export DIR [-portable]   kb import DIR
kb db migrate|check|vacuum|stats|version|backup DEST
kb feedback ls [-all] | resolve ID [-m NOTE] [-wontfix]
```

`kb export` writes `pages/*.md`, `files/*` and a `manifest.json`, and
`kb import` reads that back without loss. Importing the same directory again
changes nothing. `-portable` rewrites `kb:` links as relative `.md` paths, so
the folder reads well in an editor or on a git forge.

## Web viewer

`kb serve` renders articles server-side. Pages have red links, an infobox
built from frontmatter, a table of contents and a "what links here" view.
There are also history and diffs, search with type-ahead, recent changes,
the special pages, categories, a file gallery, tasks and notes. The viewer is
read-only, and agent-written HTML is never passed through.

## Data and safety

- **Storage.** One SQLite file in WAL mode with full-text indexes (FTS5,
  porter stemming plus trigram matching on titles).
- **Schema changes.** The schema is a sequence of numbered migrations in
  `internal/store/migrations`, embedded in the binary. Opening a kb migrates
  it, and before migrating an existing kb, kb writes a copy to
  `kb.db.pre-vN.bak`. A kb created by a newer binary is refused.
- **Deletion.** Pages and files are soft-deleted, and history is never
  rewritten. `kb db vacuum` reclaims space.
- **Network access.** No tool touches the filesystem or the network. Over
  HTTP, binding to a non-loopback address requires `KB_MCP_TOKEN`. MCP
  clients send it as `Authorization: Bearer …`. Browsers open any page once
  with `?token=…`, which sets a cookie.

## Development

```bash
make test        # Go tests with -race, plus bun tests for the viewer scripts
make lint        # go vet and golangci-lint for both build-tag sets
make dev         # serve ./dev.db with assets from web/dist and MCP at /mcp
make snapshot    # goreleaser dry run
```

See [AGENTS.md](AGENTS.md) for the layout and conventions.

### Releasing

Push a `v*` tag. The release workflow builds and signs the artifacts, then
publishes the apt repository to GitHub Pages. One-time setup:

1. Create a signing key:
   `gpg --quick-generate-key "kb apt <…>" ed25519 sign never`.
2. Store the armored private key as the `APT_SIGNING_KEY` repository secret.
3. Commit the public key as `packaging/apt-signing-key.asc`. The release
   checks the secret against this file's fingerprint.
4. Enable GitHub Pages with "GitHub Actions" as the source.
