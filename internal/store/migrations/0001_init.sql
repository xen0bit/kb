-- kb schema v1. Times are unix milliseconds. A "key" is a normalized,
-- case-folded title or file name (see internal/wikitext): two titles with the
-- same key are the same page.

-- One row per page, pointing at its current revision. Deleting a page is a
-- soft delete: the row and its history stay, and restoring brings it back.
CREATE TABLE pages (
    id          INTEGER PRIMARY KEY,
    key         TEXT    NOT NULL UNIQUE,
    title       TEXT    NOT NULL,
    current_rev INTEGER NOT NULL DEFAULT 0,
    redirect_to TEXT,                           -- target key, for a redirect page
    status      TEXT    NOT NULL DEFAULT '',    -- frontmatter status: stub, draft, good, ...
    props       TEXT    NOT NULL DEFAULT '{}',  -- frontmatter beyond the recognised keys, JSON
    lead        TEXT    NOT NULL DEFAULT '',    -- first paragraph, plain text
    size        INTEGER NOT NULL DEFAULT 0,     -- bytes of the current revision
    words       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    created_by  TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    updated_by  TEXT    NOT NULL DEFAULT '',
    deleted_at  INTEGER
);
CREATE INDEX pages_updated ON pages (updated_at);
CREATE INDEX pages_created ON pages (created_at);
CREATE INDEX pages_status  ON pages (status, updated_at);
CREATE INDEX pages_size    ON pages (size);

-- Every revision of every page, kept forever.
CREATE TABLE revisions (
    id         INTEGER PRIMARY KEY,
    page_id    INTEGER NOT NULL REFERENCES pages (id),
    parent_id  INTEGER,
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL,
    comment    TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL DEFAULT '',
    sha256     TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX revisions_page   ON revisions (page_id, id);
CREATE INDEX revisions_author ON revisions (author, created_at);

-- kb: links between pages. The target is a key, not a page id, so a link to
-- a page that does not exist yet (a red link) is a row too: that is what
-- "wanted pages" counts, and what turns blue when the page is written.
CREATE TABLE links (
    src          INTEGER NOT NULL REFERENCES pages (id),
    target_key   TEXT    NOT NULL,
    target_title TEXT    NOT NULL,
    PRIMARY KEY (src, target_key)
) WITHOUT ROWID;
CREATE INDEX links_target ON links (target_key);

CREATE TABLE categories (
    page_id      INTEGER NOT NULL REFERENCES pages (id),
    category_key TEXT    NOT NULL,
    category     TEXT    NOT NULL,
    PRIMARY KEY (page_id, category_key)
) WITHOUT ROWID;
CREATE INDEX categories_key ON categories (category_key);

-- Alternative titles from frontmatter. A link to an alias is not a red link.
CREATE TABLE aliases (
    alias_key TEXT    NOT NULL,
    page_id   INTEGER NOT NULL REFERENCES pages (id),
    alias     TEXT    NOT NULL,
    PRIMARY KEY (alias_key, page_id)
) WITHOUT ROWID;
CREATE INDEX aliases_page ON aliases (page_id);

-- File contents, addressed by hash: the same bytes uploaded under two names,
-- or re-uploaded unchanged, are stored once. Deliberately a rowid table: a
-- WITHOUT ROWID table stores its rows in the primary key's b-tree, which is
-- the wrong place for multi-megabyte values.
CREATE TABLE blobs (
    id     INTEGER PRIMARY KEY,
    sha256 TEXT    NOT NULL UNIQUE,
    size   INTEGER NOT NULL,
    data   BLOB    NOT NULL
);

CREATE TABLE files (
    id          INTEGER PRIMARY KEY,
    key         TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    mime        TEXT    NOT NULL,
    sha256      TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    current_rev INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    created_by  TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    updated_by  TEXT    NOT NULL DEFAULT '',
    deleted_at  INTEGER
);
CREATE INDEX files_updated ON files (updated_at);

CREATE TABLE file_revisions (
    id         INTEGER PRIMARY KEY,
    file_id    INTEGER NOT NULL REFERENCES files (id),
    sha256     TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    mime       TEXT    NOT NULL,
    comment    TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX file_revisions_file ON file_revisions (file_id, id);

-- kb-file: references from pages, so a file knows where it is used and a
-- reference to a file nobody uploaded is findable.
CREATE TABLE file_links (
    src      INTEGER NOT NULL REFERENCES pages (id),
    file_key TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    PRIMARY KEY (src, file_key)
) WITHOUT ROWID;
CREATE INDEX file_links_key ON file_links (file_key);

-- Full-text search. rowid is the page id. Maintained by the store in the
-- same transaction as the write it indexes.
CREATE VIRTUAL TABLE pages_fts USING fts5 (
    title, aliases, body,
    tokenize = 'porter unicode61 remove_diacritics 2'
);

-- Trigram index over titles and aliases, for "did you mean" and substring
-- title search. rowid is the page id.
CREATE VIRTUAL TABLE titles_fts USING fts5 (
    title,
    tokenize = 'trigram'
);

-- Work tracking for agents: a tree of tasks with leases, so several agents
-- can share one backlog without two of them taking the same item.
CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY,
    parent_id   INTEGER REFERENCES tasks (id),
    title       TEXT    NOT NULL,
    body        TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT 'todo', -- todo, doing, blocked, done, dropped
    priority    INTEGER NOT NULL DEFAULT 0,      -- higher first
    assignee    TEXT    NOT NULL DEFAULT '',
    lease_until INTEGER,
    page_key    TEXT,
    created_at  INTEGER NOT NULL,
    created_by  TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL
);
CREATE INDEX tasks_status ON tasks (status, priority DESC, id);
CREATE INDEX tasks_parent ON tasks (parent_id);

-- Free-form notes: progress logs, plans, scratch, and talk-page notes on a
-- page when page_key is set.
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY,
    topic      TEXT    NOT NULL DEFAULT '',
    body       TEXT    NOT NULL,
    author     TEXT    NOT NULL DEFAULT '',
    page_key   TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX notes_topic   ON notes (topic, updated_at);
CREATE INDEX notes_page    ON notes (page_key, updated_at);
CREATE INDEX notes_updated ON notes (updated_at);

CREATE VIRTUAL TABLE notes_fts USING fts5 (
    topic, body,
    tokenize = 'porter unicode61 remove_diacritics 2'
);

-- Append-only feed of every change, for recent changes and for an agent to
-- pick up where it left off: remember the last id, ask for what came after.
CREATE TABLE changes (
    id      INTEGER PRIMARY KEY,
    at      INTEGER NOT NULL,
    kind    TEXT    NOT NULL, -- page.create, page.edit, page.move, ..., file.upload, task.update, note.write
    target  TEXT    NOT NULL, -- page title, file name, task title or note topic
    ref_id  INTEGER NOT NULL DEFAULT 0,
    rev     INTEGER NOT NULL DEFAULT 0,
    size_delta INTEGER NOT NULL DEFAULT 0,
    author  TEXT    NOT NULL DEFAULT '',
    comment TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX changes_at     ON changes (at);
CREATE INDEX changes_author ON changes (author, id);
