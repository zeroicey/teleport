package sqlitemigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/store"
)

// Check is one verification result.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// VerifyReport is the outcome of comparing a source with a target.
type VerifyReport struct {
	SourceSchema string  `json:"source_schema"`
	TargetSchema string  `json:"target_schema"`
	Checks       []Check `json:"checks"`
	Passed       int     `json:"passed"`
	Failed       int     `json:"failed"`
}

// OK reports whether every check passed.
func (r *VerifyReport) OK() bool { return r.Failed == 0 }

// Verifier compares an imported target against its source.
//
// It is deliberately a separate pass from Import rather than a by-product of it.
// The import's own reconciliation happens inside the transaction and can only
// see counts; this runs afterwards, reads the target through the *store's* own
// API (the code the service will use) as well as through raw SQL, and states the
// properties a human cares about in the language they asked in: five reports,
// two keys, permanent links still permanent, revoked links still 404.
type Verifier struct {
	src  Reader
	pool *pgxpool.Pool
	st   *store.Store
	rep  *VerifyReport
}

// NewVerifier wraps a source and a target pool.
//
// The pool is borrowed, not owned: Verify does not close it (and the
// *store.Store built here must not be Close()d either, because that would close
// the caller's pool).
func NewVerifier(src Reader, pool *pgxpool.Pool) *Verifier {
	return &Verifier{
		src:  src,
		pool: pool,
		st:   store.New(pool),
		rep:  &VerifyReport{},
	}
}

// Report returns what has been checked so far.
func (v *Verifier) Report() *VerifyReport { return v.rep }

// Verify runs every check and returns the report.
//
// It returns an error only for infrastructure failures (the source or the target
// cannot be read at all). A check that finds a discrepancy is recorded as a
// failure, not an error, so one run reports everything wrong rather than the
// first thing.
//
// It is a POST-IMPORT check, and that matters once the service is live: the row
// comparison includes the mutable usage counters (share_tokens.view_count,
// agent_keys.request_count/last_used_at), which a serving deployment increments on
// its own. Measured right after the P6 cut-over: view_count 7 -> 11 on a permanent
// link, purely because the link was fetched during verification. That is traffic,
// not drift. On a live database compare the immutable columns instead (or expect
// exactly these counters to differ).
func Verify(ctx context.Context, src Reader, pool *pgxpool.Pool) (*VerifyReport, error) {
	v := NewVerifier(src, pool)
	if err := v.run(ctx); err != nil {
		return nil, err
	}
	return v.rep, nil
}

func (v *Verifier) add(name string, ok bool, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	v.rep.Checks = append(v.rep.Checks, Check{Name: name, OK: ok, Detail: detail})
	if ok {
		v.rep.Passed++
	} else {
		v.rep.Failed++
	}
}

func (v *Verifier) run(ctx context.Context) error {
	versions, err := v.src.MigrationVersions(ctx)
	if err != nil {
		return err
	}
	if err := CheckSourceMigrations(versions); err != nil {
		// The source is not a database this mapping was written for. Every
		// check below would be comparing against the wrong thing, so record the
		// failure and stop rather than produce a long list of nonsense.
		v.add("source_migrations", false, "%v", err)
		return nil
	}
	v.rep.SourceSchema = strings.Join(versions, "+")

	tables, err := v.src.Tables(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]*Table, len(tables))
	for i := range tables {
		byName[tables[i].Name] = &tables[i]
	}

	target, err := inspectTarget(ctx, v.pool)
	if err != nil {
		return err
	}
	v.rep.TargetSchema = target.Schema

	if err := checkTargetTables(target.Schema, target); err != nil {
		v.add("target_schema", false, "%v", err)
		return nil
	}
	v.add("target_schema", true, "schema %q contains exactly %s",
		target.Schema, strings.Join(expectedTargetTables(), ", "))

	v.checkColumnSets(byName, target)
	v.checkRowCounts(ctx, byName)
	v.checkRowsEqual(ctx, byName, target)
	v.checkReportContent(byName)
	v.checkOwnershipBoundary(byName)
	v.checkDatabaseHexBytes(ctx, byName, target)
	v.checkShareSemantics(byName)
	v.checkAgentKeys(byName)
	return nil
}

