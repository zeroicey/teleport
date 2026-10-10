package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zeroicey/teleport/backend/internal/domain"
)

// Store is the data-access layer. All SQL in the application lives here.
type Store struct {
	db *sql.DB
}

// New wraps an open database handle.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the handle for health checks.
func (s *Store) DB() *sql.DB { return s.db }

// NowMS returns the current time in epoch milliseconds, matching the schema
// convention (all *_at columns are ms, like JavaScript's Date.now()).
func NowMS() int64 { return time.Now().UnixMilli() }

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

	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at, owner_key_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, input.Title, category, string(format), input.Content, string(metadataJSON), ts, ts, input.OwnerKeyID,
	); err != nil {
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
	if _, err := tx.Exec(
		`INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active, view_count)
		 VALUES (?, ?, ?, ?, 1, 0)`,
		token, id, ts, expiresAt,
	); err != nil {
		return nil, nil, fmt.Errorf("insert share token: %w", err)
	}
	share := &domain.ShareToken{
		Token: token, ReportID: id, CreatedAt: ts,
		ExpiresAt: expiresAt, IsActive: true, ViewCount: 0,
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return report, share, nil
}

// RevokeToken deactivates a token. It returns false only when the token does
// not exist at all; revoking an already-revoked token is a no-op success.
func (s *Store) RevokeToken(token string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE share_tokens SET is_active = 0 WHERE token = ? AND is_active = 1`, token)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	var exists string
	err = s.db.QueryRow(`SELECT token FROM share_tokens WHERE token = ?`, token).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CreateShareToken adds another share link to an existing report. It returns
// (nil, nil) when the report does not exist.
func (s *Store) CreateShareToken(reportID string, expiresInHours float64) (*domain.ShareToken, error) {
	var exists string
	err := s.db.QueryRow(`SELECT id FROM reports WHERE id = ?`, reportID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	token, err := NewShareToken()
	if err != nil {
		return nil, err
	}
	ts := NowMS()
	expiresAt := HoursFromNow(expiresInHours, 0)
	if _, err := s.db.Exec(
		`INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active, view_count)
		 VALUES (?, ?, ?, ?, 1, 0)`,
		token, reportID, ts, expiresAt,
	); err != nil {
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
func (s *Store) UpdateShareToken(token string, patch SharePatch) (*domain.ShareToken, error) {
	set := []string{}
	args := []any{}
	if patch.ExpiresInHours != nil {
		set = append(set, "expires_at = ?")
		args = append(args, HoursFromNow(*patch.ExpiresInHours, 0))
	} else if patch.ExpiresAt != nil {
		set = append(set, "expires_at = ?")
		args = append(args, *patch.ExpiresAt)
	}
	if patch.IsActive != nil {
		set = append(set, "is_active = ?")
		args = append(args, boolToInt(*patch.IsActive))
	}
	if len(set) == 0 {
		return s.GetShareToken(token)
	}

	args = append(args, token)
	res, err := s.db.Exec(`UPDATE share_tokens SET `+joinComma(set)+` WHERE token = ?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the token is absent, or the update was a no-op. Distinguish.
		return s.GetShareToken(token)
	}
	return s.GetShareToken(token)
}

// DeleteShareToken removes a token. Returns false when it did not exist.
func (s *Store) DeleteShareToken(token string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM share_tokens WHERE token = ?`, token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecordView increments the view counter for a token. Best-effort by design:
// a failure here must never fail the page render.
func (s *Store) RecordView(token string) error {
	_, err := s.db.Exec(
		`UPDATE share_tokens SET view_count = view_count + 1 WHERE token = ?`, token)
	return err
}

// CleanupExpired removes inactive, expired tokens. Returns the number deleted.
func (s *Store) CleanupExpired() (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM share_tokens WHERE is_active = 0 AND expires_at != 0 AND expires_at < ?`,
		NowMS())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

func (s *Store) reportByID(id string) (*domain.Report, error) {
	row := s.db.QueryRow(
		`SELECT id, title, category, format, content, metadata, created_at, updated_at, owner_key_id
		   FROM reports WHERE id = ?`, id)
	return scanReport(row)
}

// GetReport returns a single report including its content.
func (s *Store) GetReport(id string) (*domain.Report, error) {
	report, err := s.reportByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return report, err
}

