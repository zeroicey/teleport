package sqlitemigrate

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/store"
	"github.com/zeroicey/teleport/backend/internal/store/storetest"
)

// These tests exercise the whole import path against a real PostgreSQL target
// and a real SQLite source, built here from the frozen legacy DDL in testdata/.
//
// They need TEST_PG_DSN (like every other PostgreSQL test) and the `sqlite3`
// shell (the importer reads through it). Without either, they skip — the gate
// that refuses to skip is scripts/test-pg.sh and scripts/verify-p5.sh.
//
// The data set here is deliberately richer than the real one: it contains a
// *revoked* link and an *expired* link, a key that has expired and a key that was
// revoked. The real source has none of those (its only revoked links are revoked,
// its keys are all live), so without this fixture the 410 branch and the
// unusable-key branch would never run in the gate.

// fixtureRows is the data set. The interesting rows are the ones that pin the
// categories the real source does not contain:
//
//	t-perm     expires_at = 0, active    -> must stay permanent (never expires)
//	t-revoked  is_active = 0             -> must stay 404
//	t-live     expires_at in 2100        -> must resolve, expiring, not permanent
//	t-expired  expires_at = 1970         -> must stay 410
//	k-live     revoked_at = 0            -> must still authenticate
//	k-expired  expires_at = 1970         -> must NOT authenticate
//	k-revoked  revoked_at = 1000         -> must NOT authenticate
//
// `metadata` on the first report is deliberately NOT in alphabetical key order:
// the source hands back the bytes the agent sent while the store hands back a
// decoded map, so a digest that compared those two spellings directly would fail
// on this row.
const fixtureRows = `
INSERT INTO schema_migrations (version, applied_at) VALUES
  ('0001_init', 1791539395765),
  ('0002_agent_keys', 1791573176957);

INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at, owner_key_id) VALUES
  ('r-markdown', 'Report one', 'general', 'markdown', '# One

Body with a ''quote'' and a tab	inside.
', '{"z":1,"a":2}', 1791563326606, 1791563326606, 'k-live'),
  ('r-html', 'Report two', 'research', 'html', '<p>html body</p>', '{}', 1791707070441, 1791707070441, '');

INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active, view_count) VALUES
  ('t-perm',    'r-markdown', 1791563326606, 0,              1, 21),
  ('t-revoked', 'r-markdown', 1791563326606, 4102444800000,  0,  0),
  ('t-live',    'r-markdown', 1791563326606, 4102444800000,  1,  7),
  ('t-expired', 'r-html',     1791563326606, 1000,           1,  3);

INSERT INTO agent_keys (id, name, token_hash, token_prefix, created_at, expires_at, revoked_at, last_used_at, request_count, note) VALUES
  ('k-live',    'live key',    '1111111111111111111111111111111111111111111111111111111111111111', 'pref1', 1791642390015, 0,           0,    1791652345425, 28, ''),
  ('k-expired', 'expired key', '2222222222222222222222222222222222222222222222222222222222222222', 'pref2', 1791642390015, 1000,        0,    0,              1, ''),
  ('k-revoked', 'revoked key', '3333333333333333333333333333333333333333333333333333333333333333', 'pref3', 1791642390015, 4102444800000, 1000, 0,              0, 'a human killed this one');

INSERT INTO key_applications (id, claim_hash, label, purpose, requested_hours, status, created_at, expires_at, decided_at, claim_deadline, issued_key_id, requester_ip, user_agent, approved_name, approved_hours, approved_note) VALUES
  ('a-claimed', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'auditor', 'audit the platform', 720.0, 'claimed', 1791641966603, 1791728366603, 1791642387622, 1791644187622, 'k-live', '121.9.113.18', 'curl/8.22.0', 'auditor', 720.0, ''),
  ('a-pending', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'newcomer', '',                   24.0,  'pending', 1791987376289, 1792073776289, 0,             0,              '',       '121.9.113.34', 'Mozilla/5.0', 'newcomer', 0,     '');

INSERT INTO key_renewals (id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at) VALUES
  -- 0.30000000000000004 needs all 17 significant digits to round-trip, which is
  -- the point: every other REAL in this fixture (720.0, 24.0, 72.0, 0) renders
  -- identically at 15 digits, so a precision loss anywhere in the float path would
  -- go unnoticed. See TestImportPreservesRealPrecision.
  ('rn-approved', 'k-live',    0.30000000000000004, 'approved', 1791992633782, 1791992633782, 4102444800000),
  ('rn-pending',  'k-expired', 24.0,                'pending',  1791992633783, 0,             0);
`

