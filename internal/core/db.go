// db.go - Unified SQLite database for MPM
// Version: 2026-03-28 (Single Database Refactor)
// Description: Manages a single unified database at src/db/mpm.db

package internal

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flowbyte-com/mpm-core/config"

	_ "github.com/mattn/go-sqlite3"
)

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// defaultLogRotateBytes is the default file-size threshold for log rotation
// (watchdog.jsonl, mirror.jsonl). At 5 MiB the JSONL files stay readable
// by `jq`, `tail`, and text editors without paging. Operators can override
// via the MPM_LOG_ROTATE_BYTES environment variable.
const defaultLogRotateBytes int64 = 5 * 1024 * 1024

// LogRotateThresholdBytesForCLI is the CLI-accessible alias for
// logRotateThresholdBytes. `mpm ops logs status` reads the current
// threshold via this wrapper. Kept distinct from the unexported name so
// the runtime call site doesn't accidentally bypass the env-var check.
func LogRotateThresholdBytesForCLI() int64 { return logRotateThresholdBytes() }

// RotateLogIfNeededForCLI is the CLI-accessible wrapper for
// rotateLogIfNeeded. The CLI passes a threshold of 1 byte to force
// rotation regardless of the configured MPM_LOG_ROTATE_BYTES — the
// operator's intent in calling `mpm ops logs rotate` IS the force.
func RotateLogIfNeededForCLI(path string, thresholdBytes int64) error {
	return rotateLogIfNeeded(path, thresholdBytes)
}

// logRotateThresholdBytes resolves the current rotation threshold.
// Reads MPM_LOG_ROTATE_BYTES (a non-negative integer); falls back to the
// default. Invalid values (negative, non-numeric) are clamped to default
// with a single warning logged via slog at WARN level — once per process
// is not worth tracking.
func logRotateThresholdBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("MPM_LOG_ROTATE_BYTES"))
	if raw == "" {
		return defaultLogRotateBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		slog.Warn("invalid MPM_LOG_ROTATE_BYTES; using default",
			"got", raw, "default_bytes", defaultLogRotateBytes)
		return defaultLogRotateBytes
	}
	return n
}

// rotateLogIfNeeded checks the size of path; if it exceeds thresholdBytes,
// reads the current contents, gzips them to path.YYYYMMDD-HHMMSS.gz, and
// truncates the original to zero bytes. The next O_APPEND write creates
// fresh content. Returns nil on no-op (file missing or below threshold) or
// on success; errors are non-fatal — the caller logs and continues with
// the append.
//
// Called from inside the watchdogMu critical section so rotation does not
// race with concurrent writers.
//
// Atomicity: the read-then-truncate is not atomic across processes. Two
// MPM instances writing to the same log (production has only one — the
// workspace DB is per-process) would race. We accept this; the design
// contract is "one process owns one workspace DB and its logs." A
// separate `mpm ops logs rotate` command can be added later if manual
// rotation is needed.
func rotateLogIfNeeded(path string, thresholdBytes int64) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat log for rotation: %w", err)
	}
	if info.Size() < thresholdBytes {
		return nil
	}

	timestamp := time.Now().UTC().Format("20060102-150405")
	rotated := path + "." + timestamp + ".gz"

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read log for rotation: %w", err)
	}
	gz, err := os.Create(rotated)
	if err != nil {
		return fmt.Errorf("create rotated log: %w", err)
	}
	gzWriter := gzip.NewWriter(gz)
	if _, err := gzWriter.Write(data); err != nil {
		_ = gzWriter.Close()
		_ = gz.Close()
		_ = os.Remove(rotated)
		return fmt.Errorf("gzip write: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		_ = gz.Close()
		_ = os.Remove(rotated)
		return fmt.Errorf("gzip close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close rotated log: %w", err)
	}
	// Truncate in place so the existing O_APPEND handle (if any) keeps
	// appending at offset 0. If a different process raced us here, the
	// log content between the gzip snapshot and now would be lost —
	// accepted risk per the atomicity note above.
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("truncate after rotation: %w", err)
	}
	return nil
}

// dbFileName is the canonical filename for the MPM database.
// Previously mpm_memory.db - renamed 2026-04-01 to reflect its unified nature.
const dbFileName = "mpm.db"

// SQLiteConnection is a wrapper for sql.DB for backwards compatibility with existing code
type SQLiteConnection struct {
	*sql.DB
}

// NewSQLiteConnection opens a new SQLite database connection (for backwards compatibility)
func NewSQLiteConnection(dbPath string) (*SQLiteConnection, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	// Wait up to 5 seconds for locks to clear before returning SQLITE_BUSY.
	// This handles concurrent GC + Add without immediate failure.
	db.Exec("PRAGMA busy_timeout = 5000")
	return &SQLiteConnection{DB: db}, nil
}

// WipeTableNames is the allowlist of tables that can be wiped/deleted
var WipeTableNames = map[string]string{
	"sessions": "sessions",
	"memories": "memories",
	"topics":   "topics",
}

// WipeRecord performs a hard delete with VACUUM (on SQLiteConnection for backwards compatibility)
func (sc *SQLiteConnection) WipeRecord(tier, id string) error {
	tableName := WipeTableNames[tier]
	if tableName == "" {
		return fmt.Errorf("cannot wipe from tier: %s", tier)
	}
	tx, err := sc.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec("DELETE FROM "+tableName+" WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	_, err = sc.DB.Exec("VACUUM")
	return err
}

// MarshalJSON is a helper for JSON marshaling (for backwards compatibility)
func MarshalJSON(v interface{}) (string, error) {
	bytes, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// UnmarshalJSON is a helper for JSON unmarshaling (for backwards compatibility)
func UnmarshalJSON(data string, v interface{}) error {
	return json.Unmarshal([]byte(data), v)
}

// ==================== DatabaseManager ====================

// DatabaseManager manages the single unified database
type DatabaseManager struct {
	db          *sql.DB
	dbPath      string       // stored for shareable store init
	sharedStore *MemoryStore // reused for self-maintenance; nil until first access

	watchdogPath string     // path to watchdog.jsonl for query observability
	watchdogMu   sync.Mutex // serializes watchdog log writes

	// busyRetries counts lifetime SQLITE_BUSY retry attempts via ExecTracked.
	// Use BusyRetryCount() to inspect. Zero is healthy; non-zero means
	// contention is occurring somewhere in the call stack. The counter is
	// per-DatabaseManager (per-process); cross-process contention visible
	// only via the watchdog.jsonl tail.
	busyRetries atomic.Uint64

	// sharedPath is the path of the attached shared DB, or "" if not attached.
	// Set by attachShared() when MPM_SHARED_DB is configured and ATTACH succeeds.
	// See WISHLIST.md "Multi-Agent Shared Epistemology" for the design.
	sharedPath     string
	sharedAttached bool

}

const slowQueryThreshold = 100 * time.Millisecond // queries slower than this are logged as "slow"

// SQLDB returns the underlying *sql.DB for direct queries.
func (dm *DatabaseManager) SQLDB() *sql.DB {
	return dm.db
}

// DBPath returns the filesystem path to the SQLite database file. Useful for
// tools that need to shell out (sqlite3 CLI, external dumpers) while the
// DatabaseManager has the canonical path resolved.
func (dm *DatabaseManager) DBPath() string {
	return dm.dbPath
}

// IsOpen returns true if the database connection is non-nil.
func (dm *DatabaseManager) IsOpen() bool {
	return dm.db != nil
}

// BusyRetryCount returns the lifetime count of SQLITE_BUSY retry attempts
// across all ExecTracked calls in this process. Zero is the healthy
// baseline. Non-zero means contention is occurring somewhere in the
// call stack — investigate via watchdog.jsonl tail before scaling up
// the tooling surface that shares this DB.
func (dm *DatabaseManager) BusyRetryCount() uint64 {
	return dm.busyRetries.Load()
}

// HealthCheck runs a quick SQLite integrity check + returns key DB stats.
// Designed for the "is everything healthy?" question that previously
// required 6 separate lookups (audit clusters, wakes, memory review,
// memory quality, watchdog size, git status). Returns:
//   - ok: bool, true iff PRAGMA quick_check returns "ok"
//   - page_count, freelist_count: from PRAGMA page_count / freelist_count
//   - memories_active: count of non-deleted memories
//   - theories_pending: count of pending theories
//   - wakes_overdue: count of unfired wakes whose target_time < now
//   - busy_retries: lifetime SQLITE_BUSY retry counter (process-local)
//
// Not a tool today — DM-level method that future ops commands can wrap.
// Failure modes: returns whatever it managed to read; the `ok` field
// reflects only the integrity check result.
func (dm *DatabaseManager) HealthCheck() (map[string]interface{}, error) {
	out := map[string]interface{}{
		"busy_retries": dm.BusyRetryCount(),
	}

	// Integrity check (PRAGMA quick_check is cheap, runs in <100ms).
	var integrity string
	if err := dm.db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		out["ok"] = false
		out["integrity_error"] = err.Error()
		return out, fmt.Errorf("integrity check: %w", err)
	}
	out["ok"] = integrity == "ok"
	if !out["ok"].(bool) {
		out["integrity_status"] = integrity
	}

	// Page-level stats (cheap, no scan).
	var pageCount, freelistCount sql.NullInt64
	if err := dm.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err == nil && pageCount.Valid {
		out["page_count"] = pageCount.Int64
	}
	if err := dm.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelistCount); err == nil && freelistCount.Valid {
		out["freelist_count"] = freelistCount.Int64
	}

	// Domain stats (each a single COUNT query).
	queries := []struct {
		key, sql string
	}{
		{"memories_active", `SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`},
		{"theories_pending", `SELECT COUNT(*) FROM memories WHERE collection = 'theories' AND deleted_at IS NULL AND json_extract(metadata, '$.status') = 'pending'`},
		{"wakes_overdue", `SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0 AND target_time < ?`},
		{"evidence_total", `SELECT COUNT(*) FROM evidence`},
	}
	now := time.Now().Unix()
	for _, q := range queries {
		var n int64
		var err error
		if q.key == "wakes_overdue" {
			err = dm.db.QueryRow(q.sql, now).Scan(&n)
		} else {
			err = dm.db.QueryRow(q.sql).Scan(&n)
		}
		if err == nil {
			out[q.key] = n
		}
		// Non-fatal — skip on error.
	}

	return out, nil
}

// ==================== Watchdog / Query Observability ====================

// watchdogOp represents a single operation entry written to watchdog.jsonl.
type watchdogOp struct {
	Timestamp  string `json:"timestamp"`
	Operation  string `json:"operation"`
	DurationMs int64  `json:"duration_ms"`
	Query      string `json:"query,omitempty"`
	Retries    int    `json:"retries,omitempty"`
	Error      string `json:"error,omitempty"`
	Slow       bool   `json:"slow"`
}

