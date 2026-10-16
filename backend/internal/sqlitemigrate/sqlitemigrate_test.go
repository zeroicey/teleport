package sqlitemigrate

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the parts of the importer that decide *what* gets written —
// the migration gate, the column matching and the value conversion — without a
// database and without the sqlite3 binary. The parts that actually move rows are
// covered by import_pg_test.go, which needs a real target, and by
// scripts/verify-p5.sh, which runs against the real 5-report source.

// fakeReader is a source that returns fixed tables.
type fakeReader struct {
	versions    []string
	tables      []Table
	versionsErr error
	tablesErr   error
}

func (f *fakeReader) MigrationVersions(context.Context) ([]string, error) {
	return f.versions, f.versionsErr
}

func (f *fakeReader) Tables(context.Context) ([]Table, error) {
	return f.tables, f.tablesErr
}

// RenderedHex is part of the Reader interface so that the verifier can compare
// the bytes each database holds. The pure tests here never reach it; the
// integration tests use the real *Source.
func (f *fakeReader) RenderedHex(context.Context, string, []string) (map[string][]RenderedValue, error) {
	return nil, errors.New("fakeReader has no database to render with")
}

func TestCheckSourceMigrations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions []string
		wantErr  string
	}{
		{"the pair this mapping was written for", []string{"0001_init", "0002_agent_keys"}, ""},
		{"recorded in the other order", []string{"0002_agent_keys", "0001_init"}, ""},
		{"missing the agent-keys migration", []string{"0001_init"}, "missing migration"},
		{"an unknown later migration", []string{"0001_init", "0002_agent_keys", "0003_more_columns"}, "unknown migration"},
		{"an unrelated database", []string{"0001_something_else"}, "missing migration"},
		{"no migrations at all", nil, "missing migration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSourceMigrations(tc.versions)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckSourceMigrations(%v) = %v, want nil", tc.versions, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckSourceMigrations(%v) = nil, want an error containing %q", tc.versions, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestImportRefusesAnUnrecognisedSource is the gate that runs before the target
// is ever touched: a source whose recorded migrations are not the pair this
// mapping was written for must be refused, and the refusal must happen before
// any connection is asked for data. The nil querier is the assertion — if the
// code reached the target, this test would panic instead of failing cleanly.
func TestImportRefusesAnUnrecognisedSource(t *testing.T) {
	src := &fakeReader{versions: []string{"0001_init", "0002_agent_keys", "0003_surprise"}}
	_, err := Import(context.Background(), src, nil, false)
	if err == nil {
		t.Fatal("Import accepted a source with an unknown migration")
	}
	if !strings.Contains(err.Error(), "unknown migration") {
		t.Errorf("error %q does not name the problem", err)
	}
}

func TestConvertValue(t *testing.T) {
	// The exact value that makes this function worth having: a millisecond
	// timestamp is ~1.79e12, which does not fit in float32, does not fit in
	// int32, and is not exactly representable as a float64 either — so any path
	// through a float changes it.
	const millis = int64(1791563326606)

	for _, tc := range []struct {
		name    string
		value   any
		column  TargetColumn
		want    any
		wantErr string
	}{
		{"nil stays nil", nil, TargetColumn{Name: "note", DataType: "text"}, nil, ""},
		{"json number to bigint", json.Number("1791563326606"), TargetColumn{Name: "created_at", DataType: "bigint"}, millis, ""},
		{"json number to integer", json.Number("1"), TargetColumn{Name: "is_active", DataType: "integer"}, int64(1), ""},
		{"json number to double precision", json.Number("720.0"), TargetColumn{Name: "requested_hours", DataType: "double precision"}, float64(720), ""},
		{"integral float into an integer column", json.Number("24.0"), TargetColumn{Name: "requested_hours", DataType: "bigint"}, int64(24), ""},
		{"text", "hello", TargetColumn{Name: "title", DataType: "text"}, "hello", ""},
		{"text keeps its bytes", "line1\nline2\t\"quoted\"", TargetColumn{Name: "content", DataType: "text"}, "line1\nline2\t\"quoted\"", ""},
		{"number in a text column keeps its decimal text", json.Number("720.0"), TargetColumn{Name: "note", DataType: "text"}, "720.0", ""},
		{"integer in a text column", json.Number("28"), TargetColumn{Name: "note", DataType: "varchar"}, "28", ""},
		{"boolean", json.Number("1"), TargetColumn{Name: "flag", DataType: "boolean"}, true, ""},
		// The refusals. Each one is a value the schema does not allow, and
		// silently producing something plausible is the failure mode being
		// prevented.
		{"fraction into an integer column", json.Number("720.5"), TargetColumn{Name: "hours", DataType: "bigint"}, nil, "fractional"},
		{"text into an integer column", json.Number("abc"), TargetColumn{Name: "n", DataType: "bigint"}, nil, "cannot convert"},
		{"a decimal string into an integer column", "720.5", TargetColumn{Name: "n", DataType: "bigint"}, nil, "cannot parse"},
		{"an unknown target type", json.Number("1"), TargetColumn{Name: "at", DataType: "timestamp with time zone"}, nil, "does not know how to convert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertValue(tc.value, tc.column)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("convertValue(%v, %s) = %v, want an error", tc.value, tc.column.DataType, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertValue(%v, %s): %v", tc.value, tc.column.DataType, err)
			}
			if got != tc.want {
				t.Errorf("convertValue(%v, %s) = %#v, want %#v", tc.value, tc.column.DataType, got, tc.want)
			}
		})
	}
}