// checkColumnSets requires every table's source and target column sets to match.
func (v *Verifier) checkColumnSets(source map[string]*Table, target *TargetSchema) {
	var bad []string
	for _, name := range Tables {
		if err := checkColumnsMatch(name, source[name].Columns, target.Tables[name]); err != nil {
			bad = append(bad, err.Error())
		}
	}
	v.add("column_sets", len(bad) == 0, "%s", strings.Join(bad, "; "))
}

// checkRowCounts compares the number of rows per table.
func (v *Verifier) checkRowCounts(ctx context.Context, source map[string]*Table) {
	var parts, bad []string
	for _, name := range Tables {
		var n int
		if err := v.pool.QueryRow(ctx, "SELECT count(*) FROM "+quoteIdent(name)).Scan(&n); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		want := len(source[name].Rows)
		parts = append(parts, fmt.Sprintf("%s=%d", name, want))
		if n != want {
			bad = append(bad, fmt.Sprintf("%s: source %d, target %d", name, want, n))
		}
	}
	v.add("row_counts", len(bad) == 0, "source and target: %s%s", strings.Join(parts, " "),
		prefixIf(len(bad) > 0, " — "+strings.Join(bad, "; ")))
}

// checkRowsEqual compares every column of every row, keyed by primary key.
//
// This is the check that makes "the data was moved" mean something stronger than
// "the right number of rows arrived": a value written to the wrong column, a
// timestamp rounded through a float, a text encoding change or a truncation all
// pass a count and all fail here.
func (v *Verifier) checkRowsEqual(ctx context.Context, source map[string]*Table, target *TargetSchema) {
	for _, name := range Tables {
		table := source[name]
		columns := target.Tables[name]
		problems, err := compareRows(ctx, v.pool, table, columns)
		if err != nil {
			v.add("row_equality["+name+"]", false, "%v", err)
			continue
		}
		// An empty source table makes this check vacuous, and the real data set
		// has one (key_renewals). Saying so in the detail line is the difference
		// between a green line that means "every row matched" and a green line
		// that means "there were no rows to match" — the distinction this
		// repository got wrong once already, when a check that could not fail
		// was counted as evidence.
		detail := fmt.Sprintf("%d row(s) compared", len(table.Rows))
		if len(table.Rows) == 0 {
			detail = "0 rows — the source table is empty, so this check compared nothing (the count check still requires both sides to be empty)"
		}
		v.add("row_equality["+name+"]", len(problems) == 0, "%s%s", detail,
			prefixIf(len(problems) > 0, " — "+strings.Join(problems, "; ")))
	}
}

// digestEntry is one row's identity and content digest.
type digestEntry struct {
	id     string
	digest string
}

// checkReportContent is the "5 reports" evidence: a digest over the bytes that
// actually matter, computed from the source's JSON and from the target through
// the store's own read path.
//
// The digest covers the report's identity and content, not the row layout: a
// report whose content survived but whose updated_at moved by a millisecond
// would already have failed row_equality, and folding every column in here would
// make this line fail for the same reason twice.
func (v *Verifier) checkReportContent(source map[string]*Table) {
	table := source["reports"]
	index := sourceColumnIndex(table)

	var sourceEntries, targetEntries []digestEntry
	for _, row := range table.Rows {
		id, err := asString(row[index["id"]])
		if err != nil {
			v.add("report_content", false, "source report id: %v", err)
			return
		}
		metadata, err := canonicalJSON(mustString(row[index["metadata"]]))
		if err != nil {
			v.add("report_content", false, "source report %s: %v", id, err)
			return
		}
		sourceEntries = append(sourceEntries, digestEntry{id: id, digest: reportDigest(
			id,
			mustString(row[index["title"]]),
			mustString(row[index["format"]]),
			mustString(row[index["content"]]),
			metadata,
		)})
	}

	// Read the target through the store, not through raw SQL: the store's read
	// path is what the service will use, so a decode/encode bug there shows up
	// here instead of only in production.
	for _, e := range sourceEntries {
		report, err := v.st.GetReport(e.id)
		if err != nil {
			v.add("report_content", false, "read target report %s: %v", e.id, err)
			return
		}
		if report == nil {
			v.add("report_content", false, "target has no report %s", e.id)
			return
		}
		targetEntries = append(targetEntries, digestEntry{id: e.id, digest: reportDigest(
			report.ID, report.Title, string(report.Format), report.Content, mustJSON(report.Metadata),
		)})
	}

	sortEntries(sourceEntries)
	sortEntries(targetEntries)
	sourceDigest := digestOfEntries(sourceEntries)
	targetDigest := digestOfEntries(targetEntries)

	ok := sourceDigest == targetDigest
	detail := fmt.Sprintf("%d report(s), content digest %s", len(sourceEntries), short(sourceDigest))
	if !ok {
		detail = fmt.Sprintf("source digest %s but target digest %s (ids: %s)",
			short(sourceDigest), short(targetDigest), strings.Join(idsOf(sourceEntries), ", "))
	}
	v.add("report_content", ok, "%s", detail)
}

