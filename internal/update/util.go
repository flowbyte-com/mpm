package update

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// sqliteROBusyTimeoutMs bounds how long a live-database read waits for
// a writer's lock before it gives up.
const sqliteROBusyTimeoutMs = 10000

// openSQLiteRO opens a database read-only with a bounded busy wait.
//
// Every live-database read in the update path goes through this: a
// backup, preflight or fingerprint taken while another process writes
// must WAIT for the lock (bounded), not fail instantly with
// SQLITE_BUSY and not wait forever. Ten seconds bounds the worst case
// while surviving a checkpoint burst.
//
// WHY THE TIMEOUT IS IN THE DSN AND NOT IN A PRAGMA AFTER THE OPEN
// --------------------------------------------------------------
//
// `sql.Open` is lazy: it opens no connection. The first statement run
// against the pool is what makes the driver open one — and the driver's
// own open path is where the busy timeout is installed:
//
//	mattn/go-sqlite3 v1.14.37, sqlite3.go:
//	  1098  busyTimeout := 5000            <- the driver's own default
//	  1491  PRAGMA busy_timeout = 5000;    <- installed during the open
//	  1746  PRAGMA synchronous = NORMAL;   <- the first statement that
//	                                          READS the database
//
// So a `PRAGMA busy_timeout = 10000` issued AFTER the open is itself
// the first database operation, and it runs under the driver's FIVE
// second default: the ten seconds asked for are never in force for the
// operation that needs them. If a writer holds the lock for longer than
// five seconds, that PRAGMA fails, `openSQLiteRO` fails, and the caller
// reports "database is locked" — after five seconds, not ten.
//
// Putting `_busy_timeout` in the DSN makes the driver install the
// requested timeout DURING the connection open (sqlite3.go:1491 uses the
// DSN's value), so it is in force for the very first operation and for
// every operation after it — including on connections the pool opens
// later, which a PRAGMA issued once could never cover.
//
// This is the same correction D-003 made to the substrate's WRITE DSN
// (internal/core/db.go:118-131), for the same reason. The update path's
// read-only opener had been missed.
func openSQLiteRO(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", sqliteRODSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// The pool is lazy, so the connection is opened HERE rather than by
	// whichever call site happens to run the first statement. A missing,
	// corrupt or locked database is this function's to report.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// sqliteRODSN builds the read-only DSN for a database path.
//
// The path is percent-escaped so that a path containing `?` or `#`
// cannot swallow the query parameters: SQLite ends the URI's path at
// the first `?` or `#`, and the driver parses everything after the
// first `?` as the query. An unescaped `?` in a directory name would
// silently drop `mode=ro` — the read would then open the live database
// WRITABLE — and would drop `_busy_timeout` with it.
func sqliteRODSN(path string) string {
	return fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d", escapeSQLiteURIPath(path), sqliteROBusyTimeoutMs)
}

// sqliteURIPathEscaper percent-escapes the characters that would
// otherwise change the MEANING of a SQLite URI filename: `?` ends the
// path and starts the query, `#` ends the path and starts the fragment,
// and `%` starts a percent-escape of its own.
//
// The replacements are applied in a single pass, so an escaped `%` is
// never escaped again.
var sqliteURIPathEscaper = strings.NewReplacer(
	"%", "%25",
	"?", "%3F",
	"#", "%23",
)

// escapeSQLiteURIPath percent-escapes a database path for use in a
// SQLite URI filename. A path that needs no escaping is unchanged.
func escapeSQLiteURIPath(path string) string {
	return sqliteURIPathEscaper.Replace(path)
}

// epochToRFC3339 renders a build epoch the way rb_write_release does.
func epochToRFC3339(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ShortSHA abbreviates a commit for display.
func ShortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// IsPlaceholderCommit reports whether a stamped commit carries no real
// revision (a Git-free export, an unstamped binary).
func IsPlaceholderCommit(commit string) bool {
	switch commit {
	case "", "unknown", "unknown-dirty":
		return true
	}
	return false
}

// FormatUpdateError renders a failure for the operator: what was being
// done, what went wrong, and where the recovery point is. It never
// includes credentials or paths the operator did not already know.
func FormatUpdateError(stage string, err error) string {
	return fmt.Sprintf("update failed at %s: %v", stage, err)
}