// TestCanonicalValueIgnoresIntegerWidth is the property the whole row comparison
// rests on: PostgreSQL returns int4 as int32 and int8 as int64, while the source
// side converts both to int64, so a comparison that looked at Go types would
// report a difference wherever the values agree.
func TestCanonicalValueIgnoresIntegerWidth(t *testing.T) {
	if canonicalValue(int64(1)) != canonicalValue(int32(1)) {
		t.Error("int64(1) and int32(1) must compare equal")
	}
	if canonicalValue(int64(720)) != canonicalValue(int(720)) {
		t.Error("int64(720) and int(720) must compare equal")
	}
	if canonicalValue(int64(0)) == canonicalValue(int64(1)) {
		t.Error("0 and 1 must not compare equal")
	}
	// A float column's whole-number value must match the same value read back as
	// a float64, and must not be confused with an integer's rendering.
	if canonicalValue(float64(720)) != canonicalValue(float64(720.0)) {
		t.Error("720.0 must compare equal to itself")
	}
	if canonicalValue(float64(720)) == canonicalValue(int64(720)) {
		t.Error("a float and an integer must not compare equal: the type tag is the only thing " +
			"keeping a value written to the wrong column from looking right")
	}
	// The string and float renderings of the same digits must differ too.
	if canonicalValue("720") == canonicalValue(float64(720)) {
		t.Error(`the string "720" and the number 720 must not compare equal`)
	}
}

