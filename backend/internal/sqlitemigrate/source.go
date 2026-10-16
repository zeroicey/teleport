// Package sqlitemigrate moves the pre-rewrite SQLite data set into the
// PostgreSQL schema, and checks the result against the source.
//
// It exists because the rewrite (SQLite -> pgx/PostgreSQL) is a port, not a
// data change: the 5 reports, 2 agent keys and 6 share links that agents and
// humans already hold links to must survive byte-for-byte, and a link that was
// permanently valid or deliberately revoked before the port must still behave
// that way after it. The import is therefore written to be *checkable* rather
// than merely to run: every row is compared after the fact, the target must be
// empty unless -replace is passed explicitly, and the whole thing is one
// transaction so a failure leaves the target exactly as it was.
//
// Two rules this package follows, both learned the hard way in this repository:
//
//  1. It never issues DDL. The schema is built by the application's own
//     migrator (store.OpenPG -> store.MigratePG), which records a content hash
//     of each migration and refuses a database whose live schema disagrees with
//     the embedded files. A merged baseline is never replayed, so a schema
//     created from an older revision of it would be silently stale forever —
//     see .ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md.
//     The importer only inserts rows.
//
//  2. It reads the source through a *different* implementation than the one the
//     target is written with: the `sqlite3` command-line shell, in JSON mode,
//     over a read-only immutable URI. A driver bug and a spelling mistake in the
//     mapping therefore cannot cancel each other out inside one library, and the
//     source file cannot be modified by a read (no journal, no WAL, nothing).
package sqlitemigrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Tables is the import order: parents before children, so the foreign keys
// (share_tokens -> reports, key_renewals -> agent_keys) hold at every
// statement. `schema_migrations` is deliberately absent: it describes the
// migrator, not the application, and the target's own migrator owns it.
var Tables = []string{
	"reports",
	"share_tokens",
	"agent_keys",
	"key_applications",
	"key_renewals",
}

// PrimaryKey names the column that identifies a row in each table, so that a
// verification failure can point at a row instead of at a table.
var PrimaryKey = map[string]string{
	"reports":          "id",
	"share_tokens":     "token",
	"agent_keys":       "id",
	"key_applications": "id",
	"key_renewals":     "id",
}

// KnownSourceMigrations is the set of SQLite migration versions this importer
// understands, and therefore the only source schema it will import.
//
// It refuses anything else on purpose. An unknown version means the source has
// been migrated by a build whose columns this mapping has never seen, and the
// failure mode of guessing is silent: a new NOT NULL column would be filled
// with its default and the row would look complete. Refusing is cheap here; a
// partial import that looks successful is not.
var KnownSourceMigrations = []string{"0001_init", "0002_agent_keys"}

// Column is one column of a source table, as SQLite describes it.
type Column struct {
	Name     string
	DeclType string
	NotNull  bool
	PK       int
}

// Table is one source table's full contents. Values are in column order, and a
// missing/NULL value is Go nil.
type Table struct {
	Name    string
	Columns []Column
	Rows    [][]any
}

// Reader is the read side of an import. It is an interface so that the mapping,
// conversion and reconciliation logic can be tested without the sqlite3 binary
// and without a database file; the production implementation is *Source.
type Reader interface {
	// MigrationVersions returns the source's recorded schema versions.
	MigrationVersions(ctx context.Context) ([]string, error)
	// Tables returns every table in import order with all of its rows.
	Tables(ctx context.Context) ([]Table, error)
	// RenderedText returns each row's requested columns rendered to text by
	// SQLite itself, keyed by the table's primary key. It exists so that one
	// verification check can compare two renderings, neither of which was
	// produced by this package's own conversion; see
	// Verifier.checkDatabaseRenderedText for why that matters.
	RenderedText(ctx context.Context, table string, columns []string) (map[string][]RenderedValue, error)
}

// ErrSourceHasWAL means a SQLite sidecar file was found next to the source.
var ErrSourceHasWAL = errors.New("source has a -wal/-shm/-journal sidecar")

