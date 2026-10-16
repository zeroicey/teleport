package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Advisory lock keys for the count-then-write decisions that SQLite used to make
// safe for free.
//
// SQLite allows exactly one writer, and `_txlock=immediate` takes that writer
// lock when a transaction begins — so "count the rows, then insert if under the
// limit" was atomic without anyone writing it down. PostgreSQL grants no such
// thing: under its default READ COMMITTED, two concurrent transactions can both
// read COUNT(*) = 0 and both insert. `SELECT COUNT(*) ... FOR UPDATE` does not
// fix it either, because the rows that would violate the cap do not exist yet
// and there is nothing to lock — the anomaly is a phantom.
//
// The three sites below therefore take a transaction-scoped advisory lock
// (pg_advisory_xact_lock) before reading the count. The lock is released on
// commit or rollback, so it cannot be leaked by a forgotten unlock, and it
// serialises exactly the decision it guards rather than all writes.
//
// The keys are namespaced with "tele" as ASCII in the high 32 bits, so that a
// lock of ours is recognisable in pg_locks. Advisory locks are scoped to one
// database, and this database belongs to teleport alone, so the low bits only
// have to be distinct from each other.
const (
	advisoryLockNamespace int64 = 0x74656c65 // "tele"

	// LockPendingApplications guards the KEY_MAX_PENDING cap in
	// CreateApplication: no more than N applications may be outstanding.
	LockPendingApplications int64 = advisoryLockNamespace<<32 | 1
	// LockActiveKeys guards the KEY_MAX_ACTIVE cap. Both paths that mint a
	// credential — ClaimApplication and CreateManualKey — take it, so they
	// cannot each spend the same last slot.
	LockActiveKeys int64 = advisoryLockNamespace<<32 | 2
	// LockPendingRenewals guards "at most one pending renewal per key" in
	// CreateRenewal, which SQLite enforced with the same free serialisation.
	LockPendingRenewals int64 = advisoryLockNamespace<<32 | 3
)

// pgMigrationFS embeds the PostgreSQL migrations, so the deployed binary carries
// its own schema and cannot run against a directory that has drifted from it.
//
// The directory is named `migrations_pg` rather than `migrations` only while the
// SQLite implementation still exists: the two engines need different DDL, and
// both must be readable during the transition. When the SQLite side is deleted
// (rewrite stage P2), this becomes `migrations/`.
//
//go:embed migrations_pg/*.sql
var pgMigrationFS embed.FS

// OpenPG connects to PostgreSQL and applies any pending migration.
func OpenPG(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := MigratePG(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// MigratePG applies every embedded PostgreSQL migration not yet recorded, in
// filename order, each inside its own transaction.
//
// PostgreSQL DDL is transactional, so a migration that fails part-way leaves the
// database exactly as it was — which is why the version row is inserted in the
// same transaction as the DDL it describes. A crash between the two is therefore
// impossible, and "recorded" always means "applied".
//
// The migrations are the schema's single source of truth: `schema.pg.sql` at the
// repository root is a generated snapshot, and TestPGSnapshotMatchesMigrations
// fails if the two disagree.
func MigratePG(ctx context.Context, pool *pgxpool.Pool) error {
	// The bookkeeping table is created here rather than by a migration, so that
	// a fresh database needs no bootstrap step. It is deliberately absent from
	// the snapshot: the snapshot describes the application's schema, and this
	// table describes the migrator.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT   PRIMARY KEY,
			applied_at BIGINT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := pgMigrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		var applied bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		if applied {
			continue
		}

		body, err := pgMigrationFS.ReadFile("migrations_pg/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`,
			name, time.Now().UnixMilli(),
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// pgMigrationNames lists the embedded migrations in application order.
func pgMigrationNames() ([]string, error) {
	entries, err := pgMigrationFS.ReadDir("migrations_pg")
	if err != nil {
		return nil, fmt.Errorf("read migrations_pg dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 5 || name[len(name)-4:] != ".sql" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// pgMigrationBody returns the concatenated migration text, which is exactly what
// `schema.pg.sql` must contain.
func pgMigrationBody() (string, error) {
	names, err := pgMigrationNames()
	if err != nil {
		return "", err
	}
	var out []byte
	for _, name := range names {
		body, err := pgMigrationFS.ReadFile("migrations_pg/" + name)
		if err != nil {
			return "", err
		}
		out = append(out, body...)
	}
	return string(out), nil
}
