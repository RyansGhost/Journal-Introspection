-- Everything here is derived from the org files (or is a login session),
-- so the database can be deleted and rebuilt at any time.

CREATE TABLE IF NOT EXISTS files (
  id         INTEGER PRIMARY KEY,
  path       TEXT NOT NULL UNIQUE,   -- relative to ORG_ROOT, forward slashes
  kind       TEXT NOT NULL,          -- journal | roam | note
  title      TEXT NOT NULL,
  mtime      INTEGER NOT NULL,       -- unix nanoseconds
  size       INTEGER NOT NULL,
  sha256     TEXT NOT NULL,
  indexed_at INTEGER NOT NULL,
  deleted_at INTEGER
);

-- One row per "** HH:MM" block of a journal file.
CREATE TABLE IF NOT EXISTS entries (
  id           INTEGER PRIMARY KEY,
  file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  date         TEXT NOT NULL,        -- YYYY-MM-DD
  time         TEXT NOT NULL,        -- HH:MM or ''
  heading      TEXT NOT NULL,
  body_org     TEXT NOT NULL,
  tags         TEXT NOT NULL,        -- space separated
  props_json   TEXT NOT NULL,
  word_count   INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  seq          INTEGER NOT NULL      -- order within the file
);
CREATE INDEX IF NOT EXISTS entries_date ON entries(date);
CREATE INDEX IF NOT EXISTS entries_hash ON entries(content_hash);
CREATE INDEX IF NOT EXISTS entries_file ON entries(file_id);

-- Grades are keyed by the entry text, not the file, so a touched file is
-- never regraded and a changed rubric or model keeps the old grades.
CREATE TABLE IF NOT EXISTS grades (
  entry_hash     TEXT NOT NULL,
  rubric_version TEXT NOT NULL,
  model          TEXT NOT NULL,
  dimension      TEXT NOT NULL,
  score          REAL,               -- score: mean level index; noul: probability; choice: NULL
  label          TEXT NOT NULL,      -- choice: the chosen option; otherwise ''
  confidence     REAL NOT NULL,
  probs_json     TEXT NOT NULL,
  PRIMARY KEY (entry_hash, rubric_version, model, dimension)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS graded (
  entry_hash     TEXT NOT NULL,
  rubric_version TEXT NOT NULL,
  model          TEXT NOT NULL,
  graded_at      INTEGER NOT NULL,
  PRIMARY KEY (entry_hash, rubric_version, model)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS grade_failures (
  entry_hash     TEXT NOT NULL,
  rubric_version TEXT NOT NULL,
  model          TEXT NOT NULL,
  attempts       INTEGER NOT NULL,
  last_error     TEXT NOT NULL,
  next_try_at    INTEGER NOT NULL,
  PRIMARY KEY (entry_hash, rubric_version, model)
) WITHOUT ROWID;

CREATE VIRTUAL TABLE IF NOT EXISTS search USING fts5(
  path, title, body, tokenize = 'porter unicode61'
);

CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS login_attempts (
  ip TEXT NOT NULL,
  at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS login_attempts_ip ON login_attempts(ip, at);
