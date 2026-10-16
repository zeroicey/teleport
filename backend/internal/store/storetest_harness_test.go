package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/store/storetest"
)

// The three tests here guard the *harness*, not the store. They exist because
// the harness is what every other PostgreSQL test's result depends on: if it
// silently shares one schema, every "passed" below it is worth less than it
// looks. See .ai/decisions/2026-10-16-p2-test-strategy.md.

// TestHarnessGivesEachTestItsOwnSchema is the guard for the whole strategy.
//
// Two pools obtained in the same test must not see each other's rows. That is
// what makes the quota and cap tests meaningful: they count rows, and a shared
// schema would let a sibling test's leftovers change the answer.
func TestHarnessGivesEachTestItsOwnSchema(t *testing.T) {
	storetest.SkipWithoutDatabase(t)
	ctx := context.Background()
	a := storetest.Pool(t, MigratePG)
	b := storetest.Pool(t, MigratePG)

	now := NowMS()
	if _, err := a.Exec(ctx, `
		INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		VALUES ('only-in-a', 't', 'general', 'markdown', 'body', '{}', $1, $1)`, now); err != nil {
		t.Fatalf("insert into the first schema: %v", err)
	}

	count := func(pool *pgxpool.Pool) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM reports`).Scan(&n); err != nil {
			t.Fatalf("count reports: %v", err)
		}
		return n
	}

	if got := count(a); got != 1 {
		t.Errorf("the schema that inserted a report sees %d, want 1", got)
	}
	if got := count(b); got != 0 {
		t.Errorf("a second test's schema sees %d reports, want 0 — the harness is sharing a schema", got)
	}
}

// TestHarnessDropsTheSchemaWhenTheTestEnds guards against the other half of the
// same failure: isolation that works but leaks schemas, so a long run slowly
// fills the database with abandoned ones.
func TestHarnessDropsTheSchemaWhenTheTestEnds(t *testing.T) {
	// The skip must happen out here: the database is obtained inside the subtest
	// below, and a skipped subtest makes t.Run report true, which this test would
	// otherwise read as "the subtest succeeded, but reported no schema".
	storetest.SkipWithoutDatabase(t)

	var schema string
	if !t.Run("inner", func(t *testing.T) {
		_, name := storetest.Schema(t, MigratePG)
		schema = name
	}) {
		t.Fatal("the inner subtest failed; nothing to assert about cleanup")
	}
	if schema == "" {
		t.Fatal("the inner subtest did not report a schema name")
	}

	// The inner subtest's cleanups have run by now.
	admin := storetest.AdminPool(t)
	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`, schema,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("schema %s outlived the test that owned it; schemas will accumulate", schema)
	}
}

// TestHarnessRefusesADatabaseWithoutTestInItsName is the falsification test for
// the DSN guard.
//
// The guard is the only thing standing between a mistyped TEST_PG_DSN and a
// harness that creates and drops schemas in the database holding real reports,
// so it is asserted on the decision itself rather than through a t.Fatalf path —
// a refusal reachable only by failing a test would fail its parent too, and the
// suite could never be green.
func TestHarnessRefusesADatabaseWithoutTestInItsName(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want error
	}{
		{"the production database is refused", "postgres://teleport:pw@127.0.0.1:5432/teleport?sslmode=disable", storetest.ErrDSNDatabaseNotTest},
		{"a database merely named differently is refused", "postgres://u:p@h:5432/appdb?sslmode=disable", storetest.ErrDSNDatabaseNotTest},
		{"a missing DSN is a skip, not a refusal", "", storetest.ErrDSNNotSet},
		{"whitespace is treated as unset", "   ", storetest.ErrDSNNotSet},
		{"a test database is accepted", "postgres://teleport:pw@127.0.0.1:5432/teleport_test?sslmode=disable", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := storetest.CheckDSN(tc.dsn)
			if tc.want == nil {
				if err != nil {
					t.Errorf("CheckDSN(%q) = %v, want nil", tc.dsn, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("CheckDSN(%q) = %v, want %v", tc.dsn, err, tc.want)
			}
		})
	}
}
