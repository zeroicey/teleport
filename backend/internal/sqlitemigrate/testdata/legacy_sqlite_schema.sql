-- ============================================================================
--  The pre-rewrite SQLite schema, as a test fixture.
--
--  This is the last revision of the old deployment's schema: `0001_init` plus
--  `0002_agent_keys`, with the ALTER from the second folded into the first (which
--  is how the source database on the old host is shaped).
--
--  It is NOT executed anywhere and is not the application's schema. It exists so
--  the importer's tests can build a source database with the right *shape* —
--  `pragma_table_info` is what the importer reads, so the columns are what
--  matter — without depending on a live host. The real data set (5 reports,
--  2 keys, 6 share links) is imported and checked by scripts/verify-p5.sh.
--
--  Indexes, the view and the updated_at trigger are omitted deliberately: they
--  are not read by the importer, and copying them here would suggest this file
--  is a schema the application still uses.
--
--  Keep the column lists byte-faithful to the source. A drift between this file
--  and reality would make the importer's column check pass here and fail on the
--  day it runs for real.
-- ============================================================================

CREATE TABLE reports (
  id           TEXT    PRIMARY KEY,
  title        TEXT    NOT NULL,
  category     TEXT    NOT NULL DEFAULT 'general',
  format       TEXT    NOT NULL DEFAULT 'markdown'
                       CHECK (format IN ('markdown', 'html')),
  content      TEXT    NOT NULL,
  metadata     TEXT    NOT NULL DEFAULT '{}'
                       CHECK (json_valid(metadata)),
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  owner_key_id TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE share_tokens (
  token       TEXT    PRIMARY KEY,
  report_id   TEXT    NOT NULL REFERENCES reports (id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL DEFAULT 0,
  is_active   INTEGER NOT NULL DEFAULT 1
                      CHECK (is_active IN (0, 1)),
  view_count  INTEGER NOT NULL DEFAULT 0
                      CHECK (view_count >= 0)
);

CREATE TABLE agent_keys (
  id            TEXT    PRIMARY KEY,
  name          TEXT    NOT NULL,
  token_hash    TEXT    NOT NULL UNIQUE,
  token_prefix  TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL DEFAULT 0,
  revoked_at    INTEGER NOT NULL DEFAULT 0,
  last_used_at  INTEGER NOT NULL DEFAULT 0,
  request_count INTEGER NOT NULL DEFAULT 0
                CHECK (request_count >= 0),
  note          TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE key_applications (
  id              TEXT    PRIMARY KEY,
  claim_hash      TEXT    NOT NULL,
  label           TEXT    NOT NULL,
  purpose         TEXT    NOT NULL DEFAULT '',
  requested_hours REAL    NOT NULL DEFAULT 0,
  status          TEXT    NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending','approved','rejected','claimed','expired')),
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  decided_at      INTEGER NOT NULL DEFAULT 0,
  claim_deadline  INTEGER NOT NULL DEFAULT 0,
  issued_key_id   TEXT    NOT NULL DEFAULT '',
  requester_ip    TEXT    NOT NULL DEFAULT '',
  user_agent      TEXT    NOT NULL DEFAULT '',
  approved_name   TEXT    NOT NULL DEFAULT '',
  approved_hours  REAL    NOT NULL DEFAULT 0,
  approved_note   TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE key_renewals (
  id                 TEXT    PRIMARY KEY,
  key_id             TEXT    NOT NULL REFERENCES agent_keys (id) ON DELETE CASCADE,
  requested_hours    REAL    NOT NULL DEFAULT 0,
  status             TEXT    NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','approved','rejected')),
  created_at         INTEGER NOT NULL,
  decided_at         INTEGER NOT NULL DEFAULT 0,
  granted_expires_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE schema_migrations (
  version    TEXT    PRIMARY KEY,
  applied_at INTEGER NOT NULL
);
