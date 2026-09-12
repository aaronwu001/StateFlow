package harness

import "testing"

// Bool runs a query that must return exactly one boolean and fails the test
// unless it returned true. why names the SPEC.md rule the assertion comes from,
// so a failure says which rule broke rather than only which query returned
// false.
//
// SPEC.md 17.1 makes database truth the interface, so an assertion written this
// way is one the owner can re-run by hand at a terminal — which is what
// CLAUDE.md 4 step 5 asks of him.
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