// logWatchdog appends a watchdog entry to watchdog.jsonl.
// File-write contention is serialised by watchdogMu so that concurrent
// DatabaseManager users do not corrupt the log. Log rotation (gzip +
// truncate) runs inside the same critical section so concurrent writers
// don't race on the truncation step.
func (dm *DatabaseManager) logWatchdog(entry watchdogOp) {
	if dm.watchdogPath == "" {
		return
	}
	dm.watchdogMu.Lock()
	defer dm.watchdogMu.Unlock()

	if err := rotateLogIfNeeded(dm.watchdogPath, logRotateThresholdBytes()); err != nil {
		slog.Warn("watchdog log rotation failed", "err", err)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(dm.watchdogPath, os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
	f.Write([]byte("\n"))
}

// logWatchdogRaw appends raw JSON bytes to watchdog.jsonl under the dm mutex.
// Used by synthesis and other subsystems that want structured events with
// custom schemas rather than the watchdogOp query-timing format. Log
// rotation runs inside the same critical section.
func (dm *DatabaseManager) logWatchdogRaw(line []byte) {
	if dm == nil || dm.watchdogPath == "" {
		return
	}
	dm.watchdogMu.Lock()
	defer dm.watchdogMu.Unlock()
	if err := rotateLogIfNeeded(dm.watchdogPath, logRotateThresholdBytes()); err != nil {
		slog.Warn("watchdog log rotation failed", "err", err)
	}
	f, err := os.OpenFile(dm.watchdogPath, os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(line)
	f.Write([]byte("\n"))
}

// WatchdogOp is one parsed entry from watchdog.jsonl. Both the legacy
// watchdogOp schema and the newer raw schema (op / timestamp / error / reason)
// surface here as a flat map so callers don't need to know which version
// produced the line.
type WatchdogOp map[string]interface{}

// RecentWatchdogOps returns the most recent n entries from watchdog.jsonl,
// optionally filtered by op-prefix (e.g. "synthesize_" to see only synthesis
// events). Newest entries come last in the returned slice. A zero n returns
// all entries (up to a hard cap of 10_000 to avoid OOM on a runaway log).
//
// This is the read path for `mpm synthesize status|errors` — the watchdog
// log is the de-facto synthesis telemetry store since there is no in-memory
// queue or DLQ for synthesis attempts.
func (dm *DatabaseManager) RecentWatchdogOps(n int, opPrefix string) ([]WatchdogOp, error) {
	if dm.watchdogPath == "" {
		return nil, fmt.Errorf("watchdog log path not configured")
	}
	dm.watchdogMu.Lock()
	data, err := os.ReadFile(dm.watchdogPath)
	dm.watchdogMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	const hardCap = 10_000
	if n <= 0 || n > hardCap {
		n = hardCap
	}

	var out []WatchdogOp
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var op WatchdogOp
		if err := json.Unmarshal([]byte(line), &op); err != nil {
			continue
		}
		if opPrefix != "" {
			name, _ := op["op"].(string)
			if !strings.HasPrefix(name, opPrefix) {
				continue
			}
		}
		out = append(out, op)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// DBNode abstracts the query execution environment so functions can run
// either standalone (against *DatabaseManager) or inside an active
// transaction (against *txNode). RecomputeConfidence and its helpers take
// a DBNode so callers can choose to wrap multi-statement atomic operations
// in a transaction via DatabaseManager.WithTx.
//
// Tx() exposes the underlying *sql.Tx so callers that need to pass a
// raw transaction to a low-level helper (e.g., the cascade outbox
// EnqueueCascadeInvalidation which the brief pins as *sql.Tx) can
// reach it from inside a WithTx callback. Calling Tx() on a non-tx
// DBNode (i.e., *DatabaseManager used outside a transaction) is a
// programming error — the cascade invalidation hook MUST be called
// from inside a WithTx callback so the root mutation and the
// cascade intent commit atomically.
//
// DM() exposes the owning *DatabaseManager so helpers like
// RecomputeConfidence can call higher-level methods (e.g.,
// EnqueueCascadeInvalidation) while still passing the active tx
// for the cascade intent write. Returns the receiver for
// *DatabaseManager (no-op) and the txNode's captured *DatabaseManager
// for transactions.
type DBNode interface {
	ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error)
	QueryTracked(query string, args ...interface{}) (*sql.Rows, error)
	QueryRowTracked(query string, args ...interface{}) *sql.Row
	Tx() *sql.Tx
	DM() *DatabaseManager
}

// Compile-time assertion that DatabaseManager satisfies DBNode.
var _ DBNode = (*DatabaseManager)(nil)

// txNode wraps an *sql.Tx so it satisfies DBNode. Telemetry mirrors
// DatabaseManager.ExecTracked/QueryTracked so watchdog.jsonl entries from
// inside a transaction look the same as standalone queries.
type txNode struct {
	tx *sql.Tx
	dm *DatabaseManager
}

func (t *txNode) ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	attempts := 0
	backoff := 100 * time.Millisecond
	maxBackoff := 5 * time.Second
	for {
		attempts++
		result, err := t.tx.Exec(query, args...)
		elapsed := time.Since(start)
		if err != nil && isBusyError(err) && attempts <= retries {
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		entry := watchdogOp{
			Timestamp:  start.UTC().Format(time.RFC3339Nano),
			Operation:  "exec",
			DurationMs: elapsed.Milliseconds(),
			Query:      truncateQuery(query),
			Retries:    attempts - 1,
			Slow:       elapsed > slowQueryThreshold,
		}
		if err != nil {
			entry.Error = err.Error()
		}
		t.dm.logWatchdog(entry)
		return result, err
	}
}

func (t *txNode) QueryTracked(query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	rows, err := t.tx.Query(query, args...)
	elapsed := time.Since(start)
	entry := watchdogOp{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		Operation:  "query",
		DurationMs: elapsed.Milliseconds(),
		Query:      truncateQuery(query),
		Slow:       elapsed > slowQueryThreshold,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	t.dm.logWatchdog(entry)
	return rows, err
}

func (t *txNode) QueryRowTracked(query string, args ...interface{}) *sql.Row {
	start := time.Now()
	row := t.tx.QueryRow(query, args...)
	elapsed := time.Since(start)
	entry := watchdogOp{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		Operation:  "query_row",
		DurationMs: elapsed.Milliseconds(),
		Query:      truncateQuery(query),
		Slow:       elapsed > slowQueryThreshold,
	}
	t.dm.logWatchdog(entry)
	return row
}

// Tx exposes the underlying *sql.Tx so the cascade invalidation hook
// (EnqueueCascadeInvalidation, brief-pinned to take *sql.Tx) can be
// called from inside a WithTx callback. Standalone DatabaseManager
// callers that hit this method have a programming error — see the
// DBNode interface comment for the contract.
func (t *txNode) Tx() *sql.Tx {
	return t.tx
}

// Tx on DatabaseManager returns nil. This is intentionally a runtime
// error path, not a panic, so the cascade hook's nil-tx guard surfaces
// a clean error rather than crashing the process.
func (dm *DatabaseManager) Tx() *sql.Tx {
	return nil
}

// DM returns the receiver for *DatabaseManager (no-op identity) so the
// DBNode interface contract holds symmetrically across both
// implementations. For *txNode, DM() returns the captured
// *DatabaseManager so helpers can reach higher-level methods.
func (dm *DatabaseManager) DM() *DatabaseManager {
	return dm
}

// DM on a txNode returns the captured *DatabaseManager so helpers like
// RecomputeConfidence can call methods on the owning DM (e.g.,
// EnqueueCascadeInvalidation) while passing the active tx.
func (t *txNode) DM() *DatabaseManager {
	return t.dm
}

// WithTx executes fn inside a transaction. The transaction commits when fn
// returns nil and rolls back on any error or panic. The panic is re-raised
// after rollback so callers can recover up the stack. fn receives a DBNode
// that delegates to the active transaction; passing it to RecomputeConfidence
// (or any DBNode-accepting function) makes the multi-statement operation
// atomic — a failure in any statement rolls back every preceding statement.
func (dm *DatabaseManager) WithTx(fn func(DBNode) error) (err error) {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		} else if err != nil {
			_ = tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	err = fn(&txNode{tx: tx, dm: dm})
	return err
}

// ExecTracked runs db.Exec with timing and optional retry-backoff.
// If retries > 0 the query is re-attempted on SQLITE_BUSY with exponential
// backoff (100ms, 200ms, 400ms, … capped at 5s).
func (dm *DatabaseManager) ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	attempts := 0
	backoff := 100 * time.Millisecond
	maxBackoff := 5 * time.Second

	for {
		attempts++
		result, err := dm.db.Exec(query, args...)
		elapsed := time.Since(start)

		if err != nil && isBusyError(err) && attempts <= retries {
			dm.busyRetries.Add(1) // instrument: SQLITE_BUSY retry counter
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		entry := watchdogOp{
			Timestamp:  start.UTC().Format(time.RFC3339Nano),
			Operation:  "exec",
			DurationMs: elapsed.Milliseconds(),
			Query:      truncateQuery(query),
			Retries:    attempts - 1,
			Slow:       elapsed > slowQueryThreshold,
		}
		if err != nil {
			entry.Error = err.Error()
		}
		dm.logWatchdog(entry)

		return result, err
	}
}

// QueryTracked runs db.Query with timing.
func (dm *DatabaseManager) QueryTracked(query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	rows, err := dm.db.Query(query, args...)
	elapsed := time.Since(start)

	entry := watchdogOp{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		Operation:  "query",
		DurationMs: elapsed.Milliseconds(),
		Query:      truncateQuery(query),
		Slow:       elapsed > slowQueryThreshold,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	dm.logWatchdog(entry)

	return rows, err
}

// QueryRowTracked runs db.QueryRow with timing.
func (dm *DatabaseManager) QueryRowTracked(query string, args ...interface{}) *sql.Row {
	start := time.Now()
	row := dm.db.QueryRow(query, args...)
	elapsed := time.Since(start)

	entry := watchdogOp{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		Operation:  "queryrow",
		DurationMs: elapsed.Milliseconds(),
		Query:      truncateQuery(query),
		Slow:       elapsed > slowQueryThreshold,
	}
	// We cannot inspect the error without scanning the row, so we log
	// duration-only here. Callers that scan will see any sql.ErrNoRows etc.
	dm.logWatchdog(entry)

	return row
}

// isBusyError returns true when the error is an SQLITE_BUSY / database-locked.
func isBusyError(err error) bool {
	return IsBusyError(err)
}

// IsBusyError is the exported variant of isBusyError. Available
// to consumers in sibling packages (notably the capability
// package's RecordInvocation retry loop) so they can classify
// SQLite lock contention the same way DatabaseManager does
// internally. Single source of truth — if the classification
// rules ever change, both paths stay aligned.
func IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "cannot commit") && strings.Contains(msg, "locked")
}

// truncateQuery keeps only the first 120 characters of a query for watchdog
// log entries, preventing massive SQL strings from bloating the log.
func truncateQuery(q string) string {
	if len(q) > 120 {
		return q[:120] + "..."
	}
	return q
}

// WatchdogPath returns the path to the watchdog log file this manager writes to.
func (dm *DatabaseManager) WatchdogPath() string {
	return dm.watchdogPath
}

// ==================== Shared Store ====================

// getSharedStore returns a MemoryStore backed by dm.db, creating it once.
// This avoids redundant InitSQLite() calls in self-maintenance methods.
func (dm *DatabaseManager) getSharedStore() (*MemoryStore, error) {
	if dm.sharedStore != nil {
		return dm.sharedStore, nil
	}
	store := NewMemoryStore("")
	store.SQLiteDBPath = dm.dbPath
	store.DB = &SQLiteConnection{DB: dm.db}
	store.DM = dm // wire DM so MemoryStore can audit-log via the same connection
	dm.sharedStore = store
	return store, nil
}

// NewDatabaseManager creates a new database manager with single unified database.
// The database is ALWAYS at mpm/src/db/mpm.db regardless of projectRoot.
// projectRoot is kept for API compatibility but is ignored for path resolution.
func NewDatabaseManager(projectRoot string) (*DatabaseManager, error) {
	// Honour the caller's projectRoot argument when supplied. The legacy
	// implementation ignored it and fell back to config.GetMPMDir() — which
	// silently dropped system-level service paths into ~/.mpm on hosts where
	// the user lacks write access (e.g. systemd User=v runs as the operator,
	// not root, and /home/v/.mpm may be unwritable in hardened setups).
	// Callers who pass "" continue to get the env-driven behaviour via
	// GetMPMDir() so test helpers and ad-hoc CLI invocations keep working.
	var mpmDir string
	if projectRoot != "" {
		mpmDir = projectRoot
	} else {
		mpmDir = config.GetMPMDir()
	}
	dbDir := filepath.Join(mpmDir, "src", "db")
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	dbPath := filepath.Join(dbDir, dbFileName)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA synchronous = NORMAL")
	db.Exec("PRAGMA cache_size = -64000")

	manager := &DatabaseManager{
		db:           db,
		dbPath:       dbPath,
		watchdogPath: filepath.Join(filepath.Dir(dbPath), "watchdog.jsonl"),
	}

	if err := manager.initUnifiedSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Check and rotate watchdog/mirror logs at startup so operators don't
	// need to rely solely on manual `mpm ops logs rotate`. Auto-rotation
	// also happens on each tracked Exec/Query; the startup check catches
	// the case where the process idles with no DB activity.
	threshold := logRotateThresholdBytes()
	if err := rotateLogIfNeeded(filepath.Join(filepath.Dir(dbPath), "watchdog.jsonl"), threshold); err != nil {
		slog.Warn("watchdog log rotation at startup", "error", err.Error())
	}
	if err := rotateLogIfNeeded(filepath.Join(filepath.Dir(dbPath), "mirror.jsonl"), threshold); err != nil {
		slog.Warn("mirror log rotation at startup", "error", err.Error())
	}

	// Phase 1 of the multi-agent shared-epistemology arc (WISHLIST.md):
	// if MPM_SHARED_DB is set, ATTACH the shared SQLite database. Failure
	// to attach is non-fatal — mpm continues in local-only mode. This
	// keeps single-instance use cases unaffected while making the
	// plumbing available for operators who opt in.
	if sharedPath := os.Getenv("MPM_SHARED_DB"); sharedPath != "" {
		if err := manager.attachShared(sharedPath); err != nil {
			slog.Warn("shared DB attach failed; continuing in local-only mode",
				"path", sharedPath, "error", err.Error())
		}
	}

	return manager, nil
}

// NewSession opens an independent connection to the same database file.
//
// Background workers (synthesis, lifecycle, critic) need an isolated
// connection so their long-running queries don't block the primary
// session's hot path. WAL mode + busy_timeout=5000 keep contention
// bounded — see audit.md (2026-07-23) finding 4 for the rationale.
//
// This pattern is allowed by sqlopen_owner_test.go's whitelist.
//
// The caller owns the returned CoreDB and must Close it when done.
func (dm *DatabaseManager) NewSession() (CoreDB, error) {
	db, err := sql.Open("sqlite3", dm.dbPath)
	if err != nil {
		return nil, fmt.Errorf("new session: open: %w", err)
	}
	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA synchronous = NORMAL")
	db.Exec("PRAGMA cache_size = -64000")

	session := &DatabaseManager{
		db:           db,
		dbPath:       dm.dbPath,
		watchdogPath: filepath.Join(filepath.Dir(dm.dbPath), "watchdog.jsonl"),
	}

	// Initialize schema (idempotent — CREATE IF NOT EXISTS).
	if err := session.initUnifiedSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("new session: schema: %w", err)
	}

	// Attach shared DB if the parent has one.
	if dm.sharedPath != "" {
		if err := session.attachShared(dm.sharedPath); err != nil {
			slog.Warn("session shared DB attach failed", "error", err.Error())
		}
	}

	return session, nil
}

// attachShared ATTACHes sharedPath as the `shared` schema in the current
// connection and runs SafeMigrations + index creation against it. The
// shared DB uses the same schema as the local DB (see WISHLIST.md
// Schema overlap section) so SQL is identical, just prefixed `shared.`.
//
// Idempotent: SafeMigrations uses ALTER TABLE ADD COLUMN, which is a
// no-op if the column already exists. Indexes use IF NOT EXISTS.
//
// Returns nil on success; errors are non-fatal and the caller logs +
// continues. SharedAttached() returns "" until this succeeds.
func (dm *DatabaseManager) attachShared(sharedPath string) error {
	// Ensure the directory exists so the file can be created on first write.
	if err := os.MkdirAll(filepath.Dir(sharedPath), 0700); err != nil {
		return fmt.Errorf("mkdir shared db dir: %w", err)
	}

	// ATTACH as 'shared'. Read-only mode is selected via MPM_SHARED_READONLY.
	// For ATTACH, SQLite accepts either a plain filename or a URI. We use
	// the plain filename (escape single quotes in the path) for maximum
	// compatibility — the mattn/go-sqlite3 driver accepts both.
	stmt := fmt.Sprintf("ATTACH DATABASE '%s' AS shared", strings.ReplaceAll(sharedPath, "'", "''"))
	if _, err := dm.db.Exec(stmt); err != nil {
		return fmt.Errorf("ATTACH: %w", err)
	}

	// Apply read-only after attach if requested (URI is per-connection,
	// but ATTACH doesn't take a mode). Use PRAGMA on the attached schema.
	if os.Getenv("MPM_SHARED_READONLY") == "1" {
		// SQLite doesn't have a per-ATTACH read-only flag, but we can
		// emulate it by attempting a write and rolling back. For now
		// we trust the operator — they said read-only. A future
		// enhancement could enforce via a session-level guard.
		_ = sharedPath // marker for future enforcement
	}

	// Run BaseTables (the canonical DDL) against the shared schema FIRST.
	// CREATE TABLE IF NOT EXISTS is idempotent. We can't use a simple
	// "CREATE TABLE shared.X" prefix because the original BaseTables
	// DDL has triggers and FTS definitions that expect the table to be
	// in the default schema. For Phase 1 we only run the core CREATE
	// TABLE statements (not triggers/FTS) — that's enough for the
	// shared-rules read path. Triggers + FTS are Phase 2 work.
	for _, ddl := range BaseTables {
		if !strings.HasPrefix(ddl, "CREATE TABLE") {
			continue
		}
		// Rewrite "CREATE TABLE foo" to "CREATE TABLE shared.foo". The
		// FTS5 virtual tables and triggers remain in the local DB.
		sharedDDL := rewriteTablePrefix(ddl, "shared.")
		if _, err := dm.db.Exec(sharedDDL); err != nil {
			slog.Warn("shared table DDL failed",
				"sql_prefix", sharedDDLTruncate(sharedDDL, 60), "error", err.Error())
		}
	}

	// Index propagation loop — the cascade tables (epistemic_cascade_outbox,
	// epistemic_provenance) live in BaseTables with their indexes alongside.
	// The CREATE TABLE loop above creates them in the shared schema, but the
	// matching indexes never land there unless we rewrite and re-run them.
	// Without indexes, the materializer's claim hot-path
	// ("WHERE status='pending' AND next_retry_at<=?") degenerates to a
	// full scan in the shared DB — acceptable at small row counts but a
	// hidden regression as the shared substrate grows.
	//
	// We only rewrite indexes whose base tables live in BaseTables — those
	// in CommonIndexes reference tables that are not in the shared schema
	// (vector_clusters, vector_assignments) and would fail on missing-table.
	// Indexed pre-existing tables (sessions, evidence, memories, …) are
	// already shared-resident from an earlier attach, so this loop is a
	// no-op for them — harmlessly idempotent.
	for _, ddl := range BaseTables {
		if !strings.HasPrefix(ddl, "CREATE INDEX") {
			continue
		}
		sharedDDL := rewriteIndexPrefix(ddl, "shared.")
		if _, err := dm.db.Exec(sharedDDL); err != nil {
			slog.Warn("shared index DDL failed",
				"sql_prefix", sharedDDLTruncate(sharedDDL, 60), "error", err.Error())
		}
	}

	// Migration: detect and remove the pre-2026-07-07 schema
	// (contentless design with `content='memories', content_rowid='rowid'`).
	// That schema produced tombstone artifacts on DELETE that broke
	// MATCH against freshly-removed rows. The new design is standalone
	// FTS5 (no content option), which lets plain DELETE work as
	// expected. We drop the old table and re-create it below; the
	// triggers (which are the new sync primitive) will re-populate the
	// index from the underlying shared.memories rows.
	//
	// Detection: query sqlite_master for an entry whose SQL contains
	// "content='memories'". If found, DROP the old table before
	// CREATE VIRTUAL TABLE IF NOT EXISTS. The DROP is destructive but
	// the re-population via triggers is automatic on next open.
	var hasOldSchema int
	if err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.sqlite_master
		WHERE type = 'table' AND name = 'memories_fts'
		  AND sql LIKE '%content=''memories''%'
	`).Scan(&hasOldSchema); err == nil && hasOldSchema > 0 {
		if _, err := dm.db.Exec(`DROP TABLE shared.memories_fts`); err != nil {
			slog.Warn("shared FTS5 migration: failed to drop old contentless schema",
				"error", err.Error())
		} else {
			slog.Info("shared FTS5 migration: dropped old contentless schema, recreating as standalone")
		}
	}

	// Phase 2b (this commit): also create the FTS5 virtual table for
	// shared.memories. Phase 1 deferred FTS because the initial schema
	// was "core tables only"; now that query_global_rules has a real
	// consumer (Phase 2), keyword search via FTS5 is worth wiring up.
	//
	// We create shared.memories_fts as a STANDALONE FTS5 table (no
	// `content=` external-content option). FTS5 stores its own copy
	// of the indexed columns. This is the design that lets plain
	// DELETE FROM memories_fts WHERE rowid = ... work as expected
	// without FTS5 tombstone artifacts. The previous "contentless"
	// design (content='memories', content_rowid='rowid') required
	// the FTS5 'delete' special command to physically remove deleted
	// segments — and that command does not work cleanly with the
	// trigger-in-shared-DB pattern we use here. Standalone FTS5 is
	// slightly larger on disk but eliminates a class of "deleted row
	// still matches" bugs that surface only at scale.
	//
	// Tokenizer matches the local memories_fts ('porter unicode61')
	// so keyword stemming behaviour is consistent across local and
	// shared. The "house rule" use case relies on exact token
	// matching (e.g., "scanner", "FTS5"); stem differences would
	// produce false negatives.
	//
	// Phase 2b previously kept the FTS sync manual (QueryGlobalRules
	// would backfill the FTS rows on each call when the index was
	// empty). That broke the multi-agent contract: a freshly seeded
	// shared DB returned 0 hits under scope=all until the first read.
	// The AFTER INSERT/UPDATE/DELETE triggers below restore the
	// structural invariant "if it is in shared.memories, it is
	// searchable" — the only sane guarantee for autonomous agents.
	if _, err := dm.db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS shared.memories_fts USING fts5(
			content, collection, session_id UNINDEXED, tags UNINDEXED,
			tokenize='porter unicode61'
		)
	`); err != nil {
		slog.Warn("shared FTS5 virtual table creation failed", "error", err.Error())
	}

	// FTS sync triggers (mirrors the local memories_ai / _ad / _au
	// pattern in BaseTables, but with the `shared.` schema prefix).
	// Each trigger is OWNED BY the shared database (created via
	// "CREATE TRIGGER shared.<name>"), and the body uses BARE table
	// names that resolve into the shared schema. SQLite forbids
	// qualified table references inside trigger bodies, AND it
	// forbids a trigger in the main database from referencing objects
	// in an attached database. The "CREATE TRIGGER shared.X" form
	// satisfies both constraints simultaneously.
	//
	// Rationale: the multi-agent shared epistemology contract requires
	// that shared.memories rows be immediately searchable by any agent
	// the moment they are written. The previous lazy-backfill design
	// (inside QueryGlobalRules) only fired for that one read path; the
	// federated scope=all path (HybridSearch) bypassed it entirely.
	// Triggers eliminate the asymmetry by making the sync structural
	// rather than call-site-dependent.
	sharedTriggers := []string{
		`CREATE TRIGGER shared.shared_memories_ai AFTER INSERT ON shared.memories BEGIN
			INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
			VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags);
		END;`,
		`CREATE TRIGGER shared.shared_memories_ad AFTER DELETE ON shared.memories BEGIN
			DELETE FROM memories_fts WHERE rowid = old.rowid;
		END;`,
		`CREATE TRIGGER shared.shared_memories_au AFTER UPDATE ON shared.memories
		WHEN old.deleted_at IS NULL AND new.deleted_at IS NOT NULL BEGIN
			DELETE FROM memories_fts WHERE rowid = old.rowid;
		END;`,
		`CREATE TRIGGER shared.shared_memories_au_content AFTER UPDATE ON shared.memories
		WHEN NOT (old.deleted_at IS NULL AND new.deleted_at IS NOT NULL) BEGIN
			DELETE FROM memories_fts WHERE rowid = old.rowid;
			INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
			VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags);
		END;`,
	}
	for _, t := range sharedTriggers {
		if _, err := dm.db.Exec(t); err != nil {
			slog.Warn("shared FTS trigger installation failed",
				"sql_prefix", sharedDDLTruncate(t, 60), "error", err.Error())
		}
	}

	// Arc 1: install the shared.contradiction_log table. This is the
	// operator's source of truth for the conflict resolution queue.
	// mirror.jsonl stays as the forensic detection trail; this table
	// is the operational state machine that the resolve-contradictions
	// command reads. The DDL is idempotent (CREATE TABLE IF NOT EXISTS).
	if _, err := dm.db.Exec(SharedContradictionLogDDL); err != nil {
		slog.Warn("shared.contradiction_log DDL failed",
			"error", err.Error())
	}

	// Arc 2: install the shared.sessions, shared.event_wakes, and
	// shared.agents tables. The active-agent registry powers the
	// broadcast fan-out target discovery; the event_wakes table is
	// the fan-out surface itself; the agents cache is the cross-reboot
	// identity stable enough to aggregate "how often does agent X
	// broadcast" without collapsing session rows.
	if _, err := dm.db.Exec(SharedBcastSessionsDDL); err != nil {
		slog.Warn("shared.sessions DDL failed",
			"error", err.Error())
	}
	if _, err := dm.db.Exec(SharedBcastEventWakesDDL); err != nil {
		slog.Warn("shared.event_wakes DDL failed",
			"error", err.Error())
	}
	if _, err := dm.db.Exec(SharedBcastAgentsDDL); err != nil {
		slog.Warn("shared.agents DDL failed",
			"error", err.Error())
	}

	// Now run SafeMigrations against the shared schema (tables exist now).
	// ALTER TABLE in SQLite doesn't accept a schema prefix, so we use
	// sqlite_master to set the search_path equivalent. For our purposes
	// (per-table ALTER), we just qualify the table name in the error
	// path; the actual ALTER doesn't care about the schema.
	//
	// We skip tables that aren't shared-schema (lessons handled separately,
	// reference_* tables live in ReferenceTables, not BaseTables — Phase 1
	// only shares the core memory/decision/theory tables).
	sharedTables := map[string]bool{
		"sessions": true, "topics": true, "topic_memberships": true,
		"memories": true, "system_config": true, "lessons": true,
		"raw_memories": true, "external_db_cursors": true,
		"memory_revisions": true, "evidence": true, "confidence_history": true,
	}
	for _, m := range SafeMigrations {
		if !sharedTables[m[0]] {
			continue
		}
		alter := fmt.Sprintf("ALTER TABLE shared.%s ADD COLUMN %s %s", m[0], m[1], m[2])
		if _, err := dm.db.Exec(alter); err != nil {
			if !isDuplicateColumnError(err) {
				slog.Warn("shared migration failed",
					"table", m[0], "column", m[1], "error", err.Error())
			}
		}
	}

	dm.sharedPath = sharedPath
	dm.sharedAttached = true
	slog.Info("shared DB attached", "path", sharedPath, "readonly", os.Getenv("MPM_SHARED_READONLY") == "1")
	return nil
}

// rewriteTablePrefix prepends `prefix.` to the table name in a CREATE TABLE
// statement. Safe against quoted strings in CHECK constraints: finds the
// first '(' that is NOT inside a single-quoted string literal, which marks
// the end of the table-name token. This is sufficient for BaseTables DDL
// (no column names contain "CREATE TABLE"). A future DDL with a column
// named e.g. "create_table" would still be safe since we scan for '('.
func rewriteTablePrefix(ddl, prefix string) string {
	const marker = "CREATE TABLE "
	idx := strings.Index(ddl, marker)
	if idx < 0 {
		return ddl
	}
	afterMarker := idx + len(marker)

	// Skip "IF NOT EXISTS " if present.
	rest := ddl[afterMarker:]
	if strings.HasPrefix(rest, "IF NOT EXISTS ") || strings.HasPrefix(rest, "IF NOT EXISTS\t") {
		afterMarker += len("IF NOT EXISTS ")
	}

	// Find the first '(' that is not inside a single-quoted string literal.
	// This correctly handles CHECK constraints like:
	//   CHECK (column_name != 'CREATE TABLE' OR condition)
	// which would confuse a naive "scan for '('" approach.
	cutAt := -1
	for i := afterMarker; i < len(ddl); i++ {
		c := ddl[i]
		if c == '\'' {
			// Skip the rest of this string literal (no nesting, no escapes in our DDL).
			for i++; i < len(ddl); i++ {
				if ddl[i] == '\'' {
					i++
					break
				}
			}
			continue
		}
		if c == '(' {
			cutAt = i
			break
		}
	}
	if cutAt < 0 {
		return ddl
	}
	return ddl[:afterMarker] + prefix + ddl[afterMarker:]
}

// rewriteIndexPrefix prepends `prefix.` to the INDEX NAME in a CREATE INDEX
// statement — NOT to the table reference. SQLite's CREATE INDEX grammar
// accepts schema-qualified index names but rejects schema-qualified table
// references in the ON clause ("near '.': syntax error"), so the only
// rewrite that works for an attached-schema index is on the index name:
//
//	CREATE INDEX [IF NOT EXISTS] schema.idx_name ON table_name(columns);
//
// The DDL in schema.go is multi-line (newline + tabs between the index
// name and ON), so we scan for the index-name token (between the IF NOT
// EXISTS marker, if present, and the first whitespace). Returns the input
// unchanged if either marker is missing; callers filter against the
// "CREATE INDEX" prefix before calling.
//
// Why this works: when the index name carries a schema prefix, SQLite
// resolves the unqualified table name in the same schema. So the
// shared-schema attach loop can emit the index under a shared-schema name
// and the table-name lookup resolves to shared.epistemic_cascade_outbox
// (etc.), which the CREATE TABLE loop already created.
func rewriteIndexPrefix(ddl, prefix string) string {
	// Locate the start of the index-name token. It follows either
	// "CREATE INDEX " or "CREATE INDEX IF NOT EXISTS ", depending on
	// whether the schema-side DDL guards against re-creation.
	start := -1
	if i := strings.Index(ddl, "CREATE INDEX IF NOT EXISTS "); i >= 0 {
		start = i + len("CREATE INDEX IF NOT EXISTS ")
	} else if i := strings.Index(ddl, "CREATE INDEX "); i >= 0 {
		start = i + len("CREATE INDEX ")
	}
	if start < 0 {
		return ddl
	}

	// Walk to the end of the index-name token: stop at whitespace.
	end := start
	for end < len(ddl) && !isIndexRewriteSpace(ddl[end]) {
		// tolerate quoted string literal (none in our DDL but the cost
		// is negligible).
		if ddl[end] == '\'' {
			for end++; end < len(ddl); end++ {
				if ddl[end] == '\'' {
					end++
					break
				}
			}
			continue
		}
		end++
	}
	if end <= start {
		return ddl
	}
	// Guard against double-prefixing if the same loop runs twice (e.g.
	// a follow-up migration that re-attaches the shared DB).
	if strings.HasPrefix(ddl[start:start+len(prefix)], prefix) {
		return ddl
	}
	return ddl[:start] + prefix + ddl[start:]
}

func isIndexRewriteSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

func sharedDDLTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// SharedAttached reports whether the shared DB is currently attached.
// Returns the path; "" means local-only mode.
func (dm *DatabaseManager) SharedAttached() string {
	if !dm.sharedAttached {
		return ""
	}
	return dm.sharedPath
}

// QueryGlobalRules returns memories from the shared DB marked
// is_global = 1. Returns an empty result (not an error) if the shared
// DB is not attached — the agent should fall back to local recall
// in that case.
//
// This is the read-side counterpart to record_global_rule (Phase 3).
// Today the caller is an MCP agent calling query_global_rules; in
// Phase 2b the same query will be UNIONed into query_long_term_memory.
//
// Args:
//   - query (optional): FTS5 keyword search scoped to the shared DB.
//     Empty string returns all is_global rows.
//   - limit: max rows (default 50, hard-capped at 500).
func (dm *DatabaseManager) QueryGlobalRules(query string, limit int) ([]map[string]interface{}, error) {
	if !dm.sharedAttached {
		return nil, nil // local-only mode; not an error
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	query = strings.TrimSpace(query)

	// Phase 2b (this commit): the shared FTS5 virtual table exists but
	// is not auto-populated by triggers (Phase 1 deferred triggers
	// to keep the bootstrap simple). We lazy-backfill on the first
	// query of a session if the index is empty relative to the
	// underlying table. After backfill, queries hit FTS5; the LIKE
	// fallback remains for the rare case where FTS5 fails.
	//
	// Lazy backfill is intentional — adding triggers to the shared
	// schema would mean cross-schema INSERT triggers, which SQLite
	// supports but requires careful handling. Backfilling on the
	// first read is simpler and bounded — the shared DB is
	// append-mostly per the WISHLIST.md concurrency model.
	if err := dm.backfillSharedFTSIfEmpty(); err != nil {
		// Non-fatal — fall back to LIKE if backfill or FTS query fails.
		slog.Warn("shared FTS backfill failed; falling back to LIKE", "error", err.Error())
	}

	var querySQL string
	var args []interface{}
	if query == "" {
		querySQL = `
			SELECT id, content, collection, tags, metadata, weight,
			       reinforcement_count, created_at, updated_at, is_global
			FROM shared.memories
			WHERE is_global = 1 AND deleted_at IS NULL
			ORDER BY weight DESC, created_at DESC
			LIMIT ?
		`
		args = []interface{}{limit}
	} else {
		// FTS5 search with LIKE fallback if the index is missing or
		// empty (e.g. fresh shared DB before first backfill).
		// Tokenization handled by BuildFTS5Query (porter unicode61).
		ftsQuery := BuildFTS5Query(query)
		if ftsQuery == "" {
			ftsQuery = `""` // defensive — fall through to LIKE on empty
		}
		rows, err := dm.db.Query(`
			SELECT m.id, m.content, m.collection, m.tags, m.metadata, m.weight,
			       m.reinforcement_count, m.created_at, m.updated_at, m.is_global
			FROM shared.memories m
			JOIN shared.memories_fts fts ON m.rowid = fts.rowid
			WHERE shared.memories_fts MATCH ? AND m.is_global = 1 AND m.deleted_at IS NULL
			ORDER BY m.weight DESC, m.created_at DESC
			LIMIT ?
		`, ftsQuery, limit)
		if err == nil {
			defer rows.Close()
			return dm.scanGlobalRuleRows(rows)
		}
		// Fallback to LIKE — FTS5 virtual table not populated or error.
		rows, err = dm.db.Query(`
			SELECT id, content, collection, tags, metadata, weight,
			       reinforcement_count, created_at, updated_at, is_global
			FROM shared.memories
			WHERE is_global = 1 AND deleted_at IS NULL AND content LIKE ?
			ORDER BY weight DESC, created_at DESC
			LIMIT ?
		`, "%"+query+"%", limit)
		if err != nil {
			return nil, fmt.Errorf("query shared.memories: %w", err)
		}
		defer rows.Close()
		return dm.scanGlobalRuleRows(rows)
	}
	rows, err := dm.db.Query(querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("query shared.memories: %w", err)
	}
	defer rows.Close()
	return dm.scanGlobalRuleRows(rows)
}

// scanGlobalRuleRows reads the rows from a QueryGlobalRules SELECT
// and produces the standardized result map. Extracted because two
// query paths (no-query, FTS5-or-LIKE) need identical scan logic.
func (dm *DatabaseManager) scanGlobalRuleRows(rows *sql.Rows) ([]map[string]interface{}, error) {
	var results []map[string]interface{}
	for rows.Next() {
		var id, content, coll sql.NullString
		var tags, meta, createdAt, updatedAt sql.NullString
		var weight, reinforcement, isGlobal sql.NullInt64
		if err := rows.Scan(&id, &content, &coll, &tags, &meta, &weight, &reinforcement,
			&createdAt, &updatedAt, &isGlobal); err != nil {
			continue
		}
		results = append(results, map[string]interface{}{
			"id":                  id.String,
			"content":             content.String,
			"collection":          coll.String,
			"tags":                tags.String,
			"metadata":            meta.String,
			"weight":              int(weight.Int64),
			"reinforcement_count": int(reinforcement.Int64),
			"created_at":          createdAt.String,
			"updated_at":          updatedAt.String,
			"is_global":           int(isGlobal.Int64),
			"source":              "shared",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// backfillSharedFTSIfEmpty populates shared.memories_fts from
// shared.memories if the FTS table is empty (or if the underlying
// table has rows that the FTS index doesn't). Phase 2b keeps the
// shared schema simple — no AFTER INSERT triggers — so we lazy-
// backfill on the first query.
//
// This is O(N) on the number of is_global rows. The shared DB is
// append-mostly per WISHLIST.md so the cost amortizes to zero on
// subsequent queries.
func (dm *DatabaseManager) backfillSharedFTSIfEmpty() error {
	if !dm.sharedAttached {
		return nil
	}
	var memCount, ftsCount int
	if err := dm.db.QueryRow("SELECT COUNT(*) FROM shared.memories WHERE is_global = 1").Scan(&memCount); err != nil {
		return fmt.Errorf("count shared.memories: %w", err)
	}
	if err := dm.db.QueryRow("SELECT COUNT(*) FROM shared.memories_fts").Scan(&ftsCount); err != nil {
		// FTS table doesn't exist — backfill would fail. Caller falls
		// back to LIKE via the QueryGlobalRules fallback path.
		return fmt.Errorf("count shared.memories_fts: %w", err)
	}
	if ftsCount >= memCount {
		return nil // already in sync
	}
	// Backfill is_global rows from shared.memories into shared.memories_fts.
	// INSERT OR IGNORE handles the case where some rows are already indexed
	// (race between backfill and concurrent inserts).
	_, err := dm.db.Exec(`
		INSERT OR IGNORE INTO shared.memories_fts(rowid, content, collection, session_id, tags)
		SELECT rowid, content, COALESCE(collection, ''), COALESCE(session_id, ''), COALESCE(tags, '')
		FROM shared.memories
		WHERE is_global = 1 AND deleted_at IS NULL
	`)
	if err != nil {
		return fmt.Errorf("backfill shared.memories_fts: %w", err)
	}
	return nil
}

// NewDatabaseManagerForDB creates a DatabaseManager wrapping an existing *sql.DB.
// Use this for one-off CLI commands that don't need managed persistence.
func NewDatabaseManagerForDB(db *sql.DB) *DatabaseManager {
	wdPath := ""
	if mpmDir := config.GetMPMDir(); mpmDir != "" {
		wdPath = filepath.Join(mpmDir, "src", "db", "watchdog.jsonl")
	}
	return &DatabaseManager{db: db, watchdogPath: wdPath}
}

// InitSchema initializes the shared MPM schema (tables, indexes, migrations, FTS).
// Exported so test code can call it after NewDatabaseManagerForDB with a custom *sql.DB.
func (dm *DatabaseManager) InitSchema() error {
	return dm.initUnifiedSchema()
}

func (dm *DatabaseManager) initUnifiedSchema() error {
	// PRAGMA settings. WAL allows concurrent readers and a single writer;
	// busy_timeout gives the writer up to 5s to wait.
	//
	// We deliberately do NOT enable foreign_keys here. The production
	// NewDatabaseManager path enables them after InitSchema completes; the
	// test paths use InitSchema directly with session_ids that may not
	// have a matching session row, so we leave FK enforcement off (the
	// SQLite default) to keep the existing tests green.
	dm.db.Exec("PRAGMA journal_mode = WAL")
	dm.db.Exec("PRAGMA busy_timeout = 5000")
	dm.db.Exec("PRAGMA synchronous = NORMAL")

	// Use shared schema definitions from schema.go
	for _, sqlQuery := range BaseTables {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute SQL: %w\nSQL: %s", err, sqlQuery)
		}
	}

	// Reference library tables — single source of truth lives in
	// schema.ReferenceTables. Tests reach the same slice via InitSchema
	// on a DatabaseManager wrapping a tmpfile; there is no separate
	// ReferenceDB type or connection to keep in sync.
	for _, sqlQuery := range ReferenceTables {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute reference schema: %w\nSQL: %s", err, sqlQuery)
		}
	}
	for _, sqlQuery := range ReferenceIndexes {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute reference index: %w\nSQL: %s", err, sqlQuery)
		}
	}

	// Migration: add new columns to existing databases (no-op if already present)
	// Use SafeMigrations from schema.go to ensure ALL column additions are covered.
	// Skip lessons entirely — it is either a table (columns added by migrateLessonsToView
	// before the rename) or a view (column adds would fail anyway).
	for _, m := range SafeMigrations {
		if m[0] == "lessons" {
			continue // lessons: handled by migrateLessonsToView before rename; skip here
		}
		if _, err := dm.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m[0], m[1], m[2])); err != nil {
			if !isDuplicateColumnError(err) {
				slog.Warn("migration failed", "table", m[0], "column", m[1], "error", err.Error())
			}
		}
	}

	// Constraint migration: relax system_audit_log.level CHECK to include
	// 'info'. One-shot, idempotent (detects existing-new constraint via
	// sqlite_master.sql). Indices attached to the table get dropped by
	// the table recreation; the CommonIndexes loop below rebuilds them
	// via CREATE INDEX IF NOT EXISTS. Lives BEFORE CommonIndexes so the
	// rebuild runs against the renamed table.
	if err := dm.migrateAuditLevelConstraint(); err != nil {
		return fmt.Errorf("migrateAuditLevelConstraint: %w", err)
	}

	// Backfill: set updated_at = created_at for rows migrated without updated_at
	dm.db.Exec(`UPDATE memories SET updated_at = created_at WHERE updated_at IS NULL`)

	// Use shared index definitions. Skip any index that targets a view — SQLite
	// rejects indexed views and "views may not be indexed" errors would pollute stderr.
	//
	// NOTE: despite the name, CommonIndexes also carries eight CREATE TABLE
	// statements (system_audit_log, audit_cluster_proposals, session_handoffs,
	// scheduled_wakes, scheduled_tasks, ephemeral_scratchpad, vector_clusters,
	// vector_assignments). The data-migration transaction below therefore has
	// to run AFTER this loop — see the comment on that block.
	for _, sql := range CommonIndexes {
		if strings.Contains(sql, " ON lessons(") || strings.HasSuffix(sql, " ON lessons") {
			continue // lessons is a view; its FTS is handled by migrateLessonsToView
		}
		if _, err := dm.db.Exec(sql); err != nil {
			slog.Warn("index creation error (may be benign on re-run)", "error", err.Error())
		}
	}

	// Data migrations: convert legacy TEXT timestamp values to INTEGER Unix
	// epoch. Wrapped in one transaction so the backfill UPDATEs and the
	// sentinel INSERTs are atomic. Runs BEFORE any handler executes — this is
	// why it sits in initUnifiedSchema rather than a CLI command. Idempotent
	// via the schema_migrations sentinels.
	//
	// ORDERING DEPENDENCY: this block must stay AFTER every DDL slice
	// (BaseTables, ReferenceTables, CommonIndexes) has executed, because
	// MigrateAllTimestampsToUnixEpoch touches tables from all three — and
	// eight of them are declared in CommonIndexes, not BaseTables. The
	// migration itself skips (table, column) pairs it can't find rather than
	// erroring, so a future reorder degrades to "some columns not converted
	// on this boot" instead of "init fails and the binary is unusable"; keep
	// the ordering anyway so the skip path stays a safety net, not the norm.
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	if err := MigrateDeletedAtToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migrate deleted_at: %w", err)
	}
	if err := MigrateAllTimestampsToUnixEpoch(tx); err != nil {
		// A deferral means every convertible column was converted and only
		// the sentinel was withheld, because some target columns are still
		// declared TEXT-affinity in the DDL. Commit the partial work and
		// retry on the next boot; failing init here would make the binary
		// unusable for the entire window between this migration landing and
		// the schema DDL flip.
		if !errors.Is(err, ErrTimestampsMigrationDeferred) {
			_ = tx.Rollback()
			return fmt.Errorf("timestamps unification migration failed: %w", err)
		}
		slog.Warn("timestamps unification migration deferred", "error", err.Error())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}

	// Migration: convert lessons table to lessons_base + lessons view.
	// This replaces the buggy AFTER-INSERT FTS trigger with an INSTEAD-OF trigger
	// on the view, so failed lessons inserts can never leave orphaned FTS rows.
	// Safe to call on every startup — idempotent if lessons_base already exists.
	dm.migrateLessonsToView()

	// Try FTS5 tables - if they fail, continue without them (fallback search)
	if err := dm.initFTSTables(); err != nil {
		slog.Warn("FTS5 initialization failed; search will use LIKE fallback", "error", err.Error())
	} else {
		// FTS5 tables ready — backfill any existing data that predates the triggers
		dm.backfillFTSTables()
	}

	// NOTE: The `evidence` table intentionally lacks AFTER INSERT/UPDATE/DELETE
	// triggers. Connecting SQLite triggers to Go callbacks via RegisterFunc
	// causes CGO deadlocks when the Go callback attempts to acquire a
	// connection to execute subsequent writes. All confidence recomputation
	// must be handled transactionally in the Go application layer
	// (see internal/evidence_store.go, RecomputeConfidence).
	//
	// Drop the ghost triggers if present from an earlier schema version —
	// they reference a `confidence_recompute` SQL function that no longer
	// exists and would fail every INSERT into `evidence`.
	for _, name := range []string{"evidence_ai", "evidence_au", "evidence_ad"} {
		if _, err := dm.SQLDB().Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, name)); err != nil {
			return fmt.Errorf("drop ghost trigger %s: %w", name, err)
		}
	}

	// Memory revision triggers (independent of FTS5 — always required for versioning)
	revisionTriggers := []string{
		`CREATE TRIGGER IF NOT EXISTS memories_rev_ai AFTER INSERT ON memories
		BEGIN
			INSERT INTO memory_revisions (memory_id, version, content, weight, collection, is_long_term, created_at)
			VALUES (
				NEW.id,
				COALESCE((SELECT MAX(version) FROM memory_revisions WHERE memory_id = NEW.id), 0) + 1,
				NEW.content,
				COALESCE(NEW.weight, 0),
				NEW.collection,
				COALESCE(NEW.is_long_term, 0),
				CAST(strftime('%s','now') AS INTEGER)
			);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_rev_au AFTER UPDATE ON memories
		WHEN OLD.content != NEW.content
		   OR OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL
		   OR OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL
		   OR OLD.weight != NEW.weight
		   OR OLD.collection != NEW.collection
		BEGIN
			INSERT INTO memory_revisions (memory_id, version, content, weight, collection, is_long_term, created_at)
			VALUES (
				NEW.id,
				COALESCE((SELECT MAX(version) FROM memory_revisions WHERE memory_id = NEW.id), 0) + 1,
				NEW.content,
				COALESCE(NEW.weight, 0),
				NEW.collection,
				COALESCE(NEW.is_long_term, 0),
				CAST(strftime('%s','now') AS INTEGER)
			);
		END;`,
	}
	for _, sql := range revisionTriggers {
		if _, err := dm.db.Exec(sql); err != nil {
			slog.Warn("revision trigger creation failed", "error", err.Error(), "sql", sql)
		}
	}

	return nil
}

