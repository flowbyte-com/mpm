package update

// The DSN the read-only path uses, and the escaping it applies to the
// path.
//
// These are unit tests of the construction; the behavioural proofs
// that the timeout is IN FORCE for the first database operation live
// in backup_busy_timeout_test.go.

import "testing"

// TestSQLiteRODSN pins the DSN.
//
// The timeout must be IN the DSN: a PRAGMA issued after the open is the
// first database operation and therefore runs under the driver's
// default, not under the requested timeout.
func TestSQLiteRODSN(t *testing.T) {
	got := sqliteRODSN("/var/lib/mpm/mpm.db")
	want := "file:/var/lib/mpm/mpm.db?mode=ro&_busy_timeout=10000"
	if got != want {
		t.Fatalf("the read-only DSN changed:\n got %q\nwant %q", got, want)
	}
}

// TestEscapeSQLiteURIPath pins the URI escaping.
//
// `?` ends the path and starts the query, `#` ends the path and starts
// the fragment, and `%` starts a percent escape of its own. An ordinary
// path must come out byte-for-byte unchanged, and an escaped `%` must
// never be escaped twice.
func TestEscapeSQLiteURIPath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"/var/lib/mpm/mpm.db", "/var/lib/mpm/mpm.db"},
		{"/var/lib/we?ird/mpm.db", "/var/lib/we%3Fird/mpm.db"},
		{"/var/lib/we#ird/mpm.db", "/var/lib/we%23ird/mpm.db"},
		{"/var/lib/we%25ird/mpm.db", "/var/lib/we%2525ird/mpm.db"},
		{"/var/lib/we?ird#x%25y/mpm.db", "/var/lib/we%3Fird%23x%2525y/mpm.db"},
		{"/var/lib/a b/mpm.db", "/var/lib/a b/mpm.db"},
	} {
		if got := escapeSQLiteURIPath(tc.in); got != tc.want {
			t.Errorf("escapeSQLiteURIPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSQLiteROBusyTimeoutIsTheRequestedOne pins the constant the update
// path asks for: ten seconds, bounded.
func TestSQLiteROBusyTimeoutIsTheRequestedOne(t *testing.T) {
	if sqliteROBusyTimeoutMs != 10000 {
		t.Fatalf("the read-only busy timeout is %d ms, want 10000", sqliteROBusyTimeoutMs)
	}
}