// Source reads a SQLite database through the sqlite3 command-line shell.
type Source struct {
	path string
	bin  string
}

// RenderedValue is one column value rendered to text by a database.
//
// Null distinguishes SQL NULL from the empty string: they are different values in
// both databases and must stay different here.
type RenderedValue struct {
	Null bool
	Text string
}

// SQLiteEnvVar overrides the sqlite3 binary, for machines where it is not on
// PATH (the shell script that drives this tool looks for it too).
const SQLiteEnvVar = "SQLITE3_BIN"

// OpenSource prepares to read path read-only.
//
// Three guards run before any data is read, and each one exists because its
// absence would produce a *quiet* wrong result:
//
//   - A -wal (or -shm/-journal) sidecar next to the file is a refusal. The
//     read-only URI below is immutable, which tells SQLite to trust the main
//     file and ignore the write-ahead log entirely. If a WAL exists, its
//     committed-but-not-checkpointed transactions would be silently dropped —
//     so the honest move is to require a checkpointed file (or a fresh
//     `sqlite3 .backup`) rather than import a plausible subset.
//   - `PRAGMA integrity_check` must say ok. A copy that was truncated by a
//     failed transfer still opens and still answers queries.
//   - A functional probe for `-json` support, because an older shell that does
//     not know the flag fails in a way that is easy to misread (it treats the
//     unknown argument as a filename). Probing beats parsing a version number.
func OpenSource(ctx context.Context, path string) (*Source, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("source path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve source path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat source %s: %w", abs, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("source %s is a directory, not a SQLite database file", abs)
	}
	if err := checkNoSidecars(abs); err != nil {
		return nil, err
	}

	bin := os.Getenv(SQLiteEnvVar)
	if bin == "" {
		bin = "sqlite3"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("find the sqlite3 shell (%s=%q): %w", SQLiteEnvVar, bin, err)
	}

	s := &Source{path: abs, bin: resolved}
	if err := s.probeJSON(ctx); err != nil {
		return nil, err
	}
	if err := s.probeIntegrity(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Path is the absolute path of the source being read.
func (s *Source) Path() string { return s.path }

// SQLiteBinary is the resolved path of the shell this source reads through.
func (s *Source) SQLiteBinary() string { return s.bin }

// SHA256 is the hash of the source file's bytes, recorded in the import report
// so a human can prove which file was imported. It is computed by this process
// from the file, not taken from a caller-supplied string.
func (s *Source) SHA256() (string, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return "", fmt.Errorf("open source for hashing: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash source: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkNoSidecars refuses a database whose write-ahead log exists.
func checkNoSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecar := path + suffix
		if _, err := os.Stat(sidecar); err == nil {
			return fmt.Errorf("%w: %s exists. The import reads the main file with "+
				"immutable=1, which ignores the write-ahead log, so committed transactions "+
				"stored there would be silently dropped. Checkpoint the source (or copy it "+
				"with `sqlite3 SOURCE \".backup DEST\"`) and remove the sidecar", ErrSourceHasWAL, sidecar)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat sidecar %s: %w", sidecar, err)
		}
	}
	return nil
}

// readOnlyURI is the URI the shell is given. mode=ro refuses writes; immutable=1
// promises no writer will touch the file while it is open, which also lets
// SQLite skip locking entirely. Both matter: the source is somebody's live
// database and this tool must not be able to modify it even by accident.
func (s *Source) readOnlyURI() string {
	return "file:" + s.path + "?mode=ro&immutable=1"
}

// query runs one statement through the shell and returns the decoded rows.
//
// Values are decoded with UseNumber so that a 64-bit epoch-millisecond integer
// stays exact: the default float64 decode would round 1791563326606 to the
// nearest double and every timestamp would land in the database a fraction of a
// millisecond off — small enough to look right and impossible to notice later.
func (s *Source) query(ctx context.Context, sql string) ([]map[string]any, error) {
	// The shell is resolved through exec.LookPath above, and the only interpolated
	// values are this package's fixed table allowlist and the database's own
	// catalogue, never a caller-supplied string; sql is assembled by readTable.
	cmd := exec.CommandContext(ctx, s.bin, s.readOnlyURI(), "-json", sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlite3 %q failed: %w: %s", sql, err, strings.TrimSpace(stderr.String()))
	}
	trimmed := bytes.TrimSpace(stdout.Bytes())
	if len(trimmed) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var rows []map[string]any
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode sqlite3 output for %q: %w", sql, err)
	}
	return rows, nil
}

// probeJSON fails when the shell predates `-json` (SQLite 3.33).
func (s *Source) probeJSON(ctx context.Context) error {
	rows, err := s.query(ctx, `SELECT 1 AS probe`)
	if err != nil {
		return fmt.Errorf("the sqlite3 shell at %s does not support -json output; "+
			"SQLite 3.33 or newer is required: %w", s.bin, err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("unexpected sqlite3 -json probe result: %v", rows)
	}
	return nil
}

// probeIntegrity runs PRAGMA integrity_check on the source.
func (s *Source) probeIntegrity(ctx context.Context) error {
	rows, err := s.query(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("integrity check %s: %w", s.path, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("integrity check %s returned no rows", s.path)
	}
	got, ok := rows[0]["integrity_check"].(string)
	if !ok || got != "ok" {
		return fmt.Errorf("source %s failed its integrity check: %v", s.path, rows)
	}
	return nil
}

// MigrationVersions returns the source's recorded migration versions, sorted.
func (s *Source) MigrationVersions(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read source schema_migrations: %w", err)
	}
	versions := make([]string, 0, len(rows))
	for _, row := range rows {
		v, ok := row["version"].(string)
		if !ok {
			return nil, fmt.Errorf("source schema_migrations.version is not text: %v", row["version"])
		}
		versions = append(versions, v)
	}
	return versions, nil
}

// CheckSourceMigrations refuses a source whose recorded versions are not
// exactly the ones this mapping was written against.
func CheckSourceMigrations(versions []string) error {
	seen := map[string]bool{}
	for _, v := range versions {
		seen[v] = true
	}
	for _, known := range KnownSourceMigrations {
		if !seen[known] {
			return fmt.Errorf("source is missing migration %q (recorded: %s); it is not the "+
				"database this importer was written for", known, strings.Join(versions, ", "))
		}
	}
	for _, v := range versions {
		if !slices.Contains(KnownSourceMigrations, v) {
			return fmt.Errorf("source records unknown migration %q (known: %s). The old schema "+
				"moved on past this mapping; adding columns here would be guesswork, and a new "+
				"NOT NULL column would be silently filled with its default",
				v, strings.Join(KnownSourceMigrations, ", "))
		}
	}
	return nil
}

// Tables reads every table in import order.
func (s *Source) Tables(ctx context.Context) ([]Table, error) {
	out := make([]Table, 0, len(Tables))
	for _, name := range Tables {
		table, err := s.readTable(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, *table)
	}
	return out, nil
}

// RenderedText renders the requested columns with SQLite's own CAST(col AS TEXT)
// and returns them keyed by primary key.
//
// The rendering is done by the database, not by Go. That is the entire point of
// the method: it is the only place a value passing between the two databases can
// be compared without going through this package's convertValue, which is the
// code under test. See Verifier.checkDatabaseRenderedText.
func (s *Source) RenderedText(ctx context.Context, table string, columns []string) (map[string][]RenderedValue, error) {
	key := PrimaryKey[table]
	selects := make([]string, 0, len(columns)+1)
	selects = append(selects, "CAST("+quoteIdent(key)+" AS TEXT) AS k")
	for _, c := range columns {
		selects = append(selects, "CAST("+quoteIdent(c)+" AS TEXT) AS "+quoteIdent(c))
	}
	rows, err := s.query(ctx, "SELECT "+strings.Join(selects, ", ")+" FROM "+quoteIdent(table))
	if err != nil {
		return nil, fmt.Errorf("render source table %s as text: %w", table, err)
	}

	out := make(map[string][]RenderedValue, len(rows))
	for _, row := range rows {
		id, ok := row["k"].(string)
		if !ok {
			return nil, fmt.Errorf("source table %s: the primary key did not render as text: %v", table, row["k"])
		}
		values := make([]RenderedValue, len(columns))
		for i, c := range columns {
			switch v := row[c].(type) {
			case nil:
				values[i] = RenderedValue{Null: true}
			case string:
				values[i] = RenderedValue{Text: v}
			default:
				// CAST(... AS TEXT) gives every value a TEXT storage class, so the
				// shell emits a JSON string. Anything else means the cast did not
				// do what this code believes it did, and comparing it would be
				// comparing the wrong thing.
				return nil, fmt.Errorf("source table %s column %s did not render as text: %T (%v)", table, c, v, v)
			}
		}
		out[id] = values
	}
	return out, nil
}

// readTable reads one table's columns and rows.
//
// The column list comes from pragma_table_info and is written out explicitly in
// the SELECT rather than using `SELECT *`, so the values' order is defined by
// this program and not by the order the file happens to store them in.
func (s *Source) readTable(ctx context.Context, name string) (*Table, error) {
	cols, err := s.columns(ctx, name)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.Name)
	}
	rows, err := s.query(ctx, "SELECT "+strings.Join(names, ", ")+" FROM "+quoteIdent(name))
	if err != nil {
		return nil, fmt.Errorf("read source table %s: %w", name, err)
	}
	table := &Table{Name: name, Columns: cols, Rows: make([][]any, 0, len(rows))}
	for _, row := range rows {
		values := make([]any, len(cols))
		for i, c := range cols {
			v, ok := row[c.Name]
			if !ok {
				return nil, fmt.Errorf("source table %s row is missing column %s", name, c.Name)
			}
			values[i] = v
		}
		table.Rows = append(table.Rows, values)
	}
	return table, nil
}