// checkDatabaseHexBytes compares each row's values as the exact bytes each
// database holds, and is the one value check that never calls convertValue.
//
// That independence is the whole reason it exists. Every other value comparison
// here converts the source through the same function the import uses, so a bug in
// that function cancels itself out: the two sides agree, the check is green, and a
// wrong value is in the database. This is not hypothetical — a source value of
// 9223372036854775808 (above int64's maximum, so SQLite stores it with REAL
// affinity) reached int64(), wrapped to -9223372036854775808, was written to
// PostgreSQL, and passed every check in this file.
//
// Bytes, not text, because a text rendering would itself arrive through this
// tool's `sqlite3 -json` + encoding/json pipe: Go's JSON decoder replaces invalid
// UTF-8 with U+FFFD, so a substituted byte would appear on BOTH sides of the
// comparison and agree with itself. SQLite's hex() returns the bytes as stored,
// and PostgreSQL's convert_to(col::text,'UTF8') returns the bytes it stored, so a
// substituted byte shows up as a difference. (Measured on the real source: zero
// text cells contain invalid UTF-8, so this is a latent hole rather than a live
// one — which is exactly when it is cheap to close.)
//
// Only integer and text columns are compared, and REAL is excluded on purpose:
// SQLite's hex() of a REAL returns that REAL's TEXT bytes (measured:
// hex(0.1) = 302E31, i.e. "0.1", not the IEEE754 bytes 3fb999999999999a), and the
// two databases render whole numbers differently ("720.0" versus "720"), so
// comparing them would report a difference where there is none. The REAL guarantee
// is elsewhere: source.go reads a REAL column through SQLite's own shortest
// round-trip form (printf('%!.17g')) rather than as a JSON number, which keeps the
// value out of the JSON float path entirely.
func (v *Verifier) checkDatabaseHexBytes(ctx context.Context, source map[string]*Table, target *TargetSchema) {
	comparable := func(dataType string) bool {
		return containsString(intTypes, dataType) || containsString(textTypes, dataType)
	}

	for _, name := range Tables {
		var columns []string
		for _, c := range target.Tables[name] {
			if comparable(c.DataType) {
				columns = append(columns, c.Name)
			}
		}

		sourceHex, err := v.src.RenderedHex(ctx, name, columns)
		if err != nil {
			v.add("db_hex_bytes["+name+"]", false, "%v", err)
			continue
		}
		targetHex, err := v.renderedTargetBytes(ctx, name, columns)
		if err != nil {
			v.add("db_hex_bytes["+name+"]", false, "%v", err)
			continue
		}

		cells := len(columns) * len(source[name].Rows)
		problems := compareRenderedBytes(columns, sourceHex, targetHex)
		detail := fmt.Sprintf("%d cell(s) compared byte for byte", cells)
		if cells == 0 {
			detail = "0 cells — the source table is empty, so this check compared nothing"
		}
		v.add("db_hex_bytes["+name+"]", len(problems) == 0, "%s%s", detail,
			prefixIf(len(problems) > 0, " — "+strings.Join(problems, "; ")))
	}
}

// renderedTargetBytes renders the same columns with PostgreSQL's own byte
// conversion, so neither side of the comparison is produced by Go.
func (v *Verifier) renderedTargetBytes(ctx context.Context, table string, columns []string) (map[string][]RenderedValue, error) {
	key := PrimaryKey[table]
	exprs := make([]string, 0, len(columns)+1)
	exprs = append(exprs, quoteIdent(key)+"::text")
	for _, c := range columns {
		exprs = append(exprs, "lower(encode(convert_to("+quoteIdent(c)+"::text, 'UTF8'), 'hex'))")
	}
	rows, err := v.pool.Query(ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+quoteIdent(table))
	if err != nil {
		return nil, fmt.Errorf("render target %s as bytes: %w", table, err)
	}
	defer rows.Close()

	out := make(map[string][]RenderedValue)
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("read rendered target %s row: %w", table, err)
		}
		id, err := asString(values[0])
		if err != nil {
			return nil, fmt.Errorf("target %s %s: %w", table, key, err)
		}
		rendered := make([]RenderedValue, len(columns))
		for i := range columns {
			rendered[i] = renderedFromDBValue(values[i+1])
		}
		out[id] = rendered
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("render target %s as bytes: %w", table, err)
	}
	return out, nil
}

