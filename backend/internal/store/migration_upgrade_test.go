package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// openLegacyDatabase builds a database that looks like a deployed one from
// before 0002: only 0001 applied, with real rows already in it.
//
// This is built by hand rather than by calling Open, because Open would apply
// every migration at once and there would be nothing left to upgrade — the
// scenario under test is precisely "a database that already has data in the old
// shape".
func openLegacyDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")

	body, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read 0001: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(string(body)); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}
	if _, err := db.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`,
	); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_migrations (version, applied_at) VALUES ('0001_init', 1)`,
	); err != nil {
		t.Fatalf("record 0001: %v", err)
	}

	// Real pre-existing data, including a share token that must survive.
	if _, err := db.Exec(
		`INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		 VALUES ('legacy-report', 'Legacy title', 'architecture', 'markdown', '# hi', '{}', 1000, 1000)`,
	); err != nil {
		t.Fatalf("insert legacy report: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active, view_count)
		 VALUES ('legacy-token', 'legacy-report', 1000, 0, 1, 7)`,
	); err != nil {
		t.Fatalf("insert legacy share: %v", err)
	}
	return db
}

// Upgrading a populated deployment must be purely additive: existing reports
// survive with no owner, and their share links keep working. Adding a NOT NULL
// column without a default is the classic way this goes wrong — it fails on a
// table that already has rows.
func TestMigrationUpgradesPopulatedDatabase(t *testing.T) {
	db := openLegacyDatabase(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on a populated database: %v", err)
	}

	var title, owner string
	if err := db.QueryRow(
		`SELECT title, owner_key_id FROM reports WHERE id = 'legacy-report'`,
	).Scan(&title, &owner); err != nil {
		t.Fatalf("legacy report did not survive the upgrade: %v", err)
	}
	if title != "Legacy title" {
		t.Errorf("title changed: %q", title)
	}
	// '' means "root-owned or predates keys", which is what makes a pre-existing
	// report readable by root and by the dashboard, and nobody else.
	if owner != "" {
		t.Errorf("legacy report owner = %q, want empty", owner)
	}

	var views int64
	var active bool
	if err := db.QueryRow(
		`SELECT view_count, is_active FROM share_tokens WHERE token = 'legacy-token'`,
	).Scan(&views, &active); err != nil {
		t.Fatalf("legacy share token did not survive: %v", err)
	}
	if views != 7 || !active {
		t.Errorf("legacy share changed: views=%d active=%v", views, active)
	}

	// The new tables must exist and be usable immediately after the upgrade.
	for _, table := range []string{"agent_keys", "key_applications", "key_renewals"} {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil {
			t.Errorf("table %s is missing after the upgrade: %v", table, err)
		}
	}

	// And the whole thing must be replayable without redoing work.
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("recorded %d migrations after a replay, want 2", count)
	}
}
