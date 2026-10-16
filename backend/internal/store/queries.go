package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/store/storedb"
)

// ---------------------------------------------------------------------------
// writes
// ---------------------------------------------------------------------------

// CreateReport inserts a report together with its first share token, in a
// single transaction so we never persist a report whose promised share link
// does not exist.
//
// A share token is always created. The previous implementation resolved the
// duration as `input.autoShareHours ?? config.defaultShareHours`, and the
// configured default is itself always a number, so the "no share token" branch
// was in fact unreachable. An effective duration of 0 means "never expires".
func (s *Store) CreateReport(input domain.CreateReportInput, defaultShareHours int) (*domain.Report, *domain.ShareToken, error) {
	id, err := NewID()
	if err != nil {
		return nil, nil, err
	}
	ts := NowMS()
	format := input.Format
	if format == "" {
		format = domain.FormatMarkdown
	}
	category := input.Category
	if category == "" {
		category = "general"
	}
	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, nil, fmt.Errorf("encode metadata: %w", err)
	}

	shareHours := float64(defaultShareHours)
	if input.AutoShareHours != nil {
		shareHours = *input.AutoShareHours
	}

	tx, err := s.pool.Begin(callCtx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(callCtx) }()
	q := s.q.WithTx(tx)

	if err := q.CreateReport(callCtx, storedb.CreateReportParams{
		ID: id, Title: input.Title, Category: category, Format: string(format),
		Content: input.Content, Metadata: string(metadataJSON),
		CreatedAt: ts, UpdatedAt: ts, OwnerKeyID: input.OwnerKeyID,
	}); err != nil {
		return nil, nil, fmt.Errorf("insert report: %w", err)
	}

	report := &domain.Report{
		ID: id, Title: input.Title, Category: category, Format: format,
		Content: input.Content, Metadata: metadata, CreatedAt: ts, UpdatedAt: ts,
		OwnerKeyID: input.OwnerKeyID,
	}

	token, err := NewShareToken()
	if err != nil {
		return nil, nil, err
	}
	expiresAt := HoursFromNow(shareHours, defaultShareHours)
	if err := q.CreateShareToken(callCtx, storedb.CreateShareTokenParams{
		Token: token, ReportID: id, CreatedAt: ts, ExpiresAt: expiresAt,
	}); err != nil {
		return nil, nil, fmt.Errorf("insert share token: %w", err)
	}
	share := &domain.ShareToken{
		Token: token, ReportID: id, CreatedAt: ts,
		ExpiresAt: expiresAt, IsActive: true, ViewCount: 0,
	}

	if err := tx.Commit(callCtx); err != nil {
		return nil, nil, err
	}
	return report, share, nil
}