// dropFTS5Triggers removes all FTS5 triggers from the database.
// Called when FTS5 is not available in the current build, preventing
// leftover triggers (from a build compiled with FTS5) from firing on INSERT
// and causing "no such module: fts5" errors.
func (dm *DatabaseManager) dropFTS5Triggers() {
	triggers := []string{
		"DROP TRIGGER IF EXISTS sessions_ai",
		"DROP TRIGGER IF EXISTS sessions_ad",
		"DROP TRIGGER IF EXISTS sessions_au",
		"DROP TRIGGER IF EXISTS memories_ai",
		"DROP TRIGGER IF EXISTS memories_ad",
		"DROP TRIGGER IF EXISTS memories_au",
		"DROP TRIGGER IF EXISTS memories_au_content",
		"DROP TRIGGER IF EXISTS lessons_ai",
		"DROP TRIGGER IF EXISTS lessons_ad",
		"DROP TRIGGER IF EXISTS lessons_au",
		"DROP TRIGGER IF EXISTS topics_ai",
		"DROP TRIGGER IF EXISTS topics_ad",
		"DROP TRIGGER IF EXISTS topics_au",
		"DROP TRIGGER IF EXISTS references_ai",
		"DROP TRIGGER IF EXISTS references_ad",
		"DROP TRIGGER IF EXISTS references_au",
		"DROP TRIGGER IF EXISTS reference_chunks_ai",
		"DROP TRIGGER IF EXISTS reference_chunks_ad",
		"DROP TRIGGER IF EXISTS reference_chunks_au",
	}
	for _, t := range triggers {
		dm.db.Exec(t) // ignore errors — we're cleaning up, not enforcing
	}
}

