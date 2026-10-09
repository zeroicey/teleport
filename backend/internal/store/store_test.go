package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/domain"
)

// newTestStore opens a fresh, migrated database in a temp directory.
//
// A file (rather than :memory:) is used deliberately: WAL mode, the pragmas and
// the migration runner all behave differently on an in-memory database, and the
// whole point is to test what production runs.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func floatPtr(f float64) *float64 { return &f }

func intPtr(i int) *int { return &i }

func boolPtr(b bool) *bool { return &b }

// TestMigrateIsIdempotent verifies that opening the same database twice does not
// re-run the migration or fail on existing objects.
func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (migrations must be idempotent): %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 recorded migration, got %d", count)
	}
}

// TestCreateReportCreatesShareToken checks the report+token transaction and the
// expiry arithmetic.
func TestCreateReportCreatesShareToken(t *testing.T) {
	s := newTestStore(t)

	t.Run("explicit hours", func(t *testing.T) {
		report, share, err := s.CreateReport(domain.CreateReportInput{
			Title: "t", Content: "c", AutoShareHours: floatPtr(1),
		}, 168)
		if err != nil {
			t.Fatalf("CreateReport: %v", err)
		}
		if report.ID == "" || report.Category != "general" {
			t.Errorf("unexpected report: %+v", report)
		}
		if share == nil {
			t.Fatal("expected a share token")
		}
		if share.ExpiresAt == 0 {
			t.Error("expires_at should be set for a 1-hour link")
		}
		// One hour from now, within a generous tolerance.
		delta := share.ExpiresAt - NowMS()
		if delta < 3_500_000 || delta > 3_650_000 {
			t.Errorf("expiry not ~1h away: %d ms", delta)
		}
	})

	t.Run("zero means never expires", func(t *testing.T) {
		_, share, err := s.CreateReport(domain.CreateReportInput{
			Title: "t", Content: "c", AutoShareHours: floatPtr(0),
		}, 168)
		if err != nil {
			t.Fatalf("CreateReport: %v", err)
		}
		if share == nil {
			t.Fatal("expected a share token")
		}
		if share.ExpiresAt != 0 {
			t.Errorf("expires_at should be 0 (never), got %d", share.ExpiresAt)
		}
	})

	t.Run("absent field uses the configured default", func(t *testing.T) {
		_, share, err := s.CreateReport(domain.CreateReportInput{
			Title: "t", Content: "c",
		}, 48)
		if err != nil {
			t.Fatalf("CreateReport: %v", err)
		}
		if share == nil {
			t.Fatal("expected a share token")
		}
		delta := share.ExpiresAt - NowMS()
		if delta < 47*3_600_000 || delta > 48*3_600_000 {
			t.Errorf("default duration not applied: %d ms", delta)
		}
	})
}

// TestResolveShareStatuses covers the ok/missing/gone classification, including
// that a revoked link is indistinguishable from an unknown one.
func TestResolveShareStatuses(t *testing.T) {
	s := newTestStore(t)

	_, share, err := s.CreateReport(domain.CreateReportInput{
		Title: "resolved", Content: "# hi",
	}, 1)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	t.Run("ok", func(t *testing.T) {
		res, err := s.ResolveShare(share.Token)
		if err != nil {
			t.Fatalf("ResolveShare: %v", err)
		}
		if res.Status != domain.StatusOK {
			t.Fatalf("expected OK, got %v", res.Status)
		}
		if res.Report.Title != "resolved" {
			t.Errorf("wrong report: %+v", res.Report)
		}
	})

	t.Run("unknown token is missing", func(t *testing.T) {
		res, err := s.ResolveShare("aaaaaaaaaaaaaaaaaaaaaa")
		if err != nil {
			t.Fatalf("ResolveShare: %v", err)
		}
		if res.Status != domain.StatusMissing {
			t.Errorf("expected Missing, got %v", res.Status)
		}
	})

	t.Run("revoked token is missing", func(t *testing.T) {
		if _, err := s.RevokeToken(share.Token); err != nil {
			t.Fatalf("RevokeToken: %v", err)
		}
		res, err := s.ResolveShare(share.Token)
		if err != nil {
			t.Fatalf("ResolveShare: %v", err)
		}
		if res.Status != domain.StatusMissing {
			t.Errorf("expected Missing for a revoked token, got %v", res.Status)
		}
	})

	t.Run("expired token is gone", func(t *testing.T) {
		_, expired, err := s.CreateReport(domain.CreateReportInput{
			Title: "expired", Content: "c",
		}, 1)
		if err != nil {
			t.Fatalf("CreateReport: %v", err)
		}
		// Push the expiry into the past.
		if _, err := s.UpdateShareToken(expired.Token, SharePatch{ExpiresAt: int64Ptr(NowMS() - 1000)}); err != nil {
			t.Fatalf("UpdateShareToken: %v", err)
		}
		res, err := s.ResolveShare(expired.Token)
		if err != nil {
			t.Fatalf("ResolveShare: %v", err)
		}
		if res.Status != domain.StatusGone {
			t.Errorf("expected Gone for an expired token, got %v", res.Status)
		}
	})
}