// fixtureCounts is what the fixture above contains, per table.
var fixtureCounts = map[string]int{
	"reports":          2,
	"share_tokens":     4,
	"agent_keys":       3,
	"key_applications": 2,
	"key_renewals":     2,
}

// fixture builds the SQLite source database once per test.
func fixture(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath(sqliteEnvOrDefault())
	if err != nil {
		t.Skipf("sqlite3 not found (%s): the importer reads its source through it", SQLiteEnvVar)
	}
	schema, err := os.ReadFile(filepath.Join("testdata", "legacy_sqlite_schema.sql"))
	if err != nil {
		t.Fatalf("read the legacy schema fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	cmd := exec.Command(bin, path)
	cmd.Stdin = strings.NewReader(string(schema) + fixtureRows)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build the SQLite fixture: %v: %s", err, stderr.String())
	}
	return path
}

func sqliteEnvOrDefault() string {
	if v := os.Getenv(SQLiteEnvVar); v != "" {
		return v
	}
	return "sqlite3"
}

// importedFixture is a target with the fixture already imported into it.
type importedFixture struct {
	ctx  context.Context
	src  *Source
	pool *pgxpool.Pool
	path string
}

func setupImported(t *testing.T) *importedFixture {
	t.Helper()
	storetest.SkipWithoutDatabase(t)
	path := fixture(t)
	// The harness gives this test its own schema, created from the application's
	// own migrator, and drops it afterwards. That is also the schema-ownership
	// statement: the importer never issues DDL.
	pool, _ := storetest.Schema(t, store.MigratePG)

	ctx := context.Background()
	src, err := OpenSource(ctx, path)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	if _, err := Import(ctx, src, pool, false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return &importedFixture{ctx: ctx, src: src, pool: pool, path: path}
}

// TestImportMovesEveryRowAndVerifies is the happy path, with the counts pinned.
func TestImportMovesEveryRowAndVerifies(t *testing.T) {
	f := setupImported(t)

	got := countTarget(t, f.ctx, f.pool)
	for table, want := range fixtureCounts {
		if got[table] != want {
			t.Errorf("%s: imported %d row(s), want %d", table, got[table], want)
		}
	}

	report, err := Verify(f.ctx, f.src, f.pool)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("verification failed:\n%s", describe(report))
	}
	if report.Passed == 0 {
		t.Fatal("no checks ran")
	}

	// All four link categories must have been exercised. Without this, a change
	// that classified every link as "live" would leave every check green while
	// three of the four would have nothing to say.
	detail := checkDetail(t, report, "permanent_links_stay_permanent")
	for _, want := range []string{"permanent(expires_at=0)=1", "revoked(is_active=0)=1", "live=1", "expired=1"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the link inventory %q does not contain %q; the fixture is not exercising "+
				"every category", detail, want)
		}
	}
}

// TestImportIsIdempotentByRefusal: a second import must be a decision, not an
// accident, so it is refused until -replace is asked for explicitly.
func TestImportRefusesANonEmptyTarget(t *testing.T) {
	f := setupImported(t)

	_, err := Import(f.ctx, f.src, f.pool, false)
	if err == nil {
		t.Fatal("a second import into the same target succeeded")
	}
	if !strings.Contains(err.Error(), "already has rows") {
		t.Errorf("error %q does not explain that the target is not empty", err)
	}
	// And the refusal must not have changed anything.
	if got := countTarget(t, f.ctx, f.pool); got["reports"] != fixtureCounts["reports"] {
		t.Errorf("the refused import modified the target: reports=%d", got["reports"])
	}
}

