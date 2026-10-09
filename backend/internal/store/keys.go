package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/domain"
)

// Sentinel errors for the key lifecycle.
//
// The API layer maps these onto status codes. They are deliberately distinct:
// "this application was rejected" and "this application expired" are different
// things for a polling agent to react to, and collapsing them into one error
// would force the handler to re-query the row and re-derive the state.
var (
	ErrApplicationMissing  = errors.New("application not found")
	ErrApplicationDecided  = errors.New("application was already decided")
	ErrApplicationRejected = errors.New("application was rejected")
	ErrApplicationExpired  = errors.New("application expired")
	ErrApplicationClaimed  = errors.New("application was already claimed")
	ErrClaimWindowClosed   = errors.New("claim window closed")
	ErrClaimMismatch       = errors.New("claim secret does not match")
	ErrKeyMissing          = errors.New("key not found")
	ErrRenewalExists       = errors.New("a renewal is already pending for this key")
	ErrRenewalMissing      = errors.New("renewal not found")
	ErrMaxPending          = errors.New("too many pending applications")
	ErrMaxActive           = errors.New("too many active keys")
)

// ApplicationInput is everything needed to open a key application.
type ApplicationInput struct {
	Label          string
	Purpose        string
	RequestedHours float64
	RequesterIP    string
	UserAgent      string
	// ClaimHash is sha256(hex) of the claim secret. The secret itself is shown
	// to the agent once and never stored.
	ClaimHash string
	// TTL bounds how long the application stays open while pending.
	TTL int64 // milliseconds
}

// applicationColumns is the single source of truth for the SELECT list, so the
// scan order can never drift from the insert order.
const applicationColumns = `id, claim_hash, label, purpose, requested_hours, status,
	created_at, expires_at, decided_at, claim_deadline, issued_key_id,
	requester_ip, user_agent, approved_name, approved_hours, approved_note`

func scanApplication(row interface{ Scan(...any) error }) (*domain.KeyApplication, error) {
	var a domain.KeyApplication
	err := row.Scan(
		&a.ID, &a.ClaimHash, &a.Label, &a.Purpose, &a.RequestedHours, &a.Status,
		&a.CreatedAt, &a.ExpiresAt, &a.DecidedAt, &a.ClaimDeadline, &a.IssuedKeyID,
		&a.RequesterIP, &a.UserAgent, &a.ApprovedName, &a.ApprovedHours, &a.ApprovedNote,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// --- application ------------------------------------------------------------

// CreateApplication opens a pending application.
//
// The pending count is checked inside the same transaction as the insert: doing
// it as a separate SELECT would let two concurrent submissions both observe
// "under the limit" and both insert.
func (s *Store) CreateApplication(in ApplicationInput, maxPending int, nowMS int64) (*domain.KeyApplication, error) {
	id, err := agentkey.NewID()
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var pending int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM key_applications WHERE status = 'pending'`,
	).Scan(&pending); err != nil {
		return nil, fmt.Errorf("count pending applications: %w", err)
	}
	if maxPending > 0 && pending >= maxPending {
		return nil, ErrMaxPending
	}

	a := &domain.KeyApplication{
		ID: id, ClaimHash: in.ClaimHash, Label: in.Label, Purpose: in.Purpose,
		RequestedHours: in.RequestedHours, Status: domain.ApplicationPending,
		CreatedAt: nowMS, ExpiresAt: nowMS + in.TTL,
		RequesterIP: in.RequesterIP, UserAgent: in.UserAgent,
	}
	if _, err := tx.Exec(
		`INSERT INTO key_applications
		   (id, claim_hash, label, purpose, requested_hours, status, created_at,
		    expires_at, decided_at, claim_deadline, issued_key_id, requester_ip,
		    user_agent, approved_name, approved_hours, approved_note)
		 VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, 0, 0, '', ?, ?, '', 0, '')`,
		a.ID, a.ClaimHash, a.Label, a.Purpose, a.RequestedHours, a.CreatedAt,
		a.ExpiresAt, a.RequesterIP, a.UserAgent,
	); err != nil {
		return nil, fmt.Errorf("insert application: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return a, nil
}

// GetApplication loads an application by id, including its claim hash and the
// parked approval metadata. Callers that hand the result to a client must strip
// those first.
func (s *Store) GetApplication(id string) (*domain.KeyApplication, error) {
	a, err := scanApplication(s.db.QueryRow(
		`SELECT `+applicationColumns+` FROM key_applications WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load application: %w", err)
	}
	return a, nil
}

