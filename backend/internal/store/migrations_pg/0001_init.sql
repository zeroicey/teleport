-- ============================================================================
--  Teleport — PostgreSQL baseline
--
--  This is the consolidated equivalent of the SQLite pair 0001_init.sql +
--  0002_agent_keys.sql. It is a fresh start rather than a replay of that
--  history: there is no PostgreSQL data to migrate yet (the existing rows come
--  over in a separate, verified import), so replaying an ALTER that only ever
--  existed because SQLite could not add a column earlier would be noise.
--
--  The SQLite migrations stay in git for reference. In PostgreSQL the filename
--  order is the application order, and `schema_migrations` records what ran.
--
--  ==========================================================================
--  Four translations that are NOT cosmetic (each would be a silent or loud
--  breakage if done by find-and-replace):
--
--  1. epoch-millisecond columns are BIGINT, not INTEGER.
--     SQLite's INTEGER is 64-bit; PostgreSQL's INTEGER is 32-bit. A millisecond
--     timestamp today is ~1.79e12, which does not fit in int32 — every insert
--     would fail with "integer out of range". Every `*_at` column is BIGINT.
--
--  2. `REAL` becomes DOUBLE PRECISION, not REAL.
--     SQLite's REAL is 8 bytes; PostgreSQL's REAL is float4 (4 bytes). The Go
--     code scans these into float64, so REAL would silently round the values.
--
--  3. `json_valid(x)` becomes `x IS JSON`.
--     `IS JSON` is the SQL-standard predicate (PostgreSQL 16). A cast in a CHECK
--     would raise an exception instead of returning false, which changes what a
--     constraint violation looks like.
--
--  4. `is_active` and friends stay INTEGER with a CHECK, not BOOLEAN.
--     The Go queries say `is_active = 1` and write 0/1. Translating to BOOLEAN
--     would be prettier and would silently break every one of those statements.
--     Faithfulness to the existing SQL beats idiom here.
--
--  And one shape change that is deliberate: the updated_at trigger is BEFORE
--  rather than AFTER. SQLite cannot assign to NEW, so the original fires after
--  the write and issues a second UPDATE. PostgreSQL can, so this version writes
--  the corrected value once — same stored result, one row version instead of two.
--  See the trigger for why the condition is preserved exactly.
--  ==========================================================================
--
--  Time convention
--    All `*_at` columns are UNIX epoch **milliseconds** (BIGINT), matching
--    JavaScript `Date.now()`. `expires_at = 0` means "never expires";
--    `revoked_at = 0` means "active".
--
--  No `PRAGMA foreign_keys = ON` equivalent is needed: PostgreSQL always
--  enforces foreign keys.
-- ============================================================================

-- ----------------------------------------------------------------------------
--  reports — one document per AI report submission
-- ----------------------------------------------------------------------------
CREATE TABLE reports (
  id           TEXT   PRIMARY KEY,                 -- UUID v4
  title        TEXT   NOT NULL,
  category     TEXT   NOT NULL DEFAULT 'general',  -- 'pentest' | 'architecture' | 'progress' | ...
  format       TEXT   NOT NULL DEFAULT 'markdown'
                      CHECK (format IN ('markdown', 'html')),
  content      TEXT   NOT NULL,                    -- Markdown + Mermaid source
  metadata     TEXT   NOT NULL DEFAULT '{}'        -- JSON blob: target host, CVE ids, status...
                      CHECK (metadata IS JSON),
  created_at   BIGINT NOT NULL,
  updated_at   BIGINT NOT NULL,
  -- '' means "published by the break-glass AGENT_SECRET_KEY (root), or by a
  -- deployment that predates keys". Those reports stay readable by root and by
  -- the dashboard and by nobody else, which is the correct default for data
  -- whose author is unknown. This column is never updated after insert.
  owner_key_id TEXT   NOT NULL DEFAULT ''
);

-- Dashboard list view: newest first, usually filtered by category.
CREATE INDEX idx_reports_created_at
  ON reports (created_at DESC);