// columns reads a table's column list from the database's own catalogue.
func (s *Source) columns(ctx context.Context, name string) ([]Column, error) {
	rows, err := s.query(ctx, "SELECT name, type, \"notnull\", pk FROM pragma_table_info("+
		quoteLiteral(name)+") ORDER BY cid")
	if err != nil {
		return nil, fmt.Errorf("read source columns of %s: %w", name, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("source has no table %q", name)
	}
	cols := make([]Column, 0, len(rows))
	for _, row := range rows {
		name, ok := row["name"].(string)
		if !ok {
			return nil, fmt.Errorf("source %s has a column whose name is not text: %v", name, row["name"])
		}
		declType, ok := row["type"].(string)
		if !ok {
			return nil, fmt.Errorf("source %s.%s has a declared type that is not text: %v", name, name, row["type"])
		}
		notNull, err := numberToInt64(row["notnull"], "notnull")
		if err != nil {
			return nil, fmt.Errorf("source %s.%s: %w", name, name, err)
		}
		pk, err := numberToInt64(row["pk"], "pk")
		if err != nil {
			return nil, fmt.Errorf("source %s.%s: %w", name, name, err)
		}
		cols = append(cols, Column{Name: name, DeclType: declType, NotNull: notNull == 1, PK: int(pk)})
	}
	return cols, nil
}

// numberToInt64 decodes a json.Number that must be an integer.
//
// json.Number (not float64) is what the decoder is configured to produce, so a
// 64-bit epoch-millisecond value survives intact. Int64() then rejects anything
// with a fractional part, which for these catalogue columns would mean the
// database is not what it claims to be.
func numberToInt64(v any, what string) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s is not a number: %v", what, v)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, fmt.Errorf("%s is not an integer: %v", what, v)
	}
	return i, nil
}

// quoteIdent double-quotes a SQL identifier.
//
// The names here come from a fixed allowlist (Tables) and from the database's
// own catalogue, never from a command-line argument, so this is defence in
// depth rather than the primary control. It is still the right habit: the cost
// of quoting is zero and the cost of being wrong once is a statement that
// executes something else.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral single-quotes a SQL string literal.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
