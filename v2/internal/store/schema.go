package store

// migrations are applied in order; PRAGMA user_version records how many have
// run. They are forward-only: never edit one that has shipped, add another.
var migrations = []string{schemaV1}

// Times are Unix milliseconds. JSON columns hold arrays. A task's id is an
// integer shown as t<id>.
const schemaV1 = `
CREATE TABLE tasks(
  id           INTEGER PRIMARY KEY,
  ticket       TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL,
  prompt       TEXT NOT NULL DEFAULT '',
  role         TEXT NOT NULL DEFAULT 'worker',
  state        TEXT NOT NULL,
  reason       TEXT NOT NULL DEFAULT '',
  since        INTEGER NOT NULL,
  provider     TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  session      TEXT NOT NULL DEFAULT '',
  branch       TEXT NOT NULL DEFAULT '',
  worktree     TEXT NOT NULL DEFAULT '',
  pr           INTEGER NOT NULL DEFAULT 0,
  touches      TEXT NOT NULL DEFAULT '[]',
  after        TEXT NOT NULL DEFAULT '[]',
  fix_attempts INTEGER NOT NULL DEFAULT 0,
  held         INTEGER NOT NULL DEFAULT 0,
  created      INTEGER NOT NULL
);
CREATE INDEX tasks_live ON tasks(id) WHERE state NOT IN ('merged','closed');
CREATE INDEX tasks_ended ON tasks(since) WHERE state IN ('merged','closed');

CREATE TABLE attempts(
  id          INTEGER PRIMARY KEY,
  task        INTEGER NOT NULL REFERENCES tasks(id),
  provider    TEXT NOT NULL,
  model       TEXT NOT NULL DEFAULT '',
  argv        TEXT NOT NULL DEFAULT '[]',
  resume_argv TEXT NOT NULL DEFAULT '[]',
  pid         INTEGER NOT NULL DEFAULT 0,
  started     INTEGER NOT NULL,
  ended       INTEGER NOT NULL DEFAULT 0,
  outcome     TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT '',
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost        REAL NOT NULL DEFAULT 0
);
CREATE INDEX attempts_task ON attempts(task, id);
CREATE INDEX attempts_running ON attempts(id) WHERE ended = 0;

CREATE TABLE inbox(
  id                INTEGER PRIMARY KEY,
  to_task           INTEGER NOT NULL REFERENCES tasks(id),
  sender            TEXT NOT NULL,
  kind              TEXT NOT NULL DEFAULT 'message',
  dedup             TEXT NOT NULL DEFAULT '',
  body              TEXT NOT NULL,
  created           INTEGER NOT NULL,
  delivered_attempt INTEGER
);
CREATE INDEX inbox_pending ON inbox(to_task, id) WHERE delivered_attempt IS NULL;
CREATE UNIQUE INDEX inbox_dedup ON inbox(to_task, dedup) WHERE dedup <> '';

CREATE TABLE questions(
  id          INTEGER PRIMARY KEY,
  task        INTEGER NOT NULL REFERENCES tasks(id),
  body        TEXT NOT NULL,
  options     TEXT NOT NULL DEFAULT '[]',
  asked       INTEGER NOT NULL,
  answer      TEXT,
  answered_by TEXT NOT NULL DEFAULT '',
  answered    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX questions_open ON questions(id) WHERE answer IS NULL;

CREATE TABLE tickets(
  forge_id    TEXT PRIMARY KEY,
  title       TEXT NOT NULL,
  body        TEXT NOT NULL DEFAULT '',
  approved_by TEXT NOT NULL DEFAULT '',
  difficulty  TEXT NOT NULL DEFAULT '',
  touches     TEXT NOT NULL DEFAULT '[]',
  after       TEXT NOT NULL DEFAULT '[]',
  needs       TEXT NOT NULL DEFAULT '[]',
  state       TEXT NOT NULL DEFAULT 'open',
  synced      INTEGER NOT NULL
);

CREATE TABLE prs(
  task      INTEGER PRIMARY KEY REFERENCES tasks(id),
  number    INTEGER NOT NULL,
  head_sha  TEXT NOT NULL DEFAULT '',
  state     TEXT NOT NULL DEFAULT '',
  checks    TEXT NOT NULL DEFAULT '',
  mergeable TEXT NOT NULL DEFAULT '',
  review    TEXT NOT NULL DEFAULT '',
  observed  INTEGER NOT NULL
);

CREATE TABLE settings(
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated    INTEGER NOT NULL,
  updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE loops(
  id       INTEGER PRIMARY KEY,
  prompt   TEXT NOT NULL,
  target   TEXT NOT NULL DEFAULT 'orchestrator',
  every_ms INTEGER NOT NULL,
  until    TEXT NOT NULL DEFAULT '',
  max_runs INTEGER NOT NULL DEFAULT 0,
  runs     INTEGER NOT NULL DEFAULT 0,
  next_at  INTEGER NOT NULL,
  ended    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE providers(
  name      TEXT PRIMARY KEY,
  available INTEGER NOT NULL DEFAULT 1,
  out_until INTEGER NOT NULL DEFAULT 0,
  detail    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE usage(
  minute     INTEGER NOT NULL,
  task       INTEGER NOT NULL,
  provider   TEXT NOT NULL,
  model      TEXT NOT NULL,
  tokens_in  INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,
  cost       REAL NOT NULL DEFAULT 0,
  PRIMARY KEY(minute, task, provider, model)
);

CREATE TABLE retries(
  key         TEXT PRIMARY KEY,
  inputs_hash TEXT NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_at     INTEGER NOT NULL,
  last_error  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE events(
  id   INTEGER PRIMARY KEY,
  ts   INTEGER NOT NULL,
  task INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT ''
);
CREATE INDEX events_ts ON events(ts);
`