// ListReports returns newest-first summaries (content omitted).
func (s *Store) ListReports(category string, limit, offset int) ([]domain.Report, error) {
	base := `SELECT id, title, category, format, '' AS content, metadata, created_at, updated_at, owner_key_id
	           FROM reports`
	var rows *sql.Rows
	var err error
	if category != "" {
		rows, err = s.db.Query(base+` WHERE category = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
			category, limit, offset)
	} else {
		rows, err = s.db.Query(base+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Report{}
	for rows.Next() {
		report, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *report)
	}
	return out, rows.Err()
}

// ListReportsByOwner returns newest-first summaries published by one key.
//
// This is the read side of the ownership model. Without it an agent can retrieve
// a report only if it still remembers the id it was handed at creation time —
// asking about anyone else's id returns 404, so a forgotten id is unrecoverable.
// Owning something you cannot enumerate is not ownership.
//
// An empty ownerKeyID is a real value, not a wildcard: it selects the reports
// published by the root credential, whose owner_key_id is ''. Callers wanting
// "everything" must use ListReports.
func (s *Store) ListReportsByOwner(ownerKeyID, category string, limit, offset int) ([]domain.Report, error) {
	base := `SELECT id, title, category, format, '' AS content, metadata, created_at, updated_at, owner_key_id
	           FROM reports WHERE owner_key_id = ?`
	args := []any{ownerKeyID}
	if category != "" {
		base += ` AND category = ?`
		args = append(args, category)
	}
	base += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(base, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Report{}
	for rows.Next() {
		report, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *report)
	}
	return out, rows.Err()
}

// GetShareToken returns a single token row.
func (s *Store) GetShareToken(token string) (*domain.ShareToken, error) {
	row := s.db.QueryRow(
		`SELECT token, report_id, created_at, expires_at, is_active, view_count
		   FROM share_tokens WHERE token = ?`, token)
	t, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// ListShareTokens returns all tokens belonging to a report, newest first.
func (s *Store) ListShareTokens(reportID string) ([]domain.ShareToken, error) {
	rows, err := s.db.Query(
		`SELECT token, report_id, created_at, expires_at, is_active, view_count
		   FROM share_tokens WHERE report_id = ? ORDER BY created_at DESC`, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.ShareToken{}
	for rows.Next() {
		t, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ResolveShare looks up a share token and its report, classifying the result as
// ok / missing / gone.
//
// Liveness is computed at read time (expires_at = 0 means "never expires"), so
// no background job is needed to flip a flag when a link expires.
func (s *Store) ResolveShare(token string) (*domain.Resolution, error) {
	row := s.db.QueryRow(
		`SELECT t.token, t.report_id, t.created_at, t.expires_at, t.is_active, t.view_count,
		        r.id, r.title, r.category, r.format, r.content, r.metadata, r.created_at, r.updated_at
		   FROM share_tokens t
		   JOIN reports r ON r.id = t.report_id
		  WHERE t.token = ?`, token)

	var (
		token2                         string
		reportID                       string
		tokenCreated, expiresAt, views int64
		isActive                       int
		rid, title, category, format   string
		content, metadataJSON          string
		rCreated, rUpdated             int64
	)
	err := row.Scan(&token2, &reportID, &tokenCreated, &expiresAt, &isActive, &views,
		&rid, &title, &category, &format, &content, &metadataJSON, &rCreated, &rUpdated)
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.Resolution{Status: domain.StatusMissing}, nil
	}
	if err != nil {
		return nil, err
	}

	share := &domain.ShareToken{
		Token: token2, ReportID: reportID, CreatedAt: tokenCreated,
		ExpiresAt: expiresAt, IsActive: isActive == 1, ViewCount: views,
	}
	report := &domain.Report{
		ID: rid, Title: title, Category: category, Format: domain.ReportFormat(format),
		Content: content, Metadata: decodeMetadata(metadataJSON),
		CreatedAt: rCreated, UpdatedAt: rUpdated,
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
// scanning helpers
// ---------------------------------------------------------------------------

// scannable is satisfied by both *sql.Row and *sql.Rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanReport(row scannable) (*domain.Report, error) {
	var (
		r            domain.Report
		format       string
		metadataJSON string
	)
	if err := row.Scan(&r.ID, &r.Title, &r.Category, &format, &r.Content, &metadataJSON,
		&r.CreatedAt, &r.UpdatedAt, &r.OwnerKeyID); err != nil {
		return nil, err
	}
	r.Format = domain.ReportFormat(format)
	r.Metadata = decodeMetadata(metadataJSON)
	return &r, nil
}

func scanShare(row scannable) (*domain.ShareToken, error) {
	var (
		t        domain.ShareToken
		isActive int
	)
	if err := row.Scan(&t.Token, &t.ReportID, &t.CreatedAt, &t.ExpiresAt, &isActive, &t.ViewCount); err != nil {
		return nil, err
	}
	t.IsActive = isActive == 1
	return &t, nil
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

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
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
// updated_at is `MAX(now, updated_at + 1)` rather than a plain `now`, and both
// halves of that matter:
//
//   - `MAX(..., updated_at + 1)` keeps it strictly increasing. Without the +1,
//     an update landing in the same millisecond as the previous write would set
//     updated_at to the value already stored, which is exactly the condition
//     trg_reports_touch_updated_at watches for — the trigger would then fire and
//     rewrite the column from strftime('%s','now') * 1000, i.e. truncated to
//     whole SECONDS. The result could land up to 999ms *before* created_at, so
//     the share page would show "Updated" earlier than "Created". Because the
//     +1 guarantees the column always changes, the trigger never fires on this
//     path and its second-granularity stamp cannot leak into the data.
//   - `MAX(now, ...)` means a backwards system clock cannot move the column
//     backwards either.
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

	res, err := s.db.Exec(
		`UPDATE reports
		    SET title      = COALESCE(?, title),
		        category   = COALESCE(?, category),
		        format     = COALESCE(?, format),
		        content    = COALESCE(?, content),
		        metadata   = COALESCE(?, metadata),
		        updated_at = MAX(?, updated_at + 1)
		  WHERE id = ?`,
		patch.Title, patch.Category, format, patch.Content, metadataJSON, nowMS, id)
	if err != nil {
		return nil, false, fmt.Errorf("update report: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("update report rows: %w", err)
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
// ON DELETE CASCADE and the connection sets foreign_keys(1), so this single
// statement is the whole operation. Without that pragma the cascade would be
// dead code and orphaned tokens would linger — harmless to readers (the join
// would find no report) but permanently unreachable rows, and a
// `DELETE FROM reports` that silently leaves debris.
func (s *Store) DeleteReport(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM reports WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete report: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete report rows: %w", err)
	}
	return affected > 0, nil
}