// ListApplications returns applications with the given status, newest first.
// An empty status returns every application.
func (s *Store) ListApplications(status string, limit int) ([]domain.KeyApplication, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + applicationColumns + ` FROM key_applications`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	defer rows.Close()

	out := []domain.KeyApplication{}
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// CountApplications counts applications in a status ("" = all).
func (s *Store) CountApplications(status string) (int, error) {
	query := `SELECT COUNT(*) FROM key_applications`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count applications: %w", err)
	}
	return n, nil
}

// ExpireApplications closes every application that ran out of time while still
// pending or approved-but-unclaimed.
//
// Without this, a stale row keeps its slot in the pending cap forever and the
// dashboard shows a queue that is mostly dead. It is called opportunistically
// before reads rather than on a timer, so the service needs no scheduler.
func (s *Store) ExpireApplications(nowMS int64) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE key_applications
		    SET status = 'expired'
		  WHERE status = 'pending' AND expires_at <= ?
		     OR status = 'approved' AND claim_deadline <= ?`,
		nowMS, nowMS)
	if err != nil {
		return 0, fmt.Errorf("expire applications: %w", err)
	}
	return res.RowsAffected()
}

// DecideApplication records a human's approval or rejection.
//
// Approval parks the resulting name/TTL on the application instead of creating
// the key, because the key's token is derived from the claim secret, which the
// server only holds a hash of until the agent claims. Only a pending
// application can be decided, so a double-click cannot re-decide a rejection.
func (s *Store) DecideApplication(id string, approve bool, name string, hours float64, note string, claimDeadlineMS, nowMS int64) error {
	status := string(domain.ApplicationRejected)
	if approve {
		status = string(domain.ApplicationApproved)
	}
	res, err := s.db.Exec(
		`UPDATE key_applications
		    SET status = ?, decided_at = ?, approved_name = ?, approved_hours = ?,
		        approved_note = ?, claim_deadline = ?
		  WHERE id = ? AND status = 'pending'`,
		status, nowMS, name, hours, note, claimDeadlineMS, id)
	if err != nil {
		return fmt.Errorf("decide application: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "never existed" from "already decided" so the handler can
		// answer 404 vs 409 instead of guessing.
		a, err := s.GetApplication(id)
		if err != nil {
			return err
		}
		if a == nil {
			return ErrApplicationMissing
		}
		return ErrApplicationDecided
	}
	return nil
}

// ClaimApplication trades an approved application for a credential.
//
// This is the security-critical path, and the ordering matters:
//
//  1. The conditional UPDATE runs *first*, so the transaction takes the write
//     lock immediately instead of reading on a snapshot that a concurrent
//     claimer may already have invalidated (which would surface as
//     SQLITE_BUSY_SNAPSHOT rather than a clean loss).
//  2. The status='approved' predicate means exactly one concurrent claimer can
//     flip the row; everyone else gets zero rows affected and loses.
//  3. Only then is the claim secret checked. A wrong secret rolls the
//     transaction back, so the status change is undone and the real claimant
//     can still claim.
//
// The token is derived here — from the claim secret the caller just proved it
// holds — and only its hash is written. The plaintext is returned to the caller
// and is never stored.
func (s *Store) ClaimApplication(id, claimSecret string, kDerive []byte, maxActive int, nowMS int64) (*domain.AgentKey, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE key_applications
		    SET status = 'claimed', decided_at = ?
		  WHERE id = ? AND status = 'approved' AND claim_deadline > ?`,
		nowMS, id, nowMS)
	if err != nil {
		return nil, "", fmt.Errorf("claim application: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, "", err
	}
	if n == 0 {
		return nil, "", s.diagnoseClaim(id, nowMS)
	}

	var claimHash, name, note string
	var hours float64
	if err := tx.QueryRow(
		`SELECT claim_hash, approved_name, approved_hours, approved_note
		   FROM key_applications WHERE id = ?`, id,
	).Scan(&claimHash, &name, &hours, &note); err != nil {
		return nil, "", fmt.Errorf("read application for claim: %w", err)
	}

	// Constant time: a plain comparison here would leak the hash byte by byte.
	if !agentkey.EqualHash(claimHash, agentkey.HashToken(claimSecret)) {
		return nil, "", ErrClaimMismatch
	}

	// The active-key ceiling is enforced HERE, inside the same transaction that
	// already holds the write lock, rather than by a pre-check in the caller. A
	// caller-side count would be a check-then-act race: N concurrent claims
	// would each read a count below the cap and all insert.
	//
	// Returning ErrMaxActive rolls the status change back, so the application
	// stays claimable once the operator frees a slot — being turned away must
	// not burn the agent's one-time credential.
	if maxActive > 0 {
		var active int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM agent_keys
			  WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`, nowMS,
		).Scan(&active); err != nil {
			return nil, "", fmt.Errorf("count active keys for claim: %w", err)
		}
		if active >= maxActive {
			return nil, "", ErrMaxActive
		}
	}

	token := agentkey.DeriveToken(kDerive, id, claimSecret)
	keyID, err := agentkey.NewID()
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		name = "agent"
	}
	expiresAt := expiryFrom(hours, nowMS)

	if _, err := tx.Exec(
		`INSERT INTO agent_keys
		   (id, name, token_hash, token_prefix, created_at, expires_at, revoked_at,
		    last_used_at, request_count, note)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?)`,
		keyID, name, agentkey.HashToken(token), agentkey.Prefix(token),
		nowMS, expiresAt, note,
	); err != nil {
		return nil, "", fmt.Errorf("insert claimed key: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE key_applications SET issued_key_id = ? WHERE id = ?`, keyID, id,
	); err != nil {
		return nil, "", fmt.Errorf("record issued key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}

	key := &domain.AgentKey{
		ID: keyID, Name: name, TokenPrefix: agentkey.Prefix(token),
		CreatedAt: nowMS, ExpiresAt: expiresAt, Note: note,
	}
	return key, token, nil
}

// diagnoseClaim explains why a claim did not win, for the handler's status code.
func (s *Store) diagnoseClaim(id string, nowMS int64) error {
	a, err := s.GetApplication(id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrApplicationMissing
	}
	switch a.Status {
	case domain.ApplicationPending:
		return ErrApplicationMissing
	case domain.ApplicationRejected:
		return ErrApplicationRejected
	case domain.ApplicationClaimed:
		return ErrApplicationClaimed
	case domain.ApplicationExpired:
		return ErrApplicationExpired
	case domain.ApplicationApproved:
		if a.ClaimDeadline <= nowMS {
			return ErrClaimWindowClosed
		}
		return ErrApplicationMissing
	}
	return ErrApplicationMissing
}

// --- keys -------------------------------------------------------------------

const keyColumns = `id, name, token_prefix, token_hash, created_at, expires_at,
	revoked_at, last_used_at, request_count, note`

func scanKey(row interface{ Scan(...any) error }) (*domain.AgentKey, error) {
	var k domain.AgentKey
	if err := row.Scan(
		&k.ID, &k.Name, &k.TokenPrefix, &k.TokenHash, &k.CreatedAt, &k.ExpiresAt,
		&k.RevokedAt, &k.LastUsedAt, &k.RequestCount, &k.Note,
	); err != nil {
		return nil, err
	}
	return &k, nil
}

// ResolveAgentKey finds a live key by the hash of the presented token.
//
// Expiry and revocation are filtered in SQL so an unusable key is simply absent
// — the caller cannot accidentally treat a revoked key as valid.
func (s *Store) ResolveAgentKey(tokenHash string, nowMS int64) (*domain.AgentKey, error) {
	k, err := scanKey(s.db.QueryRow(
		`SELECT `+keyColumns+` FROM agent_keys
		  WHERE token_hash = ? AND revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`,
		tokenHash, nowMS))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve agent key: %w", err)
	}
	return k, nil
}

