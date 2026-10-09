-- ============================================================================
--  Teleport — agent self-service keys
--  Migration: 0002_agent_keys
--
--  Adds named, revocable, expiring credentials so an AI agent can request its
--  own key, have a human approve it in the dashboard, and be held to the
--  reports it published itself.
--
--  No plaintext credential is ever stored. See internal/agentkey for why the
--  token is derived from a claim secret instead of staged in a column.
--
--  Time convention
--    All *_at columns are UNIX epoch **milliseconds** (INTEGER).
--    `expires_at = 0` means "never expires". `revoked_at = 0` means "active".
-- ============================================================================

-- ----------------------------------------------------------------------------
--  reports.owner_key_id — which key published this report.
--
--  '' means "published by the break-glass AGENT_SECRET_KEY (root) or by an
--  earlier deployment that predates keys". Those reports stay readable by root
--  and by the dashboard, and by nobody else, which is the correct default for
--  data whose author is unknown.
-- ----------------------------------------------------------------------------
ALTER TABLE reports ADD COLUMN owner_key_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_reports_owner_key_id
  ON reports (owner_key_id);

-- ----------------------------------------------------------------------------
--  agent_keys — an issued credential.
--
--  token_hash is sha256(hex) of the bearer token and is the lookup key: auth
--  hashes the presented token and probes this UNIQUE index, which keeps the hot
--  path O(log n) instead of comparing against every row.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_keys (
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

-- Dashboard list view and "is this key still live" sweeps.
CREATE INDEX IF NOT EXISTS idx_agent_keys_expires_at
  ON agent_keys (expires_at);

-- ----------------------------------------------------------------------------
--  key_applications — an agent's request, awaiting a human decision.
--
--  claim_hash is sha256 of the claim secret handed out once at submission; the
--  secret is what authorises polling for the outcome, so knowing the id alone
--  is not enough to read someone else's application.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS key_applications (
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
  -- The human's decision has to be parked here, not written straight into
  -- agent_keys: the token is *derived* from the claim secret, which the server
  -- only holds a hash of until the agent actually claims. So the credential
  -- cannot exist at approval time — it is created at claim, from these values.
  -- Consequence, and it is the right one: a key's lifetime starts when the agent
  -- receives it, not when a human clicked approve.
  approved_name   TEXT    NOT NULL DEFAULT '',
  approved_hours  REAL    NOT NULL DEFAULT 0,
  approved_note   TEXT    NOT NULL DEFAULT ''
);

-- The approval queue reads pending applications newest-first; everything else
-- filters by status.
CREATE INDEX IF NOT EXISTS idx_key_applications_status_created
  ON key_applications (status, created_at DESC);

-- ----------------------------------------------------------------------------
--  key_renewals — a request to extend an existing key.
--
--  Renewal is a request, not an action: a key that could extend its own expiry
--  would make expiry decorative, so a human approves every extension.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS key_renewals (
  id                 TEXT    PRIMARY KEY,
  key_id             TEXT    NOT NULL
                             REFERENCES agent_keys (id) ON DELETE CASCADE,
  requested_hours    REAL    NOT NULL DEFAULT 0,
  status             TEXT    NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','approved','rejected')),
  created_at         INTEGER NOT NULL,
  decided_at         INTEGER NOT NULL DEFAULT 0,
  granted_expires_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_key_renewals_key_id
  ON key_renewals (key_id);

CREATE INDEX IF NOT EXISTS idx_key_renewals_status_created
  ON key_renewals (status, created_at DESC);