// renderedFromDBValue adapts a pgx value to a RenderedValue. Only string and NULL
// can appear, because every rendering expression is a hex encoding.
func renderedFromDBValue(v any) RenderedValue {
	switch value := v.(type) {
	case nil:
		return RenderedValue{Null: true}
	case string:
		return RenderedValue{Text: value}
	default:
		// Not reachable while the query hex-encodes; returning the Go rendering
		// keeps the comparison honest rather than silently equal.
		return RenderedValue{Text: fmt.Sprintf("%v", value)}
	}
}

// compareRenderedBytes is the pure comparison behind the check, so that it can be
// tested without two databases.
func compareRenderedBytes(columns []string, source, target map[string][]RenderedValue) []string {
	var problems []string
	for _, id := range sortedRenderedKeys(source) {
		want := source[id]
		got, ok := target[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("row=%s is missing from the target", id))
			continue
		}
		for i := range columns {
			if i >= len(want) || i >= len(got) {
				problems = append(problems, fmt.Sprintf("row=%s rendered a different number of columns", id))
				break
			}
			if renderedText(want[i]) != renderedText(got[i]) {
				problems = append(problems, fmt.Sprintf("row=%s column %s: source has %s, target has %s",
					id, columns[i], describeBytes(want[i]), describeBytes(got[i])))
			}
		}
	}
	for _, id := range sortedRenderedKeys(target) {
		if _, ok := source[id]; !ok {
			problems = append(problems, fmt.Sprintf("row=%s is in the target but not the source", id))
		}
	}
	return bound(problems, 5)
}

// renderedText folds NULL into the comparison so that NULL never equals the empty
// string.
func renderedText(v RenderedValue) string {
	if v.Null {
		return "\x00null"
	}
	return "\x00h" + v.Text
}

// describeBytes shows a hex rendering as hex, plus the text when the bytes are
// valid UTF-8 — a diff of two hex strings is exact but unreadable, and this
// message is what an operator has to act on.
func describeBytes(v RenderedValue) string {
	if v.Null {
		return "NULL"
	}
	shown := v.Text
	if len(shown) > 120 {
		shown = shown[:120] + "…"
	}
	decoded, err := hex.DecodeString(shown)
	if err == nil && utf8.Valid(decoded) && isMostlyPrintable(string(decoded)) {
		return strconv.Quote(string(decoded)) + " (hex " + shown + ")"
	}
	return "hex " + shown
}