func int64Ptr(i int64) *int64 { return &i }

// TestRevokeIsIdempotent covers the three-state revoke contract.
func TestRevokeIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	_, share, err := s.CreateReport(domain.CreateReportInput{Title: "t", Content: "c"}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	first, err := s.RevokeToken(share.Token)
	if err != nil || !first {
		t.Fatalf("first revoke: found=%v err=%v", first, err)
	}
	second, err := s.RevokeToken(share.Token)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if !second {
		t.Error("revoking an already-revoked token should still report success")
	}
	unknown, err := s.RevokeToken("bbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatalf("unknown revoke: %v", err)
	}
	if unknown {
		t.Error("revoking an unknown token should report not-found")
	}
}

// TestUpdateShareToken covers expiry and active-flag patching.
func TestUpdateShareToken(t *testing.T) {
	s := newTestStore(t)
	_, share, err := s.CreateReport(domain.CreateReportInput{Title: "t", Content: "c"}, 1)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	t.Run("expiresInHours", func(t *testing.T) {
		updated, err := s.UpdateShareToken(share.Token, SharePatch{ExpiresInHours: floatPtr(24)})
		if err != nil {
			t.Fatalf("UpdateShareToken: %v", err)
		}
		delta := updated.ExpiresAt - NowMS()
		if delta < 23*3_600_000 || delta > 24*3_600_000 {
			t.Errorf("24h expiry not applied: %d ms", delta)
		}
	})

	t.Run("expiresInHours zero means never", func(t *testing.T) {
		updated, err := s.UpdateShareToken(share.Token, SharePatch{ExpiresInHours: floatPtr(0)})
		if err != nil {
			t.Fatalf("UpdateShareToken: %v", err)
		}
		if updated.ExpiresAt != 0 {
			t.Errorf("expected 0 (never), got %d", updated.ExpiresAt)
		}
	})

	t.Run("isActive toggle", func(t *testing.T) {
		updated, err := s.UpdateShareToken(share.Token, SharePatch{IsActive: boolPtr(false)})
		if err != nil {
			t.Fatalf("UpdateShareToken: %v", err)
		}
		if updated.IsActive {
			t.Error("expected is_active=false")
		}
	})

	t.Run("unknown token returns nil", func(t *testing.T) {
		updated, err := s.UpdateShareToken("cccccccccccccccccccccc", SharePatch{IsActive: boolPtr(true)})
		if err != nil {
			t.Fatalf("UpdateShareToken: %v", err)
		}
		if updated != nil {
			t.Errorf("expected nil for an unknown token, got %+v", updated)
		}
	})
}

// TestRecordViewAndDelete covers the analytics counter and hard delete.
func TestRecordViewAndDelete(t *testing.T) {
	s := newTestStore(t)
	_, share, err := s.CreateReport(domain.CreateReportInput{Title: "t", Content: "c"}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.RecordView(share.Token); err != nil {
			t.Fatalf("RecordView: %v", err)
		}
	}
	got, err := s.GetShareToken(share.Token)
	if err != nil {
		t.Fatalf("GetShareToken: %v", err)
	}
	if got.ViewCount != 3 {
		t.Errorf("expected 3 views, got %d", got.ViewCount)
	}

	found, err := s.DeleteShareToken(share.Token)
	if err != nil || !found {
		t.Fatalf("DeleteShareToken: found=%v err=%v", found, err)
	}
	again, err := s.DeleteShareToken(share.Token)
	if err != nil || again {
		t.Errorf("second delete should report not-found: found=%v err=%v", again, err)
	}
}

