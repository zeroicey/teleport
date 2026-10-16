package store

import (
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/store/storedb"
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

// --- application ------------------------------------------------------------

// CreateApplication opens a pending application.
//
// The pending count is checked inside the same transaction as the insert: doing
// it as a separate SELECT would let two concurrent submissions both observe
// "under the limit" and both insert.
//
// In SQLite that atomicity came for free from the single-writer guarantee. In
// PostgreSQL it does not exist — two transactions can both read COUNT(*) = 0
// under READ COMMITTED and both write — so the count is taken behind
// LockPendingApplications. cap_lock_pg_test.go demonstrates the hazard
// deterministically and then shows this lock removing it.
func (s *Store) CreateApplication(in ApplicationInput, maxPending int, nowMS int64) (*domain.KeyApplication, error) {
	id, err := agentkey.NewID()
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(callCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(callCtx) }()
	q := s.q.WithTx(tx)

	if err := lockCap(callCtx, tx, LockPendingApplications); err != nil {
		return nil, err
	}
	pending, err := q.CountPendingApplications(callCtx)
	if err != nil {
		return nil, fmt.Errorf("count pending applications: %w", err)
	}
	if maxPending > 0 && int(pending) >= maxPending {
		return nil, ErrMaxPending
	}

	a := &domain.KeyApplication{
		ID: id, ClaimHash: in.ClaimHash, Label: in.Label, Purpose: in.Purpose,
		RequestedHours: in.RequestedHours, Status: domain.ApplicationPending,
		CreatedAt: nowMS, ExpiresAt: nowMS + in.TTL,
		RequesterIP: in.RequesterIP, UserAgent: in.UserAgent,
	}
	if err := q.InsertApplication(callCtx, storedb.InsertApplicationParams{
		ID: a.ID, ClaimHash: a.ClaimHash, Label: a.Label, Purpose: a.Purpose,
		RequestedHours: a.RequestedHours, CreatedAt: a.CreatedAt,
		ExpiresAt: a.ExpiresAt, RequesterIp: a.RequesterIP, UserAgent: a.UserAgent,
	}); err != nil {
		return nil, fmt.Errorf("insert application: %w", err)
	}
	if err := tx.Commit(callCtx); err != nil {
		return nil, err
	}
	return a, nil
}

// GetApplication loads an application by id, including its claim hash and the
// parked approval metadata. Callers that hand the result to a client must strip
// those first.
func (s *Store) GetApplication(id string) (*domain.KeyApplication, error) {
	a, err := s.q.GetApplication(callCtx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load application: %w", err)
	}
	out := applicationFromDB(a)
	return out, nil
}

// ListApplications returns applications with the given status, newest first.
// An empty status returns every application.
func (s *Store) ListApplications(status string, limit int) ([]domain.KeyApplication, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.q.ListApplications(callCtx, storedb.ListApplicationsParams{
		Status: status, LimitCount: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	out := []domain.KeyApplication{}
	for _, a := range rows {
		out = append(out, *applicationFromDB(a))
	}
	return out, nil
}

// CountApplications counts applications in a status ("" = all).
func (s *Store) CountApplications(status string) (int, error) {
	n, err := s.q.CountApplications(callCtx, status)
	if err != nil {
		return 0, fmt.Errorf("count applications: %w", err)
	}
	return int(n), nil
}

// ExpireApplications closes every application that ran out of time while still
// pending or approved-but-unclaimed.
//
// Without this, a stale row keeps its slot in the pending cap forever and the
// dashboard shows a queue that is mostly dead. It is called opportunistically
// before reads rather than on a timer, so the service needs no scheduler.
func (s *Store) ExpireApplications(nowMS int64) (int64, error) {
	n, err := s.q.ExpireApplications(callCtx, nowMS)
	if err != nil {
		return 0, fmt.Errorf("expire applications: %w", err)
	}
	return n, nil
}

// HandoffInput is everything needed to open an already-approved application on
// behalf of a named recipient.
//
// This is the admin-initiated path: the decision is made by the human at the
// moment the hand-off link is generated, so there is no pending phase at all.
type HandoffInput struct {
	// Label is who the credential is for ("A姐"). It becomes the key's name.
	Label string
	// Note is the administrator's annotation, parked as approved_note.
	Note string
	// Hours is the granted key lifetime; 0 means "never expires" (the admin
	// has to say so explicitly, exactly as on the approve endpoint).
	Hours float64
	// ClaimHash is sha256(hex) of the claim secret. The secret itself is
	// returned once to the caller and never stored.
	ClaimHash string
	// RequesterIP is recorded for audit. It is the administrator's address,
	// because the administrator is who initiated this.
	RequesterIP string
	UserAgent   string
	// LinkTTL bounds how long the hand-off link stays claimable.
	LinkTTL int64 // milliseconds
}

// CreateHandoffApplication inserts an application that is already approved and
// immediately claimable, in one transaction.
//
// Two deliberate differences from CreateApplication:
//
//  1. No pending phase. The admin decided when generating the link; parking a
//     pending row and then deciding it would leave a stray queue entry if the
//     second write failed, and would make the admin look at a queue they do not
//     have to act on.
//  2. No KEY_MAX_PENDING check. That cap exists to bound the *public* endpoint
//     against a flood; an authenticated administrator minting a link for a
//     colleague is not the traffic it protects against, and being refused by it
//     would be a confusing failure with no remedy in the UI.
//
// The claim deadline and the application expiry are both now+LinkTTL: this row
// has no pending lifetime to speak of, so the link's validity is the only clock
// that matters.
func (s *Store) CreateHandoffApplication(in HandoffInput, nowMS int64) (*domain.KeyApplication, error) {
	id, err := agentkey.NewID()
	if err != nil {
		return nil, err
	}

	a := &domain.KeyApplication{
		ID: id, ClaimHash: in.ClaimHash, Label: in.Label,
		Status:    domain.ApplicationApproved,
		CreatedAt: nowMS, ExpiresAt: nowMS + in.LinkTTL,
		DecidedAt:     nowMS,
		ClaimDeadline: nowMS + in.LinkTTL,
		RequesterIP:   in.RequesterIP, UserAgent: in.UserAgent,
		ApprovedName: in.Label, ApprovedHours: in.Hours, ApprovedNote: in.Note,
	}
	if err := s.q.InsertHandoffApplication(callCtx, storedb.InsertHandoffApplicationParams{
		ID: a.ID, ClaimHash: a.ClaimHash, Label: a.Label,
		RequestedHours: in.Hours, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt,
		DecidedAt: a.DecidedAt, ClaimDeadline: a.ClaimDeadline,
		RequesterIp: a.RequesterIP, UserAgent: a.UserAgent,
		ApprovedName: a.ApprovedName, ApprovedHours: a.ApprovedHours, ApprovedNote: a.ApprovedNote,
	}); err != nil {
		return nil, fmt.Errorf("insert handoff application: %w", err)
	}
	return a, nil
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
	n, err := s.q.DecideApplication(callCtx, storedb.DecideApplicationParams{
		Status: status, NowMs: nowMS, ApprovedName: name, ApprovedHours: hours,
		ApprovedNote: note, ClaimDeadline: claimDeadlineMS, ID: id,
	})
	if err != nil {
		return fmt.Errorf("decide application: %w", err)
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
//  1. The conditional UPDATE runs *first*, so exactly one concurrent claimer
//     can flip the row; everyone else gets zero rows affected and loses. The
//     predicate is the whole decision — status must still be 'approved' and the
//     claim window must still be open — so it cannot race.
//  2. Only then is the claim secret checked. A wrong secret rolls the
//     transaction back, so the status change is undone and the real claimant
//     can still claim.
//
// The token is derived here — from the claim secret the caller just proved it
// holds — and only its hash is written. The plaintext is returned to the caller
// and is never stored.
func (s *Store) ClaimApplication(id, claimSecret string, kDerive []byte, maxActive int, nowMS int64) (*domain.AgentKey, string, error) {
	tx, err := s.pool.Begin(callCtx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(callCtx) }()
	q := s.q.WithTx(tx)

	// decided_at is the HUMAN's decision time and must survive the claim.
	// Overwriting it with the claim time destroyed the only record of when an
	// operator approved the request, and left the row contradicting the value the
	// API had just returned. Nothing is lost by not storing a separate claim
	// time: the key inserted below is stamped with nowMS, and issued_key_id links
	// to it, so the claim time is exactly agent_keys.created_at.
	n, err := q.MarkApplicationClaimed(callCtx, storedb.MarkApplicationClaimedParams{
		ID: id, NowMs: nowMS,
	})
	if err != nil {
		return nil, "", fmt.Errorf("claim application: %w", err)
	}
	if n == 0 {
		return nil, "", diagnoseClaim(q, id, nowMS)
	}

	forClaim, err := q.GetApplicationForClaim(callCtx, id)
	if err != nil {
		return nil, "", fmt.Errorf("read application for claim: %w", err)
	}
	claimHash, name, hours, note := forClaim.ClaimHash, forClaim.ApprovedName,
		forClaim.ApprovedHours, forClaim.ApprovedNote

	// Constant time: a plain comparison here would leak the hash byte by byte.
	if !agentkey.EqualHash(claimHash, agentkey.HashToken(claimSecret)) {
		return nil, "", ErrClaimMismatch
	}

	// The active-key ceiling is enforced HERE, inside the same transaction that
	// already holds the write lock, rather than by a pre-check in the caller. A
	// caller-side count would be a check-then-act race: N concurrent claims
	// would each read a count below the cap and all insert. The advisory lock is
	// what makes the count-then-insert atomic now that there is no single
	// writer; CreateManualKey takes the SAME key, so the two minting paths
	// cannot each spend the last slot.
	//
	// Returning ErrMaxActive rolls the status change back, so the application
	// stays claimable once the operator frees a slot — being turned away must
	// not burn the agent's one-time credential.
	if maxActive > 0 {
		if err := lockCap(callCtx, tx, LockActiveKeys); err != nil {
			return nil, "", err
		}
		active, err := q.CountActiveKeys(callCtx, nowMS)
		if err != nil {
			return nil, "", fmt.Errorf("count active keys for claim: %w", err)
		}
		if int(active) >= maxActive {
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

	if err := q.InsertAgentKey(callCtx, storedb.InsertAgentKeyParams{
		ID: keyID, Name: name, TokenHash: agentkey.HashToken(token),
		TokenPrefix: agentkey.Prefix(token), CreatedAt: nowMS, ExpiresAt: expiresAt, Note: note,
	}); err != nil {
		return nil, "", fmt.Errorf("insert claimed key: %w", err)
	}
	if err := q.SetApplicationIssuedKey(callCtx, storedb.SetApplicationIssuedKeyParams{
		IssuedKeyID: keyID, ID: id,
	}); err != nil {
		return nil, "", fmt.Errorf("record issued key: %w", err)
	}
	if err := tx.Commit(callCtx); err != nil {
		return nil, "", err
	}

	key := &domain.AgentKey{
		ID: keyID, Name: name, TokenPrefix: agentkey.Prefix(token),
		CreatedAt: nowMS, ExpiresAt: expiresAt, Note: note,
	}
	return key, token, nil
}

// diagnoseClaim explains why a claim did not win, for the handler's status code.
//
// It reads through **the same transaction's query set**, never through the pool.
// That is not a style preference: the caller still holds its connection while
// this runs, and a small pool plus N concurrent losers each asking for a second
// connection is a deadlock — the losers hold every connection and wait for one
// more. It deadlocked with 8 racers on a 4-connection pool, which is exactly the
// shape of the concurrency tests here.
//
// Reading inside the transaction is also correct. The conditional UPDATE that
// returned zero rows could only have done so after any competing writer
// committed, so under READ COMMITTED this fresh statement observes the winning
// state — which is the thing being diagnosed.
func diagnoseClaim(q *storedb.Queries, id string, nowMS int64) error {
	a, err := q.GetApplication(callCtx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApplicationMissing
	}
	if err != nil {
		return err
	}
	switch domain.ApplicationStatus(a.Status) {
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

// ResolveAgentKey finds a live key by the hash of the presented token.
//
// Expiry and revocation are filtered in SQL so an unusable key is simply absent
// — the caller cannot accidentally treat a revoked key as valid.
//
// "Live" is strict: a key past its expiry does not resolve. Callers that must
// tolerate an expired key — the renewal path, so an agent which let its key
// lapse can still ask a human for more time — use ResolveAgentKeyWithinGrace.
func (s *Store) ResolveAgentKey(tokenHash string, nowMS int64) (*domain.AgentKey, error) {
	k, _, err := s.ResolveAgentKeyWithinGrace(tokenHash, nowMS, 0)
	return k, err
}

// ResolveAgentKeyWithinGrace finds a key by token hash, tolerating one that is
// past its expiry by up to graceMS milliseconds.
//
// The second return value reports whether the row matched *only* because of the
// grace window, i.e. the key is genuinely expired and must not be treated as a
// usable credential. It is always false when graceMS is 0.
//
// Revocation is never tolerated at any grace: revoked_at != 0 means a human
// deliberately killed the key, and forgetting to renew is not the same act as
// being told to stop. An unknown, revoked or beyond-grace key yields nil — the
// three stay indistinguishable, which is what the auth middleware promises.
//
// The comparison is `expires_at > nowMS - graceMS` rather than
// `expires_at + graceMS > nowMS` so that adding two millisecond values can never
// overflow. expires_at is bounded at write time (see expiryFrom) and the config
// validator bounds grace, so the subtraction cannot underflow either.
func (s *Store) ResolveAgentKeyWithinGrace(tokenHash string, nowMS, graceMS int64) (*domain.AgentKey, bool, error) {
	row, err := s.q.ResolveAgentKey(callCtx, storedb.ResolveAgentKeyParams{
		TokenHash: tokenHash, NotExpiredAfter: nowMS - graceMS,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve agent key: %w", err)
	}
	k := keyFromDB(row)
	expired := k.ExpiresAt != 0 && k.ExpiresAt <= nowMS
	return k, expired, nil
}

// TouchAgentKey records use of a key.
//
// One small write per authenticated request. This is the hot path, and it is
// deliberately not batched: the deployment is a single low-traffic binary, and
// "when was this key last used, and how much" is the main thing that makes a
// leaked credential noticeable.
func (s *Store) TouchAgentKey(id string, nowMS int64) error {
	if err := s.q.TouchAgentKey(callCtx, storedb.TouchAgentKeyParams{
		LastUsedAt: nowMS, ID: id,
	}); err != nil {
		return fmt.Errorf("touch agent key: %w", err)
	}
	return nil
}

// GetAgentKey loads one key by id.
func (s *Store) GetAgentKey(id string) (*domain.AgentKey, error) {
	k, err := s.q.GetAgentKey(callCtx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load agent key: %w", err)
	}
	return keyFromDB(k), nil
}

// ListAgentKeys returns every key, newest first. Revoked and expired keys are
// included: the dashboard is where you go to see what happened.
func (s *Store) ListAgentKeys() ([]domain.AgentKey, error) {
	rows, err := s.q.ListAgentKeys(callCtx)
	if err != nil {
		return nil, fmt.Errorf("list agent keys: %w", err)
	}
	out := []domain.AgentKey{}
	for _, k := range rows {
		out = append(out, *keyFromDB(k))
	}
	return out, nil
}

// CountActiveKeys counts keys that could still authenticate.
func (s *Store) CountActiveKeys(nowMS int64) (int, error) {
	n, err := s.q.CountActiveKeys(callCtx, nowMS)
	if err != nil {
		return 0, fmt.Errorf("count active keys: %w", err)
	}
	return int(n), nil
}

// CreateManualKey mints a key outright, for the case where a human wants to hand
// the credential to an agent that cannot poll for it.
//
// The plaintext is generated here and returned; only its hash is written, so
// this response is the one and only place the value exists.
func (s *Store) CreateManualKey(name string, hours float64, note string, maxActive int, nowMS int64) (*domain.AgentKey, string, error) {
	tx, err := s.pool.Begin(callCtx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(callCtx) }()
	q := s.q.WithTx(tx)

	// The same advisory lock ClaimApplication takes. The two paths that mint a
	// credential must share one lock, or they can each observe the last free
	// slot and both take it.
	if maxActive > 0 {
		if err := lockCap(callCtx, tx, LockActiveKeys); err != nil {
			return nil, "", err
		}
		active, err := q.CountActiveKeys(callCtx, nowMS)
		if err != nil {
			return nil, "", fmt.Errorf("count active keys: %w", err)
		}
		if int(active) >= maxActive {
			return nil, "", ErrMaxActive
		}
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

	if err := q.InsertAgentKey(callCtx, storedb.InsertAgentKeyParams{
		ID: keyID, Name: name, TokenHash: agentkey.HashToken(token),
		TokenPrefix: agentkey.Prefix(token), CreatedAt: nowMS, ExpiresAt: expiresAt, Note: note,
	}); err != nil {
		return nil, "", fmt.Errorf("insert manual key: %w", err)
	}
	if err := tx.Commit(callCtx); err != nil {
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
//
// The patch used to be a SET list built by string concatenation. It is now one
// static statement whose columns are COALESCE(sqlc.narg, column) — a NULL means
// "absent" — except revoked_at, which needs a CASE because 0 is a real target
// value ("un-revoke") and would otherwise be indistinguishable from NULL.
func (s *Store) UpdateAgentKey(id string, patch KeyPatch, nowMS int64) (*domain.AgentKey, error) {
	if patch.Name == nil && patch.ExpiresAt == nil && patch.Note == nil && patch.Revoked == nil {
		return s.GetAgentKey(id)
	}

	n, err := s.q.UpdateAgentKey(callCtx, storedb.UpdateAgentKeyParams{
		Name: patch.Name, ExpiresAt: patch.ExpiresAt, Note: patch.Note,
		Revoked: patch.Revoked, NowMs: nowMS, ID: id,
	})
	if err != nil {
		return nil, fmt.Errorf("update agent key: %w", err)
	}
	if n == 0 {
		// Either missing, or a no-op update that matched the row but changed
		// nothing. Distinguish.
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

// CreateRenewal opens a renewal request for a key.
//
// Only one may be pending per key: otherwise an agent polling in a loop could
// bury the approval queue, and the human would have to reconcile several
// competing extensions of the same credential. The count and the insert are one
// transaction behind LockPendingRenewals, because PostgreSQL has no single
// writer to make the pair atomic for free.
func (s *Store) CreateRenewal(keyID string, requestedHours float64, nowMS int64) (*domain.KeyRenewal, error) {
	key, err := s.GetAgentKey(keyID)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, ErrKeyMissing
	}

	tx, err := s.pool.Begin(callCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(callCtx) }()
	q := s.q.WithTx(tx)

	if err := lockCap(callCtx, tx, LockPendingRenewals); err != nil {
		return nil, err
	}
	existing, err := q.CountPendingRenewals(callCtx, keyID)
	if err != nil {
		return nil, fmt.Errorf("count pending renewals: %w", err)
	}
	if existing > 0 {
		return nil, ErrRenewalExists
	}

	id, err := agentkey.NewID()
	if err != nil {
		return nil, err
	}
	if err := q.InsertRenewal(callCtx, storedb.InsertRenewalParams{
		ID: id, KeyID: keyID, RequestedHours: requestedHours, CreatedAt: nowMS,
	}); err != nil {
		return nil, fmt.Errorf("insert renewal: %w", err)
	}
	if err := tx.Commit(callCtx); err != nil {
		return nil, err
	}
	return &domain.KeyRenewal{
		ID: id, KeyID: keyID, RequestedHours: requestedHours,
		Status: domain.RenewalPending, CreatedAt: nowMS,
	}, nil
}

// GetRenewal loads one renewal.
func (s *Store) GetRenewal(id string) (*domain.KeyRenewal, error) {
	r, err := s.q.GetRenewal(callCtx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load renewal: %w", err)
	}
	return renewalFromDB(r), nil
}

// ListRenewals returns renewals with the given status ("" = all), newest first.
func (s *Store) ListRenewals(status string, limit int) ([]domain.KeyRenewal, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.q.ListRenewals(callCtx, storedb.ListRenewalsParams{
		Status: status, LimitCount: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list renewals: %w", err)
	}
	out := []domain.KeyRenewal{}
	for _, r := range rows {
		out = append(out, *renewalFromDB(r))
	}
	return out, nil
}

// DecideRenewal approves (extending the key) or rejects a renewal request.
//
// The extension and the decision are one transaction: a crash between them would
// otherwise leave an approved request that never actually extended anything.
// An expired key can still be extended here — that is the point, since an agent
// that noticed its key about to lapse is exactly who files a renewal — but a
// revoked key cannot, because revocation is a deliberate human act.
//
// The renewal row is read **FOR UPDATE**, and that is not decoration. "Read the
// status, then write" is the same check-then-act shape as the cap counts, and
// PostgreSQL gives it no atomicity on its own: two concurrent decisions would
// both read 'pending', both extend the key, and only then would the guarded
// UPDATE refuse the second — after the double grant had already happened. SQLite
// got this right for free from BEGIN IMMEDIATE, and the port must get it right
// explicitly. Because the row already exists, a row lock is the correct tool
// here (unlike the caps, where the rows that would violate the limit do not
// exist yet and `FOR UPDATE` cannot help).
//
// Measured before the fix: two overlapping transactions left the key with TWO
// extensions from ONE renewal request. See
// .ai/pitfalls/cases/2026-10-16-decide-renewal-granted-twice.md.
func (s *Store) DecideRenewal(id string, approve bool, hours float64, nowMS int64) (*domain.KeyRenewal, error) {
	tx, err := s.pool.Begin(callCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(callCtx) }()
	q := s.q.WithTx(tx)

	forDecision, err := q.LockRenewalForDecision(callCtx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRenewalMissing
	}
	if err != nil {
		return nil, fmt.Errorf("load renewal: %w", err)
	}
	keyID, claimedStatus := forDecision.KeyID, forDecision.Status
	if claimedStatus != string(domain.RenewalPending) {
		return nil, ErrApplicationDecided
	}

	granted := int64(0)
	if approve {
		key, err := q.GetKeyExpiryForRenewal(callCtx, keyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyMissing
		}
		if err != nil {
			return nil, fmt.Errorf("load key for renewal: %w", err)
		}
		// A revoked key is dead by human decision; a renewal must not quietly
		// resurrect it.
		if key.RevokedAt != 0 {
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
		case key.ExpiresAt == 0:
			granted = 0
		case hours <= 0:
			granted = 0
		default:
			base := nowMS
			if key.ExpiresAt > base {
				base = key.ExpiresAt
			}
			granted = base + hoursToMS(hours)
		}

		if err := q.ExtendAgentKey(callCtx, storedb.ExtendAgentKeyParams{
			ExpiresAt: granted, ID: keyID,
		}); err != nil {
			return nil, fmt.Errorf("extend key: %w", err)
		}
	}

	status := string(domain.RenewalRejected)
	if approve {
		status = string(domain.RenewalApproved)
	}
	// The row count is checked rather than discarded. Under SQLite this guard
	// was the only protection and it was sufficient, because the single-writer
	// guarantee meant two decisions could never both reach this line. That is no
	// longer true, so the real protection is now the FOR UPDATE on the read above
	// — this is the second line of defence, and it must not be silently dropped.
	decided, err := q.DecideRenewal(callCtx, storedb.DecideRenewalParams{
		Status: status, NowMs: nowMS, GrantedExpiresAt: granted, ID: id,
	})
	if err != nil {
		return nil, fmt.Errorf("decide renewal: %w", err)
	}
	if decided == 0 {
		// Unreachable while the locking read above holds the row: it would mean
		// the renewal stopped being pending inside this very transaction. Fail
		// closed rather than reporting a decision that was not recorded.
		return nil, ErrApplicationDecided
	}
	if err := tx.Commit(callCtx); err != nil {
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
	owner, err := s.q.ReportOwner(callCtx, reportID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load report owner: %w", err)
	}
	return owner, true, nil
}

// ShareTokenOwner returns owner_key_id of the report a share token belongs to.
func (s *Store) ShareTokenOwner(token string) (owner string, active bool, found bool, err error) {
	row, err := s.q.ShareTokenOwner(callCtx, token)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("load share token owner: %w", err)
	}
	return row.OwnerKeyID, row.IsActive == 1, true, nil
}

// expiryFrom converts an hours offset into an absolute epoch-millisecond
// expiry, where a non-positive value means "never expires".
// maxExpiryHours bounds any requested lifetime at 100 years. It exists so that
// hoursToMS can never overflow: a float64 above the int64 range (1e300, say)
// converts to an implementation-defined value, and on amd64 that value is
// negative — a request for a very long life would silently produce an
// expires_at in 1970, i.e. a key that is born expired. Clamping keeps the
// failure mode "absurdly long, but valid and positive" instead of "garbage".
//
// The API layer rejects out-of-range input with 400 before it reaches here; this
// is the invariant that holds even if a future caller forgets.
const maxExpiryHours = 100 * 365 * 24

// hoursToMS converts a requested lifetime to milliseconds, clamped to
// maxExpiryHours so the result is always non-negative and cannot overflow.
func hoursToMS(hours float64) int64 {
	if hours > maxExpiryHours {
		hours = maxExpiryHours
	}
	return int64(math.Round(hours * 3_600_000))
}

func expiryFrom(hours float64, nowMS int64) int64 {
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 {
		return 0
	}
	return nowMS + hoursToMS(hours)
}