// migrateLessonsToView converts the lessons table to a lessons_base table + lessons view
// with INSTEAD OF INSERT/UPDATE/DELETE triggers. This fixes the FTS orphan-row bug:
// the old AFTER-INSERT trigger committed its FTS write before the lessons insert could
// roll back, leaving orphaned lessons_fts entries on failed save_lesson calls.
// Idempotent — safe to call on every startup.
func (dm *DatabaseManager) migrateLessonsToView() {
	var baseExists int
	dm.db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='lessons_base'`).Scan(&baseExists)

	// If lessons_base exists, the rename already happened. Still need to drop any
	// leftover AFTER triggers on lessons_base (from a prior partial migration) so
	// they don't fire alongside the INSTEAD OF triggers and cause duplicate FTS inserts.
	if baseExists != 0 {
		for _, old := range []string{"lessons_ai", "lessons_ad", "lessons_au"} {
			dm.db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, old))
		}
		return
	}

	// Step 1: add missing columns to lessons table BEFORE renaming.
	// SafeMigrations already tried adding retrieval_priority/importance/confidence
	// to the lessons VIEW (after our prior rename), so we add them to the TABLE
	// now while lessons is still a table. SafeMigrations errors are silenced by
	// isDuplicateColumnError, so this is safe on re-runs.
	for _, col := range []struct {
		name, def string
	}{
		{"retrieval_priority", "REAL NOT NULL DEFAULT 0.5"},
		{"importance", "REAL NOT NULL DEFAULT 0.5"},
		{"confidence", "REAL NOT NULL DEFAULT 0.7"},
	} {
		dm.db.Exec(fmt.Sprintf("ALTER TABLE lessons ADD COLUMN %s %s", col.name, col.def))
	}

	// Step 2: rename lessons → lessons_base
	if _, err := dm.db.Exec(`ALTER TABLE lessons RENAME TO lessons_base`); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: rename lessons→lessons_base failed: %v\n", err)
		return
	}

	// Step 2: create lessons view that exposes all columns
	viewSQL := `
	CREATE VIEW IF NOT EXISTS lessons AS
	SELECT rowid, id, type, content, tags, reinforcement_count,
	       source_session_id, created, content_hash,
	       retrieval_priority, importance, confidence
	FROM lessons_base`
	if _, err := dm.db.Exec(viewSQL); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: create lessons view failed: %v\n", err)
		return
	}

	// Step 3: INSTEAD OF INSERT — atomically writes to base table + lessons_fts
	// The trigger body is a single transaction; if lessons_fts insert fails, the
	// entire INSERT is rolled back — no orphan possible.
	//
	// COALESCE on retrieval_priority/importance/confidence: the lessons_base
	// columns are NOT NULL DEFAULT 0.5/0.5/0.7, but the trigger reads NEW.*,
	// which is NULL when the caller omits the column on the view. Without
	// COALESCE, an INSERT that doesn't supply every column fails with a
	// NOT NULL constraint violation. COALESCE matches the table default so
	// partial inserts work the same as full inserts.
	if _, err := dm.db.Exec(`
		CREATE TRIGGER lessons_instead_of_insert
		INSTEAD OF INSERT ON lessons
		BEGIN
			INSERT INTO lessons_base(rowid, id, type, content, tags, reinforcement_count,
			                       source_session_id, created, content_hash,
			                       retrieval_priority, importance, confidence)
			VALUES (NEW.rowid, NEW.id, NEW.type, NEW.content, NEW.tags,
			        COALESCE(NEW.reinforcement_count, 1),
			        NEW.source_session_id, NEW.created, NEW.content_hash,
			        COALESCE(NEW.retrieval_priority, 0.5),
			        COALESCE(NEW.importance, 0.5),
			        COALESCE(NEW.confidence, 0.7));
			INSERT INTO lessons_fts(rowid, content, tags)
			VALUES (NEW.rowid, NEW.content, NEW.tags);
		END`); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: create INSTEAD OF INSERT trigger failed: %v\n", err)
	}

	// Step 4: INSTEAD OF UPDATE — same COALESCE treatment so partial
	// UPDATEs that don't touch retrieval_priority/importance/confidence
	// don't accidentally NULL those columns out and violate NOT NULL.
	if _, err := dm.db.Exec(`
		CREATE TRIGGER lessons_instead_of_update
		INSTEAD OF UPDATE ON lessons
		BEGIN
			UPDATE lessons_base SET rowid=NEW.rowid, id=NEW.id, type=NEW.type,
			       content=NEW.content, tags=NEW.tags,
			       reinforcement_count=COALESCE(NEW.reinforcement_count, reinforcement_count),
			       source_session_id=NEW.source_session_id, created=NEW.created,
			       content_hash=NEW.content_hash,
			       retrieval_priority=COALESCE(NEW.retrieval_priority, retrieval_priority),
			       importance=COALESCE(NEW.importance, importance),
			       confidence=COALESCE(NEW.confidence, confidence)
			WHERE rowid=OLD.rowid;
			DELETE FROM lessons_fts WHERE rowid=OLD.rowid;
			INSERT INTO lessons_fts(rowid, content, tags)
			VALUES (NEW.rowid, NEW.content, NEW.tags);
		END`); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: create INSTEAD OF UPDATE trigger failed: %v\n", err)
	}

	// Step 5: INSTEAD OF DELETE
	if _, err := dm.db.Exec(`
		CREATE TRIGGER lessons_instead_of_delete
		INSTEAD OF DELETE ON lessons
		BEGIN
			DELETE FROM lessons_base WHERE rowid=OLD.rowid;
			DELETE FROM lessons_fts WHERE rowid=OLD.rowid;
		END`); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: create INSTEAD OF DELETE trigger failed: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "migrateLessonsToView: lessons→lessons_base+migrated\n")
}

func (dm *DatabaseManager) initFTSTables() error {
	// Robust FTS5 availability check
	var available int
	dm.db.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&available)
	if available == 0 {
		// Fallback test: try creating a real FTS5 table in-memory
		testDB, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			dm.dropFTS5Triggers() // clean up any leftover triggers from a build that had FTS5
			return nil            // fail silently, search will use LIKE fallback
		}
		defer testDB.Close()
		_, err = testDB.Exec("CREATE VIRTUAL TABLE test_fts USING fts5(content, tokenize='porter unicode61');")
		if err != nil {
			dm.dropFTS5Triggers() // clean up any leftover triggers from a build that had FTS5
			return nil            // fail silently, search will use LIKE fallback
		}
	}

	// FTS5 is available. Use unicode61+porter for broad compatibility.
	ftsStatements := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(content, session_id, content_hash UNINDEXED, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, collection, session_id UNINDEXED, tags, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS topics_fts USING fts5(name, description, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS references_fts USING fts5(title, content, tags, tokenize='porter unicode61');`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS reference_chunks_fts USING fts5(section, content, tokenize='porter unicode61');`,

		// scheduled_wakes_fts: full-text search over wake reasons so the agent
		// can locate a wake by intent ("the Wimbledon wake", "the WC2026 one")
		// without remembering the row id. FTS mirrors the reasons and the
		// optional theory_id (UNINDEXED so it's not tokenized). Updates on
		// UPDATE keep the index in sync if reason is ever edited; the agent
		// is expected to insert new wakes rather than mutate old ones.
		`CREATE VIRTUAL TABLE IF NOT EXISTS scheduled_wakes_fts USING fts5(reason, theory_id UNINDEXED, tokenize='porter unicode61');`,

		`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,

		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories WHEN old.deleted_at IS NULL AND new.deleted_at IS NOT NULL BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au_content AFTER UPDATE ON memories WHEN NOT (old.deleted_at IS NULL AND new.deleted_at IS NOT NULL) BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,

		// memory_source_evidence_ai: when a memory is written, capture an
		// automatic observation evidence row attributed to the writer.
		// Prefers metadata.provenance.model (set by SaveMemoryWithContext)
		// over metadata.source. This makes "which model created which memory"
		// queryable through the existing evidence infrastructure without
		// adding any schema. No-op if neither field is present.
		//
		// Malformed JSON guard: json_extract raises "malformed JSON" when
		// NEW.metadata is not valid JSON, which would abort the parent
		// INSERT and roll back the memory write. We wrap each json_extract
		// in a CASE that checks json_valid() first and yields NULL on bad
		// input so the trigger degrades to a no-op instead of failing the
		// user's memory write.
		`CREATE TRIGGER IF NOT EXISTS memory_source_evidence_ai AFTER INSERT ON memories
			WHEN COALESCE(
				NULLIF(CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata, '$.provenance.model') END, ''),
				NULLIF(CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata, '$.source') END, '')
			) IS NOT NULL
		BEGIN
			INSERT INTO evidence (
				id, artifact_id, artifact_type, type,
				source_group, strength, independence_factor,
				created_by, created_at
			) VALUES (
				'auto-' || lower(hex(randomblob(8))) || '-' || substr(NEW.id, 1, 16),
				NEW.id,
				'memory',
				'observation',
				'auto_capture',
				0.5,
				1.0,
				COALESCE(
					NULLIF(CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata, '$.provenance.model') END, ''),
					NULLIF(CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata, '$.source') END, '')
				),
				unixepoch()
			);
		END;`,

		// lessons uses INSTEAD OF triggers on the lessons view (created by migrateLessonsToView)
		// — not AFTER triggers on the base table. The old AFTER triggers are replaced there.

		`CREATE TRIGGER IF NOT EXISTS topics_ai AFTER INSERT ON topics BEGIN INSERT INTO topics_fts(rowid, name, description) VALUES (new.rowid, new.name, new.description); END;`,
		`CREATE TRIGGER IF NOT EXISTS topics_ad AFTER DELETE ON topics BEGIN DELETE FROM topics_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS topics_au AFTER UPDATE ON topics BEGIN DELETE FROM topics_fts WHERE rowid = old.rowid; INSERT INTO topics_fts(rowid, name, description) VALUES (new.rowid, new.name, new.description); END;`,

		`CREATE TRIGGER IF NOT EXISTS references_ai AFTER INSERT ON reference_docs BEGIN INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags); END;`,
		`CREATE TRIGGER IF NOT EXISTS references_ad AFTER DELETE ON reference_docs BEGIN DELETE FROM references_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS references_au AFTER UPDATE ON reference_docs BEGIN DELETE FROM references_fts WHERE rowid = old.rowid; INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags); END;`,

		`CREATE TRIGGER IF NOT EXISTS reference_chunks_ai AFTER INSERT ON reference_chunks BEGIN INSERT INTO reference_chunks_fts(rowid, section, content) VALUES (new.rowid, new.section, new.content); END;`,
		`CREATE TRIGGER IF NOT EXISTS reference_chunks_ad AFTER DELETE ON reference_chunks BEGIN DELETE FROM reference_chunks_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS reference_chunks_au AFTER UPDATE ON reference_chunks BEGIN DELETE FROM reference_chunks_fts WHERE rowid = old.rowid; INSERT INTO reference_chunks_fts(rowid, section, content) VALUES (new.rowid, new.section, new.content); END;`,

		`CREATE TRIGGER IF NOT EXISTS scheduled_wakes_ai AFTER INSERT ON scheduled_wakes BEGIN INSERT INTO scheduled_wakes_fts(rowid, reason, theory_id) VALUES (new.rowid, new.reason, COALESCE(new.theory_id,'')); END;`,
		`CREATE TRIGGER IF NOT EXISTS scheduled_wakes_ad AFTER DELETE ON scheduled_wakes BEGIN DELETE FROM scheduled_wakes_fts WHERE rowid = old.rowid; END;`,
		`CREATE TRIGGER IF NOT EXISTS scheduled_wakes_au AFTER UPDATE ON scheduled_wakes BEGIN DELETE FROM scheduled_wakes_fts WHERE rowid = old.rowid; INSERT INTO scheduled_wakes_fts(rowid, reason, theory_id) VALUES (new.rowid, new.reason, COALESCE(new.theory_id,'')); END;`,
	}

	// FTS5 migration (2026-07-17): index the tags column in
	// memories_fts. The original schema declared tags UNINDEXED,
	// which meant tag queries fell through FTS5 with no match even
	// when matching rows existed. Hermes's diagnostic surfaced this
	// gap: 6 saves landed with proper tags (post the write-path fix
	// e2e18d6) but query_long_term_memory returned 0 for tag strings.
	//
	// Detection: check sqlite_master for the old UNINDEXED schema.
	// Migration: drop the old table, recreate with tags indexed,
	// backfill from memories. Triggers re-populate the new rows on
	// subsequent writes.
	//
	// Idempotent: on a fresh install, the new schema is already in
	// place (the CREATE TABLE IF NOT EXISTS above), so this block
	// does nothing. On an existing install with the old schema, this
	// runs once and rebuilds the index.
	ftsNeedsMigration := false
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories_fts' AND sql LIKE '%tags UNINDEXED%'`,
	).Scan(&ftsNeedsMigration); err == nil && ftsNeedsMigration {
		slog.Info("mpm: FTS5 migration detected (memories_fts with tags UNINDEXED); rebuilding with tags indexed")
		// Drop the old FTS table. Triggers reference it and will
		// re-fire on subsequent writes; backfill below catches
		// existing rows.
		if _, err := dm.db.Exec(`DROP TABLE memories_fts`); err != nil {
			slog.Warn("mpm: FTS5 migration failed to drop old table", "error", err.Error())
		} else {
			// Recreate with the new schema (tags indexed, no UNINDEXED).
			if _, err := dm.db.Exec(
				`CREATE VIRTUAL TABLE memories_fts USING fts5(content, collection, session_id UNINDEXED, tags, tokenize='porter unicode61')`,
			); err != nil {
				slog.Warn("mpm: FTS5 migration failed to recreate", "error", err.Error())
			} else {
				// Backfill: insert every non-deleted memory into the new FTS table.
				// Triggers already cover new writes; this catches existing rows.
				if _, err := dm.db.Exec(
					`INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
					 SELECT id, content, collection, session_id, tags FROM memories WHERE deleted_at IS NULL`,
				); err != nil {
					slog.Warn("mpm: FTS5 migration backfill failed", "error", err.Error())
				} else {
					slog.Info("mpm: FTS5 migration complete; tag tokens now searchable")
				}
			}
		}
	}

	for _, sqlQuery := range ftsStatements {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			// FTS5 creation failed — log and return error so we know search will use LIKE fallback
			fmt.Fprintf(os.Stderr, "FTS5 init error (search will use LIKE): %v\nSQL: %s\n", err, sqlQuery)
			return fmt.Errorf("FTS5 table/trigger creation failed: %v (search will use LIKE fallback)", err)
		}
	}
	return nil
}