// isMostlyPrintable guards describeBytes against turning a binary diff into a
// terminal-filling mess of control characters.
func isMostlyPrintable(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func sortedRenderedKeys(m map[string][]RenderedValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkOwnershipBoundary is the ADR's "关键边界" that no other check states in
// behavioural terms: a report whose owner_key_id is ” was published by the
// break-glass root key (or before keys existed), and it must stay readable by root
// and by the dashboard and by nobody else.
//
// row_equality already proves the column was copied. This asks the STORE what it
// believes afterwards, because that is where a change of meaning would actually
// bite: a root report whose owner_key_id had been filled in with somebody's key id
// would look perfectly equal in every count and would have silently changed hands.
func (v *Verifier) checkOwnershipBoundary(source map[string]*Table) {
	table := source["reports"]
	index := sourceColumnIndex(table)

	var rootReports, ownedReports []string
	var failures []string
	for _, row := range table.Rows {
		id := mustString(row[index["id"]])
		wantOwner := mustString(row[index["owner_key_id"]])
		if wantOwner == "" {
			rootReports = append(rootReports, id)
		} else {
			ownedReports = append(ownedReports, id)
		}

		owner, found, err := v.st.ReportOwner(id)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if !found {
			failures = append(failures, fmt.Sprintf("%s: the store cannot find the report", id))
			continue
		}
		if owner != wantOwner {
			if wantOwner == "" {
				failures = append(failures, fmt.Sprintf("%s: was published by the root key but now belongs to %s", id, owner))
			} else {
				failures = append(failures, fmt.Sprintf("%s: belonged to %s but now belongs to %q", id, wantOwner, owner))
			}
		}
	}

	v.add("ownership_boundary_unchanged", len(failures) == 0,
		"root(owner_key_id='')=%d %s, key-owned=%d%s",
		len(rootReports), list(rootReports), len(ownedReports),
		prefixIf(len(failures) > 0, " — "+strings.Join(failures, "; ")))
}

// checkShareSemantics is the human's checklist, stated as two properties.
//
// It asks the *store* — the same code the running service will use — what each
// link is now, and requires that to match the source row. Doing it through the
// store rather than by re-reading the columns is what makes it evidence about
// behaviour (404 vs 200) instead of evidence about storage.
//
// It also reads the token's stored row, because "resolves OK" does NOT imply
// "still permanent": a link whose expires_at had been moved to now+1h would
// resolve perfectly well and would silently stop being a permanent link. The
// property the human asked for is the stored expires_at, so that is asserted
// directly.
func (v *Verifier) checkShareSemantics(source map[string]*Table) {
	table := source["share_tokens"]
	index := sourceColumnIndex(table)
	now := time.Now().UnixMilli()

	// Each category gets its own token list and its own failure list, and a row
	// is classified BEFORE the target is read. Filing every target-side failure
	// in one bucket was the first version of this function, and it attributed a
	// missing *permanent* token to the revoked check — the same accident the
	// repository already recorded once, where a check reported a failure under a
	// name that did not describe what it had found.
	permanent := &shareCategory{}
	revoked := &shareCategory{}
	live := &shareCategory{}
	expired := &shareCategory{}
	categories := []*shareCategory{permanent, revoked, live, expired}
	failAll := func(msg string) {
		for _, c := range categories {
			c.failures = append(c.failures, msg)
		}
	}

	for _, row := range table.Rows {
		token := mustString(row[index["token"]])
		sourceExpires, err := anyToInt64(row[index["expires_at"]], "expires_at")
		if err != nil {
			failAll(fmt.Sprintf("%s: %v", token, err))
			continue
		}
		sourceActive, err := anyToInt64(row[index["is_active"]], "is_active")
		if err != nil {
			failAll(fmt.Sprintf("%s: %v", token, err))
			continue
		}

		var group *shareCategory
		switch {
		case sourceActive == 0:
			group = revoked
		case sourceExpires == 0:
			group = permanent
		case sourceExpires <= now:
			group = expired
		default:
			group = live
		}
		group.tokens = append(group.tokens, token)
		fail := func(format string, args ...any) {
			group.failures = append(group.failures, fmt.Sprintf("%s: %s", token, fmt.Sprintf(format, args...)))
		}

		stored, err := v.st.GetShareToken(token)
		if err != nil {
			fail("read target token: %v", err)
			continue
		}
		if stored == nil {
			fail("missing from the target")
			continue
		}
		res, err := v.st.ResolveShare(token)
		if err != nil {
			fail("resolve: %v", err)
			continue
		}

		// The stored expires_at is compared in every category, not just the
		// permanent one: it is the column the contract is written in, and a
		// changed expiry is a changed promise even when the current status
		// happens to be the same.
		if stored.ExpiresAt != sourceExpires {
			fail("expires_at %d in the source, %d in the target", sourceExpires, stored.ExpiresAt)
		}

		switch group {
		case revoked:
			if stored.IsActive {
				fail("revoked in the source but active in the target")
			}
			if res.Status != domain.StatusMissing {
				fail("revoked in the source (is_active=0) but resolves as %s", statusName(res.Status))
			}
		case permanent:
			if !stored.IsActive {
				fail("permanent in the source but inactive in the target")
			}
			if res.Status != domain.StatusOK {
				fail("expires_at=0 (never expires) but resolves as %s", statusName(res.Status))
			}
		case expired:
			if !stored.IsActive {
				fail("expired rather than revoked in the source, but inactive in the target")
			}
			if res.Status != domain.StatusGone {
				fail("expired at %d but resolves as %s", sourceExpires, statusName(res.Status))
			}
		default:
			if !stored.IsActive {
				fail("live in the source but inactive in the target")
			}
			if res.Status != domain.StatusOK {
				fail("live until %d but resolves as %s", sourceExpires, statusName(res.Status))
			}
		}
	}

	classified := len(permanent.tokens) + len(revoked.tokens) + len(live.tokens) + len(expired.tokens)
	if classified != len(table.Rows) {
		// Every source row must land in exactly one category. If a future edit
		// to the classification stops covering a case, the four checks below
		// would each pass while silently ignoring a link.
		failAll(fmt.Sprintf("only %d of %d source share tokens were classified", classified, len(table.Rows)))
	}

	inventory := fmt.Sprintf("permanent(expires_at=0)=%d %s, revoked(is_active=0)=%d %s, live=%d, expired=%d",
		len(permanent.tokens), list(permanent.tokens), len(revoked.tokens), list(revoked.tokens),
		len(live.tokens), len(expired.tokens))

	for _, check := range []struct {
		name  string
		group *shareCategory
	}{
		{"permanent_links_stay_permanent", permanent},
		{"revoked_links_stay_404", revoked},
		{"live_links_still_resolve", live},
		{"expired_links_stay_410", expired},
	} {
		detail := inventory
		if len(check.group.tokens) == 0 {
			// Say it out loud. With no token in this category the check compared
			// nothing, and a green line must not read as "every link of this kind was
			// verified". (Measured on the real source: expired=0 when P5 ran, and the
			// category only became non-empty once a live link actually expired.)
			detail += " — this check compared nothing: the source has no token in that category"
		}
		if len(check.group.failures) > 0 {
			detail += " — " + strings.Join(check.group.failures, "; ")
		}
		v.add(check.name, len(check.group.failures) == 0, "%s", detail)
	}
}

// shareCategory is one class of share token and what went wrong with it.
type shareCategory struct {
	tokens   []string
	failures []string
}

// checkAgentKeys is the "2 keys" evidence, through the store's auth path.
//
// A key's plaintext token is not in the database (only its sha256 is), so the
// strongest available statement is that resolving by hash yields the same key —
// which is exactly what the auth middleware does on every agent request.
func (v *Verifier) checkAgentKeys(source map[string]*Table) {
	table := source["agent_keys"]
	index := sourceColumnIndex(table)
	now := time.Now().UnixMilli()

	var usable, unusable []string
	var failures []string
	for _, row := range table.Rows {
		id := mustString(row[index["id"]])
		hash := mustString(row[index["token_hash"]])
		expiresAt, err := anyToInt64(row[index["expires_at"]], "expires_at")
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		revokedAt, err := anyToInt64(row[index["revoked_at"]], "revoked_at")
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		expectUsable := revokedAt == 0 && (expiresAt == 0 || expiresAt > now)

		key, err := v.st.ResolveAgentKey(hash, now)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: resolve: %v", id, err))
			continue
		}
		if !expectUsable {
			unusable = append(unusable, id)
			if key != nil {
				failures = append(failures, fmt.Sprintf("%s: revoked_at=%d expires_at=%d but still authenticates", id, revokedAt, expiresAt))
				continue
			}
			// "Does not authenticate" must not be confused with "is not in the target".
			// A DELETED revoked key would otherwise be counted as merely unusable and
			// this check would stay green (only row_counts/row_equality would notice).
			stored, err := v.st.GetAgentKey(id)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: read target key: %v", id, err))
				continue
			}
			if stored == nil {
				failures = append(failures, fmt.Sprintf("%s: missing from the target", id))
				continue
			}
			if stored.RevokedAt != revokedAt || stored.ExpiresAt != expiresAt {
				failures = append(failures, fmt.Sprintf("%s: revoked_at/expires_at is %d/%d in the source but %d/%d in the target",
					id, revokedAt, expiresAt, stored.RevokedAt, stored.ExpiresAt))
			}
			continue
		}
		usable = append(usable, id)
		switch {
		case key == nil:
			failures = append(failures, fmt.Sprintf("%s: live in the source but does not authenticate", id))
		case key.ID != id:
			failures = append(failures, fmt.Sprintf("%s: hash resolves to a different key %s", id, key.ID))
		case key.ExpiresAt != expiresAt:
			failures = append(failures, fmt.Sprintf("%s: expires_at %d in the source, %d in the target", id, expiresAt, key.ExpiresAt))
		}
	}
	v.add("agent_keys_still_authenticate", len(failures) == 0,
		"usable=%d %s, unusable=%d %s%s", len(usable), list(usable), len(unusable), list(unusable),
		prefixIf(len(failures) > 0, " — "+strings.Join(failures, "; ")))
}

