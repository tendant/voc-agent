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

-- Every cloud escalation decision, with the exact bytes that left the machine.
CREATE TABLE IF NOT EXISTS audit (
  id          INTEGER PRIMARY KEY,
  created_at  INTEGER NOT NULL,
  turn_key    TEXT NOT NULL,
  decision    TEXT NOT NULL,          -- approved | denied | blocked | disabled | error
  destination TEXT NOT NULL,
  model       TEXT NOT NULL,
  reason      TEXT NOT NULL,
  detail      TEXT NOT NULL DEFAULT '', -- why blocked/denied/failed
  proposed    TEXT NOT NULL DEFAULT '', -- request body shown for approval
  sent        TEXT NOT NULL DEFAULT '', -- request body actually transmitted ('' if nothing left)
  sent_sha256 TEXT NOT NULL DEFAULT '',
  response    TEXT NOT NULL DEFAULT '',
  policy      TEXT NOT NULL DEFAULT '{}' -- included/excluded sources and redactions
);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

-- Goal-mode tasks: the plan and every step's progress, so a task cut off by
-- a restart can be resumed without repeating finished steps.
CREATE TABLE IF NOT EXISTS tasks (
  id         INTEGER PRIMARY KEY,
  goal       TEXT NOT NULL,
  status     TEXT NOT NULL,          -- planned | running | done | failed | cancelled | interrupted
  plan       TEXT NOT NULL DEFAULT '{}', -- JSON: summary and steps with status/results
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Side effects done under an idempotency key (survives restarts), so a
-- resumed task doesn't repeat an action that already happened.
CREATE TABLE IF NOT EXISTS actions (
  key        TEXT PRIMARY KEY,
  result     TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
