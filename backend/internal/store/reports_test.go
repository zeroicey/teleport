package store

import (
	"testing"

	"github.com/zeroicey/teleport/backend/internal/domain"
)

func seedReport(t *testing.T, s *Store, title string) *domain.Report {
	t.Helper()
	report, _, err := s.CreateReport(domain.CreateReportInput{
		Title:    title,
		Category: "general",
		Format:   domain.FormatMarkdown,
		Content:  "# " + title,
		Metadata: map[string]any{"k": "v"},
	}, 168)
	if err != nil {
		t.Fatalf("seed report: %v", err)
	}
	return report
}

// TestUpdateReportKeepsUpdatedAtStrictlyIncreasing pins the invariant the share
// page depends on: it renders both Created and Updated, so Updated must never be
// equal to Created (nothing to show) let alone earlier than it (a page that
// claims the report was edited before it existed).
//
// The same-millisecond case is the one that actually breaks. reports.updated_at
// is also maintained by trg_reports_touch_updated_at, which fires when an UPDATE
// leaves the column unchanged and stamps it from strftime('%s','now') * 1000 —
// truncated to whole seconds. A store that wrote a plain `now` would therefore
// hand the column to the trigger whenever two writes shared a millisecond, and
// the trigger would write a value up to 999ms in the past. This test fails
// against that implementation; it was found by a falsification run, not by a
// code review.
func TestUpdateReportKeepsUpdatedAtStrictlyIncreasing(t *testing.T) {
	s := newTestStore(t)
	report := seedReport(t, s, "clock")

	if report.UpdatedAt != report.CreatedAt {
		t.Fatalf("a fresh report has updated_at %d != created_at %d", report.UpdatedAt, report.CreatedAt)
	}

	// No sleeps anywhere in this test on purpose: the point is the tight loop.
	last := report.UpdatedAt
	content := "# v"
	for i := 0; i < 5; i++ {
		body := content
		patch := domain.ReportPatch{Content: &body}
		got, found, err := s.UpdateReport(report.ID, patch, report.CreatedAt)
		if err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		if !found {
			t.Fatalf("update %d: report not found", i)
		}
		if got.UpdatedAt <= last {
			t.Fatalf("update %d: updated_at = %d did not advance past %d", i, got.UpdatedAt, last)
		}
		if got.UpdatedAt < got.CreatedAt {
			t.Fatalf("update %d: updated_at = %d is before created_at = %d", i, got.UpdatedAt, got.CreatedAt)
		}
		last = got.UpdatedAt
	}
}

// TestUpdateReportLeavesAbsentFieldsAlone is the COALESCE contract: PATCH means
// patch, so sending one field must not blank the others.
func TestUpdateReportLeavesAbsentFieldsAlone(t *testing.T) {
	s := newTestStore(t)
	report := seedReport(t, s, "original")

	title := "renamed"
	got, found, err := s.UpdateReport(report.ID, domain.ReportPatch{Title: &title}, report.CreatedAt+1)
	if err != nil || !found {
		t.Fatalf("update: found=%v err=%v", found, err)
	}
	if got.Title != "renamed" {
		t.Errorf("title = %q, want renamed", got.Title)
	}
	if got.Content != report.Content {
		t.Errorf("content changed from %q to %q; an absent field must be left alone", report.Content, got.Content)
	}
	if got.Category != report.Category || got.Format != report.Format {
		t.Errorf("category/format changed: %q/%q", got.Category, got.Format)
	}
	if got.Metadata["k"] != "v" {
		t.Errorf("metadata changed to %v; an absent metadata field must not clear it", got.Metadata)
	}
	if got.ID != report.ID || got.CreatedAt != report.CreatedAt {
		t.Errorf("identity changed: id=%q created_at=%d", got.ID, got.CreatedAt)
	}
}

// TestUpdateReportReplacesMetadataAndCanClearIt: {} is a meaningful value, and
// it must be distinguishable from "field absent".
func TestUpdateReportReplacesMetadataAndCanClearIt(t *testing.T) {
	s := newTestStore(t)
	report := seedReport(t, s, "meta")

	got, _, err := s.UpdateReport(report.ID, domain.ReportPatch{Metadata: map[string]any{"only": "this"}}, report.CreatedAt+1)
	if err != nil {
		t.Fatalf("replace metadata: %v", err)
	}
	if _, stale := got.Metadata["k"]; stale {
		t.Errorf("metadata was merged rather than replaced: %v", got.Metadata)
	}

	got, _, err = s.UpdateReport(report.ID, domain.ReportPatch{Metadata: map[string]any{}}, report.CreatedAt+2)
	if err != nil {
		t.Fatalf("clear metadata: %v", err)
	}
	if len(got.Metadata) != 0 {
		t.Errorf("metadata = %v, want empty after an explicit {}", got.Metadata)
	}
	if got.Title != report.Title {
		t.Errorf("clearing metadata also changed the title to %q", got.Title)
	}
}

// TestUpdateReportReportsMissingRow: callers turn found=false into a 404, and it
// is also the answer when the row is deleted between an ownership check and the
// write, so it must not be conflated with an error.
func TestUpdateReportReportsMissingRow(t *testing.T) {
	s := newTestStore(t)
	title := "ghost"
	if _, found, err := s.UpdateReport("no-such-id", domain.ReportPatch{Title: &title}, 1); err != nil || found {
		t.Errorf("updating a missing report: found=%v err=%v, want false/nil", found, err)
	}
}

// TestDeleteReportRemovesItsShareTokens is the cascade guard at the store layer.
//
// SQLite enforces ON DELETE CASCADE only when foreign_keys is on, and it is off
// by default — so the pragma in Open() is the single thing standing between a
// clean delete and permanently orphaned token rows. Checking the tokens
// directly, rather than inferring from a lookup that joins through reports, is
// what makes an orphan visible.
func TestDeleteReportRemovesItsShareTokens(t *testing.T) {
	s := newTestStore(t)
	report := seedReport(t, s, "cascade")

	tokens, err := s.ListShareTokens(report.ID)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) == 0 {
		t.Fatal("expected the auto-created share token")
	}

	found, err := s.DeleteReport(report.ID)
	if err != nil || !found {
		t.Fatalf("delete: found=%v err=%v", found, err)
	}

	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM share_tokens WHERE report_id = ?`, report.ID).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("orphaned share tokens = %d, want 0: ON DELETE CASCADE did not fire, "+
			"so foreign_keys(1) is missing from the DSN", orphans)
	}

	if again, err := s.DeleteReport(report.ID); err != nil || again {
		t.Errorf("second delete: found=%v err=%v, want false/nil", again, err)
	}
}
