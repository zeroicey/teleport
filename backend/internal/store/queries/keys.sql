-- ============================================================================
--  Agent keys, applications and renewals.
--
--  These are the queries behind keys.go, translated from the SQLite
--  implementation. Two things about this file are load-bearing.
--
--  **The count-then-insert statements are not safe on their own.** SQLite
--  serialised every writer, so `SELECT COUNT(*)` followed by an INSERT was
--  atomic for free. PostgreSQL grants no such thing: under READ COMMITTED two
--  transactions can both read a count below the cap and both insert. The store
--  therefore takes a transaction-scoped advisory lock *before* these counts —
--  see LockPendingApplications / LockActiveKeys / LockPendingRenewals in pg.go
--  and cap_lock_pg_test.go, which proves both that the hazard is real and that
--  the lock removes it. Each count has a comment naming the lock it depends on.
--
--  **The conditional UPDATE is the ownership/liveness check.** Every claim,
--  decide and revoke path decides in the WHERE clause, so the check and the
--  write are one statement and cannot race. The affected-row count is the
--  answer, not an error, which is why those queries are :execrows.
-- ============================================================================

-- ----------------------------------------------------------------------------
--  key_applications
-- ----------------------------------------------------------------------------

-- name: CountPendingApplications :one
-- Requires LockPendingApplications. Guards KEY_MAX_PENDING in CreateApplication.
SELECT COUNT(*) FROM key_applications WHERE status = 'pending';

-- name: CountApplications :one
-- @status = '' counts every application.
SELECT COUNT(*) FROM key_applications
WHERE (@status::text = '' OR status = @status);

-- name: InsertApplication :exec
INSERT INTO key_applications
  (id, claim_hash, label, purpose, requested_hours, status, created_at,
   expires_at, decided_at, claim_deadline, issued_key_id, requester_ip,
   user_agent, approved_name, approved_hours, approved_note)
VALUES (@id, @claim_hash, @label, @purpose, @requested_hours, 'pending', @created_at,
        @expires_at, 0, 0, '', @requester_ip, @user_agent, '', 0, '');

-- name: InsertHandoffApplication :exec
-- The admin-initiated path: approved on arrival, with the granted lifetime
-- parked on the row, because the credential cannot exist until it is claimed.
INSERT INTO key_applications
  (id, claim_hash, label, purpose, requested_hours, status, created_at,
   expires_at, decided_at, claim_deadline, issued_key_id, requester_ip,
   user_agent, approved_name, approved_hours, approved_note)
VALUES (@id, @claim_hash, @label, '', @requested_hours, 'approved', @created_at,
        @expires_at, @decided_at, @claim_deadline, '', @requester_ip, @user_agent,
        @approved_name, @approved_hours, @approved_note);

-- name: GetApplication :one
SELECT id, claim_hash, label, purpose, requested_hours, status,
       created_at, expires_at, decided_at, claim_deadline, issued_key_id,
       requester_ip, user_agent, approved_name, approved_hours, approved_note
FROM key_applications
WHERE id = @id;

-- name: ListApplications :many
SELECT id, claim_hash, label, purpose, requested_hours, status,
       created_at, expires_at, decided_at, claim_deadline, issued_key_id,
       requester_ip, user_agent, approved_name, approved_hours, approved_note
FROM key_applications
WHERE (@status::text = '' OR status = @status)
ORDER BY created_at DESC
LIMIT @limit_count;

-- name: ExpireApplications :execrows
-- Both arms are needed: an application can lapse while pending, or after
-- approval but before it is claimed. A stale row would otherwise hold a slot in
-- the pending cap forever.
UPDATE key_applications
SET status = 'expired'
WHERE (status = 'pending' AND expires_at <= @now_ms)
   OR (status = 'approved' AND claim_deadline <= @now_ms);

-- name: DecideApplication :execrows
-- Only a pending row can be decided, so a double-click cannot re-decide a
-- rejection. Zero rows means "absent or already decided"; the store tells those
-- apart with a follow-up read.
UPDATE key_applications
SET status         = @status,
    decided_at     = @now_ms,
    approved_name  = @approved_name,
    approved_hours = @approved_hours,
    approved_note  = @approved_note,
    claim_deadline = @claim_deadline
WHERE id = @id AND status = 'pending';

-- name: MarkApplicationClaimed :execrows
-- The security-critical conditional write: the status change and the liveness
-- window are decided in one statement, so exactly one concurrent claimer can
-- flip the row. `claim_deadline > @now_ms` is strict — at the deadline the
-- window is closed.
UPDATE key_applications
SET status = 'claimed'
WHERE id = @id AND status = 'approved' AND claim_deadline > @now_ms;

-- name: GetApplicationForClaim :one
SELECT claim_hash, approved_name, approved_hours, approved_note
FROM key_applications
WHERE id = @id;

-- name: SetApplicationIssuedKey :exec
UPDATE key_applications SET issued_key_id = @issued_key_id WHERE id = @id;

-- ----------------------------------------------------------------------------
--  agent_keys
-- ----------------------------------------------------------------------------

