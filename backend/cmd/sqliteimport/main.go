// Command sqliteimport moves the pre-rewrite SQLite data set into PostgreSQL,
// and verifies the result against its source.
//
// It is a separate command rather than a subcommand of `teleport` on purpose.
// It reads SQLite through the `sqlite3` command-line shell, which is a migration
// dependency and not a runtime one: the server binary should not grow an SQLite
// client (or a second database driver) for a tool that runs once, on the day of
// the cut-over.
//
// Usage:
//
//	sqliteimport import -source teleport.db -dsn postgres://...
//	sqliteimport verify -source teleport.db -dsn postgres://...
//
// The schema is built by the application's own migrator (the same code path
// `teleport migrate` uses), never by this tool. See
// .ai/decisions/2026-10-17-p5-schema-ownership.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/sqlitemigrate"
	"github.com/zeroicey/teleport/backend/internal/store"
)

const usage = `sqliteimport — move SQLite data into the PostgreSQL schema, and check it.

  sqliteimport import -source FILE [-dsn DSN] [-replace [-yes]] [-max-conns N] [-json]
  sqliteimport verify -source FILE [-dsn DSN] [-max-conns N] [-json]

  import   apply the embedded migrations to the target, insert every row in one
           transaction, and reconcile the row counts before committing.
           A non-empty target is refused unless -replace is passed; -replace
           itself only truncates when -yes is also passed, so the destructive
           step cannot happen as a side effect of a typo.
  verify   read-only. Compare every row, every count, the report digests and the
           link semantics (permanent stays permanent, revoked stays 404) between
           the source and the target.
           This is a post-import check. Once the service is live, the usage
           counters (view_count, request_count, last_used_at) legitimately differ
           from the frozen source, because serving traffic increments them; those
           differences are not drift.

  -dsn defaults to $DATABASE_URL. The source is opened read-only and immutable;
  -source is never written to, and a -wal sidecar next to it is a refusal.

Exit codes: 0 ok, 1 error or a failed check, 2 usage, 3 -replace was given
without -yes (nothing was written).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Cancel the import on SIGINT/SIGTERM. The transaction then rolls back and
	// the target is left exactly as it was found.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "import":
		os.Exit(runImport(ctx, os.Args[2:]))
	case "verify":
		os.Exit(runVerify(ctx, os.Args[2:]))
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runImport(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	var (
		source   = fs.String("source", "", "path to the SQLite database (required)")
		dsn      = fs.String("dsn", "", "PostgreSQL DSN (defaults to $DATABASE_URL)")
		replace  = fs.Bool("replace", false, "truncate the five application tables before inserting (destructive)")
		yes      = fs.Bool("yes", false, "confirm -replace; without it, -replace only reports what it would delete")
		maxConns = fs.Int("max-conns", 4, "connection pool size (the PostgreSQL instance is shared)")
		asJSON   = fs.Bool("json", false, "print the report as JSON")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *source == "" {
		fmt.Fprintln(os.Stderr, "error: -source is required")
		return 2
	}
	target, err := resolveDSN(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	src, err := sqlitemigrate.OpenSource(ctx, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	sum, err := src.SHA256()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !*asJSON {
		fmt.Printf("source  %s\n        sha256 %s\n", src.Path(), sum)
	}

	// The destructive path needs both flags. With -replace alone the tool
	// reports what it would destroy and stops, which turns "I mistyped the
	// database name" from an incident into a log line.
	if *replace && !*yes {
		pool, err := sqlitemigrate.OpenTargetPool(ctx, target, int32(*maxConns))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		defer pool.Close()
		if err := reportWhatReplaceWouldDelete(ctx, pool); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("\n-replace was given without -yes: nothing was written.")
		return 3
	}

	// store.OpenPG applies the embedded migrations, so the schema is always the
	// binary's own and never a hand-made approximation of it.
	pool, err := store.OpenPG(ctx, target, int32(*maxConns))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer pool.Close()

	report, err := sqlitemigrate.Import(ctx, src, pool, *replace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if *asJSON {
		if err := printJSON(struct {
			SourceFile   string `json:"source_file"`
			SourceSHA256 string `json:"source_sha256"`
			*sqlitemigrate.Report
		}{SourceFile: src.Path(), SourceSHA256: sum, Report: report}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	fmt.Printf("target  %s (schema %s)\n", describeDSN(target), report.TargetSchema)
	fmt.Printf("        source schema %s\n\n", report.SourceSchema)
	total := 0
	fmt.Printf("%-18s %8s %8s %8s\n", "table", "source", "inserted", "target")
	for _, t := range report.Tables {
		fmt.Printf("%-18s %8d %8d %8d\n", t.Table, t.SourceRows, t.Inserted, t.TargetRows)
		total += t.Inserted
	}
	fmt.Printf("\nimport committed: %d row(s) in one transaction\n", total)

	// The hash above was taken before the rows were read, and Import reads the
	// file again. On a frozen local copy nothing can change in between, but "the
	// printed digest describes the imported bytes" is a claim this tool makes, so
	// it verifies it rather than assuming it. The import has already committed at
	// this point, which is why this is a loud, non-zero error and not a rollback:
	// the operator needs to know the digest and the data may not correspond.
	after, err := src.SHA256()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if after != sum {
		fmt.Fprintf(os.Stderr, "error: the source changed while it was being imported: it hashed to "+
			"%s before and %s after. The rows just committed may not correspond to either digest. "+
			"Re-import with -replace -yes once the file is stable.\n", sum, after)
		return 1
	}
	return 0
}

func runVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var (
		source   = fs.String("source", "", "path to the SQLite database (required)")
		dsn      = fs.String("dsn", "", "PostgreSQL DSN (defaults to $DATABASE_URL)")
		maxConns = fs.Int("max-conns", 4, "connection pool size (the PostgreSQL instance is shared)")
		asJSON   = fs.Bool("json", false, "print the report as JSON")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *source == "" {
		fmt.Fprintln(os.Stderr, "error: -source is required")
		return 2
	}
	target, err := resolveDSN(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	src, err := sqlitemigrate.OpenSource(ctx, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	sum, err := src.SHA256()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	// Verify never migrates: it must be able to answer "is what is there
	// correct?" without first making something true.
	pool, err := sqlitemigrate.OpenTargetPool(ctx, target, int32(*maxConns))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer pool.Close()

	report, err := sqlitemigrate.Verify(ctx, src, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if *asJSON {
		if err := printJSON(struct {
			SourceFile   string `json:"source_file"`
			SourceSHA256 string `json:"source_sha256"`
			*sqlitemigrate.VerifyReport
		}{SourceFile: src.Path(), SourceSHA256: sum, VerifyReport: report}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	} else {
		fmt.Printf("source  %s (sha256 %s, schema %s)\n", src.Path(), shortSHA(sum), report.SourceSchema)
		fmt.Printf("target  %s (schema %s)\n\n", describeDSN(target), report.TargetSchema)
		for _, c := range report.Checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Printf("  %s %-32s %s\n", mark, c.Name, c.Detail)
		}
		fmt.Printf("\n%d passed, %d failed\n", report.Passed, report.Failed)
	}

	if !report.OK() {
		return 1
	}
	return 0
}

// reportWhatReplaceWouldDelete prints the rows -replace would destroy.
func reportWhatReplaceWouldDelete(ctx context.Context, pool *pgxpool.Pool) error {
	fmt.Println("-replace was given without -yes. This is what it WOULD delete:")
	tables := append([]string{}, sqlitemigrate.Tables...)
	for _, table := range tables {
		var n int
		// The schema may not exist yet; that is not a reason to fail before
		// printing the rest of the picture.
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			fmt.Printf("  %-18s (not readable: %v)\n", table, err)
			continue
		}
		var sample string
		if n > 0 {
			// The key column differs per table (share_tokens is keyed by `token`), so
			// it comes from the same map the verifier uses rather than being
			// assumed to be `id`.
			key := sqlitemigrate.PrimaryKey[table]
			// For share_tokens the primary key IS the bearer credential: printing it in
			// full would put working links into the terminal and into whatever log the
			// operator keeps. A prefix is enough to recognise which links would go.
			expr := key + "::text"
			if table == "share_tokens" {
				expr = "left(" + key + "::text, 8) || '…'"
			}
			rows, err := pool.Query(ctx, `SELECT `+expr+` FROM `+table+` ORDER BY `+key+` LIMIT 10`)
			if err == nil {
				var ids []string
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err == nil {
						ids = append(ids, id)
					}
				}
				rows.Close()
				sample = " " + strings.Join(ids, ", ")
			}
		}
		fmt.Printf("  %-18s %d row(s)%s\n", table, n, sample)
	}
	fmt.Println("\nRe-run with -replace -yes to actually truncate them.")
	return nil
}

// resolveDSN prefers the flag and falls back to DATABASE_URL.
func resolveDSN(dsn string) (string, error) {
	if strings.TrimSpace(dsn) != "" {
		return dsn, nil
	}
	if env := strings.TrimSpace(os.Getenv("DATABASE_URL")); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("no target database: pass -dsn or set DATABASE_URL")
}

// describeDSN renders a DSN for a log without its password.
func describeDSN(dsn string) string {
	// The DSN carries a password and this string is printed. Cutting at the
	// first '@' would still leak a password that contains one, so the only safe
	// rendering is to drop the userinfo half entirely.
	if at := strings.LastIndex(dsn, "@"); at >= 0 {
		scheme := dsn
		if i := strings.Index(dsn, "://"); i >= 0 {
			scheme = dsn[:i+3]
		}
		return scheme + "***" + dsn[at:]
	}
	// A keyword/value DSN ("host=… user=… password=…") has no '@', and returning it
	// unchanged printed the password — the first version did exactly that, despite
	// this function's own promise. Redact the two credential-bearing keys and keep
	// the rest, which is what an operator needs to see.
	if !strings.Contains(dsn, "=") {
		return dsn
	}
	fields := strings.Fields(dsn)
	for i, f := range fields {
		eq := strings.Index(f, "=")
		if eq < 0 {
			continue
		}
		switch strings.ToLower(f[:eq]) {
		case "password", "passfile":
			fields[i] = f[:eq+1] + "***"
		}
	}
	return strings.Join(fields, " ")
}

func shortSHA(sum string) string {
	if len(sum) <= 16 {
		return sum
	}
	return sum[:16] + "…"
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
