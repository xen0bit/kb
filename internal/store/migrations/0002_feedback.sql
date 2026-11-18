-- kb schema v2: feedback about kb itself, from the agents using it.
-- Kept apart from notes: notes are part of the kb's content, feedback is
-- about the tool (a confusing description, a failed call, a missing
-- feature) and is read by whoever maintains kb.
CREATE TABLE feedback (
    id         INTEGER PRIMARY KEY,
    at         INTEGER NOT NULL,
    kind       TEXT    NOT NULL, -- bug, friction, idea, praise
    tool       TEXT    NOT NULL DEFAULT '',
    message    TEXT    NOT NULL,
    details    TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL DEFAULT '',
    status     TEXT    NOT NULL DEFAULT 'open', -- open, resolved, wontfix
    resolution TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX feedback_status ON feedback (status, id);
CREATE INDEX feedback_tool   ON feedback (tool, id);
