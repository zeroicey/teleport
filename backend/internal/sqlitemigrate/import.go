package sqlitemigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableResult is the reconciliation of one table.
type TableResult struct {
	Table      string `json:"table"`
	SourceRows int    `json:"source_rows"`
	Inserted   int    `json:"inserted"`
	TargetRows int    `json:"target_rows"`
}

// Report is what an import did, in a form a human can compare with the numbers
// they were told to expect (5 reports, 2 keys, every share link).
type Report struct {
	SourceSchema string        `json:"source_schema"`
	TargetSchema string        `json:"target_schema"`
	Replaced     bool          `json:"replaced"`
	Tables       []TableResult `json:"tables"`
}

// Import moves every row from src into the target in ONE transaction and
// reconciles the row counts before committing.
//
// All-or-nothing is the whole point. The target it runs against is the database
// the rewritten service will serve from, `expires_at = 0` links are permanent
// and cannot be re-created from anywhere else, and a half-imported set would be
// indistinguishable from a complete one by looking at the dashboard. A failure
// leaves the five tables byte-identical to how they were found.
//
// That guarantee covers the ROWS. The CLI opens the target through store.OpenPG,
// which applies the schema migrations *before* this function runs, so a failed
// import can still leave a freshly migrated (empty) schema behind — the
// deployment's intended state, not a data change.
//
// replace is the explicit opt-in that allows importing over existing rows. The
// default refuses a non-empty target: a second import must be a decision, not
// an accident, because the second import would otherwise be an upsert whose
// semantics (which columns win, what happens to a row that is no longer in the
// source) nobody has specified.
//
// The truncate is inside the same transaction as the inserts, and PostgreSQL's
// TRUNCATE is transactional, so a failure *after* a -replace leaves the previous
// rows in place rather than an empty table — the destructive step is undoable
// right up to the commit.
func Import(ctx context.Context, src Reader, q querier, replace bool) (*Report, error) {
	versions, err := src.MigrationVersions(ctx)
	if err != nil {
		return nil, err
	}
	if err := CheckSourceMigrations(versions); err != nil {
		return nil, err
	}
	// Refuse a source holding tables this importer does not read. Nothing else can
	// see this loss: the counts match, every row matches, and the extra table's rows
	// are simply gone.
	if unknown, err := src.UnknownTables(ctx); err != nil {
		return nil, err
	} else if len(unknown) > 0 {
		return nil, fmt.Errorf("the source contains table(s) this importer does not read: %s. "+
			"Refusing, because importing only the known tables would leave those rows behind "+
			"silently — every count would still match", strings.Join(unknown, ", "))
	}

	source, err := src.Tables(ctx)
	if err != nil {
		return nil, err
	}
	target, err := inspectTarget(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := checkTargetTables(target.Schema, target); err != nil {
		return nil, err
	}
	for i := range source {
		if err := checkColumnsMatch(source[i].Name, source[i].Columns, target.Tables[source[i].Name]); err != nil {
			return nil, err
		}
	}

	tx, err := beginTx(ctx, q)
	if err != nil {
		return nil, err
	}
	// Rollback is a no-op after a successful Commit, so this needs no flag.
	defer func() { _ = tx.Rollback(ctx) }()

	// The emptiness check runs inside the transaction, as its first statement, so
	// that it and the inserts it guards are one unit of work.
	//
	// It is a decision, not a lock: `SELECT count(*)` takes only AccessShareLock,
	// which does not conflict with the RowExclusiveLock an INSERT takes, so a
	// concurrent writer is NOT blocked and can add rows between this count and the
	// inserts. What catches an additive race is the re-count before the commit —
	// and a count-preserving concurrent change (a delete plus an insert, or an
	// UPDATE after the last count) is not detectable here at all. That is why the
	// target is expected to be an idle, freshly migrated database, and why `verify`
	// exists and runs immediately afterwards. An earlier version of this comment
	// claimed the check made the race impossible; it does not, and the difference
	// matters to anyone deciding whether to run this against a live database.
	existing, err := countAll(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !replace {
		var nonEmpty []string
		for _, name := range Tables {
			if existing[name] > 0 {
				nonEmpty = append(nonEmpty, fmt.Sprintf("%s=%d", name, existing[name]))
			}
		}
		if len(nonEmpty) > 0 {
			return nil, fmt.Errorf("target schema %q already has rows (%s). Refusing to import over "+
				"them: pass -replace to truncate the five application tables first, or import into a "+
				"fresh schema", target.Schema, strings.Join(nonEmpty, ", "))
		}
	}
	if replace {
		if _, err := tx.Exec(ctx, truncateStatement()); err != nil {
			return nil, fmt.Errorf("truncate target tables: %w", err)
		}
	}

	report := &Report{
		SourceSchema: strings.Join(versions, "+"),
		TargetSchema: target.Schema,
		Replaced:     replace,
		Tables:       make([]TableResult, 0, len(source)),
	}
	for i := range source {
		table := &source[i]
		columns := target.Tables[table.Name]
		inserted, err := insertTable(ctx, tx, table, columns)
		if err != nil {
			return nil, err
		}
		report.Tables = append(report.Tables, TableResult{
			Table:      table.Name,
			SourceRows: len(table.Rows),
			Inserted:   inserted,
		})
	}

	// Count again from inside the same transaction and require the counts to
	// match the source exactly. A count mismatch is the cheapest possible
	// detector for a whole class of mistakes (a table silently skipped, a WHERE
	// clause in the wrong place, a statement that stopped early), and it costs
	// one query per table.
	after, err := countAll(ctx, tx)
	if err != nil {
		return nil, err
	}
	for i := range report.Tables {
		res := &report.Tables[i]
		res.TargetRows = after[res.Table]
		if res.TargetRows != res.SourceRows {
			return nil, fmt.Errorf("table %s: source has %d rows but the target has %d after the "+
				"insert; nothing was committed", res.Table, res.SourceRows, res.TargetRows)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit import: %w", err)
	}
	return report, nil
}

// beginTx starts the import transaction, and requires a POOL rather than
// accepting anything that merely looks like one.
//
// "Merely looks like one" is not hypothetical: pgx v5's Tx interface itself has
// Begin (pseudo-nested transactions are implemented as savepoints), so an
// assertion on `Begin(ctx) (pgx.Tx, error)` is satisfied by a *pgxpool.Tx as well
// as by a *pgxpool.Pool. The first version of this function asserted exactly that,
// so passing a transaction in would have silently opened a savepoint instead of
// failing. The concrete type is named here for that reason.
//
// A transaction must not be passed in anyway: taking one from the pool while
// already holding one is the exact bug that wedged this store during P2.3
// (.ai/pitfalls/cases/2026-10-16-store-nested-pool-acquire-deadlocks.md).
func beginTx(ctx context.Context, q querier) (pgxTx, error) {
	pool, ok := q.(*pgxpool.Pool)
	if !ok {
		return nil, fmt.Errorf("import target must be a *pgxpool.Pool (got %T); a transaction would "+
			"make pgx open a savepoint instead of a transaction", q)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin import transaction: %w", err)
	}
	return tx, nil
}

// countAll counts every application table.
func countAll(ctx context.Context, q querier) (map[string]int, error) {
	out := make(map[string]int, len(Tables))
	for _, name := range Tables {
		var n int
		if err := q.QueryRow(ctx, "SELECT count(*) FROM "+quoteIdent(name)).Scan(&n); err != nil {
			return nil, fmt.Errorf("count target table %s: %w", name, err)
		}
		out[name] = n
	}
	return out, nil
}

// truncateStatement empties every application table in one statement.
//
// One statement, not five: the foreign keys are immediate, so truncating
// share_tokens before reports (or the reverse, depending on how PostgreSQL picks
// an order) would fail if the statement did not cover the whole set. TRUNCATE
// lists let it drop and re-check them as a unit. `schema_migrations` is not
// touched: it is the migrator's opinion of the schema, and re-importing rows
// does not change the schema.
func truncateStatement() string {
	names := make([]string, len(Tables))
	for i, name := range Tables {
		names[i] = quoteIdent(name)
	}
	return "TRUNCATE " + strings.Join(names, ", ")
}

// insertTable inserts every row of one source table.
//
// One prepared statement, executed once per row. That is deliberate rather than a
// batch or a COPY: the tables here are tiny (the whole real data set is 17 rows),
// the statement is parsed once, and a per-row execution is what lets the error
// name the offending row instead of reporting a batch offset. A future data set
// large enough for COPY to matter would be a different tool with different
// failure semantics, not a parameter on this one.
//
// The column list comes from the *target's* catalogue in ordinal order, and each
// source value is looked up by name. That direction is deliberate: the INSERT
// names the columns it is filling, so the order it lists them in cannot silently
// shift a value into the wrong column, and a target column with no source value
// is impossible because checkColumnsMatch already required the two sets to be
// equal.
func insertTable(ctx context.Context, tx pgxTx, table *Table, columns []TargetColumn) (int, error) {
	index := sourceColumnIndex(table)
	names := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, c := range columns {
		names[i] = quoteIdent(c.Name)
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	stmt := "INSERT INTO " + quoteIdent(table.Name) + " (" + strings.Join(names, ", ") + ") VALUES (" +
		strings.Join(placeholders, ", ") + ")"

	inserted := 0
	for rowNum, row := range table.Rows {
		args := make([]any, len(columns))
		for i, c := range columns {
			pos, ok := index[c.Name]
			if !ok {
				return 0, fmt.Errorf("table %s has no source column %s", table.Name, c.Name)
			}
			value, err := convertValue(row[pos], c)
			if err != nil {
				return 0, fmt.Errorf("table %s row %d column %s: %w", table.Name, rowNum, c.Name, err)
			}
			args[i] = value
		}
		if _, err := tx.Exec(ctx, stmt, args...); err != nil {
			// The row's identity is included because the interesting failures
			// here are per-row (a CHECK, a foreign key) and a bare SQLSTATE
			// gives no way to find the offending row in a 30 KB report.
			return 0, fmt.Errorf("insert into %s %s=%v: %w", table.Name,
				PrimaryKey[table.Name], row[index[PrimaryKey[table.Name]]], err)
		}
		inserted++
	}
	return inserted, nil
}

// intTypes, floatTypes, textTypes and boolTypes are the PostgreSQL type names
// this importer knows how to convert a SQLite value into.
//
// An unknown type is a refusal, not a passthrough. Every column here arrives
// from a JSON document whose primitive type was decided by SQLite's storage
// class, and letting an unrecognised column fall through to pgx's own guess is
// how a timestamp ends up inserted as a string that PostgreSQL then coerces
// with the server's timezone.
var (
	intTypes   = []string{"bigint", "integer", "smallint", "int8", "int4", "int2"}
	floatTypes = []string{"double precision", "real", "numeric", "float8", "float4"}
	textTypes  = []string{"text", "character varying", "character", "varchar", "char"}
	boolTypes  = []string{"boolean", "bool"}
)

// convertValue adapts one JSON-decoded SQLite value to the Go type pgx should
// send for the target column.
//
// The JSON decoder was configured with UseNumber, so integers arrive as
// json.Number holding their exact decimal text. Converting here rather than
// letting pgx infer keeps a 64-bit epoch-millisecond value exact: via float64 it
// would be rounded to the nearest representable double, which is a difference of
// a fraction of a millisecond — invisible in a diff of the rendered page and
// still a changed byte in the database.
func convertValue(v any, column TargetColumn) (any, error) {
	if v == nil {
		// The schema is NOT NULL throughout, so a NULL here is a source the
		// mapping was not written for. Letting PostgreSQL raise it names the
		// column; returning nil would too, but with a worse message.
		return nil, nil
	}
	switch {
	case containsString(intTypes, column.DataType):
		switch value := v.(type) {
		case string:
			// A number stored in a TEXT-affinity source column still has to be
			// parsed rather than rejected: SQLite's affinity rules let the
			// storage class and the declared type disagree, and the number is
			// what the target column wants.
			return parseInt(value, column.Name)
		default:
			return anyToInt64(value, column.Name)
		}
	case containsString(floatTypes, column.DataType):
		return anyToFloat64(v, column.Name)
	case containsString(textTypes, column.DataType):
		switch value := v.(type) {
		case string:
			return value, nil
		case bool:
			return fmt.Sprintf("%t", value), nil
		case json.Number:
			// A numeric storage class in a TEXT target column. SQLite's TEXT
			// affinity converts a number to text on the way in, and the decimal
			// text the shell printed is that conversion's result — so keeping it
			// verbatim is the faithful choice ("720.0" stays "720.0", not "720").
			return value.String(), nil
		default:
			return fmt.Sprintf("%v", value), nil
		}
	case containsString(boolTypes, column.DataType):
		n, err := anyToInt64(v, column.Name)
		if err != nil {
			return nil, err
		}
		return n != 0, nil
	default:
		return nil, fmt.Errorf("target column %s has type %q, which this importer does not know how "+
			"to convert; add it to the type lists rather than passing the value through", column.Name, column.DataType)
	}
}

func anyToInt64(v any, column string) (int64, error) {
	switch value := v.(type) {
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case float64:
		return integral(value, column)
	case json.Number:
		// The common case: every integer the shell prints arrives as a
		// json.Number holding its exact decimal text, which is what keeps a
		// 64-bit epoch-millisecond value from being rounded through a float.
		if i, err := value.Int64(); err == nil {
			return i, nil
		}
		// A PLAIN integer literal that Int64() rejected is out of range, so it is
		// refused here rather than sent through float64. The bound was asymmetric
		// otherwise: +2^63 is caught by integral()'s exclusive upper bound, but the
		// double nearest to -9223372036854775809 is exactly -2^63, which is the
		// INCLUSIVE lower bound — so it would have been accepted as MinInt64 with no
		// error at all. Only spellings with a fraction or exponent need the float
		// path (a REAL in an INTEGER-affinity column).
		if isIntegerLiteral(value.String()) {
			return 0, fmt.Errorf("column %s: %s does not fit in a 64-bit integer", column, value.String())
		}
		f, err := value.Float64()
		if err != nil {
			return 0, fmt.Errorf("column %s: cannot convert %s to an integer: %w", column, value.String(), err)
		}
		return integral(f, column)
	case string:
		return parseInt(value, column)
	default:
		return 0, fmt.Errorf("column %s: cannot convert %T (%v) to an integer", column, v, v)
	}
}

func anyToFloat64(v any, column string) (float64, error) {
	switch value := v.(type) {
	case float64:
		return value, nil
	case json.Number:
		f, err := value.Float64()
		if err != nil {
			return 0, fmt.Errorf("column %s: cannot convert %s to a number: %w", column, value.String(), err)
		}
		return f, nil
	case int64:
		return float64(value), nil
	case int:
		return float64(value), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0, fmt.Errorf("column %s: cannot parse %q as a number: %w", column, value, err)
		}
		// ParseFloat accepts "inf", "infinity" and "nan" without error, so a TEXT
		// value in a REAL-affinity column spelled that way would become Infinity/NaN
		// in the target — and no check could see it, because REAL columns are excluded
		// from the byte comparison and row_equality would agree (both sides parsed the
		// same string). Non-finite is refused instead.
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("column %s: %q is not a finite number", column, value)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("column %s: cannot convert %T (%v) to a number", column, v, v)
	}
}

// integral refuses a fractional or out-of-range value for an integer column.
//
// Go's int64(float64) conversion is the dangerous one: it truncates silently AND
// its result is undefined for a value outside int64's range, which on this
// platform wraps to math.MinInt64. strconv parses "720.0" as 720 and ignores the
// fraction. Both are wrong in the same direction: they turn a value the schema
// does not allow into a plausible-looking integer, and the imported row then
// disagrees with the source in a way no count can see.
//
// The range check is not theoretical. SQLite's INTEGER is dynamically typed, so a
// value above int64's maximum is stored with REAL affinity and `sqlite3 -json`
// prints it in exponent form; that reached int64() and landed as
// -9223372036854775808. Measured before this check existed: the source held
// 9223372036854775808, PostgreSQL received -9223372036854775808, and the verifier
// reported GREEN — it converts the source through this same function, so the two
// sides were wrong in exactly the same way. On top of that, verify.go now has a
// second, independent comparison that renders each value with the source and the
// target database and never calls this function at all. See
// .ai/pitfalls/cases/2026-10-16-verifier-shares-the-conversion-it-checks.md.
func integral(f float64, column string) (int64, error) {
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("column %s: %v has a fractional part but the target column is an integer", column, f)
	}
	// int64's range as float64 values. 2^63 is exactly representable, so it is an
	// exclusive upper bound; -2^63 is exactly representable and IS a valid int64,
	// so it is an inclusive lower bound.
	const maxInt64Exclusive = float64(1 << 63)
	const minInt64Inclusive = -float64(1 << 63)
	if f >= maxInt64Exclusive || f < minInt64Inclusive {
		return 0, fmt.Errorf("column %s: %v does not fit in a 64-bit integer (the target column is an "+
			"integer). SQLite stores such a value with REAL affinity, so it arrives here as a float, "+
			"and converting it would wrap silently", column, f)
	}
	return int64(f), nil
}

// isIntegerLiteral reports whether s is a plain decimal integer literal: an
// optional sign followed by digits, with no fraction and no exponent.
//
// It exists to tell "this integer is out of range" (refuse) from "this is a REAL
// that happens to be spelled without a fraction" (go through the float path and let
// integral() decide).
func isIntegerLiteral(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseInt parses a decimal integer, refusing anything with a fractional part.
func parseInt(s, column string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("column %s: cannot parse %q as an integer: %w", column, s, err)
	}
	return n, nil
}