// compareRows compares every row of one table, keyed by primary key, and returns
// a bounded list of human-readable differences.
func compareRows(ctx context.Context, q querier, source *Table, columns []TargetColumn) ([]string, error) {
	index := sourceColumnIndex(source)
	key := PrimaryKey[source.Name]

	sourceRows := make(map[string]string, len(source.Rows))
	for rowNum, row := range source.Rows {
		parts := make([]string, len(columns))
		for i, c := range columns {
			pos, ok := index[c.Name]
			if !ok {
				// A target column the source does not have must be an ERROR, not a
				// silent read of column 0: sourceColumnIndex returns the zero value for
				// a missing key, so the id cell would be compared against this column
				// and the difference would be reported against the wrong column.
				return nil, fmt.Errorf("table %s has no source column %s", source.Name, c.Name)
			}
			value, err := convertValue(row[pos], c)
			if err != nil {
				return nil, fmt.Errorf("row %d column %s: %w", rowNum, c.Name, err)
			}
			parts[i] = canonicalValue(value)
		}
		id, err := asString(row[index[key]])
		if err != nil {
			return nil, err
		}
		if _, dup := sourceRows[id]; dup {
			return nil, fmt.Errorf("source has two rows with %s=%s", key, id)
		}
		sourceRows[id] = joinCanonicalRow(parts)
	}

	rows, err := q.Query(ctx, selectQuery(source.Name, columns))
	if err != nil {
		return nil, fmt.Errorf("read target %s: %w", source.Name, err)
	}
	defer rows.Close()

	keyPos := -1
	for i, c := range columns {
		if c.Name == key {
			keyPos = i
		}
	}
	if keyPos < 0 {
		return nil, fmt.Errorf("target %s has no %s column", source.Name, key)
	}

	targetRows := make(map[string]string, len(source.Rows))
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("read target %s row: %w", source.Name, err)
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = canonicalValue(value)
		}
		id, err := asString(values[keyPos])
		if err != nil {
			return nil, fmt.Errorf("target %s %s: %w", source.Name, key, err)
		}
		targetRows[id] = joinCanonicalRow(parts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read target %s: %w", source.Name, err)
	}

	var problems []string
	for _, id := range sortedKeys(sourceRows) {
		targetRow, ok := targetRows[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s=%s is missing from the target", key, id))
			continue
		}
		if targetRow != sourceRows[id] {
			problems = append(problems, fmt.Sprintf("%s=%s differs at %s",
				key, id, firstDifference(columns, sourceRows[id], targetRow)))
		}
	}
	for _, id := range sortedKeys(targetRows) {
		if _, ok := sourceRows[id]; !ok {
			problems = append(problems, fmt.Sprintf("%s=%s is in the target but not the source", key, id))
		}
	}
	return bound(problems, 5), nil
}