CREATE INDEX idx_reports_category_created_at
  ON reports (category, created_at DESC);

-- "everything this key published" — the ownership boundary is read on every
-- agent-scoped listing.
CREATE INDEX idx_reports_owner_key_id
  ON reports (owner_key_id);

-- ----------------------------------------------------------------------------
--  share_tokens — many independent, independently-expiring links per report
-- ----------------------------------------------------------------------------
CREATE TABLE share_tokens (
  token       TEXT    PRIMARY KEY,                 -- high-entropy URL-safe string
  report_id   TEXT    NOT NULL
                      REFERENCES reports (id) ON DELETE CASCADE,
  created_at  BIGINT  NOT NULL,
  expires_at  BIGINT  NOT NULL DEFAULT 0,          -- 0 = never expires
  is_active   INTEGER NOT NULL DEFAULT 1
                      CHECK (is_active IN (0, 1)),
  view_count  INTEGER NOT NULL DEFAULT 0
                      CHECK (view_count >= 0)
);

-- Resolve "all live links for this report" on the dashboard.
CREATE INDEX idx_share_tokens_report_id
  ON share_tokens (report_id);

-- Retention/cleanup sweeps over absolute expiry.
CREATE INDEX idx_share_tokens_expires_at
  ON share_tokens (expires_at);

-- Hot path for GET /api/share/{token}: the PK already covers the token lookup;
-- this partial index keeps the active-only scans small.
CREATE INDEX idx_share_tokens_active
  ON share_tokens (is_active, expires_at)
  WHERE is_active = 1;

-- ----------------------------------------------------------------------------
--  agent_keys — an issued credential
--
--  token_hash is sha256(hex) of the bearer token and is the lookup key: auth
--  hashes the presented token and probes this UNIQUE index, which keeps the hot
--  path O(log n) instead of comparing against every row.
-- ----------------------------------------------------------------------------
CREATE TABLE agent_keys (
  id            TEXT    PRIMARY KEY,
  name          TEXT    NOT NULL,
  token_hash    TEXT    NOT NULL UNIQUE,
  token_prefix  TEXT    NOT NULL,
  created_at    BIGINT  NOT NULL,
  expires_at    BIGINT  NOT NULL DEFAULT 0,
  revoked_at    BIGINT  NOT NULL DEFAULT 0,
  last_used_at  BIGINT  NOT NULL DEFAULT 0,
  request_count BIGINT  NOT NULL DEFAULT 0
                CHECK (request_count >= 0),
  note          TEXT    NOT NULL DEFAULT ''
);

-- Dashboard list view and "is this key still live" sweeps.
CREATE INDEX idx_agent_keys_expires_at
  ON agent_keys (expires_at);

-- ----------------------------------------------------------------------------
--  key_applications — an agent's request, awaiting a human decision
--
--  claim_hash is sha256 of the claim secret handed out once at submission; the
--  secret is what authorises polling for the outcome, so knowing the id alone is
--  not enough to read someone else's application.
-- ----------------------------------------------------------------------------
CREATE TABLE key_applications (
  id              TEXT    PRIMARY KEY,
  claim_hash      TEXT    NOT NULL,
  label           TEXT    NOT NULL,
  purpose         TEXT    NOT NULL DEFAULT '',
  requested_hours DOUBLE PRECISION NOT NULL DEFAULT 0,
  status          TEXT    NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending','approved','rejected','claimed','expired')),
  created_at      BIGINT  NOT NULL,
  expires_at      BIGINT  NOT NULL,
  decided_at      BIGINT  NOT NULL DEFAULT 0,
  claim_deadline  BIGINT  NOT NULL DEFAULT 0,
  issued_key_id   TEXT    NOT NULL DEFAULT '',
  requester_ip    TEXT    NOT NULL DEFAULT '',
  user_agent      TEXT    NOT NULL DEFAULT '',
  -- The human's decision is parked here, not written straight into agent_keys:
  -- the token is *derived* from the claim secret, of which the server holds only
  -- a hash until the agent claims. So the credential cannot exist at approval
  -- time — it is created at claim, from these values. Consequence, and it is the
  -- right one: a key's lifetime starts when the agent receives it, not when a
  -- human clicked approve.
  approved_name   TEXT    NOT NULL DEFAULT '',
  approved_hours  DOUBLE PRECISION NOT NULL DEFAULT 0,
  approved_note   TEXT    NOT NULL DEFAULT ''
);

