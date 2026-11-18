package mcpserver

// instructions is the guidance a connected client hands its model.
const instructions = `kb is a Wikipedia-style knowledge base of markdown pages and files, with full revision history. It is larger than your context: find things with search, list_pages and special_pages instead of reading everything, and read long pages by section.

Start every session with kb_overview: it shows the size of the kb, the latest changes, open tasks, recent notes and the most-wanted missing pages. Record progress as you go (task_* and note_write) so the next session, or another agent, can continue; resume with recent_changes after_id=<last id you saw>.

If a kb tool confuses you, fails unexpectedly or is missing something you need, say so with kb_feedback (one or two sentences, then carry on).

` + conventions

// conventions is the authoring guide, repeated in kb_overview because some
// clients never show a model the server instructions.
const conventions = `Writing pages:
- Link pages with standard markdown: [label](kb:Page_Title) or [label](kb:Page_Title#Section). Use underscores for spaces, or wrap the destination in angle brackets: [label](<kb:Page Title>). Parentheses work as written: [Mercury](kb:Mercury_(planet)). Titles are case-insensitive except for display; the first letter is capitalized.
- Embed files with ![alt](kb-file:Name.png); link a non-image as [label](kb-file:Name.pdf). Upload with upload_file (base64).
- Optional YAML frontmatter at the top holds metadata:
  ---
  categories: [Physicists, Nobel laureates]
  aliases: [Einstein]          # other titles that should reach this page
  status: stub                  # stub | draft | good | ... (your convention; special_pages kind=stubs lists stubs)
  summary: One-line summary     # optional; otherwise the first paragraph is used
  born: 1879-03-14              # any other key is structured data (an infobox)
  ---
- A redirect is a page whose frontmatter is just: redirect: Target Title. move_page leaves one automatically.
- A link to a page that does not exist yet is a red link: it is fine and expected. special_pages kind=wanted ranks missing pages by how many pages link to them, which is the natural queue of what to write next. Every write reports its red_links.
- Before linking many titles, resolve_titles tells you in one call which exist, which are redirects or aliases, and suggests near matches.

Editing safely:
- Every read and write returns rev: a revision id, unique across the whole kb (not a per-page counter, so a page's revs can jump from 1 to 5). write_page needs base_rev, the rev you last read or wrote; if someone else edited since, you get a conflict: re-read and reapply.
- Prefer edit_page for changes: str_replace edits (old_text must match exactly once), replace one section, or append/prepend. Use bulk_write to create many pages in one call.
- Give a short comment on each edit; it shows in history and recent_changes.`