// joinCanonicalRow concatenates a row's canonical cells, length-prefixing each one.
//
// The first version joined them with a separator byte and claimed "a control
// character cannot appear in any of these columns' values". That claim is false:
// reports.content, reports.title, key_applications.purpose and agent_keys.note
// hold arbitrary agent- and human-supplied text, and a value containing the
// separator followed by another cell's type tag could shift a cell boundary so
// that two DIFFERENT rows produce the same string and compare equal. A NUL byte in
// the tag makes that hard to hit accidentally, and "hard to hit" is not a property
// worth relying on in the one check that decides whether the data moved correctly.
//
// A length prefix cannot be forged: the parse consumes a known number of bytes, so
// a boundary can only fall where the writer put it. It also lets
// firstDifference name the right column, which the separator form could not.
func joinCanonicalRow(parts []string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.String()
}

// splitCanonicalRow is the inverse of joinCanonicalRow, for diagnostics only. A
// malformed tail is returned as-is rather than dropped, so a reporting bug cannot
// silently hide a cell.
func splitCanonicalRow(s string) []string {
	var out []string
	for len(s) > 0 {
		colon := strings.IndexByte(s, ':')
		if colon < 0 {
			return append(out, s)
		}
		n, err := strconv.Atoi(s[:colon])
		if err != nil || n < 0 || colon+1+n > len(s) {
			return append(out, s)
		}
		out = append(out, s[colon+1:colon+1+n])
		s = s[colon+1+n:]
	}
	return out
}

