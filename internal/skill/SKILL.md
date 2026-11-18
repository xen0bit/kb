---
name: kb
description: Build and maintain a Wikipedia-style knowledge base with the kb MCP server (tools named kb_overview, search, read_page, bulk_write, edit_page, upload_file, task_*, note_*). Use whenever a task involves writing, organizing, researching into, or resuming work on a kb, including large multi-session efforts such as writing hundreds of linked articles.
---

# Working with kb

kb is a markdown encyclopedia with full history, files, red links, tasks and notes, served over MCP. It is usually far larger than your context: never try to read all of it. Find things with `search`, `list_pages` and `special_pages`, and read long pages by section.

## Before the first call

If your client lists only tool names (for example Claude Code shows `mcp__kb__*` as deferred tools), load the full definitions of the tools you will use before calling them. Guessing argument names is the most common way to waste calls: the shapes below are the ones that trip agents up.

| Tool | Shape |
|---|---|
| `task_create` | `{"tasks":[{"title":"Write Venus","body":"...","priority":2,"page":"Venus"}]}` or one task: `{"title":"..."}` |
| `task_update` | `{"id":3,"status":"done"}` or `{"ids":[1,2,3],"status":"done","append":"log line"}` |
| `bulk_write` | `{"pages":[{"title":"Venus","body":"..."}],"comment":"..."}`: the field is `body`, not `content` |
| `edit_page` | `{"title":"Venus","edits":[{"old_text":"...","new_text":"..."}],"comment":"..."}` |
| `upload_file` | text files (SVG, CSV, JSON): `{"name":"Orbits.svg","text":"<svg ...>"}`; binary: `data_base64` |

## Workflow

1. **Orient.** Call `kb_overview`. Note `stats.last_change_id`, the open tasks, the recent notes and the wanted pages. When resuming, read the `progress` notes (`note_list topic=progress`) and `recent_changes after_id=<id from your last note>`.
2. **Plan as tasks** before writing: one task per article or chunk of work, with `page` set. For large efforts nest them (`parent_id`) and take work with `task_claim_next`, which is safe when several agents share the kb.
3. **Check titles** with one `resolve_titles` call for everything you plan to write or link. It tells you what exists, what redirects, and suggests disambiguations ("Mercury" → "Mercury (planet)").
4. **Write.** Use `bulk_write` for new pages (up to 50 per call). Its `red_links` are computed after the whole batch, so links between pages of one batch are not red.
5. **Change** existing pages with `edit_page`, not `write_page`: str_replace edits, one section, or append. `write_page` replaces everything and needs `base_rev`.
6. **Verify.** Use `page_links` (`direction=red` for one page) and `special_pages kind=wanted` for the kb as a whole.
7. **Record.** Close tasks in one `task_update ids=[...]` call, and keep one running note per effort (`note_write topic=progress`, then `id` + `append=true`): what is done, what is next, and the last change id.

## Page conventions

```markdown
---
categories: [Planets, Terrestrial planets]
aliases: [Morning star]       # other titles that reach this page
status: draft                 # stub | draft | good: stubs are listed by special_pages kind=stubs
summary: One line for listings.
mass_kg: 4.87e24              # any other key is infobox data; reuse names other pages use
---
**Venus** is the second planet from the [Sun](kb:Sun).

## Atmosphere
...
```

- **Links:** `[label](kb:Page_Title)`. Spaces become underscores. Parentheses work as written: `[Mercury](kb:Mercury_(planet))`. `[label](<kb:Page Title>)` also works. A bare space (`kb:Page Title`) is **not** a link; writes warn about it.
- **Sections:** `[label](kb:Venus#Atmosphere)`.
- **Files:** `![alt](kb-file:Orbits.svg)`. `upload_file` returns the exact markdown to paste.
- **Red links** to pages you have not written are expected: they become `special_pages kind=wanted`, the queue of what to write next. Prefer linking a missing title to leaving it unlinked.
- **Redirects** are pages whose frontmatter is just `redirect: Target`. `move_page` leaves one and rewrites inbound links.
- **Infobox fields:** before inventing names, check how an existing page in the same category names them (`read_page` shows `props`).

## Pitfalls

- `rev` is a revision id unique across the whole kb, not a per-page counter. Pass back exactly the `rev` you last read or wrote as `base_rev`.
- **Conflicts:** a conflict means someone else edited. Re-read (by section if long), reapply your change, and never blindly overwrite.
- **Failed `old_text`:** if `old_text` does not match, the error says whether whitespace or case differed. Re-read the section and copy the text exactly.
- **Long pages:** `read_page outline=true` first, then `section=...`. Output is capped; the header's `continue` says where to resume.
- **Search syntax:** `search` takes plain words (all must match, stemmed) or FTS5 syntax: `"exact phrase"`, `prefix*`, `a OR b`, `title:word`.

## Feedback

If a kb tool confuses you, fails in a way its error does not explain, or lacks something you need, call `kb_feedback` right then (kind `bug`, `friction` or `idea`, the tool name, one or two sentences), and carry on.