// TestImportReplaceRestoresTheSource is the recovery path: -replace truncates and
// re-imports, which is also the only way a second run is allowed to proceed.
func TestImportReplaceRestoresTheSource(t *testing.T) {
	f := setupImported(t)

	if _, err := f.pool.Exec(f.ctx, `DELETE FROM reports WHERE id = 'r-html'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	before, err := Verify(f.ctx, f.src, f.pool)
	if err != nil {
		t.Fatalf("Verify after tamper: %v", err)
	}
	if before.OK() {
		t.Fatal("verification passed after a report was deleted")
	}

	if _, err := Import(f.ctx, f.src, f.pool, true); err != nil {
		t.Fatalf("Import with replace: %v", err)
	}
	after, err := Verify(f.ctx, f.src, f.pool)
	if err != nil {
		t.Fatalf("Verify after re-import: %v", err)
	}
	if !after.OK() {
		t.Fatalf("verification still fails after a replace-import:\n%s", describe(after))
	}
}

// TestVerifyDetectsEachKindOfTamper is the falsification the repository requires
// of a verification tool: every check must be shown to fail when the thing it
// claims to check is broken. Each case breaks exactly one property and names the
// check that must notice.
//
// The tamper runs on a target this test's own schema owns, after a successful
// import, so the only difference between the failing run and the passing one above
// is the tamper itself.
func TestVerifyDetectsEachKindOfTamper(t *testing.T) {
	// This call is load-bearing, not a nicety. Without it the parent SKIPS nothing
	// (each subtest skips instead, and t.Run reports true for a skip) so `go test`
	// prints `--- PASS` for a test that asserted nothing — a green line for a test
	// with no result. The no-DB gate counts only top-level skips, so the parent
	// must skip for that count to mean anything. storetest documents this exact
	// trap and why it takes a testing.T.
	storetest.SkipWithoutDatabase(t)

	for _, tc := range []struct {
		name      string
		sql       []string
		wantCheck string
	}{
		{
			name:      "a deleted report",
			sql:       []string{`DELETE FROM reports WHERE id = 'r-html'`},
			wantCheck: "row_equality[reports]",
		},
		{
			name:      "a changed report body",
			sql:       []string{`UPDATE reports SET content = '<p>different</p>' WHERE id = 'r-html'`},
			wantCheck: "report_content",
		},
		{
			name:      "a changed title",
			sql:       []string{`UPDATE reports SET title = 'renamed' WHERE id = 'r-markdown'`},
			wantCheck: "row_equality[reports]",
		},
		{
			name:      "a revoked link made active again",
			sql:       []string{`UPDATE share_tokens SET is_active = 1 WHERE token = 't-revoked'`},
			wantCheck: "revoked_links_stay_404",
		},
		{
			name:      "a permanent link given an expiry",
			sql:       []string{`UPDATE share_tokens SET expires_at = 4102444800000 WHERE token = 't-perm'`},
			wantCheck: "permanent_links_stay_permanent",
		},
		{
			name:      "a live link's expiry moved",
			sql:       []string{`UPDATE share_tokens SET expires_at = 4102444800001 WHERE token = 't-live'`},
			wantCheck: "live_links_still_resolve",
		},
		{
			name:      "an expired link made permanent",
			sql:       []string{`UPDATE share_tokens SET expires_at = 0 WHERE token = 't-expired'`},
			wantCheck: "expired_links_stay_410",
		},
		{
			name:      "a live key revoked",
			sql:       []string{`UPDATE agent_keys SET revoked_at = 1792130000000 WHERE id = 'k-live'`},
			wantCheck: "agent_keys_still_authenticate",
		},
		{
			name:      "a revoked key un-revoked",
			sql:       []string{`UPDATE agent_keys SET revoked_at = 0 WHERE id = 'k-revoked'`},
			wantCheck: "agent_keys_still_authenticate",
		},
		{
			// The root report (owner_key_id = '') is handed to a key.
			name:      "the root report claimed by a key",
			sql:       []string{`UPDATE reports SET owner_key_id = 'k-live' WHERE id = 'r-html'`},
			wantCheck: "ownership_boundary_unchanged",
		},
		{
			// The mirror: a key's report is quietly turned into a root report.
			name:      "a key-owned report turned into a root report",
			sql:       []string{`UPDATE reports SET owner_key_id = '' WHERE id = 'r-markdown'`},
			wantCheck: "ownership_boundary_unchanged",
		},
		{
			name:      "a dropped view",
			sql:       []string{`DROP VIEW v_live_shares`},
			wantCheck: "target_schema",
		},
		{
			name:      "an extra row in a table with no cap",
			sql:       []string{`UPDATE key_renewals SET requested_hours = 96 WHERE id = 'rn-approved'`},
			wantCheck: "row_equality[key_renewals]",
		},
		{
			// The silent-loss scenario: a target column the source cannot fill would
			// take its DEFAULT and the row would look complete.
			name:      "a column the source does not have",
			sql:       []string{`ALTER TABLE key_applications ADD COLUMN surprise TEXT NOT NULL DEFAULT ''`},
			wantCheck: "column_sets",
		},
		{
			name:      "a renamed column",
			sql:       []string{`ALTER TABLE reports RENAME COLUMN category TO kind`},
			wantCheck: "column_sets",
		},
		{
			// A REAL rounded to fewer digits: 0.30000000000000004 -> 0.3. Only
			// detectable because the source reads REAL columns through SQLite's own
			// shortest-round-trip form rather than through the JSON float path.
			name:      "a double rounded to 15 digits",
			sql:       []string{`UPDATE key_renewals SET requested_hours = 0.3 WHERE id = 'rn-approved'`},
			wantCheck: "row_equality[key_renewals]",
		},
		{
			// The byte-comparison check through the real path, so that it is not
			// merely a unit-tested pure function: an integer cell changed in the
			// target must make db_hex_bytes[...] fail.
			name:      "a changed integer cell",
			sql:       []string{`UPDATE key_applications SET decided_at = 0 WHERE id = 'a-claimed'`},
			wantCheck: "db_hex_bytes[key_applications]",
		},
		{
			name:      "a changed text cell",
			sql:       []string{`UPDATE agent_keys SET note = 'different' WHERE id = 'k-revoked'`},
			wantCheck: "db_hex_bytes[agent_keys]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupImported(t)
			for _, stmt := range tc.sql {
				if _, err := f.pool.Exec(f.ctx, stmt); err != nil {
					t.Fatalf("tamper (%s): %v", stmt, err)
				}
			}
			report, err := Verify(f.ctx, f.src, f.pool)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if report.OK() {
				t.Fatalf("verification passed after the tamper %q — that check cannot fail, so it "+
					"proves nothing", tc.name)
			}
			if !failed(report, tc.wantCheck) {
				t.Errorf("%q was expected to fail, but the failures were: %s", tc.wantCheck, strings.Join(failedNames(report), ", "))
			}
		})
	}
}

// TestImportRefusesAValueThatWouldWrap is the end-to-end guard for the silent wrap
// that was measured: a value above int64's maximum in an INTEGER column.
//
// The unit test pins convertValue. This one pins the thing that actually matters —
// that the import stops, says which row and column, and leaves the target empty
// rather than writing a wrapped number. It is deliberately built as a mutation of
// the standard fixture so that the only difference from the passing run above is
// the one value.
func TestImportRefusesAValueThatWouldWrap(t *testing.T) {
	storetest.SkipWithoutDatabase(t)
	path := fixture(t)
	ctx := context.Background()

	// 2^63 in an INTEGER column: SQLite stores it with REAL affinity, so it comes
	// back through the shell as 9.2233720368547758e+18.
	runSQLite(t, path, `UPDATE key_renewals SET granted_expires_at = 9223372036854775808 WHERE id = 'rn-approved'`)

	src, err := OpenSource(ctx, path)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	pool, _ := storetest.Schema(t, store.MigratePG)

	_, err = Import(ctx, src, pool, false)
	if err == nil {
		t.Fatal("the import accepted a value that does not fit in an int64; it would have been written " +
			"as a wrapped number")
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("error %q does not say the value is out of range", err)
	}
	if !strings.Contains(err.Error(), "granted_expires_at") {
		t.Errorf("error %q does not name the offending column", err)
	}

	// Nothing may have been committed: the wrap was inside the single transaction.
	for table, n := range countTarget(t, ctx, pool) {
		if n != 0 {
			t.Errorf("table %s has %d row(s) after a refused import, want 0", table, n)
		}
	}
}

// runSQLite executes statements against a fixture database.
func runSQLite(t *testing.T, path, sql string) {
	t.Helper()
	bin, err := exec.LookPath(sqliteEnvOrDefault())
	if err != nil {
		t.Skipf("sqlite3 not found (%s)", SQLiteEnvVar)
	}
	cmd := exec.Command(bin, path, sql)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sqlite3 %q: %v: %s", sql, err, stderr.String())
	}
}

// TestVerifyFailsBeforeItInventsASchema: verify must not migrate. Pointing it at
// a schema with no tables must report that the target is unprepared, not create
// one — otherwise the tool would answer "is this correct?" by changing the answer.
func TestVerifyDoesNotMigrate(t *testing.T) {
	storetest.SkipWithoutDatabase(t)
	path := fixture(t)
	ctx := context.Background()
	src, err := OpenSource(ctx, path)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}

	// A schema that exists and is empty.
	pool, schema := storetest.Schema(t, store.MigratePG)
	if _, err := pool.Exec(ctx, `DROP VIEW v_live_shares`); err != nil {
		t.Fatalf("drop view: %v", err)
	}
	for _, table := range Tables {
		if _, err := pool.Exec(ctx, `DROP TABLE `+table+` CASCADE`); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}

	report, err := Verify(ctx, src, pool)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.OK() {
		t.Fatal("verification passed against an empty schema")
	}
	if !failed(report, "target_schema") {
		t.Errorf("want target_schema to fail, got: %s", strings.Join(failedNames(report), ", "))
	}
	if report.TargetSchema != schema {
		t.Errorf("reported target schema %q, want %q", report.TargetSchema, schema)
	}
}

// TestImportRefusesATransactionNotAPool is the falsification of the guard that
// beginTx's comment used to overstate.
//
// The first version asserted an interface with `Begin(ctx) (pgx.Tx, error)`, which
// pgx v5's Tx interface ALSO satisfies (pseudo-nested transactions are savepoints),
// so a transaction would have been accepted and the import would have opened a
// savepoint instead of failing. This test passes a real transaction in and requires
// a refusal.
func TestImportRefusesATransactionNotAPool(t *testing.T) {
	storetest.SkipWithoutDatabase(t)
	path := fixture(t)
	ctx := context.Background()
	src, err := OpenSource(ctx, path)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	pool, _ := storetest.Schema(t, store.MigratePG)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := Import(ctx, src, tx, false); err == nil {
		t.Fatal("Import accepted a transaction; it would have opened a savepoint instead of a " +
			"transaction, so the import would not have been all-or-nothing")
	} else if !strings.Contains(err.Error(), "*pgxpool.Pool") {
		t.Errorf("error %q does not say a pool is required", err)
	}
}

// TestImportPreservesRealPrecision is the guard for the float path that the
// byte-comparison check deliberately skips (see checkDatabaseHexBytes for why REAL
// is excluded there).
//
// It asserts the value with the fewest ambiguities: PostgreSQL's own
// float8send(col) must equal the eight IEEE754 bytes of the double the source
// holds, computed here from the decimal literal SQLite stores. A path that
// rounded to 15 significant digits would land on 0.29999999999999998 and fail.
func TestImportPreservesRealPrecision(t *testing.T) {
	f := setupImported(t)

	var hexBytes string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT encode(float8send(requested_hours), 'hex') FROM key_renewals WHERE id = 'rn-approved'`,
	).Scan(&hexBytes); err != nil {
		t.Fatalf("read float8send: %v", err)
	}
	want := fmt.Sprintf("%x", math.Float64bits(0.30000000000000004))
	if hexBytes != want {
		t.Errorf("requested_hours is %s, want %s (0.30000000000000004): the value lost precision "+
			"somewhere in the float path", hexBytes, want)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func countTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range Tables {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+quoteIdent(table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func checkDetail(t *testing.T, report *VerifyReport, name string) string {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c.Detail
		}
	}
	t.Fatalf("no check named %q; the checks were: %s", name, strings.Join(checkNames(report), ", "))
	return ""
}

func failed(report *VerifyReport, name string) bool {
	for _, c := range report.Checks {
		if c.Name == name {
			return !c.OK
		}
	}
	return false
}

func failedNames(report *VerifyReport) []string {
	var out []string
	for _, c := range report.Checks {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

func checkNames(report *VerifyReport) []string {
	out := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		out = append(out, c.Name)
	}
	return out
}

func describe(report *VerifyReport) string {
	var b strings.Builder
	for _, c := range report.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		b.WriteString("  " + mark + " " + c.Name + " " + c.Detail + "\n")
	}
	return b.String()
}