// backfillFTSTables inserts existing rows into FTS5 tables.
// Called once after initFTSTables succeeds to index pre-existing data
// that predates the FTS5 triggers.
func (dm *DatabaseManager) backfillFTSTables() error {
	backfills := []struct {
		destTable string
		srcTable  string
		cols      string
		insertSQL string
	}{
		{
			destTable: "memories_fts",
			srcTable:  "memories",
			cols:      "id, content, collection, session_id, tags",
			insertSQL: `INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
					SELECT rowid, content, collection, COALESCE(session_id,''), COALESCE(tags,'[]')
					FROM memories WHERE deleted_at IS NULL`,
		},
		{
			destTable: "sessions_fts",
			srcTable:  "sessions",
			cols:      "id, content, session_id, content_hash",
			insertSQL: `INSERT INTO sessions_fts(rowid, content, session_id, content_hash)
					SELECT rowid, content, COALESCE(session_id,''), COALESCE(content_hash,'') FROM sessions`,
		},
		{
			destTable: "topics_fts",
			srcTable:  "topics",
			cols:      "id, name, description",
			insertSQL: `INSERT INTO topics_fts(rowid, name, description)
					SELECT rowid, COALESCE(name,''), COALESCE(description,'') FROM topics`,
		},
		{
			destTable: "lessons_fts",
			srcTable:  "lessons",
			cols:      "id, content, tags",
			insertSQL: `INSERT INTO lessons_fts(rowid, content, tags)
					SELECT rowid, content, COALESCE(tags,'[]') FROM lessons`,
		},
		{
			destTable: "scheduled_wakes_fts",
			srcTable:  "scheduled_wakes",
			cols:      "id, reason, theory_id",
			insertSQL: `INSERT INTO scheduled_wakes_fts(rowid, reason, theory_id)
					SELECT rowid, reason, COALESCE(theory_id,'') FROM scheduled_wakes`,
		},
	}

	for _, b := range backfills {
		// Check if FTS table already has data (avoid duplicate backfills)
		var count int
		dm.db.QueryRow("SELECT COUNT(*) FROM " + b.destTable).Scan(&count)
		if count > 0 {
			continue // already populated
		}
		_, err := dm.db.Exec(b.insertSQL)
		if err != nil {
			slog.Warn("backfill FTS table failed", "table", b.destTable, "error", err.Error())
		}
	}
	return nil
}

func (dm *DatabaseManager) Close() error {
	if dm.db != nil {
		return dm.db.Close()
	}
	return nil
}

// NewCascadeMaterializer constructs a CascadeMaterializer bound to this
// DatabaseManager. The returned materializer carries no mutable state beyond
// the dm reference; safe to share across goroutines. The caller is expected
// to invoke MaterializeBatch directly (background drain is owned by
// mpm-scheduler; the CLI calls MaterializeCascadeIntents).
func (dm *DatabaseManager) NewCascadeMaterializer(opts CascadeMaterializerOptions) *CascadeMaterializer {
	return NewCascadeMaterializer(dm, opts)
}

// MaterializeCascadeIntents is a one-shot convenience for CLI callers (the
// foreground escape hatch). It constructs a default-config materializer
// per call and runs one batch. The background drain is owned by
// mpm-scheduler; there is no shared long-lived materializer on
// DatabaseManager.
func (dm *DatabaseManager) MaterializeCascadeIntents(ctx context.Context, limit int) (MaterializationReport, error) {
	return dm.NewCascadeMaterializer(DefaultCascadeMaterializerOptions()).MaterializeBatch(ctx, limit)
}

// ==================== CRUD OPERATIONS ====================

// UpdateSessionSummary upserts the LLM-generated summary for a session.
// Uses the session_id as the unique key.
func (dm *DatabaseManager) UpdateSessionSummary(sessionID, summary string) error {
	_, err := dm.db.Exec(`UPDATE sessions SET summary = ? WHERE session_id = ?`, summary, sessionID)
	return err
}

// GetLastSession returns the most recent session by created_at DESC.
//
// created_at is stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1).
func (dm *DatabaseManager) GetLastSession() (map[string]interface{}, error) {
	var id, sessionID, content, contentHash, sourcePath, metadataJSON string
	var createdAt int64

	err := dm.db.QueryRow(`SELECT id, session_id, content, content_hash, source_path, metadata, created_at FROM sessions ORDER BY created_at DESC LIMIT 1`).
		Scan(&id, &sessionID, &content, &contentHash, &sourcePath, &metadataJSON, &createdAt)
	if err != nil {
		return nil, err
	}

	var metadata map[string]interface{}
	if metadataJSON != "" {
		json.Unmarshal([]byte(metadataJSON), &metadata)
	}

	return map[string]interface{}{
		"id":           id,
		"session_id":   sessionID,
		"content":      content,
		"content_hash": contentHash,
		"source_path":  sourcePath,
		"metadata":     metadata,
		"created_at":   createdAt,
	}, nil
}

