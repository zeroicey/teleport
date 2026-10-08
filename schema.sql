-- ============================================================================
--  Teleport — D1 schema (one-shot / manual execution)
--
--  This file is a readable snapshot of the incremental migrations in
--  migrations/. The DDL below must stay identical to 0001_init.sql — verify
--  with:  npm run db:check-schema
--
--  Apply incrementally (preferred):
--    npm run db:migrate:local     # local dev database
--    npm run db:migrate:remote    # production database
--
--  Or execute this file directly against a database:
--    npx wrangler d1 execute teleport-db --local  --file=./schema.sql
--    npx wrangler d1 execute teleport-db --remote --file=./schema.sql
--
--  Time convention
--    All *_at columns are UNIX epoch **milliseconds** (INTEGER), matching
--    JavaScript `Date.now()`. `expires_at = 0` means "never expires".
-- ============================================================================

-- D1 already runs with foreign keys enforced; this is explicit for clarity and
-- for local `sqlite3`/`wrangler d1 execute` runs.
PRAGMA foreign_keys = ON;

-- ----------------------------------------------------------------------------
--  reports — one immutable-ish document per AI report submission
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS reports (
  id          TEXT    PRIMARY KEY,                 -- UUID v4
  title       TEXT    NOT NULL,
  category    TEXT    NOT NULL DEFAULT 'general',  -- 'pentest' | 'architecture' | 'progress' | ...
  format      TEXT    NOT NULL DEFAULT 'markdown'
                      CHECK (format IN ('markdown', 'html')),
  content     TEXT    NOT NULL,                    -- Markdown + Mermaid source
  metadata    TEXT    NOT NULL DEFAULT '{}'        -- JSON blob: target host, CVE ids, status...
                      CHECK (json_valid(metadata)),
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- Dashboard list view: newest first, usually filtered by category.
CREATE INDEX IF NOT EXISTS idx_reports_created_at
  ON reports (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_reports_category_created_at
  ON reports (category, created_at DESC);

-- ----------------------------------------------------------------------------
--  share_tokens — many independent, independently-expiring links per report
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS share_tokens (
  token       TEXT    PRIMARY KEY,                 -- high-entropy URL-safe string
  report_id   TEXT    NOT NULL
                      REFERENCES reports (id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL DEFAULT 0,          -- 0 = never expires
  is_active   INTEGER NOT NULL DEFAULT 1
                      CHECK (is_active IN (0, 1)),
  view_count  INTEGER NOT NULL DEFAULT 0
                      CHECK (view_count >= 0)
);

-- Resolve "all live links for this report" on the dashboard.
CREATE INDEX IF NOT EXISTS idx_share_tokens_report_id
  ON share_tokens (report_id);

-- Retention/cleanup sweeps over absolute expiry.
CREATE INDEX IF NOT EXISTS idx_share_tokens_expires_at
  ON share_tokens (expires_at);

-- Hot path for GET /api/share/:token: the PK already covers the token lookup,
-- this partial index keeps the active-only scans small.
CREATE INDEX IF NOT EXISTS idx_share_tokens_active
  ON share_tokens (is_active, expires_at)
  WHERE is_active = 1;

-- ----------------------------------------------------------------------------
--  Convenience view — a token joined to its report, with derived liveness.
--  "live" is computed at read time so no cron job is needed to flip flags.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_live_shares;
CREATE VIEW v_live_shares AS
SELECT
  t.token,
  t.report_id,
  t.created_at        AS token_created_at,
  t.expires_at,
  t.view_count,
  (t.expires_at = 0 OR t.expires_at > CAST(strftime('%s', 'now') AS INTEGER) * 1000)
                      AS not_expired,
  r.title,
  r.category,
  r.format
FROM share_tokens t
JOIN reports r ON r.id = t.report_id
WHERE t.is_active = 1;

-- ----------------------------------------------------------------------------
--  Trigger — keep reports.updated_at honest without trusting the caller.
-- ----------------------------------------------------------------------------
DROP TRIGGER IF EXISTS trg_reports_touch_updated_at;
CREATE TRIGGER trg_reports_touch_updated_at
AFTER UPDATE ON reports
FOR EACH ROW
WHEN NEW.updated_at = OLD.updated_at
BEGIN
  UPDATE reports
     SET updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
   WHERE id = NEW.id;
END;
