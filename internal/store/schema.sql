CREATE TABLE IF NOT EXISTS notes (
  id        INTEGER PRIMARY KEY,
  path      TEXT NOT NULL UNIQUE,      -- relative to the notes dir
  title     TEXT NOT NULL,
  body      TEXT NOT NULL,
  mtime     INTEGER NOT NULL,          -- unix nanos of the file when indexed
  privacy   TEXT NOT NULL DEFAULT 'personal' -- public | personal | local_only (M3)
);

CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(
  title, body, content='notes', content_rowid='id', tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes BEGIN
  INSERT INTO notes_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
END;
CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes BEGIN
  INSERT INTO notes_fts(notes_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
END;
CREATE TRIGGER IF NOT EXISTS notes_au AFTER UPDATE ON notes BEGIN
  INSERT INTO notes_fts(notes_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
  INSERT INTO notes_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
END;

CREATE TABLE IF NOT EXISTS reminders (
  id         INTEGER PRIMARY KEY,
  idem_key   TEXT NOT NULL UNIQUE,     -- makes create_reminder safe to retry
  text       TEXT NOT NULL,
  due_at     INTEGER NOT NULL,         -- unix seconds
  status     TEXT NOT NULL DEFAULT 'pending', -- pending | fired | cancelled
  source     TEXT NOT NULL DEFAULT '', -- e.g. the note path it refers to
  created_at INTEGER NOT NULL,
  fired_at   INTEGER
);
CREATE INDEX IF NOT EXISTS reminders_due ON reminders(status, due_at);

CREATE TABLE IF NOT EXISTS tool_calls (
  id         INTEGER PRIMARY KEY,
  turn_key   TEXT NOT NULL,
  name       TEXT NOT NULL,
  args       TEXT NOT NULL,
  result     TEXT NOT NULL,
  ok         INTEGER NOT NULL,
  ms         INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id         INTEGER PRIMARY KEY,
  role       TEXT NOT NULL,
  content    TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
