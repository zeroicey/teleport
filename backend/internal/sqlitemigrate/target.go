package sqlitemigrate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenTargetPool opens a pool on the target WITHOUT applying migrations.
//
// It is the read side's entry point, and the not-migrating part is the point:
// `verify` must be able to answer "is what is in there correct?" without first
// making something true. An operator checking a database they have not imported
// into yet should get "target is missing reports", not a brand-new empty schema
// that then fails the checks for a different reason.
//
// The connection budget is deliberately small and explicit: the PostgreSQL
// instance is shared with clip, registry and serenique, so a pool is a slice of
// a budget rather than the whole instance (see the shared-instance pitfall).
func OpenTargetPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse target DSN: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open target: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping target: %w", err)
	}
	return pool, nil
}

// querier is the slice of pgx that both a pool and a transaction provide. The
// importer needs the same introspection inside the transaction as outside it
// (the row counts are compared before the commit), so the interface keeps one
// implementation of that logic rather than two.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgxTx is pgx.Tx under a local name.
//
// It is a type ALIAS rather than a fresh interface, and that matters: beginTx
// type-asserts the pool against a method set whose Begin returns exactly this
// type. A distinct interface with the same methods would not match
// *pgxpool.Pool's method set, the assertion would fail for every input, and the
// failure would look like "the target is not a pool".
type pgxTx = pgx.Tx

// TargetColumn is one column of a target table.
type TargetColumn struct {
	Name     string
	DataType string
	NotNull  bool
}

// TargetSchema is what the target database says its schema is.
type TargetSchema struct {
	// Schema is current_schema() for the connection, i.e. the schema the
	// unqualified table names resolve to. Recording it in the report is what
	// makes "the import went to schema X" a verifiable statement.
	Schema string
	// Tables maps table name -> its columns, in ordinal order. Views are not
	// included: the importer writes rows into tables, and a view is part of the
	// schema the migrator owns (see Views).
	Tables map[string][]TargetColumn
	// Views are the views the migrator creates, by name.
	Views []string
}