// TestListReportsAndTokens checks ordering and the content projection.
func TestListReportsAndTokens(t *testing.T) {
	s := newTestStore(t)

	first, _, err := s.CreateReport(domain.CreateReportInput{
		Title: "older", Content: "body-one", Category: "pentest",
	}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	// Guarantee a distinct created_at so the DESC ordering is deterministic.
	waitForNextMillisecond()
	second, _, err := s.CreateReport(domain.CreateReportInput{
		Title: "newer", Content: "body-two", Category: "architecture",
	}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	all, err := s.ListReports("", 50, 0)
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(all))
	}
	if all[0].ID != second.ID || all[1].ID != first.ID {
		t.Errorf("expected newest-first ordering, got %s then %s", all[0].Title, all[1].Title)
	}
	// The list projection must not ship content blobs.
	for _, r := range all {
		if r.Content != "" {
			t.Errorf("list projection leaked content for %s", r.ID)
		}
	}

	filtered, err := s.ListReports("pentest", 50, 0)
	if err != nil {
		t.Fatalf("ListReports filter: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != first.ID {
		t.Errorf("category filter wrong: %+v", filtered)
	}

	tokens, err := s.ListShareTokens(second.ID)
	if err != nil {
		t.Fatalf("ListShareTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("expected 1 token, got %d", len(tokens))
	}
}

// TestMetadataRoundTrip covers JSON persistence and the decode-time fallback.
//
// The schema's `json_valid(metadata)` CHECK makes corrupt data unreachable
// through SQL, so the fallback is exercised directly against the decoder — that
// is the code path that would protect a future schema change or a hand-edited
// database file.
func TestMetadataRoundTrip(t *testing.T) {
	s := newTestStore(t)

	report, _, err := s.CreateReport(domain.CreateReportInput{
		Title: "t", Content: "c",
		Metadata: map[string]any{"severity": "high", "score": 9.1, "tags": []any{"a", "b"}},
	}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	got, err := s.GetReport(report.ID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if got.Metadata["severity"] != "high" {
		t.Errorf("metadata not persisted: %+v", got.Metadata)
	}
	if tags, ok := got.Metadata["tags"].([]any); !ok || len(tags) != 2 {
		t.Errorf("nested metadata not persisted: %+v", got.Metadata["tags"])
	}

	t.Run("decodeMetadata degrades on corrupt input", func(t *testing.T) {
		for _, raw := range []string{"", "not json", "[]", "null", "{"} {
			out := decodeMetadata(raw)
			if out == nil {
				t.Errorf("decodeMetadata(%q) returned nil; want a usable empty map", raw)
			}
		}
		if len(decodeMetadata(`{"a":1}`)) != 1 {
			t.Error("decodeMetadata failed on valid input")
		}
	})
}

// TestMetadataMustBeValidJSON verifies the schema's json_valid CHECK is active,
// which is what guarantees the decode path never sees garbage in practice.
func TestMetadataMustBeValidJSON(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(
		`INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		 VALUES ('x', 't', 'c', 'markdown', 'body', 'notjson', 1, 1)`)
	if err == nil {
		t.Fatal("expected the json_valid CHECK constraint to reject invalid JSON")
	}
}

// TestUpdatedAtTrigger verifies the schema trigger actually fires.
func TestUpdatedAtTrigger(t *testing.T) {
	s := newTestStore(t)
	report, _, err := s.CreateReport(domain.CreateReportInput{Title: "t", Content: "c"}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	// Force a stale updated_at, then touch the row.
	if _, err := s.db.Exec(
		`UPDATE reports SET updated_at = 1000 WHERE id = ?`, report.ID); err != nil {
		t.Fatalf("reset updated_at: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE reports SET title = 'changed' WHERE id = ?`, report.ID); err != nil {
		t.Fatalf("update title: %v", err)
	}

	after, err := s.GetReport(report.ID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if after.UpdatedAt <= 1000 {
		t.Errorf("trigger did not refresh updated_at: %d", after.UpdatedAt)
	}
}

// TestCleanupExpired removes only tokens that are BOTH revoked and expired.
//
// A revoked token whose expires_at is 0 (never expires) is deliberately
// retained, because expires_at=0 carries no information that would justify
// deleting the row.
func TestCleanupExpired(t *testing.T) {
	s := newTestStore(t)

	_, active, err := s.CreateReport(domain.CreateReportInput{Title: "a", Content: "c"}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	_, revocable, err := s.CreateReport(domain.CreateReportInput{Title: "b", Content: "c"}, 1)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	if _, err := s.RevokeToken(revocable.Token); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	// Backdate the expiry so the row qualifies for cleanup.
	if _, err := s.UpdateShareToken(revocable.Token, SharePatch{
		ExpiresAt: int64Ptr(NowMS() - 60_000),
	}); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	deleted, err := s.CleanupExpired()
	if err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted token, got %d", deleted)
	}
	// The active, never-expiring token must survive.
	if still, err := s.GetShareToken(active.Token); err != nil || still == nil {
		t.Errorf("active token was removed: %+v err=%v", still, err)
	}
	if gone, err := s.GetShareToken(revocable.Token); err != nil || gone != nil {
		t.Errorf("expired+revoked token survived: %+v err=%v", gone, err)
	}
}

// TestForeignKeyCascade confirms the pragma is on and the schema's cascade works.
func TestForeignKeyCascade(t *testing.T) {
	s := newTestStore(t)
	report, _, err := s.CreateReport(domain.CreateReportInput{Title: "t", Content: "c"}, 0)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM reports WHERE id = ?`, report.ID); err != nil {
		t.Fatalf("delete report: %v", err)
	}
	tokens, err := s.ListShareTokens(report.ID)
	if err != nil {
		t.Fatalf("ListShareTokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("ON DELETE CASCADE did not remove tokens: %+v", tokens)
	}
}

// TestConcurrentWrites exercises the WAL + busy_timeout configuration.
func TestConcurrentWrites(t *testing.T) {
	s := newTestStore(t)
	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _, err := s.CreateReport(domain.CreateReportInput{Title: "c", Content: "c"}, 0)
			errCh <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent write failed: %v", err)
		}
	}
	reports, err := s.ListReports("", 100, 0)
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(reports) != n {
		t.Errorf("expected %d reports, got %d", n, len(reports))
	}
}

func waitForNextMillisecond() {
	start := NowMS()
	for NowMS() == start {
	}
}

var _ = sql.ErrNoRows
var _ = intPtr