-- name: CountActiveKeys :one
-- Requires LockActiveKeys. Guards KEY_MAX_ACTIVE, shared by ClaimApplication
-- and CreateManualKey — both must take the *same* lock, or they can each spend
-- the last slot.
SELECT COUNT(*) FROM agent_keys
WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > @now_ms);

-- name: InsertAgentKey :exec
INSERT INTO agent_keys
  (id, name, token_hash, token_prefix, created_at, expires_at, revoked_at,
   last_used_at, request_count, note)
VALUES (@id, @name, @token_hash, @token_prefix, @created_at, @expires_at, 0, 0, 0, @note);

-- name: GetAgentKey :one
-- The column list is the table's own order (token_hash before token_prefix),
-- not the domain type's field order, so that sqlc can reuse the AgentKey model
-- instead of minting a row type per query.
SELECT id, name, token_hash, token_prefix, created_at, expires_at,
       revoked_at, last_used_at, request_count, note
FROM agent_keys
WHERE id = @id;

-- name: ListAgentKeys :many
-- Revoked and expired keys are included: the dashboard is where you go to see
-- what happened.
SELECT id, name, token_hash, token_prefix, created_at, expires_at,
       revoked_at, last_used_at, request_count, note
FROM agent_keys
ORDER BY created_at DESC;

-- name: ResolveAgentKey :one
-- Expiry and revocation are filtered here so an unusable key is simply absent —
-- a caller cannot accidentally treat a revoked key as valid.
--
-- The comparison is `expires_at > @not_expired_after` rather than
-- `expires_at + grace > now` so adding two millisecond values can never
-- overflow. Revocation is never tolerated at any grace.
SELECT id, name, token_hash, token_prefix, created_at, expires_at,
       revoked_at, last_used_at, request_count, note
FROM agent_keys
WHERE token_hash = @token_hash
  AND revoked_at = 0
  AND (expires_at = 0 OR expires_at > @not_expired_after);

-- name: TouchAgentKey :exec
UPDATE agent_keys
SET last_used_at = @last_used_at, request_count = request_count + 1
WHERE id = @id;

-- name: UpdateAgentKey :execrows
-- The three COALESCE columns follow the same "NULL means absent" rule as
-- UpdateReport. revoked_at cannot use COALESCE, because 0 is a meaningful target
-- value ("un-revoke") and would be indistinguishable from NULL: hence the CASE.
UPDATE agent_keys
SET name       = COALESCE(sqlc.narg('name')::text, name),
    expires_at = COALESCE(sqlc.narg('expires_at')::bigint, expires_at),
    note       = COALESCE(sqlc.narg('note')::text, note),
    revoked_at = CASE
                   WHEN sqlc.narg('revoked')::boolean IS NULL THEN revoked_at
                   WHEN sqlc.narg('revoked')::boolean THEN @now_ms::bigint
                   ELSE 0
                 END
WHERE id = sqlc.arg('id');

-- ----------------------------------------------------------------------------
--  key_renewals
-- ----------------------------------------------------------------------------

-- name: CountPendingRenewals :one
-- Requires LockPendingRenewals. Enforces "at most one pending renewal per key".
SELECT COUNT(*) FROM key_renewals
WHERE key_id = @key_id AND status = 'pending';

-- name: InsertRenewal :exec
INSERT INTO key_renewals (id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at)
VALUES (@id, @key_id, @requested_hours, 'pending', @created_at, 0, 0);

-- name: GetRenewal :one
SELECT id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at
FROM key_renewals
WHERE id = @id;

-- name: ListRenewals :many
SELECT id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at
FROM key_renewals
WHERE (@status::text = '' OR status = @status)
ORDER BY created_at DESC
LIMIT @limit_count;

-- name: LockRenewalForDecision :one
-- FOR UPDATE is load-bearing here, and it is a different mechanism from the cap
-- locks above.
--
-- This row already exists, so a row lock is available — unlike a COUNT over a set
-- that new rows can join, where FOR UPDATE cannot help and an advisory lock is
-- the only option.
--
-- Without it, two concurrent decisions both read status='pending', both extend
-- the key, and only then does the guarded UPDATE at the end refuse the second
-- one. The refusal arrives too late: the extension already happened, so one
-- renewal request granted two extensions. SQLite's BEGIN IMMEDIATE made that
-- interleaving impossible for free, which is why the old code needed no lock and
-- this one does. See
-- .ai/pitfalls/cases/2026-10-16-decide-renewal-granted-twice.md.
SELECT key_id, status FROM key_renewals WHERE id = @id FOR UPDATE;

-- name: GetKeyExpiryForRenewal :one
-- revoked_at is read too: a renewal must not resurrect a key a human killed.
SELECT expires_at, revoked_at FROM agent_keys WHERE id = @id;

-- name: ExtendAgentKey :exec
UPDATE agent_keys SET expires_at = @expires_at WHERE id = @id;

-- name: DecideRenewal :execrows
-- Guarded on status = 'pending' so a second decision changes nothing.
UPDATE key_renewals
SET status             = @status,
    decided_at         = @now_ms,
    granted_expires_at = @granted_expires_at
WHERE id = @id AND status = 'pending';