// TouchAgentKey records use of a key.
//
// One small write per authenticated request. This is the hot path, and it is
// deliberately not batched: the deployment is a single low-traffic binary, and
// "when was this key last used, and how much" is the main thing that makes a
// leaked credential noticeable.
func (s *Store) TouchAgentKey(id string, nowMS int64) error {
	if _, err := s.db.Exec(
		`UPDATE agent_keys SET last_used_at = ?, request_count = request_count + 1
		  WHERE id = ?`, nowMS, id); err != nil {
		return fmt.Errorf("touch agent key: %w", err)
	}
	return nil
}

// GetAgentKey loads one key by id.
func (s *Store) GetAgentKey(id string) (*domain.AgentKey, error) {
	k, err := scanKey(s.db.QueryRow(`SELECT `+keyColumns+` FROM agent_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load agent key: %w", err)
	}
	return k, nil
}

// ListAgentKeys returns every key, newest first. Revoked and expired keys are
// included: the dashboard is where you go to see what happened.
func (s *Store) ListAgentKeys() ([]domain.AgentKey, error) {
	rows, err := s.db.Query(`SELECT ` + keyColumns + ` FROM agent_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list agent keys: %w", err)
	}
	defer rows.Close()

	out := []domain.AgentKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// CountActiveKeys counts keys that could still authenticate.
func (s *Store) CountActiveKeys(nowMS int64) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM agent_keys
		  WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`, nowMS,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active keys: %w", err)
	}
	return n, nil
}

// CreateManualKey mints a key outright, for the case where a human wants to hand
// the credential to an agent that cannot poll for it.
//
// The plaintext is generated here and returned; only its hash is written, so
// this response is the one and only place the value exists.
func (s *Store) CreateManualKey(name string, hours float64, note string, maxActive int, nowMS int64) (*domain.AgentKey, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM agent_keys
		  WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`, nowMS,
	).Scan(&active); err != nil {
		return nil, "", fmt.Errorf("count active keys: %w", err)
	}
	if maxActive > 0 && active >= maxActive {
		return nil, "", ErrMaxActive
	}

	token, err := agentkey.NewToken()
	if err != nil {
		return nil, "", err
	}
	keyID, err := agentkey.NewID()
	if err != nil {
		return nil, "", err
	}
	expiresAt := expiryFrom(hours, nowMS)

	if _, err := tx.Exec(
		`INSERT INTO agent_keys
		   (id, name, token_hash, token_prefix, created_at, expires_at, revoked_at,
		    last_used_at, request_count, note)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?)`,
		keyID, name, agentkey.HashToken(token), agentkey.Prefix(token),
		nowMS, expiresAt, note,
	); err != nil {
		return nil, "", fmt.Errorf("insert manual key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}

	key := &domain.AgentKey{
		ID: keyID, Name: name, TokenPrefix: agentkey.Prefix(token),
		CreatedAt: nowMS, ExpiresAt: expiresAt, Note: note,
	}
	return key, token, nil
}

// KeyPatch describes a partial update. A nil field is left untouched, which is
// what makes "rename without touching the expiry" expressible.
type KeyPatch struct {
	Name      *string
	ExpiresAt *int64
	Revoked   *bool
	Note      *string
}

// UpdateAgentKey applies a patch. Setting Revoked=false is an un-revoke, which
// is allowed so a mistaken click is recoverable; it never resurrects a key that
// has already expired, because expiry is evaluated separately.
func (s *Store) UpdateAgentKey(id string, patch KeyPatch, nowMS int64) (*domain.AgentKey, error) {
	set := []string{}
	args := []any{}
	if patch.Name != nil {
		set = append(set, "name = ?")
		args = append(args, *patch.Name)
	}
	if patch.ExpiresAt != nil {
		set = append(set, "expires_at = ?")
		args = append(args, *patch.ExpiresAt)
	}
	if patch.Note != nil {
		set = append(set, "note = ?")
		args = append(args, *patch.Note)
	}
	if patch.Revoked != nil {
		if *patch.Revoked {
			set = append(set, "revoked_at = ?")
			args = append(args, nowMS)
		} else {
			set = append(set, "revoked_at = 0")
		}
	}
	if len(set) == 0 {
		return s.GetAgentKey(id)
	}

	query := `UPDATE agent_keys SET ` + joinComma(set) + ` WHERE id = ?`
	args = append(args, id)
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return nil, fmt.Errorf("update agent key: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Either missing, or a no-op update. Distinguish.
		k, err := s.GetAgentKey(id)
		if err != nil {
			return nil, err
		}
		if k == nil {
			return nil, ErrKeyMissing
		}
	}
	return s.GetAgentKey(id)
}

// --- renewals ---------------------------------------------------------------

const renewalColumns = `id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at`

func scanRenewal(row interface{ Scan(...any) error }) (*domain.KeyRenewal, error) {
	var r domain.KeyRenewal
	if err := row.Scan(&r.ID, &r.KeyID, &r.RequestedHours, &r.Status,
		&r.CreatedAt, &r.DecidedAt, &r.GrantedExpiresAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRenewal opens a renewal request for a key.
//
// Only one may be pending per key: otherwise an agent polling in a loop could
// bury the approval queue, and the human would have to reconcile several
// competing extensions of the same credential.
func (s *Store) CreateRenewal(keyID string, requestedHours float64, nowMS int64) (*domain.KeyRenewal, error) {
	key, err := s.GetAgentKey(keyID)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, ErrKeyMissing
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM key_renewals WHERE key_id = ? AND status = 'pending'`, keyID,
	).Scan(&existing); err != nil {
		return nil, fmt.Errorf("count pending renewals: %w", err)
	}
	if existing > 0 {
		return nil, ErrRenewalExists
	}

	id, err := agentkey.NewID()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO key_renewals (id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at)
		 VALUES (?, ?, ?, 'pending', ?, 0, 0)`,
		id, keyID, requestedHours, nowMS,
	); err != nil {
		return nil, fmt.Errorf("insert renewal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &domain.KeyRenewal{
		ID: id, KeyID: keyID, RequestedHours: requestedHours,
		Status: domain.RenewalPending, CreatedAt: nowMS,
	}, nil
}

// GetRenewal loads one renewal.
func (s *Store) GetRenewal(id string) (*domain.KeyRenewal, error) {
	r, err := scanRenewal(s.db.QueryRow(
		`SELECT `+renewalColumns+` FROM key_renewals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load renewal: %w", err)
	}
	return r, nil
}

// ListRenewals returns renewals with the given status ("" = all), newest first.
func (s *Store) ListRenewals(status string, limit int) ([]domain.KeyRenewal, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + renewalColumns + ` FROM key_renewals`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list renewals: %w", err)
	}
	defer rows.Close()

	out := []domain.KeyRenewal{}
	for rows.Next() {
		r, err := scanRenewal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// DecideRenewal approves (extending the key) or rejects a renewal request.
//
// The extension and the decision are one transaction: a crash between them would
// otherwise leave an approved request that never actually extended anything.
// An expired key can still be extended here — that is the point, since an agent
// that noticed its key about to lapse is exactly who files a renewal — but a
// revoked key cannot, because revocation is a deliberate human act.
func (s *Store) DecideRenewal(id string, approve bool, hours float64, nowMS int64) (*domain.KeyRenewal, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var keyID string
	var claimedStatus string
	err = tx.QueryRow(
		`SELECT key_id, status FROM key_renewals WHERE id = ?`, id,
	).Scan(&keyID, &claimedStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRenewalMissing
	}
	if err != nil {
		return nil, fmt.Errorf("load renewal: %w", err)
	}
	if claimedStatus != string(domain.RenewalPending) {
		return nil, ErrApplicationDecided
	}

	granted := int64(0)
	if approve {
		var currentExpiry, revokedAt int64
		err := tx.QueryRow(
			`SELECT expires_at, revoked_at FROM agent_keys WHERE id = ?`, keyID,
		).Scan(&currentExpiry, &revokedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyMissing
		}
		if err != nil {
			return nil, fmt.Errorf("load key for renewal: %w", err)
		}
		// A revoked key is dead by human decision; a renewal must not quietly
		// resurrect it.
		if revokedAt != 0 {
			return nil, ErrKeyMissing
		}

		// A renewal EXTENDS a key's life; it never shortens it. The new expiry is
		// measured from whichever is later, the current expiry or now, so that
		// renewing early keeps the time already paid for and renewing late still
		// yields a window that is entirely in the future.
		//
		// Granting a period from `now` alone would silently punish exactly the
		// behaviour the guide recommends ("renew days in advance"): a key with 7
		// days left renewed for 7 more would come out with 7 days, not 14.
		//
		// Two cases stay at "never expires" (0) and are never downgraded:
		//   - the key already never expires (0 means unbounded, not "expired"), and
		//   - the human granted 0 hours, which means "make it permanent".
		switch {
		case currentExpiry == 0:
			granted = 0
		case hours <= 0:
			granted = 0
		default:
			base := nowMS
			if currentExpiry > base {
				base = currentExpiry
			}
			granted = base + int64(math.Round(hours*3_600_000))
		}

		if _, err := tx.Exec(
			`UPDATE agent_keys SET expires_at = ? WHERE id = ?`, granted, keyID,
		); err != nil {
			return nil, fmt.Errorf("extend key: %w", err)
		}
	}

	status := string(domain.RenewalRejected)
	if approve {
		status = string(domain.RenewalApproved)
	}
	if _, err := tx.Exec(
		`UPDATE key_renewals SET status = ?, decided_at = ?, granted_expires_at = ?
		  WHERE id = ? AND status = 'pending'`,
		status, nowMS, granted, id,
	); err != nil {
		return nil, fmt.Errorf("decide renewal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &domain.KeyRenewal{
		ID: id, KeyID: keyID, Status: domain.RenewalStatus(status),
		CreatedAt: 0, DecidedAt: nowMS, GrantedExpiresAt: granted,
	}, nil
}

// --- ownership --------------------------------------------------------------

// ReportOwner returns owner_key_id for a report. found is false when the report
// does not exist, so callers can keep answering 404 for both "absent" and "not
// yours" — the two must stay indistinguishable to a caller who has no right to
// know which it was.
func (s *Store) ReportOwner(reportID string) (string, bool, error) {
	var owner string
	err := s.db.QueryRow(`SELECT owner_key_id FROM reports WHERE id = ?`, reportID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load report owner: %w", err)
	}
	return owner, true, nil
}

// ShareTokenOwner returns owner_key_id of the report a share token belongs to.
func (s *Store) ShareTokenOwner(token string) (owner string, active bool, found bool, err error) {
	err = s.db.QueryRow(
		`SELECT r.owner_key_id, t.is_active
		   FROM share_tokens t JOIN reports r ON r.id = t.report_id
		  WHERE t.token = ?`, token).Scan(&owner, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("load share token owner: %w", err)
	}
	return owner, active, true, nil
}

// expiryFrom converts an hours offset into an absolute epoch-millisecond
// expiry, where a non-positive value means "never expires".
func expiryFrom(hours float64, nowMS int64) int64 {
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 {
		return 0
	}
	return nowMS + int64(math.Round(hours*3_600_000))
}