// inspectTarget reads the target's own description of its tables.
//
// The description comes from information_schema rather than from a list written
// in this package: the point of the check is to compare the source's columns
// with what the database actually has, and a list in Go would agree with itself.
//
// Only BASE TABLEs are read as tables. information_schema.columns describes
// views too, and a view whose name is not an expected table would otherwise be
// reported as an unrecognised table — which is what happened on the first run of
// this tool, against the very schema it was written for (`v_live_shares`).
func inspectTarget(ctx context.Context, q querier) (*TargetSchema, error) {
	var schema string
	if err := q.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return nil, fmt.Errorf("read current_schema(): %w", err)
	}

	rows, err := q.Query(ctx, `
		SELECT c.table_name, c.column_name, c.data_type, c.is_nullable
		  FROM information_schema.columns c
		  JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE c.table_schema = current_schema()
		   AND t.table_type = 'BASE TABLE'
		 ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("read information_schema.columns: %w", err)
	}
	defer rows.Close()

	out := &TargetSchema{Schema: schema, Tables: map[string][]TargetColumn{}}
	for rows.Next() {
		var table, column, dataType, nullable string
		if err := rows.Scan(&table, &column, &dataType, &nullable); err != nil {
			return nil, fmt.Errorf("scan information_schema.columns: %w", err)
		}
		out.Tables[table] = append(out.Tables[table], TargetColumn{
			Name:     column,
			DataType: dataType,
			NotNull:  nullable == "NO",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read information_schema.columns: %w", err)
	}

	viewRows, err := q.Query(ctx,
		`SELECT table_name FROM information_schema.views WHERE table_schema = current_schema()`)
	if err != nil {
		return nil, fmt.Errorf("read information_schema.views: %w", err)
	}
	defer viewRows.Close()
	for viewRows.Next() {
		var name string
		if err := viewRows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan information_schema.views: %w", err)
		}
		out.Views = append(out.Views, name)
	}
	if err := viewRows.Err(); err != nil {
		return nil, fmt.Errorf("read information_schema.views: %w", err)
	}
	sort.Strings(out.Views)
	return out, nil
}

// expectedTargetTables is everything a migrated target must contain: the five
// application tables plus the migrator's own bookkeeping table.
func expectedTargetTables() []string {
	out := append([]string{}, Tables...)
	out = append(out, "schema_migrations")
	return out
}

// expectedTargetViews is every view a migrated target must contain.
//
// It is short and it is written out rather than derived, because the check's
// whole purpose is to notice when the database disagrees with what this importer
// believes the schema is. A view missing from here would not be imported into
// (views hold no rows of their own), but a target whose views do not match is
// still a target from a different revision of the baseline.
var expectedTargetViews = []string{"v_live_shares"}

// checkTargetTables refuses a target that contains anything this importer does
// not know about.
//
// Two distinct accidents look identical from the row counts alone and both are
// caught here: pointing DATABASE_URL at another service's database, and running
// against a schema left behind by an older revision of the baseline (which
// would have the right table names and the wrong columns — the column check
// catches that, this one catches the rest).
func checkTargetTables(schema string, target *TargetSchema) error {
	var missing []string
	for _, want := range expectedTargetTables() {
		if _, ok := target.Tables[want]; !ok {
			missing = append(missing, want)
		}
	}
	for _, want := range expectedTargetViews {
		if !containsString(target.Views, want) {
			missing = append(missing, want+" (view)")
		}
	}

	names := make([]string, 0, len(target.Tables))
	for name := range target.Tables {
		names = append(names, name)
	}
	sort.Strings(names)

	var suffix string
	if schema != "public" {
		suffix = " (this pool's search_path resolves unqualified names to " + schema + ")"
	}
	if len(missing) > 0 {
		return fmt.Errorf("target schema %q is missing %s%s; run the application's `migrate` "+
			"(or open the pool through store.OpenPG) before importing",
			schema, strings.Join(missing, ", "), suffix)
	}
	for _, name := range names {
		if !containsString(expectedTargetTables(), name) {
			return fmt.Errorf("target schema %q contains unexpected table %q (found: %s)%s. "+
				"Refusing to import into a schema this importer does not recognise",
				schema, name, strings.Join(names, ", "), suffix)
		}
	}
	for _, name := range target.Views {
		if !containsString(expectedTargetViews, name) {
			return fmt.Errorf("target schema %q contains unexpected view %q (found: %s)%s. "+
				"Refusing to import into a schema this importer does not recognise",
				schema, name, strings.Join(target.Views, ", "), suffix)
		}
	}
	return nil
}

// checkColumnsMatch refuses a source/target pair whose column sets differ.
//
// Order is deliberately not compared: the import writes an explicit column list
// per INSERT, so order is irrelevant to correctness, and insisting on it would
// reject a harmless, legitimate divergence. The *set* is what matters — a target
// column the source cannot fill would take its DEFAULT and the row would look
// complete, and a source column the target lacks would be dropped. Both are
// silent data loss, so both are refusals.
func checkColumnsMatch(table string, source []Column, target []TargetColumn) error {
	sourceNames := make([]string, 0, len(source))
	for _, c := range source {
		sourceNames = append(sourceNames, c.Name)
	}
	targetNames := make([]string, 0, len(target))
	for _, c := range target {
		targetNames = append(targetNames, c.Name)
	}
	sort.Strings(sourceNames)
	sort.Strings(targetNames)
	if strings.Join(sourceNames, ",") == strings.Join(targetNames, ",") {
		// Names match, so also compare the TYPE CLASS. Names alone let a target
		// column be retyped (BIGINT -> TEXT) invisibly: convertValue would take the
		// text branch, store the same decimal digits, and db_hex_bytes would compare
		// equal — import and verify both green while the column is no longer a
		// number. The comparison is by affinity class, not by exact type, because
		// SQLite's declared types do not map one-to-one onto PostgreSQL's.
		targetByName := make(map[string]string, len(target))
		for _, c := range target {
			targetByName[c.Name] = pgTypeClass(c.DataType)
		}
		var mismatched []string
		for _, c := range source {
			want, got := sqliteAffinityClass(c.DeclType), targetByName[c.Name]
			// Skip when the source's declared type constrains nothing, or when the
			// target type is unknown.
			//
			// SQLite gives a column with no declared type BLOB affinity, and BLOB
			// affinity means "store whatever comes, do not coerce" — it can hold any
			// storage class, so comparing it against the target's class would be a
			// false refusal (a legacy `CREATE TABLE t(a)` mapped to a TEXT column is a
			// perfectly good import). What guards that case is the value-level refusal
			// in convertValue, which keys off the TARGET type.
			if want == "blob" || got == "" {
				continue
			}
			if want != got {
				mismatched = append(mismatched, fmt.Sprintf("%s (source %s/%q, target %s/%q)",
					c.Name, want, c.DeclType, got, targetByName[c.Name]))
			}
		}
		if len(mismatched) > 0 {
			return fmt.Errorf("table %s: column type classes differ: %s",
				table, strings.Join(mismatched, ", "))
		}
		return nil
	}
	return fmt.Errorf("table %s: the SQLite source and the PostgreSQL target do not have the "+
		"same columns.\n  source: %s\n  target: %s",
		table, strings.Join(sourceNames, ", "), strings.Join(targetNames, ", "))
}

// sqliteAffinityClass reduces a SQLite declared type to one of SQLite's own
// affinity classes (INTEGER, TEXT, BLOB, REAL, NUMERIC), using the documented rules
// rather than a guess: the declared type is the only thing SQLite offers.
func sqliteAffinityClass(declType string) string {
	upper := strings.ToUpper(declType)
	switch {
	case strings.Contains(upper, "INT"):
		return "integer"
	case strings.Contains(upper, "CHAR"), strings.Contains(upper, "CLOB"), strings.Contains(upper, "TEXT"):
		return "text"
	case upper == "" || strings.Contains(upper, "BLOB"):
		return "blob"
	case strings.Contains(upper, "REAL"), strings.Contains(upper, "FLOA"), strings.Contains(upper, "DOUB"):
		return "real"
	default:
		return "numeric"
	}
}

// pgTypeClass reduces a PostgreSQL data_type to the same vocabulary, so the two
// sides can be compared without pretending the type names are the same.
func pgTypeClass(dataType string) string {
	switch dataType {
	case "bigint", "integer", "smallint", "int8", "int4", "int2":
		return "integer"
	case "double precision", "real", "float8", "float4":
		return "real"
	case "numeric", "decimal":
		return "numeric"
	case "text", "character varying", "character", "varchar", "char":
		return "text"
	case "bytea":
		return "blob"
	default:
		return dataType
	}
}

// sourceColumnIndex maps column name -> position in a source row.
func sourceColumnIndex(table *Table) map[string]int {
	out := make(map[string]int, len(table.Columns))
	for i, c := range table.Columns {
		out[c.Name] = i
	}
	return out
}

// selectQuery builds the SELECT for one table's columns.
func selectQuery(table string, columns []TargetColumn) string {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = quoteIdent(c.Name)
	}
	return "SELECT " + strings.Join(names, ", ") + " FROM " + quoteIdent(table)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