func TestCheckColumnsMatch(t *testing.T) {
	source := []Column{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	for _, tc := range []struct {
		name    string
		target  []TargetColumn
		wantErr bool
	}{
		{"same set", []TargetColumn{{Name: "a"}, {Name: "b"}, {Name: "c"}}, false},
		// Order is not part of the contract: each INSERT names its columns, so a
		// target that stores them in a different order is still imported
		// correctly.
		{"same set, different order", []TargetColumn{{Name: "c"}, {Name: "a"}, {Name: "b"}}, false},
		{"target has an extra column", []TargetColumn{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}, true},
		{"target is missing a column", []TargetColumn{{Name: "a"}, {Name: "b"}}, true},
		{"a renamed column", []TargetColumn{{Name: "a"}, {Name: "b"}, {Name: "cc"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkColumnsMatch("t", source, tc.target)
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"key order is normalised", `{"b":2,"a":1}`, `{"a":1,"b":2}`},
		{"already sorted", `{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{"an empty object", `{}`, `{}`},
		{"an empty column is an empty object, as the store's decode does", ``, `{}`},
		{"nested objects are normalised too", `{"z":{"b":1,"a":2}}`, `{"z":{"a":2,"b":1}}`},
		{"the escape the real data contains", `{"scope":"a \u0026 b"}`, `{"scope":"a \u0026 b"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalJSON(tc.raw)
			if err != nil {
				t.Fatalf("canonicalJSON(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("canonicalJSON(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	// This assertion changed with the function, deliberately and visibly.
	//
	// The first version returned an error for invalid JSON and preserved any valid
	// JSON verbatim. Both were wrong for the same reason: the digest's claim is
	// "the store reads the same value back", and the store's decodeMetadata
	// unmarshals into map[string]any and yields {} for anything that is not an
	// object — including invalid JSON, and including storable non-objects like '5'
	// and '[1,2]' (both schemas only require *valid* JSON). Preserving '5' made a
	// byte-perfect copy report a metadata change. The assertion now pins the
	// mirroring instead; row_equality still compares the raw column bytes, so
	// nothing about the data is lost by degrading here.
	got, err := canonicalJSON(`{"a":`)
	if err != nil {
		t.Fatalf("canonicalJSON on invalid JSON returned an error: %v", err)
	}
	if got != "{}" {
		t.Errorf("canonicalJSON on invalid JSON = %q, want {} (what the store reads back)", got)
	}
}

// TestOpenSourceRefusesAWalSidecar guards the silent-subset failure: the source
// is read with immutable=1, which ignores the write-ahead log, so a database
// with a -wal file would import only the transactions that had already been
// checkpointed and look completely successful.
//
// The check runs before the sqlite3 binary is looked for, so this test needs
// neither sqlite3 nor a database file.
func TestOpenSourceRefusesAWalSidecar(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "teleport.db")
			if err := os.WriteFile(path, []byte("not really a database"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+suffix, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := OpenSource(context.Background(), path)
			if err == nil {
				t.Fatalf("OpenSource accepted a source with a %s sidecar", suffix)
			}
			if !errors.Is(err, ErrSourceHasWAL) {
				t.Errorf("error %v does not wrap ErrSourceHasWAL", err)
			}
		})
	}
}

func TestOpenSourceRejectsAMissingFile(t *testing.T) {
	if _, err := OpenSource(context.Background(), filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Fatal("OpenSource accepted a path that does not exist")
	}
	if _, err := OpenSource(context.Background(), ""); err == nil {
		t.Fatal("OpenSource accepted an empty path")
	}
	dir := t.TempDir()
	if _, err := OpenSource(context.Background(), dir); err == nil {
		t.Fatal("OpenSource accepted a directory")
	}
}

// TestConvertValueRefusesIntegersOutsideInt64 is the guard for a silent wrap that
// was measured, not imagined.
//
// SQLite's INTEGER is dynamically typed, so 9223372036854775808 (2^63) is stored
// with REAL affinity and `sqlite3 -json` prints it as 9.2233720368547758e+18. That
// reached int64(), whose result for an out-of-range float is undefined and on this
// platform wraps to math.MinInt64 — so the source value 9223372036854775808 was
// written to PostgreSQL as -9223372036854775808, and the verifier called it green
// because it converts the source through this same function.
func TestConvertValueRefusesIntegersOutsideInt64(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   json.Number
		want    int64
		wantErr bool
	}{
		// The exact boundary values, both of which are legal int64 values.
		{name: "int64 max as a plain integer literal", value: json.Number("9223372036854775807"), want: math.MaxInt64},
		{name: "int64 min as a plain integer literal", value: json.Number("-9223372036854775808"), want: math.MinInt64},
		// The same values as they arrive from SQLite when stored with REAL affinity.
		{name: "int64 max via exponent notation", value: json.Number("9.2233720368547758e+18"), wantErr: true},
		{name: "2^63 exactly", value: json.Number("9223372036854775808"), wantErr: true},
		{name: "2^63 via exponent notation", value: json.Number("9.223372036854775808e+18"), wantErr: true},
		{name: "far above int64", value: json.Number("1e30"), wantErr: true},
		{name: "far below int64", value: json.Number("-1e30"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertValue(tc.value, TargetColumn{Name: "created_at", DataType: "bigint"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("convertValue(%s) = %v, want an error: this value does not fit in an int64 "+
						"and converting it wraps silently", tc.value, got)
				}
				if !strings.Contains(err.Error(), "does not fit") {
					t.Errorf("error %q does not say the value is out of range", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertValue(%s): %v", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("convertValue(%s) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestCompareRenderedBytes is the falsification of the one check that does not go
// through convertValue: it must report a difference, name the column, and treat
// NULL as different from the empty string.
//
// The hex strings here are what the two databases actually produce for the values
// named in the comments — see the measured probes in
// .ai/pitfalls/cases/2026-10-16-verifier-shares-the-conversion-it-checks.md.
func TestCompareRenderedBytes(t *testing.T) {
	columns := []string{"created_at", "title"}

	t.Run("identical bytes report nothing", func(t *testing.T) {
		source := map[string][]RenderedValue{
			"r1": {{Text: "31373931353633333236363036"}, {Text: "6869"}},
		}
		target := map[string][]RenderedValue{
			"r1": {{Text: "31373931353633333236363036"}, {Text: "6869"}},
		}
		if got := compareRenderedBytes(columns, source, target); len(got) != 0 {
			t.Errorf("got %v, want no problems", got)
		}
	})

	t.Run("a wrapped integer is reported with its column", func(t *testing.T) {
		// Exactly the shape of the measured bug: SQLite holds 2^63 (stored with
		// REAL affinity, so hex() gives the text bytes of its rendering), and
		// PostgreSQL holds the wrapped value.
		source := map[string][]RenderedValue{
			"r1": {{Text: hexOf("9.2233720368547758e+18")}, {Text: hexOf("hi")}},
		}
		target := map[string][]RenderedValue{
			"r1": {{Text: hexOf("-9223372036854775808")}, {Text: hexOf("hi")}},
		}
		got := compareRenderedBytes(columns, source, target)
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one problem", got)
		}
		if !strings.Contains(got[0], "created_at") {
			t.Errorf("problem %q does not name the column", got[0])
		}
	})

	t.Run("a substituted byte is reported", func(t *testing.T) {
		// The invalid-UTF-8 shape: SQLite holds the byte 0xFF, the JSON pipe
		// replaces it with U+FFFD, and the target therefore holds efbfbd. A text
		// comparison could not see this, because both sides would have gone
		// through the same substitution; byte comparison can.
		source := map[string][]RenderedValue{"r1": {{Text: "31"}, {Text: "ff"}}}
		target := map[string][]RenderedValue{"r1": {{Text: "31"}, {Text: "efbfbd"}}}
		got := compareRenderedBytes(columns, source, target)
		if len(got) != 1 || !strings.Contains(got[0], "title") {
			t.Errorf("got %v, want one problem naming title", got)
		}
	})

	t.Run("NULL and the empty string are different values", func(t *testing.T) {
		source := map[string][]RenderedValue{"r1": {{Null: true}, {Text: ""}}}
		target := map[string][]RenderedValue{"r1": {{Text: ""}, {Null: true}}}
		if got := compareRenderedBytes(columns, source, target); len(got) != 2 {
			t.Errorf("got %v, want two problems: NULL must never compare equal to the empty string", got)
		}
	})

	t.Run("a missing row is reported", func(t *testing.T) {
		source := map[string][]RenderedValue{"r1": {{Text: "31"}, {Text: "6162"}}}
		if got := compareRenderedBytes(columns, source, map[string][]RenderedValue{}); len(got) != 1 {
			t.Errorf("got %v, want one problem about the missing row", got)
		}
	})

	t.Run("an extra row is reported", func(t *testing.T) {
		target := map[string][]RenderedValue{"r9": {{Text: "31"}, {Text: "6162"}}}
		if got := compareRenderedBytes(columns, map[string][]RenderedValue{}, target); len(got) != 1 {
			t.Errorf("got %v, want one problem about the extra row", got)
		}
	})
}

// hexOf is the lower(hex()) rendering of a string's UTF-8 bytes, which is what
// SQLite's hex() returns for a TEXT value.
func hexOf(s string) string { return hex.EncodeToString([]byte(s)) }

// TestJoinCanonicalRowCannotBeForgedByAValue is the guard for the separator
// collision: two DIFFERENT rows must never produce the same canonical string.
//
// The first version joined cells with a 0x1f byte and claimed a control character
// could not appear in these columns. reports.content and agent_keys.note hold
// arbitrary text, so that claim was false, and the failure mode was the worst
// kind: two different rows comparing equal in the check that decides whether the
// data moved correctly.
func TestJoinCanonicalRowCannotBeForgedByAValue(t *testing.T) {
	// A value crafted to look like a cell boundary followed by another cell.
	forged := "X\x1f\x00sY"

	a := joinCanonicalRow([]string{"\x00s" + forged, "\x00sZ"})
	b := joinCanonicalRow([]string{"\x00sX", "\x00sY\x1f\x00sZ"})
	if a == b {
		t.Errorf("two different rows produced the same canonical string %q: a value forged a cell "+
			"boundary", a)
	}

	// And the round trip has to be exact, including empty cells, so that a
	// diagnostic never mis-attributes a difference to the wrong column.
	for _, parts := range [][]string{
		{"\x00sabc", "\x00i1", "\x00null"},
		{"", ""},
		{"\x00s0:5x", "\x00s", ""},
	} {
		got := splitCanonicalRow(joinCanonicalRow(parts))
		if len(got) != len(parts) {
			t.Fatalf("round trip of %q produced %d cells, want %d: %q", parts, len(got), len(parts), got)
		}
		for i := range parts {
			if got[i] != parts[i] {
				t.Errorf("round trip of %q: cell %d is %q, want %q", parts, i, got[i], parts[i])
			}
		}
	}
}

// TestCanonicalJSONMirrorsTheStore: the digest must read metadata exactly the way
// the store's decodeMetadata does, or a byte-perfect copy reports a metadata
// change. Both schema CHECKs require only *valid* JSON, so '5' and '[1,2]' are
// storable; the store degrades both to {}.
func TestCanonicalJSONMirrorsTheStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"an object", `{"b":2,"a":1}`, `{"a":1,"b":2}`},
		{"a bare number is not an object", `5`, `{}`},
		{"an array is not an object", `[1,2]`, `{}`},
		{"a bare string is not an object", `"x"`, `{}`},
		{"null", `null`, `{}`},
		{"an empty column", ``, `{}`},
		{"an empty object", `{}`, `{}`},
		{"invalid JSON", `{"a":`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalJSON(tc.raw)
			if err != nil {
				t.Fatalf("canonicalJSON(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("canonicalJSON(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestHasRealAffinity pins SQLite's own affinity rules for the declared types this
// tool has to recognise, because reading a REAL column the wrong way is exactly
// the precision hole the printf path exists to close.
func TestHasRealAffinity(t *testing.T) {
	for _, tc := range []struct {
		declType string
		want     bool
	}{
		{"REAL", true},
		{"real", true},
		{"double precision", true},
		{"DOUBLE", true},
		{"FLOAT", true},
		{"", false},
		{"INTEGER", false},
		{"TEXT", false},
		{"BIGINT", false},
		{"VARCHAR(20)", false},
	} {
		if got := hasRealAffinity(tc.declType); got != tc.want {
			t.Errorf("hasRealAffinity(%q) = %v, want %v", tc.declType, got, tc.want)
		}
	}
}