// RevokeToken deactivates a token. It returns false only when the token does
// not exist at all; revoking an already-revoked token is a no-op success.
func (s *Store) RevokeToken(token string) (bool, error) {
	n, err := s.q.RevokeShareToken(callCtx, token)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	// Nothing was flipped: the token is absent, or it was already revoked.
	// Distinguishing the two is what makes a repeated revoke a success rather
	// than a 404.
	exists, err := s.q.ShareTokenExists(callCtx, token)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// CreateShareToken adds another share link to an existing report. It returns
// (nil, nil) when the report does not exist.
func (s *Store) CreateShareToken(reportID string, expiresInHours float64) (*domain.ShareToken, error) {
	exists, err := s.q.ReportExists(callCtx, reportID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	token, err := NewShareToken()
	if err != nil {
		return nil, err
	}
	ts := NowMS()
	expiresAt := HoursFromNow(expiresInHours, 0)
	if err := s.q.CreateShareToken(callCtx, storedb.CreateShareTokenParams{
		Token: token, ReportID: reportID, CreatedAt: ts, ExpiresAt: expiresAt,
	}); err != nil {
		return nil, err
	}
	return &domain.ShareToken{
		Token: token, ReportID: reportID, CreatedAt: ts,
		ExpiresAt: expiresAt, IsActive: true, ViewCount: 0,
	}, nil
}

// SharePatch is a partial update of a share token.
type SharePatch struct {
	ExpiresInHours *float64
	ExpiresAt      *int64
	IsActive       *bool
}

// UpdateShareToken applies a partial update and returns the updated row, or
// (nil, nil) when the token does not exist.
//
// The two expiry fields are resolved here, in the original's order: an explicit
// expiresInHours wins over expiresAt, and either one becomes a single nullable
// expires_at argument. sqlc.narg turns a nil into SQL NULL, which the query's
// COALESCE reads as "leave this column alone" — that is what makes PATCH a patch
// without building a SET list by string concatenation.
func (s *Store) UpdateShareToken(token string, patch SharePatch) (*domain.ShareToken, error) {
	var expiresAt *int64
	if patch.ExpiresInHours != nil {
		v := HoursFromNow(*patch.ExpiresInHours, 0)
		expiresAt = &v
	} else if patch.ExpiresAt != nil {
		expiresAt = patch.ExpiresAt
	}
	var isActive *int32
	if patch.IsActive != nil {
		v := int32(boolToInt(*patch.IsActive))
		isActive = &v
	}
	if expiresAt == nil && isActive == nil {
		// Nothing to change. Skipping the write keeps the original's behaviour
		// of not touching the row at all, rather than issuing a no-op UPDATE
		// that bumps the row version.
		return s.GetShareToken(token)
	}

	if _, err := s.q.UpdateShareToken(callCtx, storedb.UpdateShareTokenParams{
		ExpiresAt: expiresAt, IsActive: isActive, Token: token,
	}); err != nil {
		return nil, err
	}
	return s.GetShareToken(token)
}

// DeleteShareToken removes a token. Returns false when it did not exist.
func (s *Store) DeleteShareToken(token string) (bool, error) {
	n, err := s.q.DeleteShareToken(callCtx, token)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordView increments the view counter for a token. Best-effort by design:
// a failure here must never fail the page render.
func (s *Store) RecordView(token string) error {
	return s.q.RecordShareView(callCtx, token)
}

// CleanupExpired removes inactive, expired tokens. Returns the number deleted.
func (s *Store) CleanupExpired() (int64, error) {
	return s.q.CleanupExpiredShareTokens(callCtx, NowMS())
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

// reportByID loads one report, leaving pgx.ErrNoRows to the caller so that
// "absent" and "read failed" stay distinguishable.
func (s *Store) reportByID(id string) (*domain.Report, error) {
	row, err := s.q.GetReport(callCtx, id)
	if err != nil {
		return nil, err
	}
	return reportFromDB(row), nil
}

// GetReport returns a single report including its content.
func (s *Store) GetReport(id string) (*domain.Report, error) {
	report, err := s.reportByID(id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return report, err
}

// ListReports returns newest-first summaries (content omitted).
func (s *Store) ListReports(category string, limit, offset int) ([]domain.Report, error) {
	rows, err := s.q.ListReports(callCtx, storedb.ListReportsParams{
		Category: category, LimitCount: int32(limit), OffsetCount: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := []domain.Report{}
	for _, r := range rows {
		out = append(out, *reportFromDB(storedb.Report(r)))
	}
	return out, nil
}

// ListReportsByOwner returns newest-first summaries published by one key.
//
// This is the read side of the ownership model. Without it an agent can retrieve
// a report only if it still remembers the id it was handed at creation time —
// asking about anyone else's id returns 404, so a forgotten id is unrecoverable.
// Owning something you cannot enumerate is not ownership.
//
// An empty ownerKeyID is a real value, not a wildcard: it selects the reports
// published by the root credential, whose owner_key_id is the empty string.
// Callers wanting "everything" must use ListReports.
func (s *Store) ListReportsByOwner(ownerKeyID, category string, limit, offset int) ([]domain.Report, error) {
	rows, err := s.q.ListReportsByOwner(callCtx, storedb.ListReportsByOwnerParams{
		OwnerKeyID: ownerKeyID, Category: category,
		LimitCount: int32(limit), OffsetCount: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := []domain.Report{}
	for _, r := range rows {
		out = append(out, *reportFromDB(storedb.Report(r)))
	}
	return out, nil
}

// GetShareToken returns a single token row.
func (s *Store) GetShareToken(token string) (*domain.ShareToken, error) {
	row, err := s.q.GetShareToken(callCtx, token)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return shareFromDB(row), nil
}

// ListShareTokens returns all tokens belonging to a report, newest first.
func (s *Store) ListShareTokens(reportID string) ([]domain.ShareToken, error) {
	rows, err := s.q.ListShareTokens(callCtx, reportID)
	if err != nil {
		return nil, err
	}
	out := []domain.ShareToken{}
	for _, t := range rows {
		out = append(out, *shareFromDB(t))
	}
	return out, nil
}

// ResolveShare looks up a share token and its report, classifying the result as
// ok / missing / gone.
//
// Liveness is computed at read time (expires_at = 0 means "never expires"), so
// no background job is needed to flip a flag when a link expires.
func (s *Store) ResolveShare(token string) (*domain.Resolution, error) {
	row, err := s.q.ResolveShare(callCtx, token)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.Resolution{Status: domain.StatusMissing}, nil
	}
	if err != nil {
		return nil, err
	}

	share := &domain.ShareToken{
		Token: row.Token, ReportID: row.ReportID, CreatedAt: row.TokenCreatedAt,
		ExpiresAt: row.TokenExpiresAt, IsActive: row.TokenIsActive == 1,
		ViewCount: row.TokenViewCount,
	}
	report := &domain.Report{
		ID: row.ReportIDOfReport, Title: row.ReportTitle, Category: row.ReportCategory,
		Format:  domain.ReportFormat(row.ReportFormat),
		Content: row.ReportContent, Metadata: decodeMetadata(row.ReportMetadata),
		CreatedAt: row.ReportCreatedAt, UpdatedAt: row.ReportUpdatedAt,
	}

	// Unknown, revoked and expired are distinct only in status code; a revoked
	// link intentionally reports the same "missing" as one that never existed.
	if !share.IsActive {
		return &domain.Resolution{Status: domain.StatusMissing}, nil
	}
	if share.ExpiresAt != 0 && share.ExpiresAt <= NowMS() {
		return &domain.Resolution{Status: domain.StatusGone}, nil
	}
	return &domain.Resolution{Status: domain.StatusOK, Report: report, Share: share}, nil
}

// ---------------------------------------------------------------------------
// writes: update and delete
// ---------------------------------------------------------------------------

// UpdateReport applies a partial update and returns the stored row afterwards.
//
// found is false when the report does not exist — including when it was deleted
// between the caller's ownership check and this statement, which is why the
// caller must treat it as a 404 rather than assume its earlier read still holds.
//
// The COALESCE trick is what makes PATCH a patch: a nil argument leaves the
// column untouched, so "field absent" and "field set to this value" stay
// distinguishable without building SQL by string concatenation.
//
// updated_at is `GREATEST(now, updated_at + 1)` rather than a plain `now`, and
// both halves of that matter:
//
//   - The `+ 1` keeps it strictly increasing. Without it, an update landing in
//     the same millisecond as the previous write would set updated_at to the
//     value already stored, which is exactly the condition
//     trg_reports_touch_updated_at watches for — the trigger would then fire and
//     rewrite the column from the database clock. That would make the
//     "strictly increasing, never before created_at" invariant depend on the
//     clock's resolution instead of on this statement.
//   - `GREATEST(now, ...)` means a backwards system clock cannot move the column
//     backwards either.
//
// SQLite's two-argument `max(a, b)` is PostgreSQL's `GREATEST`: `max()` here is
// an aggregate function, so the naive translation would not even mean the same
// thing.
func (s *Store) UpdateReport(id string, patch domain.ReportPatch, nowMS int64) (*domain.Report, bool, error) {
	var metadataJSON *string
	if patch.Metadata != nil {
		encoded, err := json.Marshal(patch.Metadata)
		if err != nil {
			return nil, false, fmt.Errorf("encode metadata: %w", err)
		}
		asString := string(encoded)
		metadataJSON = &asString
	}

	var format *string
	if patch.Format != nil {
		f := string(*patch.Format)
		format = &f
	}

	affected, err := s.q.UpdateReport(callCtx, storedb.UpdateReportParams{
		Title: patch.Title, Category: patch.Category, Format: format,
		Content: patch.Content, Metadata: metadataJSON, NowMs: nowMS, ID: id,
	})
	if err != nil {
		return nil, false, fmt.Errorf("update report: %w", err)
	}
	if affected == 0 {
		return nil, false, nil
	}

	report, err := s.reportByID(id)
	if err != nil {
		return nil, false, fmt.Errorf("reload report: %w", err)
	}
	return report, true, nil
}

// DeleteReport removes a report. found is false when there was nothing to
// delete, so a repeated DELETE answers 404 rather than pretending to succeed.
//
// The report's share tokens go with it: share_tokens.report_id declares
// ON DELETE CASCADE and PostgreSQL always enforces foreign keys, so this single
// statement is the whole operation. (SQLite needed a pragma for that; losing the
// pragma there silently orphaned rows. PostgreSQL has no such switch, so the
// cascade is now unconditional rather than configuration-dependent.)
func (s *Store) DeleteReport(id string) (bool, error) {
	affected, err := s.q.DeleteReport(callCtx, id)
	if err != nil {
		return false, fmt.Errorf("delete report: %w", err)
	}
	return affected > 0, nil
}
