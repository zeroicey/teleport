// Package store owns the PostgreSQL connection, schema migration and all SQL.
package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/store/storedb"
)

// Store is the data-access layer. All SQL in the application lives here.
//
// It owns the connection pool and a sqlc-generated query set bound to it. The
// generated statements live in one place (internal/store/queries/*.sql) that is
// also read by the drift guard, so a query cannot be checking against a schema
// other than the one the binary embeds and applies.
type Store struct {
	pool *pgxpool.Pool
	q    *storedb.Queries
}

// New wraps an open pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: storedb.New(pool)}
}

// Pool exposes the handle for health checks and for tests that need to assert
// something the store's own API cannot express (an orphan count, a raw column).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// callCtx is the context every store method runs under.
//
// The Store's method signatures deliberately do NOT take a context.Context. P2
// is a port, not a redesign, and adding a parameter would touch every call site
// *and* every assertion in a suite that this port's own decision record freezes
// ("a failing assertion is investigated individually — never bulk-edited").
// Threading a request context through is a separate change, and it belongs with
// the P3 HTTP port where a per-request context actually exists.
//
// A package-level background context is not a shared mutable resource: it is
// exactly what context.Background() returns, and a store call that must be
// cancelled can be given one when the signatures change.
var callCtx = context.Background()

// NowMS returns the current time in epoch milliseconds, matching the schema
// convention (all *_at columns are ms, like JavaScript's Date.now()).
func NowMS() int64 { return time.Now().UnixMilli() }

// ---------------------------------------------------------------------------
// row mapping: sqlc rows -> domain types
// ---------------------------------------------------------------------------
//
// sqlc generates a row type per query shape and reuses a table model when a
// SELECT lists the table's columns in the table's own order (see
// queries/keys.sql). These helpers are the single place the generated types are
// translated into the domain types the API layer consumes, so a column that is
// added later cannot be half-wired.

func reportFromDB(r storedb.Report) *domain.Report {
	return &domain.Report{
		ID: r.ID, Title: r.Title, Category: r.Category,
		Format:  domain.ReportFormat(r.Format),
		Content: r.Content, Metadata: decodeMetadata(r.Metadata),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, OwnerKeyID: r.OwnerKeyID,
	}
}

func shareFromDB(t storedb.ShareToken) *domain.ShareToken {
	return &domain.ShareToken{
		Token: t.Token, ReportID: t.ReportID, CreatedAt: t.CreatedAt,
		ExpiresAt: t.ExpiresAt, IsActive: t.IsActive == 1, ViewCount: t.ViewCount,
	}
}

func keyFromDB(k storedb.AgentKey) *domain.AgentKey {
	return &domain.AgentKey{
		ID: k.ID, Name: k.Name, TokenPrefix: k.TokenPrefix, TokenHash: k.TokenHash,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
		LastUsedAt: k.LastUsedAt, RequestCount: k.RequestCount, Note: k.Note,
	}
}

func applicationFromDB(a storedb.KeyApplication) *domain.KeyApplication {
	return &domain.KeyApplication{
		ID: a.ID, ClaimHash: a.ClaimHash, Label: a.Label, Purpose: a.Purpose,
		RequestedHours: a.RequestedHours, Status: domain.ApplicationStatus(a.Status),
		CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, DecidedAt: a.DecidedAt,
		ClaimDeadline: a.ClaimDeadline, IssuedKeyID: a.IssuedKeyID,
		RequesterIP: a.RequesterIp, UserAgent: a.UserAgent,
		ApprovedName: a.ApprovedName, ApprovedHours: a.ApprovedHours, ApprovedNote: a.ApprovedNote,
	}
}

func renewalFromDB(r storedb.KeyRenewal) *domain.KeyRenewal {
	return &domain.KeyRenewal{
		ID: r.ID, KeyID: r.KeyID, RequestedHours: r.RequestedHours,
		Status:    domain.RenewalStatus(r.Status),
		CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt, GrantedExpiresAt: r.GrantedExpiresAt,
	}
}

// decodeMetadata parses the stored JSON blob, degrading to an empty object so a
// corrupt value can never break a read path.
func decodeMetadata(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
