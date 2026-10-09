// Package store owns the SQLite connection, schema migration and all SQL.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	// Register the pure-Go SQLite driver under the name "sqlite".
	//
	// This is a blank import because nothing references the package directly;
	// database/sql discovers drivers through their init() side effect. Omitting
	// it compiles and vets cleanly and then fails at runtime with
	// `sql: unknown driver "sqlite"`, so it must not be removed.
	_ "modernc.org/sqlite"
)

// migrationFS embeds the SQL migrations so the deployed binary is a single
// self-contained file — the server has no sqlite3 CLI and we do not want a
// separate artifact to keep in sync.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Open opens (creating if necessary) the SQLite database at path and applies
// any pending migrations.
//
// The pragmas are set on the DSN so they apply to every pooled connection:
//   - busy_timeout: wait rather than fail instantly when another write is in
//     flight (SQLite allows one writer; the dashboard and agent API share it).
//   - journal_mode=WAL: readers never block the writer, which matters because
//     share-page views increment a counter on every read.
//   - foreign_keys=ON: SQLite defaults this off, but the schema relies on
//     ON DELETE CASCADE.
//   - synchronous=NORMAL: the durable-enough default for WAL; a full fsync per
//     commit would dominate write latency on a small VPS.
func Open(path string) (*sql.DB, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		path = "file:" + path
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite writes are serialised anyway; a small pool keeps contention
	// explicit and avoids "database is locked" storms under load.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies every embedded migration that has not been applied yet.
//
// Migrations are tracked in a `schema_migrations` table. Each file must be
// idempotent-safe to re-run in the sense that it uses IF NOT EXISTS where the
// original D1 migration did; we still record applied versions so a future
// destructive migration is never replayed.
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		// The whole file runs in one transaction: SQLite DDL is transactional,
		// so a failure part-way leaves the schema untouched and the version
		// unrecorded (safe to retry).
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, unixepoch('subsec') * 1000)`,
			version,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
		slog.Info("migration applied", "version", version)
	}
	return nil
}