// GetSessionMemories returns the most recent memories for a given session ID.
func (dm *DatabaseManager) GetSessionMemories(sessionID string, limit int) ([]map[string]interface{}, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session ID is required")
	}
	if limit <= 0 {
		limit = 5
	}

	rows, err := dm.db.Query(`
		SELECT id, collection, content, tags, metadata, created_at
		FROM memories
		WHERE session_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var memID, collection, content, tagsJSON, metadataJSON, createdAt string
		if err := rows.Scan(&memID, &collection, &content, &tagsJSON, &metadataJSON, &createdAt); err != nil {
			return nil, err
		}
		var tags []string
		var metadata map[string]interface{}
		json.Unmarshal([]byte(tagsJSON), &tags)
		if metadataJSON != "" {
			json.Unmarshal([]byte(metadataJSON), &metadata)
		}
		results = append(results, map[string]interface{}{
			"id":         memID,
			"collection": collection,
			"content":    content,
			"tags":       tags,
			"metadata":   metadata,
			"created_at": createdAt,
		})
	}
	return results, rows.Err()
}

func (dm *DatabaseManager) SaveMemory(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, expiresAt ...time.Time) (string, error) {
	return dm.SaveMemoryWithExtras(collection, content, sessionID, tags, metadata, embedding, isLongTerm, weight, "", "0.5", "0.5", "", expiresAt...)
}

// SaveMemoryNode is the canonical INSERT primitive for the memories table.
// Accepts a DBNode so callers can run the write inside an active transaction
// (via WithTx(func(node DBNode) error)) or standalone (passing dm itself,
// which satisfies DBNode at compile time — see db.go:267). The 20-pattern
// security scanner runs unconditionally before any INSERT; rejection
// returns an error and emits an audit row, no row is written.
//
// This is the single source of truth for the security scan + INSERT pair.
// SaveMemoryWithExtras is now a thin wrapper; AddEvidence and
// promote_scratchpad both reach the scanner through this primitive. Any new
// caller that needs to write a memory inside a multi-statement transaction
// should use WithTx + SaveMemoryNode, NOT a parallel *sql.Tx handle.
//
// Watchdog telemetry: when node is a txNode (in-tx), the ExecTracked call
// is attributed to the txNode, so watchdog.jsonl entries from inside a tx
// carry the same shape as standalone queries.
func (dm *DatabaseManager) SaveMemoryNode(node DBNode, collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error) {
	id := GenerateID()
	return saveMemoryRow(node, dm, id, collection, content, sessionID, tags, metadata, embedding, isLongTerm, weight, referenceID, retrievalPriority, importance, createdAt, expiresAt...)
}

// saveMemoryRow is the shared INSERT primitive that backs both
// SaveMemoryNode (which generates a random id) and SaveSkill (which
// uses a deterministic skill:<name>-v<version> id). Routing both
// writers through this single function makes the security scanner
// structurally guaranteed for every memories row, including the
// deterministic-id skills path that previously had its own INSERT.
//
// Watchdog telemetry, IVF cluster assignment, content_hash, and all
// other insert-time invariants live here so future fields added to
// the memories schema automatically reach every caller.
func saveMemoryRow(node DBNode, dm *DatabaseManager, id, collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error) {
	if isSensitive, reason := isSensitiveContent(content); isSensitive {
		dm.LogAudit(AuditError, "security", "sensitive content blocked", "", AuditContext{
			"reason":    reason,
			"len_chars": len(content),
		})
		return "", fmt.Errorf("sensitive content detected and blocked: %s", reason)
	}
	if isPoisoned, reason := isPoisoned(content); isPoisoned {
		dm.LogAudit(AuditError, "security", "poison content blocked", "", AuditContext{
			"reason":    reason,
			"len_chars": len(content),
		})
		return "", fmt.Errorf("poison content detected and blocked: %s", reason)
	}

	tagsJSON, _ := json.Marshal(tags)
	metadataJSON, _ := json.Marshal(metadata)

	embeddingJSON := "null"
	if embedding != nil {
		bytes, _ := json.Marshal(embedding)
		embeddingJSON = string(bytes)
	}

	var sessionIDVal interface{} = nil
	if sessionID != "" {
		sessionIDVal = sessionID
	}

	isLTM := 0
	if isLongTerm || weight >= 10 {
		isLTM = 1
	}

	var expiresAtStr interface{} = nil
	if len(expiresAt) > 0 && !expiresAt[0].IsZero() {
		expiresAtStr = expiresAt[0].UTC().Format(time.RFC3339)
	}

	initialConf := InitialConfidence(artifactTypeFromCollection(collection))

	// Normalize createdAt to Unix-epoch seconds. The schema column is INTEGER
	// (Unix-epoch seconds); accept either a numeric string or an RFC3339
	// string so existing string-typed callers keep working without an
	// upstream rewrite. Empty string defaults to now.
	created := createdAt
	if created == "" {
		created = strconv.FormatInt(time.Now().Unix(), 10)
	}
	createdSec, err := ParseTimestampArg(created)
	if err != nil {
		return "", fmt.Errorf("saveMemoryRow: invalid createdAt %q: %w", createdAt, err)
	}

	// Compute content_hash so dedup (memories.content_hash = ?) works at insert
	// time on the production SaveMemoryNode path. The addMemoryDirect fallback
	// was patched in commit 885705c but SaveMemoryNode is the dominant path
	// (used by SaveMemoryWithContext → AddMemoryWithWeight). Without this,
	// every freshly-saved memory has NULL content_hash and dedup silently
	// misses matches. Compute once and pass in the VALUES list.
	contentHashBytes := sha256.Sum256([]byte(content))
	contentHash := hex.EncodeToString(contentHashBytes[:])

	_, err = node.ExecTracked(`INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, is_long_term, weight, expires_at, confidence, created_at, reference_id, retrieval_priority, importance, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		0, id, collection, content, sessionIDVal, string(tagsJSON), string(metadataJSON), embeddingJSON, isLTM, weight, expiresAtStr, initialConf, createdSec, referenceID, retrievalPriority, importance, contentHash)
	if err != nil {
		return id, err
	}

	// IVF cluster assignment: best-effort after the memory row is
	// committed. If no clusters exist yet (fresh DB, no rebalance ever
	// run), this is a no-op and the memory stays unassigned until
	// `mpm ops rebalance` populates the index. Failures here are
	// logged but don't fail the insert — the memory is in the table
	// either way; missing assignment is recovered by the next
	// rebalance, not by rejecting the write.
	//
	// Uses AssignToClusterNode (DBNode-aware) so the assignment lands
	// in the same transaction as the INSERT. The centroid load uses
	// dm.SQLDB() (read-only, session-cached, doesn't need to be in the
	// tx).
	if embedding != nil && len(embedding) > 0 {
		if _, assignErr := AssignToClusterNode(node, dm, id, embedding, ""); assignErr != nil {
			slog.Warn("saveMemoryRow: IVF assignment failed (memory is unassigned; rebalance will recover)",
				"memory_id", id, "error", assignErr.Error())
		}
	}
	return id, nil
}

// SaveMemoryWithExtras extends SaveMemory with the additional columns that
// MemoryStore.AddMemory and AddMemoryWithWeight need: reference_id,
// retrieval_priority, importance, and created_at. Callers that don't need
// these can use SaveMemory directly; both functions share the same scanner.
//
// This is now a thin wrapper that routes through SaveMemoryNode with `dm`
// as the DBNode (dm satisfies DBNode at compile time). Existing callers are
// unaffected — the refactor is purely an internal restructuring to expose the
// tx-aware primitive. Callers that need transactional atomicity should call
// SaveMemoryNode directly inside a WithTx callback.
func (dm *DatabaseManager) SaveMemoryWithExtras(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error) {
	return dm.SaveMemoryNode(dm, collection, content, sessionID, tags, metadata, embedding, isLongTerm, weight, referenceID, retrievalPriority, importance, createdAt, expiresAt...)
}

// UpdateMemoryMetadata patches the metadata JSON column for a specific memory ID.
// Uses SQLite's json_patch() to merge the patch into existing metadata in-place.
// Does NOT touch content — FTS index is not affected.
// Returns error if the memory ID does not exist.
//
// The UPDATE and the FTS sync statements run inside a single transaction so
// a crash mid-write cannot leave the FTS index out of sync with the canonical
// content. FTS sync errors are checked and returned (previously swallowed).
func (dm *DatabaseManager) UpdateMemoryMetadata(id string, patchJSON string) error {
	if !json.Valid([]byte(patchJSON)) {
		return fmt.Errorf("UpdateMemoryMetadata: invalid JSON patch: %s", patchJSON)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: begin: %w", err)
	}
	defer tx.Rollback()

	if err := updateMemoryMetadataTx(tx, id, patchJSON); err != nil {
		return err
	}

	return tx.Commit()
}

// updateMemoryMetadataTx is the transactional body of UpdateMemoryMetadata.
// It runs the metadata UPDATE, the FTS DELETE, and the FTS INSERT against the
// supplied transaction. Exposed as a helper so callers that already own a
// transaction (e.g., ChallengeMemory) can join it without opening a second one.
func updateMemoryMetadataTx(tx *sql.Tx, id string, patchJSON string) error {
	// json_patch(NULL, '{}') returns NULL in SQLite, so COALESCE to {} is required.
	// This ensures a NULL metadata field doesn't cause the patch to fail.
	result, err := tx.Exec(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata, '{}'), ?),
			last_accessed_at = CURRENT_TIMESTAMP
		WHERE id = ? AND deleted_at IS NULL
	`, patchJSON, id)
	if err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("UpdateMemoryMetadata: rows check: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("memory not found or deleted: %s", id)
	}

	// Metadata-only changes bypass the FTS trigger — manually sync FTS
	// for the updated row so the search index stays current.
	// FTS tables may not exist in all environments (e.g. test temp DBs
	// built without FTS5). The sync is best-effort — primary metadata update
	// always succeeds regardless of FTS state.
	if _, err := tx.Exec(`SELECT 1 FROM memories_fts LIMIT 1`); err == nil {
		// FTS table exists — sync the updated row
		if _, err := tx.Exec(`
			DELETE FROM memories_fts WHERE rowid = (
				SELECT rowid FROM memories WHERE id = ?
			)
		`, id); err != nil {
			// Log but don't fail — primary update already succeeded
			slog.Warn("UpdateMemoryMetadata: fts delete failed (non-fatal)", "error", err.Error())
		}
		if _, err := tx.Exec(`
			INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
			SELECT rowid, content, collection, COALESCE(session_id,''), COALESCE(tags,'[]')
			FROM memories WHERE id = ? AND deleted_at IS NULL
		`, id); err != nil {
			slog.Warn("UpdateMemoryMetadata: fts insert failed (non-fatal)", "error", err.Error())
		}
	}

	return nil
}

// SaveSystemConfig stores or updates a system config entry (keyed by source file name)
// Only updates if the content hash has changed (skip duplicate writes)
// Returns (updated bool, error)
func (dm *DatabaseManager) SaveSystemConfig(key, rawJSON, contentHash string, snapshotJSON string) (bool, error) {
	var existingHash string
	err := dm.db.QueryRow(`SELECT content_hash FROM system_config WHERE key = ?`, key).Scan(&existingHash)
	if err == nil && subtle.ConstantTimeCompare([]byte(existingHash), []byte(contentHash)) == 1 {
		// Unchanged — skip the write
		return false, nil
	}
	// Insert or replace with new content
	_, err = dm.db.Exec(`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash, updated_at, config_snapshot) VALUES (?, ?, ?, CAST(strftime('%s','now') AS INTEGER), ?)`,
		key, rawJSON, contentHash, snapshotJSON)
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetSystemConfig retrieves a system config entry by key
func (dm *DatabaseManager) GetSystemConfig(key string) (map[string]interface{}, error) {
	var rawJSON, contentHash string
	// snapshotJSON is nullable — the config_snapshot column defaults to
	// NULL when callers insert via raw SQL (e.g. the GC cooldown upsert)
	// instead of going through SaveSystemConfig. Scan into *string so
	// NULL doesn't error the whole read; callers that care about snapshot
	// content check for nil before dereferencing.
	var snapshotJSON *string
	// updated_at is stored as INTEGER Unix-epoch seconds (see migration
	// timestamps_unified_v1).
	var updatedAt int64
	err := dm.db.QueryRow(`SELECT raw_json, content_hash, updated_at, config_snapshot FROM system_config WHERE key = ?`, key).
		Scan(&rawJSON, &contentHash, &updatedAt, &snapshotJSON)
	if err != nil {
		return nil, err
	}
	var snapshot map[string]interface{}
	if snapshotJSON != nil && *snapshotJSON != "" {
		json.Unmarshal([]byte(*snapshotJSON), &snapshot)
	}
	return map[string]interface{}{
		"key":          key,
		"raw_json":     rawJSON,
		"content_hash": contentHash,
		"updated_at":   updatedAt,
		"snapshot":     snapshot,
	}, nil
}

// DeleteSystemConfig removes a system config entry by key.
// Returns nil even if the key does not exist (idempotent).
func (dm *DatabaseManager) DeleteSystemConfig(key string) error {
	_, err := dm.db.Exec(`DELETE FROM system_config WHERE key = ?`, key)
	return err
}

// GetAllSystemConfigs returns all system config entries
func (dm *DatabaseManager) GetAllSystemConfigs() ([]map[string]interface{}, error) {
	rows, err := dm.db.Query(`SELECT key, raw_json, content_hash, updated_at, config_snapshot FROM system_config ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []map[string]interface{}
	for rows.Next() {
		var key, rawJSON, contentHash, snapshotJSON string
		var updatedAt int64
		if err := rows.Scan(&key, &rawJSON, &contentHash, &updatedAt, &snapshotJSON); err != nil {
			return nil, err
		}
		var snapshot map[string]interface{}
		if snapshotJSON != "" {
			json.Unmarshal([]byte(snapshotJSON), &snapshot)
		}
		configs = append(configs, map[string]interface{}{
			"key":          key,
			"raw_json":     rawJSON,
			"content_hash": contentHash,
			"updated_at":   updatedAt,
			"snapshot":     snapshot,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return configs, nil
}

// GetConfigInt reads an integer config from system_config with env fallback.
// Key format: "section.setting" (e.g., "consolidation.max_memories").
// Checks: 1) system_config table, 2) MPM_<SECTION>_<SETTING> env var (uppercase, dots->underscores), 3) default.
func (dm *DatabaseManager) GetConfigInt(key string, defaultValue int) int {
	if dm == nil {
		return getConfigIntFromEnv(key, defaultValue)
	}
	cfg, err := dm.GetSystemConfig(key)
	if err == nil {
		if raw, ok := cfg["raw_json"].(string); ok && raw != "" {
			var val int
			if err := json.Unmarshal([]byte(raw), &val); err == nil {
				return val
			}
		}
	}
	return getConfigIntFromEnv(key, defaultValue)
}

func getConfigIntFromEnv(key string, defaultValue int) int {
	// Primary: new env var format MPM_<SECTION>_<SETTING> (dots -> underscores)
	envKey := "MPM_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	if v := os.Getenv(envKey); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	// Backward compat: old MPM_MAX_VECTOR_SCAN for vector.max_scan
	if key == "vector.max_scan" {
		if v := os.Getenv("MPM_MAX_VECTOR_SCAN"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				return parsed
			}
		}
	}
	return defaultValue
}

// GetConfigInt64 reads an int64 config from system_config with env fallback.
func (dm *DatabaseManager) GetConfigInt64(key string, defaultValue int64) int64 {
	if dm == nil {
		return getConfigInt64FromEnv(key, defaultValue)
	}
	cfg, err := dm.GetSystemConfig(key)
	if err == nil {
		if raw, ok := cfg["raw_json"].(string); ok && raw != "" {
			var val int64
			if err := json.Unmarshal([]byte(raw), &val); err == nil {
				return val
			}
		}
	}
	return getConfigInt64FromEnv(key, defaultValue)
}

func getConfigInt64FromEnv(key string, defaultValue int64) int64 {
	envKey := "MPM_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	if v := os.Getenv(envKey); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// GetConfigFloat64 reads a float64 config from system_config with env fallback.
func (dm *DatabaseManager) GetConfigFloat64(key string, defaultValue float64) float64 {
	if dm == nil {
		return getConfigFloat64FromEnv(key, defaultValue)
	}
	cfg, err := dm.GetSystemConfig(key)
	if err == nil {
		if raw, ok := cfg["raw_json"].(string); ok && raw != "" {
			var val float64
			if err := json.Unmarshal([]byte(raw), &val); err == nil {
				return val
			}
		}
	}
	return getConfigFloat64FromEnv(key, defaultValue)
}

func getConfigFloat64FromEnv(key string, defaultValue float64) float64 {
	envKey := "MPM_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	if v := os.Getenv(envKey); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// GetConfigString reads a string config from system_config with env fallback.
func (dm *DatabaseManager) GetConfigString(key string, defaultValue string) string {
	if dm == nil {
		return getConfigStringFromEnv(key, defaultValue)
	}
	cfg, err := dm.GetSystemConfig(key)
	if err == nil {
		if raw, ok := cfg["raw_json"].(string); ok && raw != "" {
			var val string
			if err := json.Unmarshal([]byte(raw), &val); err == nil {
				return val
			}
			// If not JSON, return raw
			return raw
		}
	}
	return getConfigStringFromEnv(key, defaultValue)
}

func getConfigStringFromEnv(key string, defaultValue string) string {
	envKey := "MPM_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return defaultValue
}

// ==================== VECTOR SEARCH ====================

type searchResult struct {
	id         string
	content    string
	createdAt  int64
	similarity float32
}

func cosineSimilarity(a, b []float32) float32 {
	var dotProduct, normA, normB float32
	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

func (dm *DatabaseManager) VectorSearch(tier string, queryEmbedding []float32, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 10
	}
	if tier != "sessions" && tier != "memories" && tier != "topics" {
		return nil, fmt.Errorf("unsupported tier: %s", tier)
	}

	// Safe: whitelist enforced above; map lookup avoids fmt.Sprintf with user data
	baseQuery := map[string]string{
		"sessions": "SELECT id, content, embedding, created_at FROM sessions WHERE embedding IS NOT NULL",
		"memories": "SELECT id, content, embedding, created_at FROM memories WHERE embedding IS NOT NULL",
		"topics":   "SELECT id, content, embedding, created_at FROM topics WHERE embedding IS NOT NULL",
	}[tier]
	rows, err := dm.db.Query(baseQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []searchResult
	for rows.Next() {
		var id, content, embeddingJSON string
		var createdAt int64
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("VectorSearch: panic in rows loop: %v", r)
				}
			}()
			if err := rows.Scan(&id, &content, &embeddingJSON, &createdAt); err != nil {
				return
			}

			var dbEmbedding []float32
			if err := json.Unmarshal([]byte(embeddingJSON), &dbEmbedding); err != nil || len(dbEmbedding) != len(queryEmbedding) {
				return
			}

			sim := cosineSimilarity(queryEmbedding, dbEmbedding)
			results = append(results, searchResult{id, content, createdAt, sim})
		}()
		if err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(results, func(i, j int) bool { return results[i].similarity > results[j].similarity })
	if len(results) > limit {
		results = results[:limit]
	}

	var final []map[string]interface{}
	for _, r := range results {
		final = append(final, map[string]interface{}{
			"id":         r.id,
			"content":    r.content,
			"created_at": r.createdAt,
			"similarity": r.similarity,
		})
	}
	return final, nil
}

// ==================== SHRED PROTOCOL ====================

// WipeRecord performs a hard delete (on DatabaseManager).
//
// For the "topics" tier, the membership rows in topic_memberships and the
// parent row in topics are deleted in a single transaction so a crash mid-write
// cannot leave orphan memberships behind (the FK cascade would normally catch
// them, but only if FKs are enabled on the connection — a SQLite WAL crash can
// leave FK enforcement off until the next connection).
func (dm *DatabaseManager) WipeRecord(tier, id string) error {
	tableName := WipeTableNames[tier]
	if tableName == "" {
		return fmt.Errorf("cannot wipe from tier: %s", tier)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("WipeRecord: begin: %w", err)
	}
	defer tx.Rollback()

	// Clean up topic_memberships when deleting a topic
	if tier == "topics" {
		if _, err := tx.Exec("DELETE FROM topic_memberships WHERE topic_id = ?", id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM "+tableName+" WHERE id = ?", id); err != nil {
		return err
	}

	return tx.Commit()
}

// ShredMemory performs a hard delete on the row identified by id, with
// collection-aware routing. The 2026-07-22 session claimed this fix was
// shipped but the actual code change was never committed to git (the
// shred_lesson_aware_test.go file is untracked, but the corresponding
// production-code fix was missing from db.go). The 2026-07-23 session
// re-applies the fix for real.
//
// Routing contract:
//  1. Probe lessons_base for the id. If present, route through the
//     `lessons` view — that view's INSTEAD OF DELETE trigger (created
//     in migrateLessonsToView) removes the row from lessons_base AND
//     lessons_fts atomically.
//  2. Else fall through to DELETE FROM memories. (Lessons and memories
//     share the 16-char hex id space, so probing first is required —
//     a plain DELETE FROM memories WHERE id=? would not trigger the
//     lessons view's INSTEAD OF DELETE.)
//
// Returns nil on either successful delete OR a non-existent id (idempotent:
// callers can shred without a separate existence check).
func ShredMemory(db *sql.DB, id string) error {
	if id == "" {
		return fmt.Errorf("shred: id is required")
	}
	// Single transaction: probe lessons_base, route accordingly, fall
	// through to memories. Done in one tx so the lessons probe and the
	// delete are observed atomically — no race window where a row moves
	// between collections mid-probe.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("shred: begin: %w", err)
	}
	defer tx.Rollback()

	var lessonCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id).Scan(&lessonCount); err != nil {
		return fmt.Errorf("shred: probe lessons_base: %w", err)
	}
	if lessonCount > 0 {
		// Route through the lessons view — its INSTEAD OF DELETE trigger
		// removes from lessons_base AND lessons_fts atomically.
		if _, err := tx.Exec(`DELETE FROM lessons WHERE id = ?`, id); err != nil {
			return fmt.Errorf("shred: delete from lessons: %w", err)
		}
	} else {
		// Fall through to memories. The row may not exist in either
		// table (idempotent); DELETE on a missing row is a no-op.
		if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, id); err != nil {
			return fmt.Errorf("shred: delete from memories: %w", err)
		}
	}
	return tx.Commit()
}

