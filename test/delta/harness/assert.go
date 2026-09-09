package harness

import (
	"strconv"
	"testing"
)

// The assertions in this suite are all of one shape: a SQL query that returns a
// single value, compared against what SPEC.md says that value must be. That is
// deliberate. SPEC.md 17.1 makes database truth the interface, and an assertion
// written against the database is an assertion the owner can re-run by hand at
// a terminal, which is what CLAUDE.md 4 step 5 asks of him.

// Bool runs a query that must return exactly one boolean and fails the test
// unless it returned true. why names the SPEC rule the assertion comes from, so
// a failure says which rule broke rather than only which query returned false.
func Bool(t testing.TB, why, sql string, vars ...string) {
	t.Helper()
	out, err := PSQL(sql, vars...)
	if err != nil {
		t.Errorf("%s\n  query failed: %v", why, err)
		return
	}
	switch out {
	case "t":
		return
	case "":
		t.Errorf("%s\n  the query returned no row\n  query: %s", why, sql)
	default:
		t.Errorf("%s\n  expected t, got %q\n  query: %s", why, out, sql)
	}
}

// Scalar returns the single value a query produced, failing the test if the
// query itself failed.
func Scalar(t testing.TB, sql string, vars ...string) string {
	t.Helper()
	out, err := PSQL(sql, vars...)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	return out
}

// Int returns the single integer a query produced.
func Int(t testing.TB, sql string, vars ...string) int {
	t.Helper()
	raw := Scalar(t, sql, vars...)
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("expected an integer, got %q\n  query: %s", raw, sql)
	}
	return n
}

// Equal fails the test unless got equals want. It is what the digest
// assertions use: SPEC.md 6.7 makes dead_letter_queue append-only and SPEC.md
// 14 leaves earlier steps alone on a later replay, and both are claims that
// something did NOT change - which is a comparison of two readings, not a
// predicate over one.
func Equal(t testing.TB, why, want, got string) {
	t.Helper()
	if want != got {
		t.Errorf("%s\n  before: %s\n  after:  %s", why, want, got)
	}
}