// canonicalValue renders a value so that two databases' representations of the
// same value compare equal.
//
// The width differences are the point: PostgreSQL hands back int4 as int32 and
// int8 as int64, while the source side converted both to int64, so comparing Go
// types would report a difference where the values agree.
func canonicalValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "\x00null"
	case string:
		return "\x00s" + value
	case []byte:
		return "\x00b" + hex.EncodeToString(value)
	case bool:
		return "\x00t" + strconv.FormatBool(value)
	case int64:
		return "\x00i" + strconv.FormatInt(value, 10)
	case int32:
		return "\x00i" + strconv.FormatInt(int64(value), 10)
	case int:
		return "\x00i" + strconv.FormatInt(int64(value), 10)
	case int16:
		return "\x00i" + strconv.FormatInt(int64(value), 10)
	case float64:
		return "\x00f" + strconv.FormatFloat(value, 'f', -1, 64)
	case float32:
		return "\x00f" + strconv.FormatFloat(float64(value), 'f', -1, 64)
	default:
		return fmt.Sprintf("\x00%T:%v", v, v)
	}
}

// firstDifference names the first column whose canonical values disagree.
func firstDifference(columns []TargetColumn, a, b string) string {
	left := splitCanonicalRow(a)
	right := splitCanonicalRow(b)
	for i := range columns {
		if i >= len(left) || i >= len(right) {
			return "a different number of columns"
		}
		if left[i] != right[i] {
			return fmt.Sprintf("%s (%s vs %s)", columns[i].Name, display(left[i]), display(right[i]))
		}
	}
	return "no column (the cells differ only in how they were delimited, which is a bug in this check)"
}

// display strips the type tag and shortens a value for a one-line message.
//
// The tag is what makes int64(1), int32(1) and the string "1" comparable at
// all; it is noise in a message a human reads, so it is removed here rather than
// kept in the canonical form.
func display(v string) string {
	plain := v
	if len(plain) >= 2 && plain[0] == 0 {
		plain = plain[2:]
	}
	if len(plain) > 60 {
		return strconv.Quote(plain[:60] + "…")
	}
	return strconv.Quote(plain)
}

// reportDigest is the digest of one report's identity and content.
func reportDigest(id, title, format, content, metadata string) string {
	h := sha256.New()
	for _, part := range []string{id, title, format, content, metadata} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// digestOfEntries digests a set of rows keyed by identity.
func digestOfEntries(entries []digestEntry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s=%s\n", e.id, e.digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func idsOf(entries []digestEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.id)
	}
	return out
}

func sortEntries(entries []digestEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
}

// mustJSON renders a decoded JSON value back to text, so that the digest
// compares content rather than key order.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<!unencodable: %v>", err)
	}
	return string(b)
}

// canonicalJSON re-encodes a JSON text the way the store's decodeMetadata reads
// it, so that two spellings of the same object compare equal.
//
// The two sides arrive in different shapes: the source hands back the exact bytes
// the agent sent — key order and all — while the store hands back a
// map[string]any, which Go re-encodes in its own key order. Comparing those
// spellings directly would fail for any report whose metadata keys were not
// already alphabetical. It passed on the real data set only because every metadata
// object in it happens to be sorted, which is luck, not a property.
//
// It also has to degrade exactly as the store does. decodeMetadata unmarshals into
// map[string]any and yields {} for anything that is not an object, so a hand-edited
// or legacy row holding '5' or '[1,2]' reads back as {} — both schema CHECKs accept
// those values, because both only require *valid* JSON. A digest that instead
// preserved '5' would report a metadata change for a byte-perfect copy and print a
// message saying the content differs. row_equality still compares the raw column
// bytes, so this only removes a false alarm; it cannot hide a real change.
func canonicalJSON(raw string) (string, error) {
	var value map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &value); err != nil || value == nil {
		return "{}", nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("metadata cannot be re-encoded: %w", err)
	}
	return string(b), nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func bound(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(items[:n], fmt.Sprintf("… and %d more", len(items)-n))
}

func list(items []string) string { return "[" + strings.Join(items, " ") + "]" }

func short(digest string) string {
	if len(digest) <= 16 {
		return digest
	}
	return digest[:16] + "…"
}

func prefixIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func statusName(s domain.ResolutionStatus) string {
	switch s {
	case domain.StatusOK:
		return "ok(200)"
	case domain.StatusMissing:
		return "missing(404)"
	case domain.StatusGone:
		return "gone(410)"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// asString requires a value to be a string, for columns this importer knows are
// TEXT.
func asString(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected text, got %T (%v)", v, v)
	}
	return s, nil
}

// mustString is asString for callers that have already established the column is
// text and would only be able to abort with an error message.
func mustString(v any) string {
	s, _ := asString(v)
	return s
}