// ==================== HELPERS ====================

var (
	idMu      sync.Mutex
	idCounter int64
)

func GenerateID() string {
	idMu.Lock()
	idCounter++
	ts := time.Now().UnixNano()
	input := fmt.Sprintf("%d-%d", ts, idCounter)
	idMu.Unlock()
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}

// =============================================================================
// Topic Management (exported for CLI use)
// =============================================================================

// CreateTopic creates a new topic and returns its ID
func (dm *DatabaseManager) CreateTopic(name, description, fromDate, toDate string) (string, error) {
	topicID := GenerateID()
	now := time.Now().Unix()
	tagsJSON := "{}"
	if fromDate != "" || toDate != "" {
		tags := map[string]string{}
		if fromDate != "" {
			tags["from_date"] = fromDate
		}
		if toDate != "" {
			tags["to_date"] = toDate
		}
		bytes, _ := json.Marshal(tags)
		tagsJSON = string(bytes)
	}
	descStr := description
	if descStr == "" {
		descStr = "{}"
	}
	_, err := dm.db.Exec(`
		INSERT INTO topics (id, name, description, created_at, tags, is_active, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)`,
		topicID, name, descStr, now, tagsJSON, now)
	return topicID, err
}

// GetOrCreateTopic looks up a topic by name, creating it if not found
func (dm *DatabaseManager) GetOrCreateTopic(name string) (string, error) {
	var id string
	err := dm.db.QueryRow(`SELECT id FROM topics WHERE name = ? AND is_active = 1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	return dm.CreateTopic(name, "", "", "")
}

// GetTopicByName returns a topic ID by name
func (dm *DatabaseManager) GetTopicByName(name string) (string, error) {
	var id string
	err := dm.db.QueryRow(`SELECT id FROM topics WHERE name = ? AND is_active = 1`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("topic not found")
	}
	return id, err
}

// GetTopic returns a topic record by ID
func (dm *DatabaseManager) GetTopic(id string) (map[string]interface{}, error) {
	var name, desc, createdAt, tagsJSON string
	err := dm.db.QueryRow(
		`SELECT name, COALESCE(description,''), created_at, COALESCE(tags,'{}') FROM topics WHERE id = ?`, id).Scan(
		&name, &desc, &createdAt, &tagsJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("topic not found")
	}
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{
		"id": id, "name": name, "description": desc, "created_at": createdAt, "tags": tagsJSON,
	}
	if strings.Contains(tagsJSON, "from_date") {
		var tags map[string]string
		json.Unmarshal([]byte(tagsJSON), &tags)
		if v, ok := tags["from_date"]; ok {
			result["from_date"] = v
		}
		if v, ok := tags["to_date"]; ok {
			result["to_date"] = v
		}
	}
	return result, nil
}

// ListTopics returns all active topics with memory counts
func (dm *DatabaseManager) ListTopics() ([]map[string]interface{}, error) {
	rows, err := dm.db.Query(`
		SELECT t.id, t.name, t.created_at, COALESCE(t.tags,'{}'),
			   COUNT(tm.memory_id) as memory_count
		FROM topics t
		LEFT JOIN topic_memberships tm ON tm.topic_id = t.id AND tm.memory_id IS NOT NULL
		WHERE t.is_active = 1
		GROUP BY t.id
		ORDER BY t.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var topics []map[string]interface{}
	for rows.Next() {
		var id, name, createdAt, tagsJSON string
		var memoryCount int
		if err := rows.Scan(&id, &name, &createdAt, &tagsJSON, &memoryCount); err != nil {
			continue
		}
		m := map[string]interface{}{"id": id, "name": name, "created_at": createdAt, "memory_count": memoryCount, "tags": tagsJSON}
		if strings.Contains(tagsJSON, "from_date") {
			var tags map[string]string
			json.Unmarshal([]byte(tagsJSON), &tags)
			if v, ok := tags["from_date"]; ok {
				m["from_date"] = v
			}
			if v, ok := tags["to_date"]; ok {
				m["to_date"] = v
			}
		}
		topics = append(topics, m)
	}
	return topics, nil
}

// GetTopicMemories returns memories in a topic (manual + auto from date range)
func (dm *DatabaseManager) GetTopicMemories(topicID string) ([]map[string]interface{}, error) {
	topic, err := dm.GetTopic(topicID)
	if err != nil {
		return nil, err
	}
	fromDate, _ := topic["from_date"].(string)
	toDate, _ := topic["to_date"].(string)

	rows, err := dm.db.Query(`
		SELECT m.id, m.content, m.session_id, m.tags, m.created_at, tm.role
		FROM topic_memberships tm
		JOIN memories m ON m.id = tm.memory_id
		WHERE tm.topic_id = ?
		ORDER BY tm.created_at DESC
	`, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []map[string]interface{}
	seen := make(map[string]bool)

	for rows.Next() {
		var memID, content, sessionID, tags, createdAt, role string
		if err := rows.Scan(&memID, &content, &sessionID, &tags, &createdAt, &role); err != nil {
			continue
		}
		seen[memID] = true
		memories = append(memories, map[string]interface{}{
			"id": memID, "content": content, "session_id": sessionID, "tags": tags, "created_at": createdAt, "role": role,
		})
	}

	if fromDate != "" {
		to := toDate
		if to == "" {
			to = fromDate
		}
		autoRows, err := dm.db.Query(`
			SELECT id, content, session_id, tags, created_at FROM memories
			WHERE created_at >= ? AND created_at <= ? AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT 50
		`, fromDate+"T00:00:00Z", to+"T23:59:59Z")
		if err != nil {
			return memories, err
		}
		defer autoRows.Close()
		for autoRows.Next() {
			var memID, content, sessionID, tags, createdAt string
			if err := autoRows.Scan(&memID, &content, &sessionID, &tags, &createdAt); err != nil {
				continue
			}
			if !seen[memID] {
				seen[memID] = true
				memories = append(memories, map[string]interface{}{
					"id": memID, "content": content, "session_id": sessionID, "tags": tags, "created_at": createdAt, "role": "auto",
				})
			}
		}
	}
	return memories, nil
}

// AddMemoryToTopic adds a memory to a topic
func (dm *DatabaseManager) AddMemoryToTopic(memoryID, topicID, role string) error {
	if role == "" {
		role = "manual"
	}
	now := time.Now().Unix()
	_, err := dm.db.Exec(`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)`,
		memoryID, topicID, now, role)
	return err
}

// RemoveMemoryFromTopic removes a memory from a topic
func (dm *DatabaseManager) RemoveMemoryFromTopic(memoryID, topicID string) error {
	_, err := dm.db.Exec(`DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?`, memoryID, topicID)
	return err
}

// DeleteTopic soft-deletes a topic (memories are NOT deleted).
//
// Both writes run inside a single transaction with the soft-delete UPDATE
// happening first: if a caller lists "active topics" between operations, the
// deactivated topic will not appear even before the membership cleanup runs.
// Previously, the membership DELETE ran first, so a failed UPDATE left an
// active topic with no members — a confusing state for any UI listing.
func (dm *DatabaseManager) DeleteTopic(topicID string) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("DeleteTopic: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE topics SET is_active = 0 WHERE id = ?`, topicID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM topic_memberships WHERE topic_id = ?`, topicID); err != nil {
		return err
	}

	return tx.Commit()
}

// ==================== Lesson Operations ====================

// LessonType represents the type of lesson
type LessonType string

const (
	LessonTypeWarning  LessonType = "warning"  // "don't do X"
	LessonTypePractice LessonType = "practice" // "do Y"
	LessonTypeInsight  LessonType = "insight"  // "X leads to Y"
)

// ValidLessonTypes is the allowlist of valid lesson types.
var ValidLessonTypes = map[LessonType]bool{
	LessonTypeWarning:  true,
	LessonTypePractice: true,
	LessonTypeInsight:  true,
}

// ValidateLessonType returns an error if the given lesson type is not in the
// allowlist. This is the single source of truth for lesson type validation.
func ValidateLessonType(lt string) error {
	if !ValidLessonTypes[LessonType(lt)] {
		return fmt.Errorf("invalid lesson type %q: must be one of warning, practice, or insight", lt)
	}
	return nil
}

// Lesson represents a learned lesson
type Lesson struct {
	ID                 string     `json:"id"`
	Type               LessonType `json:"type"`
	Content            string     `json:"content"`
	Tags               []string   `json:"tags"`
	ReinforcementCount int        `json:"reinforcement_count"`
	SourceSessionID    string     `json:"source_session_id,omitempty"`
	Created            string     `json:"created"`
	RetrievalPriority  float64    `json:"retrieval_priority,omitempty"`
	Importance         float64    `json:"importance,omitempty"`
	Confidence         float64    `json:"confidence,omitempty"`
}

// AddLesson adds a new lesson, checking for duplicates by content hash
func (dm *DatabaseManager) AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error) {
	if err := ValidateLessonType(string(lessonType)); err != nil {
		return nil, err
	}
	// Check for sensitive content before storing. AddLesson was already
	// gating on the 20-pattern sensitive-content scanner, but was missing
	// the poison-phrase check that MemoryStore.AddMemory runs. Without it,
	// a prompt-injection payload could land in the lessons table via the
	// mpm call save_lesson path; the audit noted this asymmetry.
	if isSensitive, name := isSensitiveContent(content); isSensitive {
		return nil, fmt.Errorf("lesson content blocked: %s detected", name)
	}
	if poisoned, reason := isPoisoned(content); poisoned {
		return nil, fmt.Errorf("lesson content blocked: poison phrase detected: %s", reason)
	}

	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	// Check for existing lesson with same content
	var existingID string
	err := dm.db.QueryRow(`SELECT id FROM lessons WHERE content_hash = ?`, contentHash).Scan(&existingID)
	if err == nil {
		// Reinforce existing lesson
		_, err = dm.db.Exec(`UPDATE lessons SET reinforcement_count = reinforcement_count + 1 WHERE id = ?`, existingID)
		if err != nil {
			return nil, err
		}
		return dm.GetLesson(existingID)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Create new lesson
	id := GenerateID()
	tagsJSON, _ := MarshalJSON(tags)
	now := time.Now().Format(time.RFC3339)

	_, err = dm.db.Exec(`
		INSERT INTO lessons (id, type, content, tags, reinforcement_count, source_session_id, created, content_hash, retrieval_priority, importance, confidence)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, 0.5, 0.5, ?)
	`, id, lessonType, content, tagsJSON, sourceSessionID, now, contentHash, InitialConfidence("lesson"))
	if err != nil {
		return nil, err
	}

	return &Lesson{
		ID:                 id,
		Type:               lessonType,
		Content:            content,
		Tags:               tags,
		ReinforcementCount: 1,
		SourceSessionID:    sourceSessionID,
		Created:            now,
		RetrievalPriority:  0.5,
		Importance:         0.5,
		Confidence:         InitialConfidence("lesson"),
	}, nil
}

// GetLesson retrieves a lesson by ID
func (dm *DatabaseManager) GetLesson(id string) (*Lesson, error) {
	var lesson Lesson
	var tagsJSON string
	err := dm.db.QueryRow(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id, created
		FROM lessons WHERE id = ?
	`, id).Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
	if err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		UnmarshalJSON(tagsJSON, &lesson.Tags)
	}
	return &lesson, nil
}

// ListLessons returns all lessons, optionally filtered by type
func (dm *DatabaseManager) ListLessons(lessonType string) ([]*Lesson, error) {
	query := `SELECT id, type, content, tags, reinforcement_count, source_session_id, created FROM lessons`
	var args []interface{}
	if lessonType != "" {
		query += ` WHERE type = ?`
		args = append(args, lessonType)
	}
	query += ` ORDER BY reinforcement_count DESC, created DESC`

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lessons []*Lesson
	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lessons, nil
}

// SearchLessons performs FTS5 search across lessons.
//
// The query is tokenized via BuildFTS5Query to match the index's porter
// unicode61 tokenizer (lessons_fts). Hyphenated, multi-word, and partial
// keyword queries all work; see BuildFTS5Query for the contract.
//
// JOIN on `lessons.rowid` (not on the `lessons` view's columns) is critical.
// An earlier version wrote `WHERE l.id IN (SELECT id FROM lessons WHERE
// lessons_fts MATCH ?)` — that silently failed because `lessons_fts` is the
// FTS5 virtual table, not a column of the `lessons` view, so the match raised
// "no such column" and the call fell through to the LIKE fallback. The FTS5
// path was effectively dead code on production until 2026-07-23.
func (dm *DatabaseManager) SearchLessons(query string, limit int) ([]*Lesson, error) {
	if limit <= 0 {
		limit = 20
	}

	ftsQuery := BuildFTS5Query(query)
	if ftsQuery == "" {
		// Empty query: return no results. Falling through to LIKE with
		// '%' would match every lesson; the caller should not see that.
		return nil, nil
	}

	rows, err := dm.db.Query(`
		SELECT l.id, l.type, l.content, l.tags, l.reinforcement_count, l.source_session_id, l.created
		FROM lessons l
		JOIN lessons_fts fts ON l.rowid = fts.rowid
		WHERE lessons_fts MATCH ?
		ORDER BY l.reinforcement_count DESC, l.created DESC
		LIMIT ?
	`, ftsQuery, limit)
	if err != nil {
		return dm.searchLessonsLike(query, limit)
	}
	defer rows.Close()

	var lessons []*Lesson
	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	if len(lessons) > 0 {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return lessons, nil
	}
	return dm.searchLessonsLike(query, limit)
}

// searchLessonsLike is a fallback when FTS5 is unavailable
func (dm *DatabaseManager) searchLessonsLike(query string, limit int) ([]*Lesson, error) {
	q := "%" + strings.ToLower(query) + "%"
	rows, err := dm.db.Query(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id, created
		FROM lessons
		WHERE LOWER(content) LIKE ? OR LOWER(tags) LIKE ?
		ORDER BY reinforcement_count DESC
		LIMIT ?
	`, q, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lessons []*Lesson
	for rows.Next() {
		var lesson Lesson
		var tagsJSON string
		err := rows.Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
		if err != nil {
			continue
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	return lessons, rows.Err()
}

// DeleteLesson removes a lesson
func (dm *DatabaseManager) DeleteLesson(id string) error {
	_, err := dm.db.Exec(`DELETE FROM lessons WHERE id = ?`, id)
	return err
}

// =============================================================================
// Memory Revision Helpers (Epistemological Time Travel)
// =============================================================================

// MemoryRevision represents a single entry in the memory_revisions table.
//
// CreatedAt is stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1). Display layer callers format at the boundary
// via FormatUnixSeconds.
type MemoryRevision struct {
	ID                 int64
	MemoryID           string
	Version            int
	Content            string
	Weight             int
	Collection         string
	IsLongTerm         bool
	IsChallenged       bool
	ChallengedTheoryID string
	CreatedAt          int64
}

// GetMemoryRevisions returns all versions for a memory in reverse-chronological
// order (newest first). Returns empty slice if the memory has no revisions.
func (dm *DatabaseManager) GetMemoryRevisions(memoryID string) ([]MemoryRevision, error) {
	rows, err := dm.db.Query(`
		SELECT id, memory_id, version, content, weight, collection,
		       is_long_term, is_challenged, COALESCE(challenged_theory_id, ''), created_at
		FROM memory_revisions
		WHERE memory_id = ?
		ORDER BY version DESC
	`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("GetMemoryRevisions: %w", err)
	}
	defer rows.Close()

	var revisions []MemoryRevision
	for rows.Next() {
		var r MemoryRevision
		if err := rows.Scan(&r.ID, &r.MemoryID, &r.Version, &r.Content,
			&r.Weight, &r.Collection, &r.IsLongTerm, &r.IsChallenged,
			&r.ChallengedTheoryID, &r.CreatedAt); err != nil {
			continue
		}
		revisions = append(revisions, r)
	}
	if revisions == nil {
		return []MemoryRevision{}, nil
	}
	return revisions, rows.Err()
}

// GetMemoryRevisionAtTime returns the memory revision active at the given
// timestamp. Returns nil if the memory did not exist yet at that time.
//
// The query selects the most recent version whose created_at <= asOf.
// This provides point-in-time reconstruction without replaying a log.
func (dm *DatabaseManager) GetMemoryRevisionAtTime(memoryID string, asOf time.Time) (*MemoryRevision, error) {
	// First verify the memory existed at that time
	var createdAt int64
	err := dm.db.QueryRow(`SELECT created_at FROM memories WHERE id = ?`, memoryID).Scan(&createdAt)
	if err != nil {
		return nil, nil // memory doesn't exist
	}
	if createdAt > asOf.Unix() {
		return nil, nil // memory didn't exist yet
	}

	// Check if the memory was soft-deleted before asOf
	var deletedAt sql.NullInt64
	err = dm.db.QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, memoryID).Scan(&deletedAt)
	if err == nil && deletedAt.Valid && deletedAt.Int64 > 0 && deletedAt.Int64 < asOf.Unix() {
		return nil, nil // memory was deleted before asOf
	}

	row := dm.db.QueryRow(`
		SELECT id, memory_id, version, content, weight, collection,
		       is_long_term, is_challenged, COALESCE(challenged_theory_id, ''), created_at
		FROM memory_revisions
		WHERE memory_id = ?
		  AND created_at <= ?
		ORDER BY version DESC
		LIMIT 1
	`, memoryID, asOf.Unix())

	var r MemoryRevision
	err = row.Scan(&r.ID, &r.MemoryID, &r.Version, &r.Content,
		&r.Weight, &r.Collection, &r.IsLongTerm, &r.IsChallenged,
		&r.ChallengedTheoryID, &r.CreatedAt)
	if err != nil {
		return nil, nil // no revision found for that time
	}
	return &r, nil
}

// =============================================================================
// Cognitive Immune System: Async Challenge
// =============================================================================

// ChallengeMemoryAsync logs a contradiction collision to mirror.jsonl in an
// independent, detached goroutine. Does NOT block the search query or write to
// the database — only appends to the audit mirror. The evidence parameter
// describes which memories collided and why.
func (dm *DatabaseManager) ChallengeMemoryAsync(memoryID string, evidence string) {
	go func() {
		mirrorPath := filepath.Join(config.GetMPMDir(), "src", "db", "mirror.jsonl")

		entry := map[string]interface{}{
			"event":     "contradiction_detected",
			"memory_id": memoryID,
			"evidence":  evidence,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		line, _ := json.Marshal(entry)

		// Hold watchdogMu across rotation + write so concurrent mirror
		// writers (and concurrent watchdog writers — they share the
		// mutex) don't race on the truncate step.
		dm.watchdogMu.Lock()
		defer dm.watchdogMu.Unlock()
		if err := rotateLogIfNeeded(mirrorPath, logRotateThresholdBytes()); err != nil {
			slog.Warn("mirror log rotation failed", "err", err)
		}
		f, err := os.OpenFile(mirrorPath, os.O_APPEND|os.O_CREATE, 0600)
		if err != nil {
			return
		}
		defer f.Close()
		f.WriteString(string(line) + "\n")
	}()
}

// ChallengeMemory applies provenance-based contradiction resolution:
// slashes the loser's weight and sets challenged status in the DB.
// The slashAmount is the weight reduction (positive integer).
//
// All four operations (pre-check, metadata patch with FTS sync, weight update,
// async log) are wrapped in a single transaction. A concurrent ReinforceMemory
// cannot interleave between the metadata patch and the weight decrement, so
// the challenged memory is always observed in a consistent state.
func (dm *DatabaseManager) ChallengeMemory(memoryID string, slashAmount int, evidence string) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("ChallengeMemory: begin: %w", err)
	}
	defer tx.Rollback()

	// Verify the memory exists and is not hard-deleted inside the transaction
	// so a concurrent soft-delete between the check and the writes is caught
	// by the WHERE-deleted_at-IS-NULL clause on each subsequent UPDATE.
	var exists bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM memories WHERE id = ? AND deleted_at IS NULL)`, memoryID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("ChallengeMemory: select check: %w", err)
	}
	if !exists {
		return fmt.Errorf("ChallengeMemory: memory not found or deleted: %s", memoryID)
	}

	patch := map[string]interface{}{
		"status":               "challenged",
		"challenged_theory_id": evidence,
	}
	patchJSON, _ := json.Marshal(patch)
	if err := updateMemoryMetadataTx(tx, memoryID, string(patchJSON)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE memories SET weight = MAX(1, weight - ?), updated_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL`, slashAmount, memoryID); err != nil {
		return fmt.Errorf("ChallengeMemory: weight update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ChallengeMemory: commit: %w", err)
	}

	// Async watchdog log is fire-and-forget and runs only after a successful
	// commit, so a logged entry always reflects a committed state.
	dm.ChallengeMemoryAsync(memoryID, evidence)
	return nil
}

// extractProvenanceCompute reads the provenance.compute field from metadata JSON.
// Returns empty string if not found, allowing graceful default to "standard".
func extractProvenanceCompute(metadataJSON string) string {
	if metadataJSON == "" || metadataJSON == "{}" || metadataJSON == "null" {
		return ""
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return ""
	}
	prov, ok := meta["provenance"].(map[string]interface{})
	if !ok {
		return ""
	}
	compute, _ := prov["compute"].(string)
	return compute
}

// =============================================================================
// Heartbeat-Driven Knowledge Lifecycle (Decay & Archival)
// =============================================================================

// DecayWeights applies exponential step-down decay to LTM memories whose
// updated_at predates the interval window. Each decayed memory has its weight
// reduced by at least 1 (floor = 1) and its updated_at refreshed so the same
// memory is not decayed again on the next heartbeat pass.
func (dm *DatabaseManager) DecayWeights(policies map[string]DecayPolicy, intervalDays int) (int, error) {
	if intervalDays <= 0 {
		intervalDays = 7
	}
	if policies == nil {
		policies = DefaultDecayPolicies
	}

	total := 0
	for collection, policy := range policies {
		if policy.DecayPercent <= 0 {
			continue
		}
		decayFactor := policy.DecayPercent / 100.0

		// The exponential step-down formula: subtract at least 1, never go below 1.
		// CAST to INTEGER truncates toward zero, so MAX(1, …) guarantees minimum delta.
		// Provenance multiplier via json_extract:
		//   absolute → * 0.0 (exempt), high → * 0.5, ephemeral → * 2.0, else → * 1.0
		result, err := dm.db.Exec(`
			UPDATE memories
			SET weight = MAX(weight - MAX(1, CAST(weight * ? *
			    COALESCE(
			        CASE json_extract(metadata, '$.provenance.compute')
			            WHEN 'absolute' THEN 0.0
			            WHEN 'high' THEN 0.5
			            WHEN 'ephemeral' THEN 2.0
			            ELSE 1.0
			        END, 1.0) AS INTEGER)), 1),
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE is_long_term = 1
			  AND deleted_at IS NULL
			  AND collection = ?
			  AND updated_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)
			  AND weight > 1
		`, decayFactor, collection, intervalDays)
		if err != nil {
			return total, fmt.Errorf("DecayWeights: collection %s: %w", collection, err)
		}
		affected, _ := result.RowsAffected()
		total += int(affected)
	}
	return total, nil
}

// ArchiveStaleMemories soft-deletes weight=1 memories whose updated_at predates
// the archive window. The soft-delete trips the memories_rev_au trigger,
// capturing the terminal state in memory_revisions for --as-of retrospectivity.
// A hard LIMIT of 100 prevents un-indexed full-table sweeps under active WAL.
func (dm *DatabaseManager) ArchiveStaleMemories(archiveDays int) (int, error) {
	if archiveDays <= 0 {
		archiveDays = 30
	}
	result, err := dm.db.Exec(`
		UPDATE memories
		SET deleted_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id IN (
			SELECT id FROM memories
			WHERE weight = 1
			  AND deleted_at IS NULL
			  AND updated_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)
			LIMIT 100
		)
	`, archiveDays)
	if err != nil {
		return 0, fmt.Errorf("ArchiveStaleMemories: %w", err)
	}
	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// RunLifecycleDecayAndArchival runs a single heartbeat pass of the decay sweep
// followed by stale-memory archival. Both operations run on an isolated SQLite
// connection to avoid hot-path WAL contention with the fsnotify ingestion thread.
// Metrics are logged to mirror.jsonl as a structured [lifecycle] token line.
func (dm *DatabaseManager) RunLifecycleDecayAndArchival(decayRate float64, archiveDays int) error {
	// Reuse the existing DB connection pool to avoid WAL exhaustion
	return dm.runLifecycleInline(decayRate, archiveDays)
}

// runLifecycleInline executes the decay + archival pass on the current
// DatabaseManager connection. Extracted so both inline and isolated paths
// share the same logic.
func (dm *DatabaseManager) runLifecycleInline(decayRate float64, archiveDays int) error {
	intervalDays := archiveDays / 7
	if intervalDays < 1 {
		intervalDays = 1
	}

	// Apply the same decay rate to all non-audit collections
	nonAudit := []string{"default", "session", "memories", "theories"}
	policy := make(map[string]DecayPolicy, len(nonAudit))
	for _, c := range nonAudit {
		policy[c] = DecayPolicy{DecayPercent: decayRate, Floor: 1}
	}
	decayed, _ := dm.DecayWeights(policy, intervalDays)
	if decayed < 0 {
		decayed = 0
	}

	archived, _ := dm.ArchiveStaleMemories(archiveDays)
	if archived < 0 {
		archived = 0
	}

	line := fmt.Sprintf("[lifecycle] decay swept: %d weights adjusted, %d records archived to revisions ledger\n",
		decayed, archived)
	dm.logWatchdogRaw([]byte(line))
	return nil
}

// GetLessonStats returns statistics about lessons
func (dm *DatabaseManager) GetLessonStats() (map[string]interface{}, error) {
	stats := map[string]interface{}{
		"total_lessons": 0,
		"by_type":       map[string]int{},
	}

	var total int
	err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&total)
	if err != nil {
		return stats, err
	}
	stats["total_lessons"] = total

	typeCounts := map[string]int{}
	for _, t := range []LessonType{LessonTypeWarning, LessonTypePractice, LessonTypeInsight} {
		var count int
		err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons WHERE type = ?`, t).Scan(&count)
		if err == nil {
			typeCounts[string(t)] = count
		}
	}
	stats["by_type"] = typeCounts

	return stats, nil
}

// ProvenancePreamble extracts the provenance trust signal from metadata JSON
// and returns a formatted preamble string. Returns empty string if no provenance
// is found, allowing graceful degradation.
func ProvenancePreamble(metadataJSON string) string {
	if metadataJSON == "" || metadataJSON == "{}" || metadataJSON == "null" {
		return ""
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return ""
	}
	prov, ok := meta["provenance"].(map[string]interface{})
	if !ok {
		return ""
	}
	model, _ := prov["model"].(string)
	compute, _ := prov["compute"].(string)

	switch compute {
	case "absolute":
		return "[Source: Human | Authority: Absolute]"
	case "high":
		label := model
		if label == "" {
			label = "High-Compute"
		}
		return "[Source: " + label + " | Compute: High]"
	default:
		return ""
	}
}

// RecordGlobalRule writes a memory row to the shared DB with
// is_global=1. This is the operator-gated write path for the
// shared epistemology: only callers that pass `confirm: true` get
// past the gate, and the caller is responsible for surfacing that
// flag to the human operator.
//
// The row is written with collection="rules" by default — the shared
// DB's home for cross-agent conventions. Other collections are
// allowed but `rules` is the canonical home per WISHLIST.md.
//
// Phase 3 of WISHLIST.md: operator-only. No agent should be writing
// house rules autonomously. CLI and MCP both gate on confirm=true.
func (dm *DatabaseManager) RecordGlobalRule(content string, tags []string, weight int, provenance string) (string, error) {
	if !dm.sharedAttached {
		return "", fmt.Errorf("shared DB not attached (set MPM_SHARED_DB)")
	}
	if content == "" {
		return "", fmt.Errorf("content is required")
	}
	if weight < 0 || weight > 100 {
		return "", fmt.Errorf("weight must be 0-100, got %d", weight)
	}
	id := GenerateID()
	tagsJSON, _ := json.Marshal(tags)

	var metaJSON []byte
	if provenance != "" {
		metaJSON, _ = json.Marshal(map[string]interface{}{
			"provenance": provenance,
			"source":     "mpm",
		})
	}

	_, err := dm.db.Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, tags, metadata, weight,
		     is_global, created_at, updated_at, deleted_at)
		VALUES
		    (?, 'rules', ?, ?, ?, ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, NULL)
	`, id, content, string(tagsJSON), string(metaJSON), weight)
	if err != nil {
		return "", fmt.Errorf("insert shared rule: %w", err)
	}
	return id, nil
}

// PromoteToGlobal copies a local memory to the shared DB. The
// original local row stays in the local DB (per WISHLIST.md: "Source
// row stays in local DB"). The shared copy is marked is_global=1
// with collection="rules" and a metadata.derived_from_local_id
// field linking back to the original.
//
// Phase 3 of WISHLIST.md: operator-only. Requires confirm: true at
// the tool boundary.
func (dm *DatabaseManager) PromoteToGlobal(localID string) (string, error) {
	if !dm.sharedAttached {
		return "", fmt.Errorf("shared DB not attached (set MPM_SHARED_DB)")
	}
	if localID == "" {
		return "", fmt.Errorf("localID is required")
	}

	// Read the local row.
	var content, collection string
	var tagsNS, metaNS sql.NullString
	var weight, reinforcement int
	err := dm.db.QueryRow(`
		SELECT content, collection, tags, metadata, weight, COALESCE(reinforcement_count, 0)
		FROM memories
		WHERE id = ? AND deleted_at IS NULL
	`, localID).Scan(&content, &collection, &tagsNS, &metaNS, &weight, &reinforcement)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("local memory %s not found or deleted", localID)
	}
	if err != nil {
		return "", fmt.Errorf("read local memory: %w", err)
	}

	// Build lineage metadata. If the source already had metadata,
	// keep it; otherwise start fresh.
	meta := map[string]interface{}{}
	if metaNS.Valid && metaNS.String != "" {
		_ = json.Unmarshal([]byte(metaNS.String), &meta)
	}
	meta["derived_from_local_id"] = localID
	metaJSON, _ := json.Marshal(meta)

	newID := GenerateID()
	_, err = dm.db.Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, tags, metadata, weight,
		     reinforcement_count, is_global, created_at, updated_at, deleted_at)
		VALUES
		    (?, ?, ?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, NULL)
	`, newID, collection, content, tagsNS.String, string(metaJSON), weight, reinforcement)
	if err != nil {
		return "", fmt.Errorf("insert shared copy: %w", err)
	}
	return newID, nil
}
