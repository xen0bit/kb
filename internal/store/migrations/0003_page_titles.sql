-- kb schema v3: tasks and notes keep the title of the page they are about as
-- it was written. page_key alone is the case-folded lookup key, so a task
-- about a page that did not exist yet could only show "mercury (planet)".
ALTER TABLE tasks ADD COLUMN page_title TEXT;
ALTER TABLE notes ADD COLUMN page_title TEXT;

-- Existing rows: the live page's title where there is one. Rows about pages
-- that were never written keep NULL and fall back to the key.
UPDATE tasks SET page_title = (SELECT title FROM pages WHERE key = tasks.page_key)
    WHERE page_key IS NOT NULL;
UPDATE notes SET page_title = (SELECT title FROM pages WHERE key = notes.page_key)
    WHERE page_key IS NOT NULL;
