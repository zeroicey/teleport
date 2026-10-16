package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
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

// lockCap takes the transaction-scoped advisory lock that serialises one
// count-then-write decision.
//
// It must be called in the same transaction as, and before, the count it guards:
// the lock is released on commit or rollback, so the count and the write that
// depends on it cannot be separated. Taking more than one advisory lock in one
// transaction is deliberately avoided, so lock ordering can never deadlock.
func lockCap(ctx context.Context, tx pgx.Tx, key int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return fmt.Errorf("take advisory lock %d: %w", key, err)
	}
	return nil
}

// pgMigrationFS embeds the PostgreSQL migrations, so the deployed binary carries
// its own schema and cannot run against a directory that has drifted from it.
//
// The `_pg` suffix is now historical — the SQLite side was deleted in P2, so
// this is the only migrations directory — and is kept deliberately: the
// decision record and the readable snapshot (`schema.pg.sql` at the repository
// root) both use the same name, and renaming the directory would make the
// records disagree with the code for no behavioural gain.
//
// The same directory is `schema:` in backend/sqlc.yaml, so the generated queries
// and the server read one description of the schema.
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

	// Two server-side timeouts, because the store deliberately runs every call on
	// context.Background() (see store.callCtx) and therefore carries no deadline:
	//
	//   - lock_timeout bounds a wait for a lock. Four write paths take a
	//     transaction-scoped advisory lock to make a count-then-insert atomic, and
	//     pg_advisory_xact_lock waits indefinitely by default. Without this, one
	//     wedged holder would block every credential issuance forever, with no
	//     error and no log line — the availability cliff of the mechanism that
	//     replaced SQLite's single writer.
	//   - idle_in_transaction_session_timeout kills a connection that is holding a
	//     transaction open and doing nothing. That is how a leaked transaction
	//     becomes a wedged lock holder, and it is also how one abandoned client can
	//     pin a connection on an instance shared with other services.
	//
	// They are generous relative to this workload (the critical sections are single
	// statements) and they convert "hangs forever" into "fails, visibly".
	// statement_timeout is deliberately NOT set: a large report insert is
	// legitimate work, and a lock wait is the only thing here known to be able to
	// block without progress.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["lock_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = "10s"
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60s"
	}

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
// Each applied migration's **content hash** is recorded alongside its name, and
// a database whose recorded hash no longer matches the embedded file is refused
// rather than served. That check exists because its absence produced a false
// claim of verification: the baseline is a single merged file, so a database
// created from one revision of it silently keeps that revision's DDL forever
// (the version is recorded; the file is never replayed), and the file-to-file
// snapshot guard cannot see a live database at all. See
// .ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md.
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
			applied_at BIGINT NOT NULL,
			checksum   TEXT   NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	// Databases created before checksums existed get the column with the empty
	// default, which is treated below as "cannot be verified" rather than as
	// "matches".
	if _, err := pool.Exec(ctx,
		`ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT NOT NULL DEFAULT ''`,
	); err != nil {
		return fmt.Errorf("add schema_migrations.checksum: %w", err)
	}

	names, err := pgMigrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		body, err := pgMigrationFS.ReadFile("migrations_pg/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		want := migrationChecksum(body)

		var recorded string
		err = pool.QueryRow(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`, name).Scan(&recorded)
		switch {
		case err == nil:
			if recorded == "" {
				return fmt.Errorf("migration %s was applied by a migrator that did not record a "+
					"checksum, so the live schema cannot be shown to match the file. The baseline is "+
					"merged rather than incremental, so there is nothing to replay: recreate the "+
					"database (or, for a test database, drop and recreate it)", name)
			}
			if recorded != want {
				return fmt.Errorf("migration %s has changed since it was applied: the database "+
					"holds schema from a different revision of that file (recorded %s, file %s). "+
					"Editing a merged baseline is not a migration — add a new migration file, or "+
					"recreate the database", name, recorded[:12], want[:12])
			}
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read schema_migrations: %w", err)
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
			`INSERT INTO schema_migrations (version, applied_at, checksum) VALUES ($1, $2, $3)`,
			name, time.Now().UnixMilli(), want,
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

// migrationChecksum is the hex sha256 of a migration's exact bytes.
//
// Content, not filename, because the filename is what the database already
// records — and a filename that still matches while the content has changed is
// precisely the failure this guards.
func migrationChecksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
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