-- The approval queue reads pending applications newest-first; everything else
-- filters by status.
CREATE INDEX idx_key_applications_status_created
  ON key_applications (status, created_at DESC);

-- ----------------------------------------------------------------------------
--  key_renewals — a request to extend an existing key
--
--  Renewal is a request, not an action: a key that could extend its own expiry
--  would make expiry decorative, so a human approves every extension. There is
--  deliberately no claim-secret column here either — the value is recomputable
--  from the same K_derive subkey the tokens use, so adding one would put a
--  secret back at rest for no gain.
-- ----------------------------------------------------------------------------
CREATE TABLE key_renewals (
  id                 TEXT    PRIMARY KEY,
  key_id             TEXT    NOT NULL
                             REFERENCES agent_keys (id) ON DELETE CASCADE,
  requested_hours    DOUBLE PRECISION NOT NULL DEFAULT 0,
  status             TEXT    NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','approved','rejected')),
  created_at         BIGINT  NOT NULL,
  decided_at         BIGINT  NOT NULL DEFAULT 0,
  granted_expires_at BIGINT  NOT NULL DEFAULT 0
);

CREATE INDEX idx_key_renewals_key_id
  ON key_renewals (key_id);

CREATE INDEX idx_key_renewals_status_created
  ON key_renewals (status, created_at DESC);

-- ----------------------------------------------------------------------------
--  v_live_shares — a token joined to its report, with derived liveness.
--
--  No Go code reads this; it exists for manual inspection. "live" is computed at
--  read time so no job has to flip flags.
--
--  `not_expired` is BOOLEAN here where SQLite produced 0/1 — it is a predicate,
--  and nothing that parses this view expects an integer.
-- ----------------------------------------------------------------------------
CREATE OR REPLACE VIEW v_live_shares AS
SELECT
  t.token,
  t.report_id,
  t.created_at        AS token_created_at,
  t.expires_at,
  t.view_count,
  (t.expires_at = 0 OR t.expires_at > (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT)
                      AS not_expired,
  r.title,
  r.category,
  r.format
FROM share_tokens t
JOIN reports r ON r.id = t.report_id
WHERE t.is_active = 1;

-- ----------------------------------------------------------------------------
--  Trigger — keep reports.updated_at honest without trusting the caller.
--
--  The condition is preserved EXACTLY from the SQLite version: fire only when
--  the caller left updated_at unchanged (NEW IS NOT DISTINCT FROM OLD compares
--  null-safely, matching SQLite's `NEW.updated_at = OLD.updated_at` for a NOT
--  NULL column).
--
--  Why it must almost never fire: the update path sets
--  `updated_at = GREATEST(now, updated_at + 1)`, which guarantees the column
--  always changes. That is not gold-plating — under SQLite this trigger stamped
--  the column from `strftime('%s','now') * 1000`, i.e. truncated to whole
--  SECONDS, so firing it could move updated_at up to 999 ms *before* created_at
--  and make the share page claim a report was updated before it existed.
--  PostgreSQL's now() has microsecond precision, so that particular corruption is
--  no longer possible here — but the Go-side GREATEST is kept anyway, because
--  strictly-increasing updated_at is a property the tests assert and a UI
--  depends on, and it should not depend on the clock's resolution.
-- ----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION reports_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at THEN
    NEW.updated_at := (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_reports_touch_updated_at
BEFORE UPDATE ON reports
FOR EACH ROW
EXECUTE FUNCTION reports_touch_updated_at();
