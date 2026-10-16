# A skipped subtest makes t.Run return true, so "the subtest succeeded" can be a skip

**Severity:** 🟠 medium · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

A new harness test passed with a database and failed without one — the opposite of the
usual direction, which is what made it interesting:

```text
--- FAIL: TestHarnessDropsTheSchemaWhenTheTestEnds (0.00s)
    storetest_harness_test.go:64: the inner subtest did not report a schema name
```

With `TEST_PG_DSN` set it was green; with the variable unset it failed.

## Root cause

The test obtained its database **inside** a subtest, so that the subtest's `t.Cleanup`
would run at a known moment and the parent could then assert the schema had been dropped:

```go
var schema string
if !t.Run("inner", func(t *testing.T) {
    _, name := storetest.Schema(t, MigratePG) // calls t.Skip when TEST_PG_DSN is unset
    schema = name
}) {
    t.Fatal("the inner subtest failed")
}
if schema == "" {
    t.Fatal("the inner subtest did not report a schema name") // <-- the failure
}
```

`t.Run` returns **true** both when a subtest passes *and* when it **skips**. There is no
three-valued return, and `t.Skipped()` cannot be consulted from the parent for a subtest that
has already finished.

So the outer test read a skip as success, continued past the point where `schema` would have
been assigned, and reported a failure about its own scaffolding — a confusing message that
points at the wrong thing, and only on machines without a database.

The same inversion bites in the other direction: a test that does
`if !t.Run(...) { t.Fatal("failed") }` is correct, but one that does
`if t.Run(...) { /* assert the inner thing worked */ }` will assert on a skip.

## Fix

Skip the whole test up front, before any subtest exists, so a skip is a skip of the test
that would otherwise assert. `storetest.SkipWithoutDatabase(t)` does this and documents why:

```go
func TestHarnessDropsTheSchemaWhenTheTestEnds(t *testing.T) {
    // The skip must happen out here: a skipped subtest makes t.Run report true.
    storetest.SkipWithoutDatabase(t)
    ...
}
```

## Guard

The two harness tests that need a database now call `storetest.SkipWithoutDatabase(t)` as
their first statement, so the no-database path is exercised on every machine that lacks a
database — which is every machine running `pnpm run check`. The failure was found by running
the gate **without** `TEST_PG_DSN`; running only the Postgres gate would never have shown it.

**The general rule**: when a test's result depends on *whether a subtest ran*, determine that
from something the subtest itself recorded, never from `t.Run`'s return value. If a helper
can skip, call it outside the subtest.
