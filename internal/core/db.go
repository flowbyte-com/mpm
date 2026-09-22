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
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/seed"

	_ "github.com/mattn/go-sqlite3"
)

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// isNoSuchTableError reports whether err is a SQLite "no such table"
// failure. Used by the drift-remediation sweep to gracefully skip the
// shared.memories branch on single-DB / test paths that never call
// attachShared. Matches both the bare "no such table" form and the
// shared-schema-prefixed "no such table: shared.memories" form.
func isNoSuchTableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table")
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

// sqliteWriteDSN appends the foreign-key pragma to a SQLite DSN so that
// EVERY pooled connection opens with `PRAGMA foreign_keys = ON`.
//
// Why the DSN and not an Exec: `db.Exec("PRAGMA foreign_keys = ON")` only
// runs on whichever single connection the Exec happens to grab. database/sql
// lazily creates new connections as concurrency grows, and those new
// connections open with FK enforcement OFF (the SQLite default) — so the
// FK-audit cascade guarantees silently evaporate under agent concurrency.
// mattn/go-sqlite3 turns the `_foreign_keys=1` DSN param into the PRAGMA at
// connection-open time, making enforcement structural per connection.
//
// (busy_timeout / synchronous / cache_size have the same per-connection
// nature and the same Exec race; the Exec calls are kept as belt-and-
// suspenders for the first connection, but _foreign_keys is the load-bearing
// guarantee.)
// SqliteWriteDSN appends the foreign-key pragma to a SQLite DSN so that
// the resulting *sql.DB enforces foreign keys at the connection level.
// It also appends _busy_timeout=5000 so every pooled connection waits
// the configured window for SQLITE_BUSY / SQLITE_LOCKED to clear before
// returning the error.
//
// D-003 (alpha-4.1.2): busy_timeout was previously applied via a single
// `db.Exec("PRAGMA busy_timeout = 5000")` after sql.Open, which only
// affects the FIRST pooled connection. Subsequent connections opened
// from the pool ran with the SQLite default of 0 (no waiting), so
// parallel AppendWorkEvent callers contended on table locks and the
// retry path in WithTx / ExecTracked always lost the race. By moving
// _busy_timeout into the DSN — the same channel _foreign_keys uses —
// every connection inherits the setting at open time.
//
// Exported so callers outside this package (cmd/mpm, tests) can construct
// the same DSN shape without drifting from the load-bearing guarantee here.
// The internal alias sqliteWriteDSN is preserved for the three call sites
// inside this package.
//
// Empty input is treated as ":memory:" — mattn/go-sqlite3 interprets a DSN
// starting with '?' as "create a file with that literal name". An unguarded
// empty path therefore silently creates a SQLite file named
// "?_foreign_keys=1&_busy_timeout=5000" in the cwd rather than opening the
// intended path. Returning ":memory:" on empty input converts the caller
// mistake into the in-memory DB.
func SqliteWriteDSN(path string) string {
	if path == "" {
		return ":memory:"
	}
	suffix := "_foreign_keys=1&_busy_timeout=5000"
	if strings.Contains(path, "?") {
		return path + "&" + suffix
	}
	return path + "?" + suffix
}

// sqliteWriteDSN is the internal alias used by the three call sites within
// this package — kept identical to SqliteWriteDSN to preserve the
// "one helper, one DSN strategy" invariant.
func sqliteWriteDSN(path string) string { return SqliteWriteDSN(path) }

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
	db, err := sql.Open("sqlite3", sqliteWriteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
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
	dbPath      string       // absolute, realpath-resolved path to the SQLite file
	dbPathRaw   string       // raw path passed to NewDatabaseManager (symlinks un-resolved)
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
	// See docs/archive/shared-epistemology.md "Multi-Agent Shared Epistemology" for the design.
	sharedPath     string
	sharedAttached bool

	// mirrorWG tracks in-flight fire-and-forget mirror writes
	// (ChallengeMemoryAsync) so Close() can join them instead of tearing the
	// DB down under a live writer. audit-go treats a WaitGroup-bound
	// goroutine as lifecycle-safe; the mirror writes stay detached from the
	// search path but are no longer unjoinable.
	mirrorWG sync.WaitGroup

	// ProvenanceResolver is the process-wide provenance resolver.
	// Constructed lazily on first access via GetProvenanceResolver().
	// Tests override the field directly and restore in t.Cleanup.
	// No package-level global mutability — tests cannot pollute
	// parallel tests, and production code cannot be accidentally
	// affected by test-SetOverride.
	ProvenanceResolver *ProvenanceResolver

	// perCallProvenanceOverride is set by callers that need to inject
	// per-call provenance (e.g., the cascade materializer setting
	// parent_artifact_id). It is read once by saveMemoryRow and cleared
	// before the function returns. Tests use t.Cleanup to ensure the
	// override is cleared on test exit.
	perCallProvenanceOverride *EffectiveProvenance
}

// GetProvenanceResolver returns the resolver, lazily initializing
// from env if not set. Tests override the field directly and restore
// in t.Cleanup. No package-level global mutability.
func (dm *DatabaseManager) GetProvenanceResolver() *ProvenanceResolver {
	if dm.ProvenanceResolver == nil {
		dm.ProvenanceResolver = NewFromEnv()
	}
	return dm.ProvenanceResolver
}

// WithProvenanceOverride sets a per-call provenance override, calls fn,
// and clears the override before returning. The override is read once
// by getEffectiveProvenance inside saveMemoryRow so the wrapped call's
// provenance row carries the parent_artifact_id.
func (dm *DatabaseManager) WithProvenanceOverride(prov *EffectiveProvenance, fn func() error) error {
	dm.perCallProvenanceOverride = prov
	defer func() { dm.perCallProvenanceOverride = nil }()
	return fn()
}

// getEffectiveProvenance returns the effective provenance for a write.
// It prefers any per-call override (set by WithProvenanceOverride) over
// the process-wide resolver. This allows the cascade materializer to
// thread parent_artifact_id into a single SaveMemoryNode call.
func (dm *DatabaseManager) getEffectiveProvenance() *EffectiveProvenance {
	if dm.perCallProvenanceOverride != nil {
		return dm.perCallProvenanceOverride
	}
	r := dm.GetProvenanceResolver()
	if r == nil {
		return &EffectiveProvenance{ActorKind: "unknown"}
	}
	return r.Resolve("", "", "", "")
}

// provenanceWithParent returns an effective provenance with
// parent_artifact_id set to the dead artifact. Used by the
// cascade materializer so the resulting theory's provenance row
// carries the causal chain.
func (dm *DatabaseManager) provenanceWithParent(parentArtifactID string) *EffectiveProvenance {
	r := dm.GetProvenanceResolver()
	if r == nil {
		return &EffectiveProvenance{ActorKind: "unknown", ParentArtifactID: parentArtifactID}
	}
	return r.Resolve("", "", parentArtifactID, "")
}

// provenanceWithParentInvocation returns an effective provenance with
// parent_invocation_id set to the spawning invocation's ID. Used by
// agents that spawn sub-invocations (Hermes → Claude Code → memory)
// so the resulting artifact's provenance row can be walked back to
// its origin via the invocation tree. Nullable: agents that did not
// themselves get spawned by a parent invocation pass "" (the column
// stores NULL).
func (dm *DatabaseManager) provenanceWithParentInvocation(parentInvocationID string) *EffectiveProvenance {
	r := dm.GetProvenanceResolver()
	if r == nil {
		return &EffectiveProvenance{ActorKind: "unknown", ParentInvocationID: parentInvocationID}
	}
	return r.Resolve("", "", "", parentInvocationID)
}

// nodeUnwrapTx returns the *sql.Tx from a DBNode. When DBNode is a
// txNode (from a WithTx call), the *sql.Tx is extracted so all writes
// join the same transaction. When DBNode is the standalone DatabaseManager,
// nil is returned — RecordArtifactProvenance handles that as a fresh tx.
func nodeUnwrapTx(node DBNode) *sql.Tx {
	if tn, ok := node.(*txNode); ok {
		return tn.tx
	}
	return nil
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
//   - wakes_overdue: count of actionable unfired wakes whose target_time < now
//     (notification, cascade, or untagged; cron-kind bookkeeping is
//     excluded — it is retained and boundedly swept by the cron
//     retention sweep in internal/scheduler/wake_expiration.go)
//   - busy_retries: lifetime SQLITE_BUSY retry counter (process-local)
//
// Not a tool today — DM-level method that future ops commands can wrap.
// Failure modes: returns whatever it managed to read; the `ok` field
// reflects only the integrity check result.
func (dm *DatabaseManager) HealthCheck() (map[string]interface{}, error) {
	out := map[string]interface{}{
		"busy_retries": dm.BusyRetryCount(),
		// db_path — the absolute, realpath-resolved SQLite file this
		// DatabaseManager is attached to. Surface it as part of every
		// health check so integration plugins can compare the live
		// attachment against their host-pinned expected path and
		// refuse to boot if they disagree (the silent-orphan-db
		// failure mode that motivated the 2026-08-13 hardening).
		//
		// db_path_raw — the raw string passed to NewDatabaseManager,
		// before symlink resolution. Diagnostic only; agents should
		// compare on db_path, not db_path_raw, since the contract is
		// inode-equivalent (the directive id 22736bb9122e0e32 pins
		// `/home/v/.mpm/src/db/mpm.db` as canonical, but the
		// project-source, openclaw-mirror, and canonical symlinks
		// all resolve to the same inode).
		//
		// shared_path — the path of any attached shared DB, or ""
		// when federation is off. Same comparison contract applies
		// if a host pins an expected shared attachment.
		"db_path":         dm.dbPath,
		"db_path_raw":     dm.dbPathRaw,
		"shared_attached": dm.sharedAttached,
		"shared_path":     dm.sharedPath,
	}

	// Integrity check (PRAGMA quick_check is cheap, runs in <100ms).
	// First, checkpoint the WAL to ensure all uncommitted frames are flushed
	// to the main DB. Without this, a concurrent writer holding an uncommitted
	// WAL frame can cause PRAGMA quick_check to report a false FTS5 checksum
	// mismatch even when the DB is perfectly healthy (the Go layer then reports
	// "fts5: checksum mismatch" which is purely a WAL-read timing artifact).
	// TRUNCATE also resets the WAL file, keeping it small.
	// Errors here are non-fatal — the checkpoint is best-effort; the integrity
	// check still runs regardless.
	_, _ = dm.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)

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
	if err := dm.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		dm.LogAudit(AuditWarn, "health", fmt.Sprintf("PRAGMA page_count scan failed, skipping: %v", err), "", AuditContext{})
	} else if pageCount.Valid {
		out["page_count"] = pageCount.Int64
	}
	if err := dm.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelistCount); err != nil {
		dm.LogAudit(AuditWarn, "health", fmt.Sprintf("PRAGMA freelist_count scan failed, skipping: %v", err), "", AuditContext{})
	} else if freelistCount.Valid {
		out["freelist_count"] = freelistCount.Int64
	}

	// Domain stats (each a single COUNT query).
	// wakes_overdue counts ACTIONABLE overdue wakes only — the same
	// kind-set the ad-hoc dispatcher claims (notification, cascade,
	// untagged). Cron-kind rows are bookkeeping for one-shot events
	// generated by a recurring schedule and are NEVER consumed by
	// the dispatcher (the wedge protection at scheduler.go:485-510);
	// they are retained and boundedly swept by the cron_retention
	// tick handler (wake_expiration.go). Including them in
	// wakes_overdue made the metric grow unboundedly without
	// representing any actionable backlog.
	//
	// cascade_summary rows are snapshot rows written periodically
	// by the cascade subsystem; they too are not consumed by the
	// dispatcher and would similarly inflate the metric.
	//
	// The kind-filter expression matches dispatchClaimNextAdHocWake
	// (internal/scheduler/dispatch.go) modulo the addition of
	// 'cascade' which the fold surfaces with a per-check cap.
	queries := []struct {
		key, sql string
	}{
		{"memories_active", `SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND NOT (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)`},
		{"theories_pending", `SELECT COUNT(*) FROM memories WHERE collection = 'theories' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > strftime('%s','now')) AND json_extract(metadata, '$.status') = 'pending'`},
		{"wakes_overdue", `SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0 AND target_time < ? AND (metadata IS NULL OR metadata = '' OR json_extract(metadata, '$.kind') IS NULL OR json_extract(metadata, '$.kind') = '' OR json_extract(metadata, '$.kind') IN ('notification','cascade'))`},
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
		} else {
			// Non-fatal — log audit and skip on error.
			dm.LogAudit(AuditWarn, "health", fmt.Sprintf("HealthCheck stat %q scan failed, skipping: %v", q.key, err), "", AuditContext{})
		}
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
	f, err := os.OpenFile(dm.watchdogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
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
	f, err := os.OpenFile(dm.watchdogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
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
	// D-003 (alpha-4.1.2): retry Begin() on SQLITE_BUSY / SQLITE_LOCKED.
	//
	// We intentionally do NOT retry the callback itself on busy. Callers
	// that need callback-level retry (e.g. capability.Store.RecordInvocation
	// has its own withRetry primitive at the Store layer) wrap WithTx in
	// their own retry loop, and a nested retry inside WithTx would
	// multiply their per-attempt budget. The AppendWorkEvent SELECT that
	// motivated this comment originally ran into busy at Begin() time
	// because 8+ goroutines raced on the same table — retrying Begin
	// alone closes that hole. Mid-transaction busy is handled by
	// ExecTracked's per-statement retry (which already retries 5 times
	// per statement).
	const maxAttempts = 5
	backoff := 100 * time.Millisecond
	maxBackoff := 5 * time.Second

	var tx *sql.Tx
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tx, err = dm.db.Begin()
		if err != nil {
			if !IsBusyError(err) || attempt == maxAttempts {
				return fmt.Errorf("begin transaction: %w", err)
			}
			dm.busyRetries.Add(1)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		break
	}
	if tx == nil {
		// Begin failed on every retry; loop terminated without a break.
		// The last error is already set; return it.
		return fmt.Errorf("begin transaction: exhausted %d retries: %w", maxAttempts, err)
	}

	// Canonical pattern: covering defer at function scope. The lint
	// gate (internal/audit/tx.go) classifies a tx as `tx-defer-rollback`
	// when a `defer tx.Rollback()` lives in covering position relative to
	// the Begin statement; the covering defer here satisfies that rule
	// and covers panic + non-commit error returns in a single construct.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = fn(&txNode{tx: tx, dm: dm}); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

// Begin starts a new database transaction. Exported so packages that import
// mpm-core (e.g. mpm-core/tools) can begin their own txs for non-atomic
// provenance writes.
func (dm *DatabaseManager) Begin() (*sql.Tx, error) {
	return dm.db.Begin() //nolint:all
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

// queryTrackedDefaultRetries is the retry budget for QueryTracked when the
// driver returns SQLITE_BUSY. Mirrors the default-budget policy in
// ExecTracked callers that pass retries=5 — chosen as a conservative
// match so long-running HybridSearch / recall scans survive the same
// write-side contention ExecTracked already tolerates.
const queryTrackedDefaultRetries = 5

// QueryTracked runs db.Query with timing and SQLITE_BUSY retry-backoff.
// Retries mirror ExecTracked (exponential 100ms→5s, capped) but with a
// fixed default budget — query callers don't need to specify it because
// every existing call site expects "transparent retry" semantics. If a
// future call site needs a different budget, introduce a
// QueryTrackedWithRetry variant rather than changing this signature.
func (dm *DatabaseManager) QueryTracked(query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	attempts := 0
	backoff := 100 * time.Millisecond
	maxBackoff := 5 * time.Second

	for {
		attempts++
		rows, err := dm.db.Query(query, args...)
		elapsed := time.Since(start)

		if err != nil && isBusyError(err) && attempts <= queryTrackedDefaultRetries {
			dm.busyRetries.Add(1) // instrument: SQLITE_BUSY retry counter
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		entry := watchdogOp{
			Timestamp:  start.UTC().Format(time.RFC3339Nano),
			Operation:  "query",
			DurationMs: elapsed.Milliseconds(),
			Query:      truncateQuery(query),
			Retries:    attempts - 1,
			Slow:       elapsed > slowQueryThreshold,
		}
		if err != nil {
			entry.Error = err.Error()
		}
		dm.logWatchdog(entry)

		return rows, err
	}
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
//
// D-003 (alpha-4.1.2): the matcher must also recognize the
// "database table is locked" wording. SQLite returns that message
// (SQLITE_LOCKED) when a write transaction contends with another
// transaction's RESERVED lock at table granularity — distinct
// from the database-wide "database is locked" (SQLITE_BUSY). The
// two share retry semantics in modern SQLite: busy_timeout applies
// to both, so classifying them together keeps ExecTracked and
// WithTx retry loops effective. Before this, parallel work-note
// callers (8 simultaneous AppendWorkEvent transactions) lost ~7
// of 8 events with the table-locked error falling through.
func IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
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
	db, err := sql.Open("sqlite3", sqliteWriteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	// NOTE: SetMaxOpenConns(1) is INTENTIONALLY NOT called here.
	//
	// The architectural intent (CLAUDE.md, sqlopen_owner_test.go) reads
	// as "single connection", but database/sql's connection pool cannot
	// safely be limited to 1 in this codebase: a goroutine holding a
	// connection inside an open transaction blocks any other goroutine
	// that tries to Begin() a second transaction, which then deadlocks
	// waiting for the first goroutine's transaction to release the
	// connection it already holds. Verified empirically on 2026-08-14:
	// a test run hung at database/sql.(*DB).connectionOpener after
	// SetMaxOpenConns(1) was added.
	//
	// SQLite's WAL mode + busy_timeout=5000 + ExecTracked/QueryTracked
	// retry-backoff handle the single-writer constraint at the driver
	// level. The "single shared connection" claim in docs is aspirational
	// — what we actually have is "many connections, but the writes are
	// serialized via SQLite's file lock".

	db.Exec("PRAGMA foreign_keys = ON")
	db.Exec("PRAGMA journal_mode = WAL")
	// busy_timeout is the per-connection SQLite lock-wait window. The DSN
	// suffix `_busy_timeout=5000` (see sqliteWriteDSN) is supposed to set
	// this at connection-open time, but mattn/go-sqlite3's lazy-connection
	// pool can open subsequent pooled connections without honoring it,
	// leaving busy_timeout at the SQLite default of 0 (no waiting).
	// Combined with go-sqlite3 v1.14.37's silent swallow of SQLITE_BUSY in
	// (*SQLiteRows).nextSyncLocked (sqlite3.go:2236-2245), a transient
	// BUSY response becomes an unbounded retry loop burning a full core —
	// see scheduler.go:572 / dispatch.go:90 (dispatchClaimNextAdHocWake)
	// for the canonical repro. Setting busy_timeout explicitly here closes
	// the gap structurally; database/sql re-runs the Exec on the first
	// connection of every newly pooled handle, so this complements rather
	// than duplicates the DSN path. (D-003 alpha-4.1.2 fix in the DSN
	// layer; this is the belt-and-suspenders Exec at construction time.)
	db.Exec("PRAGMA busy_timeout = 5000")
	db.Exec("PRAGMA synchronous = NORMAL")
	db.Exec("PRAGMA cache_size = -64000")

	manager := &DatabaseManager{
		db:           db,
		dbPath:       dbPath,
		watchdogPath: filepath.Join(filepath.Dir(dbPath), "watchdog.jsonl"),
	}

	// Resolve symlinks for health_check / integration gating. The raw
	// dbPath is preserved as dbPathRaw so operators can distinguish
	// "operator passed this path" from "the inode we actually opened".
	// evalSymlinksOnDB returns "" if the file doesn't exist yet (cold
	// boot) — that's fine, the HealthCheck response will simply show
	// raw == resolved.
	resolved, rerr := evalSymlinksOnDB(dbPath)
	if rerr == nil && resolved != "" {
		manager.dbPathRaw = manager.dbPath
		manager.dbPath = resolved
	}

	if err := manager.initUnifiedSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Baseline Cognitive Bootstrap — every production runtime boots
	// with the constitutional directives present (see the helper doc).
	if err := manager.seedBaselineDirectives(); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed baseline directives: %w", err)
	}

	// Baseline Cognitive Bootstrap for scheduled tasks. Runs AFTER
	// seedBaselineDirectives so the canonical directives
	// (mpm-seed-epistemic-compaction-policy) are present in the
	// memories table before the apply loop validates directive_id
	// references. Same idempotency contract: existing rows are
	// detected by stable id and preserved verbatim — operator's
	// custom cron / name / status / directive_id are NEVER
	// overwritten.
	if _, err := manager.seedBaselineScheduledTasks(); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed baseline scheduled tasks: %w", err)
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

	// Phase 1 of the multi-agent shared-epistemology arc (docs/archive/shared-epistemology.md):
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
	db, err := sql.Open("sqlite3", sqliteWriteDSN(dm.dbPath))
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
		dbPathRaw:    dm.dbPathRaw,
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
// shared DB uses the same schema as the local DB (see docs/archive/shared-epistemology.md
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
	`).Scan(&hasOldSchema); err != nil {
		slog.Warn("shared FTS5 migration: schema probe failed, skipping detection", "error", err.Error())
	} else if hasOldSchema > 0 {
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
//   - includeRetired: when true, include retired rules (retired_at IS NOT
//     NULL); when false (default), retired rules are filtered out so the
//     query returns the active set the agent should be working with.
//     Retired rules remain retrievable through this flag for forensic /
//     post-mortem queries.
//
// Part 1 (2026-09-06): added includeRetired parameter so retired rules
// are filterable. The retired_at column was added via SafeMigrations
// (schema.go:1245); fresh DBs see it from the shared CREATE TABLE loop
// (db.go:1267-1278), upgraded DBs gain it via attachShared's SafeMigrations
// pass (db.go:1461).
func (dm *DatabaseManager) QueryGlobalRules(query string, limit int, includeRetired bool) ([]map[string]interface{}, error) {
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
	// append-mostly per the docs/archive/shared-epistemology.md concurrency model.
	if err := dm.backfillSharedFTSIfEmpty(); err != nil {
		// Non-fatal — fall back to LIKE if backfill or FTS query fails.
		slog.Warn("shared FTS backfill failed; falling back to LIKE", "error", err.Error())
	}

	retiredClause := "AND retired_at IS NULL"
	if includeRetired {
		retiredClause = ""
	}

	var querySQL string
	var args []interface{}
	if query == "" {
		querySQL = `
			SELECT id, content, collection, tags, metadata, weight,
			       reinforcement_count, created_at, updated_at, is_global, retired_at
			FROM shared.memories
			WHERE is_global = 1 AND deleted_at IS NULL ` + retiredClause + `
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
			       m.reinforcement_count, m.created_at, m.updated_at, m.is_global, m.retired_at
			FROM shared.memories m
			JOIN shared.memories_fts fts ON m.rowid = fts.rowid
			WHERE shared.memories_fts MATCH ? AND m.is_global = 1 AND m.deleted_at IS NULL `+retiredClause+`
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
			       reinforcement_count, created_at, updated_at, is_global, retired_at
			FROM shared.memories
			WHERE is_global = 1 AND deleted_at IS NULL AND content LIKE ? `+retiredClause+`
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
//
// Part 1 (2026-09-06): reads retired_at as a nullable int64. Active
// rows surface no key for the field; retired rows surface
// `retired_at` (epoch seconds) so callers can distinguish and the
// display layer can render the retire timestamp.
func (dm *DatabaseManager) scanGlobalRuleRows(rows *sql.Rows) ([]map[string]interface{}, error) {
	var results []map[string]interface{}
	for rows.Next() {
		var id, content, coll sql.NullString
		var tags, meta, createdAt, updatedAt sql.NullString
		var weight sql.NullFloat64
		var reinforcement, isGlobal sql.NullInt64
		var retiredAt sql.NullInt64
		if err := rows.Scan(&id, &content, &coll, &tags, &meta, &weight, &reinforcement,
			&createdAt, &updatedAt, &isGlobal, &retiredAt); err != nil {
			return nil, fmt.Errorf("scanning global rule row: %w", err)
		}
		row := map[string]interface{}{
			"id":                  id.String,
			"content":             content.String,
			"collection":          coll.String,
			"tags":                tags.String,
			"metadata":            meta.String,
			"weight":              int(weight.Float64),
			"reinforcement_count": int(reinforcement.Int64),
			"created_at":          createdAt.String,
			"updated_at":          updatedAt.String,
			"is_global":           int(isGlobal.Int64),
			"source":              "shared",
		}
		if retiredAt.Valid && retiredAt.Int64 != 0 {
			row["retired_at"] = retiredAt.Int64
		}
		results = append(results, row)
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
// append-mostly per docs/archive/shared-epistemology.md so the cost amortizes to zero on
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

// evalSymlinksOnDB resolves the symlink chain of a database file path.
// Used by HealthCheck so the integration layer can compare the live
// attachment against a host-pinned expected path (symlink-equal).
//
// Returns ("", nil) when the file does not exist (cold-boot / first-run)
// — the underlying NewDatabaseManager will create it on next open.
func evalSymlinksOnDB(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return resolved, nil
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
	// We deliberately do NOT enable foreign_keys here. FK enforcement is the
	// DSN's job: the production open paths (NewDatabaseManager, NewSession,
	// NewSQLiteConnection) pass a DSN carrying `_foreign_keys=1`, so every
	// pooled connection enforces FKs structurally. This method is also used
	// by the test paths (InitSchema over NewDatabaseManagerForDB) with
	// session_ids that may not have a matching session row, so FK
	// enforcement stays off on connections whose DSN did not opt in (the
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

	// Blob storage table — MCP result spilling (Phase 1 pointer architecture).
	for _, sqlQuery := range BlobsTable {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute blob schema: %w\nSQL: %s", err, sqlQuery)
		}
	}

	// Work items table
	for _, sqlQuery := range WorkTables {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			return fmt.Errorf("failed to execute work schema: %w\nSQL: %s", err, sqlQuery)
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

	// Migration: create work_events table and seed initial created events for
	// existing works rows. Idempotent — uses migrated_at flag to prevent
	// double-seeding and CREATE TABLE IF NOT EXISTS for the table itself.
	if err := dm.migrateWorkEvents(); err != nil {
		return fmt.Errorf("migrateWorkEvents: %w", err)
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

	// Constraint migration: widen artifact_provenance for alpha-3
	// telemetry. Adds parent_invocation_id column (traces agent-of-agent
	// invocation trees) and extends artifact_type CHECK to include
	// 'handoff' and 'directive'. Idempotent — same read-sqlite_master /
	// skip-if-current pattern as migrateAuditLevelConstraint. Rebuilds
	// the six indexes inline so the migration is atomic with the
	// table recreate.
	if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
		return fmt.Errorf("migrateArtifactProvenanceWorkType: %w", err)
	}
	if err := dm.migrateArtifactProvenanceSchema(); err != nil {
		return fmt.Errorf("migrateArtifactProvenanceSchema: %w", err)
	}

	// Constraint migration: widen evidence.artifact_type CHECK to include
	// 'work'. Required so evidence rows can reference work artifacts for the
	// evidence-derived verification model. Same table-recreate pattern as
	// migrateAuditLevelConstraint. Idempotent via sqlite_master check.
	if err := dm.migrateEvidenceWorkType(); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: %w", err)
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
	defer tx.Rollback() // safe no-op after Commit; covers panic / early returns
	if err := MigrateDeletedAtToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migrate deleted_at: %w", err)
	}
	if err := MigrateDeletedAtZeroToNull(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("normalize legacy deleted_at=0 sentinel: %w", err)
	}
	if err := MigrateExpiresAtZeroToNull(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("normalize legacy expires_at=0 sentinel: %w", err)
	}
	if err := MigrateAllTimestampsToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("timestamps unification migration failed: %w", err)
	}
	// last_accessed_at_drift_v1 — focused remediation for the
	// memories.last_accessed_at column. Even after the broad
	// timestamps_unified_v1 sweep, several write paths continued
	// inserting CURRENT_TIMESTAMP (TEXT) into this INTEGER column;
	// a fresh write could reintroduce drift on a previously-clean
	// database. The dedicated migration guarantees the column is
	// INTEGER at boot, regardless of intermediate state.
	if err := MigrateLastAccessedAtToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("last_accessed_at drift migration failed: %w", err)
	}
	if err := MigrateWeightToReal(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("weight column real conversion failed: %w", err)
	}
	// confidence_history CHECK widening (2026-09-05 audit P0): the
	// pre-alpha-final schema accepts only 6 trigger values; the
	// canonical schema.go declaration accepts 9 (adds concept_drift,
	// supersede, invalidate). Without this migration, mpm_decisions
	// supersede/invalidate both fail on the live DB with a CHECK
	// constraint violation because the writers attempt to insert
	// trigger='supersede' / trigger='invalidate' rows. Idempotent via
	// the schema_migrations sentinel.
	if err := MigrateConfidenceHistoryCheckWidening(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("confidence_history check widening migration failed: %w", err)
	}
	// H-3 fix (post-M3 audit, 2026-08-31): adds the wake_scheduled
	// column to epistemic_cascade_outbox so that a crash between
	// markMaterialized and ScheduleWake is recoverable — the
	// reconcile pass scans materialized rows with wake_scheduled=0
	// and re-schedules the wake. Idempotent via sentinel.
	if err := MigrateCascadeWakeScheduled(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("cascade wake_scheduled migration failed: %w", err)
	}
	// 2026-09-19 release-pass: fired_by column on scheduled_wakes so
	// explicit operator/agent resolutions (wake-resolver) can be told
	// apart from automatic cascade materializer / reconciler firings.
	// Idempotent via wake_resolver_fired_by_v1 sentinel.
	if err := MigrateWakeResolverFiredBy(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("wake resolver fired_by migration failed: %w", err)
	}
	// Positive-direction (constructive) cascade: polarity column on
	// epistemic_provenance. Idempotent via the schema_migrations sentinel
	// pattern. See migration_epistemic_provenance_polarity.go for the
	// back-compat invariant (NULL polarity must never fire positive
	// cascade). NOT in SafeMigrations because that list is ADD COLUMN
	// only and doesn't support inline CHECK constraints.
	if err := MigrateEpistemicProvenancePolarity(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("epistemic_provenance polarity migration failed: %w", err)
	}
	// session_handoffs.session_id → nullable. See
	// migration_session_handoffs_optional_session_id.go for the
	// rationale (external session identifiers are correlation metadata,
	// not a prerequisite for handoff persistence).
	if err := MigrateSessionHandoffsOptionalSessionID(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("session_handoffs session_id optional migration failed: %w", err)
	}
	// session_handoffs mpm_session_id + framework_session_id — additive
	// ALTER TABLE migration that introduces the MPM-owned session
	// identity column (mpm_session_id) and the host-owned session
	// identity column (framework_session_id). Both nullable on
	// pre-existing rows; new writes from EndSessionV2 populate them.
	// Idempotent via the session_handoffs_mpm_session_id_v1 sentinel.
	if err := MigrateSessionHandoffsMPMSessionID(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("session_handoffs mpm_session_id migration failed: %w", err)
	}
	// tool_invocations mpm_session_id + framework_session_id — additive
	// ALTER TABLE migration that surfaces the canonical session
	// identity dimensions on the audit substrate so recent_activity
	// can filter by either dimension. Both nullable so pre-existing
	// rows remain valid (their original session_id is unchanged).
	// Idempotent via tool_invocations_session_identity_v1 sentinel.
	if err := MigrateToolInvocationsSessionIdentity(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tool_invocations session_identity migration failed: %w", err)
	}
	// created_at_backfill_v1 — repairs rows whose created_at was
	// inserted as NULL by the pre-fix seed path. read_directives
	// scans created_at into a non-NULL Go string, so a single
	// NULL row crashes the agent's wake-prime-directives step.
	// Idempotent via sentinel. See migration_memories_created_at_backfill.go.
	if err := MigrateMemoriesCreatedAtBackfill(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("created_at backfill migration failed: %w", err)
	}
	// tags_json_normalize_v1 — repair legacy corrupted `memories.tags`
	// rows produced by SupersedeDecision/InvalidateDecision before the
	// JSON-array fix landed. Without this, every `mpm memory list`
	// prints "failed to unmarshal tags" warnings for any decision that
	// went through supersede/invalidate pre-fix. Idempotent via sentinel.
	if err := MigrateTagsJSONNormalize(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tags JSON normalize migration failed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}

	// Migration: convert lessons table to lessons_base + lessons view.
	// This replaces the buggy AFTER-INSERT FTS trigger with an INSTEAD-OF trigger
	// on the view, so failed lessons inserts can never leave orphaned FTS rows.
	// Safe to call on every startup — idempotent if lessons_base already exists.
	dm.migrateLessonsToView()

	// 2026-09-10 lesson lifecycle fix: deleted_at tombstone on
	// lessons_base. The SafeMigrations loop runs BEFORE this
	// migrateLessonsToView call, when lessons_base doesn't exist
	// yet — so the inline ADD COLUMN inside migrateLessonsToView
	// covers fresh DBs, and this ALTER covers existing databases
	// whose lessons_base was created by an earlier boot. Idempotent
	// via isDuplicateColumnError.
	if _, err := dm.db.Exec(`ALTER TABLE lessons_base ADD COLUMN deleted_at INTEGER`); err != nil {
		if !isDuplicateColumnError(err) {
			slog.Warn("migrateLessonsToView: post-add deleted_at failed",
				"error", err.Error())
		}
	}

	// Column-affinity rebuild: flip legacy DATETIME/TEXT timestamp
	// columns on `memories` (and `shared.memories`) to INTEGER so
	// mattn/go-sqlite3 returns integer-shaped values that scan
	// cleanly into *int64. Without this, `mpm doctor`'s Review
	// backlog check fails with "converting driver.Value type
	// time.Time to a int64: invalid syntax".
	//
	// Runs AFTER the outer migration transaction commits because
	// the rebuild manages its own PRAGMA foreign_keys envelope
	// (PRAGMA foreign_keys is a no-op inside a transaction, per
	// SQLite's transaction-restrictions doc). Idempotent via the
	// `memories_column_affinity_v1` and `shared_memories_column_affinity_v1`
	// sentinels — no-op on databases that have already been migrated.
	//
	// Codifies the fifth member of the Substrate Defense Triad:
	// column-affinity-rebuild. Together with commit 627d0d8's
	// column-write-shape guard, this closes the schema-timestamp-class
	// problem (prevent new drift + clean legacy drift).
	if err := RebuildMemoriesColumnAffinity(dm.db); err != nil {
		return fmt.Errorf("memories column affinity rebuild: %w", err)
	}

	// EnforceMemoriesCreatedAtNotNull runs after the affinity rebuild
	// because both use the FK envelope (outside any transaction). The
	// affinity rebuild flips created_at to INTEGER; this migration
	// then flips the column to NOT NULL — completing the
	// schema-level discipline triad for the one timestamp column
	// where "missing" has no defensible meaning. Idempotent via
	// sentinel + schema-shape probe. See
	// migration_memories_created_at_not_null.go.
	if err := EnforceMemoriesCreatedAtNotNull(dm.db); err != nil {
		return fmt.Errorf("memories created_at NOT NULL enforcement: %w", err)
	}

	// alpha-5 D-12.1: alter scheduled_tasks timestamp column DECLARED TYPEs
	// (DATETIME/DATE/TIMESTAMP) to INTEGER. mattn/go-sqlite3 returns Go types
	// based on declared TYPE; even after timestamps_unified_v1 converted the
	// storage class to INTEGER, the legacy declared TYPE made the driver
	// hand back time.Time and break every scan into int64. SQLite does not
	// support ALTER COLUMN, so this uses the table-recreate pattern. Runs
	// after the outer migration transaction commits and rebuilds its own
	// index. Idempotent — no-op when no DATETIME column remains.
	if err := dm.migrateScheduledTasksToIntegerColumns(); err != nil {
		return fmt.Errorf("scheduled_tasks column affinity rebuild: %w", err)
	}

	// Try FTS5 tables - if they fail, continue without them (fallback search)
	if err := dm.initFTSTables(); err != nil {
		slog.Warn("FTS5 initialization failed; search will use LIKE fallback", "error", err.Error())
	} else {
		// FTS5 tables ready — backfill any existing data that predates the triggers
		dm.backfillFTSTables()
		// F2 repair: lessons_fts can be rowid/content-desynced from
		// lessons_base on databases that predate the view+INSTEAD OF
		// trigger migration. A desync makes lesson search confidently
		// return WRONG lessons (indexed text JOINed to the wrong base
		// row). Verify + rebuild once, gated by a schema_migrations
		// sentinel so steady-state boot cost is one indexed lookup.
		if err := dm.EnsureLessonsFTSInSync(); err != nil {
			slog.Warn("lessons_fts sync verification failed; search may fall back to LIKE", "error", err.Error())
		}
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

// seedBaselineDirectives runs the Baseline Cognitive Bootstrap
// (tiered fallback seeding, 2026-08-19): the four constitutional
// global directives are ALWAYS seeded into the local store, so a
// standalone runtime (no MPM_SHARED_DB) is never directive-blind.
// Idempotent by stable-id primary key — existing rows with matching
// content are skipped, operator edits are preserved (flagged, never
// overwritten). Deliberately NOT part of initUnifiedSchema: that
// function is also the hermetic test path (NewDatabaseManagerForDB +
// InitSchema), which must stay directive-free so fixture assertions
// don't depend on boot policy. Production entry points all construct
// via NewDatabaseManager, so the guarantee holds for every runtime
// binary (CLI, MCP, scheduler, critic). `mpm ops init directives`
// remains available for manual re-init / shared-DB seeding.
func (dm *DatabaseManager) seedBaselineDirectives() error {
	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		return fmt.Errorf("seed baseline directives: %w", err)
	}
	if len(summary.Created) > 0 {
		slog.Info("baseline directives seeded", "created", len(summary.Created))
	}
	return nil
}

// seedBaselineScheduledTasks runs the Baseline Cognitive Bootstrap
// for scheduled tasks (2026-09-01 productization pass). The canonical
// scheduled task list lives in seed.SeedScheduledTasks; the apply
// logic lives here in core because it depends on core-only types
// (ScheduledTask, CalculateNextRun, the dm's UpsertScheduledTask
// writer). The split is registry-in-seed, apply-in-core — a seed ->
// core import for the apply loop would create a cycle.
//
// Contract (parallel to seedBaselineDirectives):
//
//   - StableID absent       → Created (UpsertScheduledTask via the
//     canonical writer; cron validated and
//     next_run_at computed by CalculateNextRun)
//   - StableID present      → Skipped (no-op). Operator's custom
//     cron_expr / name / directive_id / status
//     are preserved verbatim.
//   - DirectiveID missing   → Missing (the task is skipped; the
//     operator can run `mpm ops init
//     directives` first and re-run this).
//
// This runs AFTER seedBaselineDirectives so the canonical
// directives (specifically mpm-seed-epistemic-compaction-policy)
// are present in the memories table before the apply loop
// validates the directive_id FK.
//
// `mpm ops init tasks` remains available for manual re-init /
// shared-DB seeding.
func (dm *DatabaseManager) seedBaselineScheduledTasks() (seed.SeedTaskSummary, error) {
	summary := seed.SeedTaskSummary{
		Created: []string{},
		Skipped: []string{},
		Missing: []string{},
	}

	now := time.Now().UTC()

	for _, st := range seed.SeedScheduledTasks {
		// 1. Look up the row by id. We don't filter on status: even
		// a soft-deleted (paused-then-deleted) task should not be
		// silently re-created — but since the table has no soft-delete
		// column (DELETE is hard), the only way to get a "missing"
		// row is the operator running `mpm tasks delete`.
		var existingID string
		err := dm.db.QueryRow(
			`SELECT id FROM scheduled_tasks WHERE id = ?`, st.StableID,
		).Scan(&existingID)
		switch {
		case err == nil:
			summary.Skipped = append(summary.Skipped, st.StableID)
			continue
		case err == sql.ErrNoRows:
			// fall through to directive-existence check
		default:
			return summary, fmt.Errorf("seed lookup %s: %w", st.StableID, err)
		}

		// 2. Verify the directive exists before inserting. The
		// scheduled_tasks table does not enforce a FK on directive_id
		// (the column is text and the directive lives in memories),
		// so a missing directive would only surface at wake time —
		// too late. Fail fast here.
		var dirID string
		err = dm.db.QueryRow(
			`SELECT id FROM memories WHERE id = ? AND collection = 'directives' AND deleted_at IS NULL LIMIT 1`,
			st.DirectiveID,
		).Scan(&dirID)
		switch {
		case err == nil:
			// fall through to insert
		case err == sql.ErrNoRows:
			summary.Missing = append(summary.Missing, st.StableID)
			continue
		default:
			return summary, fmt.Errorf("seed directive lookup for %s: %w", st.StableID, err)
		}

		// 3. Status defaulting. SeedScheduledTask entries may leave
		// Status empty to mean "use the substrate default". The
		// substrate default is "active" (ScheduledTaskActive) — the
		// seed never inserts a paused task.
		status := st.Status
		if status == "" {
			status = seed.ScheduledTaskActive
		}

		// 4. Compute next_run_at via the canonical cron calculator.
		// The apply loop uses CalculateNextRun directly (not
		// UpsertScheduledTask) so the seed package does not need
		// to import core — registry-in-seed, apply-in-core split
		// keeps the dependency direction one-way (core -> seed).
		//
		// A canonical cron expression always parses, but we still
		// handle the error to surface typos in the registry before
		// they cause a poison-pill loop in the daemon.
		nextRun, err := CalculateNextRun(st.CronExpr, now)
		if err != nil {
			return summary, fmt.Errorf("seed cron calc for %s: %w", st.StableID, err)
		}

		// 5. Insert via the canonical writer. ComputeNextRun already
		// happened above; the writer recomputes it from `now` to
		// avoid a microsecond drift between the seed path and the
		// operator's `mpm tasks upsert` path.
		task := ScheduledTask{
			ID:          st.StableID,
			Name:        st.Name,
			CronExpr:    st.CronExpr,
			DirectiveID: st.DirectiveID,
			Status:      status,
			NextRunAt:   nextRun.Unix(),
			CreatedAt:   now.Unix(),
			UpdatedAt:   now.Unix(),
		}
		if err := dm.UpsertScheduledTask(task); err != nil {
			return summary, fmt.Errorf("upsert seed task %s: %w", st.StableID, err)
		}
		summary.Created = append(summary.Created, st.StableID)
	}

	if len(summary.Created) > 0 {
		slog.Info("baseline scheduled tasks seeded", "created", len(summary.Created))
	}
	return summary, nil
}

// SeedBaselineScheduledTasks is the public wrapper around
// seedBaselineScheduledTasks. It is the entry point the CLI's
// `mpm ops init tasks` command uses for manual re-init. Production
// boot path calls the unexported seedBaselineScheduledTasks
// directly from NewDatabaseManager — that path is what makes a
// fresh install arrive with the canonical tasks configured.
//
// The wrapper exists so the CLI does not need to import the
// seed package directly for the apply loop (which lives in core,
// not seed). Returns the same SeedTaskSummary shape used by the
// unexported function.
func (dm *DatabaseManager) SeedBaselineScheduledTasks() (seed.SeedTaskSummary, error) {
	return dm.seedBaselineScheduledTasks()
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
	// Probe object types up front so recovery can distinguish the three
	// historical states:
	//   A. lessons table, no lessons_base      → fresh legacy install
	//   B. lessons_base + lessons VIEW          → migration already done
	//   B'.lessons_base + lessons view, triggers missing → interrupted after
	//      Step 3 of an older non-atomic run (lesson writes fail with
	//      "cannot modify lessons because it is a view" until repaired)
	//   C. lessons_base + lessons TABLE         → interrupted between rename
	//      and view creation; a later boot's BaseTables re-created an empty
	//      `lessons` table, orphaning the operator's data in lessons_base
	//
	// States C/B' were reachable because this migration historically ran as
	// independent auto-commit statements — a crash mid-sequence left a
	// permanently wedged lesson surface. The whole sequence now runs inside
	// one transaction (SQLite DDL is transactional), and the recovery path
	// below repairs databases already sitting in C or B'.
	var baseExists int
	if err := dm.db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='lessons_base'`).Scan(&baseExists); err != nil {
		// sql.ErrNoRows on a fresh install is the EXPECTED state —
		// lessons_base hasn't been created yet, the migration will do
		// that on this run. Anything else (corrupt sqlite_master,
		// permission denied, IO error) is a real failure.
		// D-004 (alpha-4.1.2): previously this logged a WARN for the
		// expected ErrNoRows path too, polluting every fresh-boot
		// slog stream with "treating as absent" noise. Now we only
		// log real errors.
		if err != sql.ErrNoRows {
			slog.Warn("migrateLessonsToView: probe lessons_base failed", "error", err)
		}
	}

	// If lessons_base exists, the rename already happened.
	if baseExists != 0 {
		dm.finishOrRepairLessonsView()
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
		// 2026-09-10 lesson lifecycle fix: the deleted_at tombstone
		// lets `delete` be reversible. Inline here for fresh DBs;
		// existing DBs pick it up via the post-migrateLessonsToView
		// ALTER in initUnifiedSchema.
		{"deleted_at", "INTEGER"},
	} {
		dm.db.Exec(fmt.Sprintf("ALTER TABLE lessons ADD COLUMN %s %s", col.name, col.def))
	}

	// Steps 2–5 run inside ONE transaction. SQLite DDL is transactional,
	// so any crash mid-sequence rolls back to the pre-migration state and
	// the next boot retries cleanly. (The historical auto-commit sequence
	// here is what produced the permanently-wedged interrupted states that
	// finishOrRepairLessonsView now also heals.)
	tx, err := dm.db.Begin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: begin tx failed: %v\n", err)
		return
	}
	defer tx.Rollback()

	// Step 2: rename lessons → lessons_base
	if _, err := tx.Exec(`ALTER TABLE lessons RENAME TO lessons_base`); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: rename lessons→lessons_base failed: %v\n", err)
		return
	}

	// Step 2.5: ensure lessons_fts exists BEFORE creating the INSTEAD OF
	// triggers below (D2 fix, 2026-08-25).
	//
	// The trigger bodies reference lessons_fts. Historically lessons_fts was
	// only created later by initFTSTables — which runs AFTER
	// RebuildMemoriesColumnAffinity in initUnifiedSchema's boot sequence.
	// SQLite tolerates triggers referencing missing tables at CREATE time,
	// but any later ALTER TABLE ... RENAME forces a full schema re-parse and
	// fails hard with "error in trigger lessons_instead_of_insert: no such
	// table: main.lessons_fts". On a legacy database (the exact population
	// the affinity rebuild exists to migrate) this wedged every subsequent
	// invocation behind "failed to initialize schema".
	//
	// Same DDL as initFTSTables so the later statement is a pure no-op.
	// Best-effort: on builds without FTS5 the CREATE fails and we proceed —
	// initFTSTables owns the LIKE-fallback degradation path.
	if _, err := tx.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');`); err != nil {
		slog.Warn("migrateLessonsToView: early lessons_fts creation failed (non-FTS5 build?)", "error", err.Error())
	}

	if err := createLessonsViewAndTriggers(tx); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: %v\n", err)
		return
	}

	if err := tx.Commit(); err != nil {
		fmt.Fprintf(os.Stderr, "migrateLessonsToView: commit failed: %v\n", err)
		return
	}

	// Alpha-4 D-004: was fmt.Fprintf(os.Stderr, ...) — demoted to
	// slog.Info so the success message is routed through the same
	// channel as the rest of the operational telemetry. Under
	// machine mode (mpm call, mpm-mcp), slog is set to io.Discard
	// so the wire stays parseable. The error branches above remain
	// on direct stderr because they represent true migration faults
	// the operator must see.
	slog.Info("migrateLessonsToView: lessons→lessons_base+migrated")
}

// execQuerier is satisfied by both *sql.DB and *sql.Tx; the trigger DDL is
// shared between the fresh-migration tx and the repair path below.
type execQuerier interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// createLessonsViewAndTriggers emits the lessons view + the three INSTEAD OF
// triggers. Called from migrateLessonsToView's tx and from the repair path.
func createLessonsViewAndTriggers(q execQuerier) error {
	// Step 2: create lessons view that exposes all columns
	viewSQL := `
	CREATE VIEW IF NOT EXISTS lessons AS
	SELECT rowid, id, type, content, tags, reinforcement_count,
	       source_session_id, created, content_hash,
	       retrieval_priority, importance, confidence
	FROM lessons_base`
	if _, err := q.Exec(viewSQL); err != nil {
		return fmt.Errorf("create lessons view failed: %w", err)
	}

	// Step 3: INSTEAD OF INSERT — atomically writes to base table + lessons_fts
	// The trigger body is a single statement-pair; if lessons_fts insert fails, the
	// entire INSERT is rolled back — no orphan possible.
	//
	// COALESCE on retrieval_priority/importance/confidence: the lessons_base
	// columns are NOT NULL DEFAULT 0.5/0.5/0.7, but the trigger reads NEW.*,
	// which is NULL when the caller omits the column on the view. Without
	// COALESCE, an INSERT that doesn't supply every column fails with a
	// NOT NULL constraint violation. COALESCE matches the table default so
	// partial inserts work the same as full inserts.
	if _, err := q.Exec(`
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
		return fmt.Errorf("create INSTEAD OF INSERT trigger failed: %w", err)
	}

	// Step 4: INSTEAD OF UPDATE — same COALESCE treatment so partial
	// UPDATEs that don't touch retrieval_priority/importance/confidence
	// don't accidentally NULL those columns out and violate NOT NULL.
	if _, err := q.Exec(`
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
		return fmt.Errorf("create INSTEAD OF UPDATE trigger failed: %w", err)
	}

	// Step 5: INSTEAD OF DELETE
	if _, err := q.Exec(`
		CREATE TRIGGER lessons_instead_of_delete
		INSTEAD OF DELETE ON lessons
		BEGIN
			DELETE FROM lessons_base WHERE rowid=OLD.rowid;
			DELETE FROM lessons_fts WHERE rowid=OLD.rowid;
		END`); err != nil {
		return fmt.Errorf("create INSTEAD OF DELETE trigger failed: %w", err)
	}
	return nil
}

// finishOrRepairLessonsView completes or heals a database whose
// lessons→lessons_base rename already happened:
//
//   - healthy state (lessons is a view + all three triggers): drop any leftover
//     legacy AFTER triggers and return.
//   - interrupted state B' (view exists, INSTEAD OF triggers missing — reachable
//     via a crash in a pre-transactional boot between CREATE VIEW and CREATE
//     TRIGGER): lesson writes fail with "cannot modify lessons because it is a
//     view" until the triggers are re-created. Create the missing ones.
//   - interrupted state C (lessons is a plain TABLE again because a later
//     boot's BaseTables re-created it after a crash between rename and view
//     creation): salvage any rows that landed in the recreated table into
//     lessons_base, drop the shadow table, and build the real view + triggers.
func (dm *DatabaseManager) finishOrRepairLessonsView() {
	for _, old := range []string{"lessons_ai", "lessons_ad", "lessons_au"} {
		dm.db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, old))
	}

	// 2026-09-10 lesson lifecycle fix: ensure deleted_at exists on
	// the already-migrated lessons_base. Fresh DBs pick this up via
	// the inline ADD COLUMN at the top of migrateLessonsToView; this
	// branch covers the case where lessons_base already existed
	// (and was renamed from `lessons` by a prior boot) without the
	// tombstone. Idempotent via isDuplicateColumnError.
	if _, err := dm.db.Exec(`ALTER TABLE lessons_base ADD COLUMN deleted_at INTEGER`); err != nil {
		if !isDuplicateColumnError(err) {
			slog.Warn("migrateLessonsToView: repair add deleted_at failed",
				"error", err.Error())
		}
	}

	// What object is `lessons` today?
	var objType string
	err := dm.db.QueryRow(`SELECT type FROM sqlite_master WHERE name='lessons'`).Scan(&objType)
	if err != nil {
		// View/table entirely absent (crash after rename, before anything else
		// ran AND before BaseTables re-created the table): just finish the job.
		if ferr := dm.ensureLessonsFTS(); ferr != nil {
			slog.Warn("migrateLessonsToView: repair lessons_fts creation failed", "error", ferr.Error())
		}
		tx, terr := dm.db.Begin()
		if terr != nil {
			fmt.Fprintf(os.Stderr, "migrateLessonsToView: repair begin failed: %v\n", terr)
			return
		}
		defer tx.Rollback()
		if cerr := createLessonsViewAndTriggers(tx); cerr != nil {
			fmt.Fprintf(os.Stderr, "migrateLessonsToView: repair (missing view) failed: %v\n", cerr)
			return
		}
		if cerr := tx.Commit(); cerr != nil {
			fmt.Fprintf(os.Stderr, "migrateLessonsToView: repair commit failed: %v\n", cerr)
			return
		}
		slog.Warn("migrateLessonsToView: repaired interrupted migration (lessons view was missing)")
		return
	}

	switch objType {
	case "view":
		// Healthy or B'. Ensure all three INSTEAD OF triggers exist.
		var missing int
		if scanErr := dm.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN
			('lessons_instead_of_insert','lessons_instead_of_update','lessons_instead_of_delete')`).Scan(&missing); scanErr != nil {
			// Probe failed — force the repair path rather than trusting an
			// unknown trigger state.
			slog.Warn("migrateLessonsToView: trigger-count probe failed; forcing repair", "error", scanErr.Error())
			missing = -1
		}
		if missing == 3 {
			return // healthy — nothing to do
		}
		if err := dm.ensureLessonsFTS(); err != nil {
			slog.Warn("migrateLessonsToView: repair lessons_fts creation failed", "error", err.Error())
		}
		for _, trig := range []string{"lessons_instead_of_insert", "lessons_instead_of_update", "lessons_instead_of_delete"} {
			dm.db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, trig))
		}
		if err := createLessonsViewAndTriggers(dm.db); err != nil {
			fmt.Fprintf(os.Stderr, "migrateLessonsToView: trigger repair failed: %v\n", err)
			return
		}
		slog.Warn("migrateLessonsToView: repaired interrupted migration (INSTEAD OF triggers were missing)")
	case "table":
		// Interrupted state C: BaseTables re-created an empty (or partially
		// written) lessons TABLE after the real data was renamed to
		// lessons_base. Salvage rows, drop the shadow table, rebuild the
		// view — one atomic WithTx (canonical transaction shape).
		salvageErr := dm.WithTx(func(node DBNode) error {
			// The shadow table may pre-date the retrieval-metadata columns;
			// map explicitly and let lessons_base defaults cover the rest.
			if _, err := node.Tx().Exec(`
				INSERT OR IGNORE INTO lessons_base
					(id, type, content, tags, reinforcement_count, source_session_id, created, content_hash)
				SELECT id, type, content, tags,
				       COALESCE(reinforcement_count, 1), source_session_id, created, content_hash
				FROM lessons`); err != nil {
				return fmt.Errorf("salvage copy: %w", err)
			}
			if _, err := node.Tx().Exec(`DELETE FROM lessons`); err != nil {
				return fmt.Errorf("salvage clear: %w", err)
			}
			if _, err := node.Tx().Exec(`DROP TABLE lessons`); err != nil {
				return fmt.Errorf("salvage drop: %w", err)
			}
			if err := dm.ensureLessonsFTS(); err != nil {
				slog.Warn("migrateLessonsToView: repair lessons_fts creation failed", "error", err.Error())
			}
			if err := createLessonsViewAndTriggers(node.Tx()); err != nil {
				return fmt.Errorf("salvage rebuild: %w", err)
			}
			return nil
		})
		if salvageErr != nil {
			fmt.Fprintf(os.Stderr, "migrateLessonsToView: salvage failed: %v\n", salvageErr)
			return
		}
		slog.Warn("migrateLessonsToView: repaired interrupted migration (shadow lessons table removed)")
	default:
		slog.Warn("migrateLessonsToView: unexpected lessons object type", "type", objType)
	}
}

// ensureLessonsFTS creates lessons_fts if absent. Same DDL as initFTSTables;
// best-effort so non-FTS5 builds degrade to initFTSTables' LIKE fallback.
func (dm *DatabaseManager) ensureLessonsFTS() error {
	if _, err := dm.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');`); err != nil {
		return err
	}
	return nil
}

func (dm *DatabaseManager) initFTSTables() error {
	// Robust FTS5 availability check
	var available int
	if err := dm.db.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&available); err != nil {
		slog.Warn("initFTSTables: probe FTS5 compile options, treating as unavailable", "error", err)
	}
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
		// 2026-09-22 release-acceptance repair: fts_recovery drops the
		// INSTEAD OF triggers on the `lessons` view whenever it sees
		// them reference a missing virtual table. If we recreate the
		// vtab here without also recreating the INSTEAD OF triggers,
		// every subsequent INSERT into the lessons view surfaces
		// "cannot modify lessons because it is a view". The DROP IF
		// EXISTS + CREATE TRIGGER pair makes the recovery idempotent
		// (canonical sync triggers for memories/sessions/etc. use
		// IF NOT EXISTS; the lessons triggers can't because SQLite's
		// INSTEAD OF trigger syntax requires a name, not IF NOT
		// EXISTS, in the CREATE statement). See createLessonsViewAndTriggers
		// for the canonical DDL kept in lockstep with these strings.
		`DROP TRIGGER IF EXISTS lessons_instead_of_insert`,
		`CREATE TRIGGER lessons_instead_of_insert INSTEAD OF INSERT ON lessons BEGIN INSERT INTO lessons_base(rowid, id, type, content, tags, reinforcement_count, source_session_id, created, content_hash, retrieval_priority, importance, confidence) VALUES (NEW.rowid, NEW.id, NEW.type, NEW.content, NEW.tags, COALESCE(NEW.reinforcement_count, 1), NEW.source_session_id, NEW.created, NEW.content_hash, COALESCE(NEW.retrieval_priority, 0.5), COALESCE(NEW.importance, 0.5), COALESCE(NEW.confidence, 0.7)); INSERT INTO lessons_fts(rowid, content, tags) VALUES (NEW.rowid, NEW.content, NEW.tags); END;`,
		`DROP TRIGGER IF EXISTS lessons_instead_of_update`,
		`CREATE TRIGGER lessons_instead_of_update INSTEAD OF UPDATE ON lessons BEGIN UPDATE lessons_base SET rowid=NEW.rowid, id=NEW.id, type=NEW.type, content=NEW.content, tags=NEW.tags, reinforcement_count=COALESCE(NEW.reinforcement_count, reinforcement_count), source_session_id=NEW.source_session_id, created=NEW.created, content_hash=NEW.content_hash, retrieval_priority=COALESCE(NEW.retrieval_priority, retrieval_priority), importance=COALESCE(NEW.importance, importance), confidence=COALESCE(NEW.confidence, confidence) WHERE rowid=OLD.rowid; DELETE FROM lessons_fts WHERE rowid=OLD.rowid; INSERT INTO lessons_fts(rowid, content, tags) VALUES (NEW.rowid, NEW.content, NEW.tags); END;`,
		`DROP TRIGGER IF EXISTS lessons_instead_of_delete`,
		`CREATE TRIGGER lessons_instead_of_delete INSTEAD OF DELETE ON lessons BEGIN DELETE FROM lessons_base WHERE rowid=OLD.rowid; DELETE FROM lessons_fts WHERE rowid=OLD.rowid; END;`,
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
	).Scan(&ftsNeedsMigration); err != nil {
		slog.Warn("mpm: FTS5 schema probe failed, skipping migration detection", "error", err.Error())
		ftsNeedsMigration = false
	} else if ftsNeedsMigration {
		slog.Info("mpm: FTS5 migration detected (memories_fts with tags UNINDEXED); rebuilding with tags indexed")
	} else {
		// Shape-mismatch detection (Stage 7): a memories_fts whose declared
		// columns don't include `collection` cannot accept the new-style sync
		// triggers' INSERT(rowid, content, collection, session_id, tags).
		// Every INSERT would fail at trigger time — on boot-critical paths
		// that wedges the database ("table memories_fts has no column named
		// collection"). Drop and recreate with the canonical shape so the
		// generic backfill below reindexes existing content.
		var shapeMismatch int
		if err := dm.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories_fts'
			 AND (sql NOT LIKE '%collection%' OR sql IS NULL)`,
		).Scan(&shapeMismatch); err != nil {
			slog.Warn("mpm: FTS5 shape probe failed, skipping mismatch detection", "error", err.Error())
		} else if shapeMismatch > 0 {
			slog.Info("mpm: FTS5 shape mismatch detected (memories_fts without collection column); rebuilding")
			ftsNeedsMigration = true
		}
	}
	if ftsNeedsMigration {
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
				// rowid (not id): memories_fts is standalone FTS5 keyed by the
				// memories table's rowid so the sync triggers' DELETE-by-rowid
				// works. The previous `SELECT id` put a TEXT hex id into rowid,
				// which coerced to 0 and aborted the statement on the second row.
				if _, err := dm.db.Exec(
					`INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
					 SELECT rowid, content, collection, session_id, tags FROM memories WHERE deleted_at IS NULL`,
				); err != nil {
					slog.Warn("mpm: FTS5 migration backfill failed", "error", err.Error())
				} else {
					slog.Info("mpm: FTS5 migration complete; tag tokens now searchable")
				}
			}
		}
	}

	// Stage 0 (Pi alpha) follow-on: an existing MPM database can enter a
	// partially-initialized FTS5 state in which the base tables remain
	// intact but the FTS virtual tables are missing while their shadow
	// tables remain (orphan state). The Round 6 backup/restore pipeline
	// silently created that state by stripping both `CREATE VIRTUAL TABLE`
	// (rejected by validator) and `INSERT INTO sqlite_master VALUES(...,
	// 'CREATE VIRTUAL TABLE ...')` (rejected by mattn driver-compat),
	// while still replaying the FTS shadow tables as plain CREATE TABLE.
	// repairOrphanedFTS5 detects that state, drops the orphaned shadow
	// tables (which are disposable projections), recreates the virtual
	// tables, reattaches the canonical sync triggers, and rebuilds the
	// indices from canonical base-table data. It is a no-op on clean DBs.
	if repaired, err := dm.repairOrphanedFTS5(); err != nil {
		// Repair errors are non-fatal — recovery is best-effort, the
		// CREATE VIRTUAL TABLE IF NOT EXISTS loop below will still try
		// to create whatever can be created.
		slog.Warn("initFTSTables: fts_recovery reported errors (continuing)", "repaired", repaired, "error", err.Error())
	} else if repaired > 0 {
		slog.Info("initFTSTables: fts_recovery repaired orphan FTS state", "domains", repaired)
	}

	for _, sqlQuery := range ftsStatements {
		if _, err := dm.db.Exec(sqlQuery); err != nil {
			// FTS5 init is best-effort: never let one failed CREATE block
			// the others. Each statement targets a different FTS module
			// and they don't depend on each other (the triggers fire on
			// INSERT not on init). Triggers that reference a missing
			// virtual table here are unreachable to a writer until that
			// virtual table exists, so a partial-failure state leaves
			// writes broken on the failed domains but not on the others.
			//
			// Pre-fix behavior was `return error` here, which made one
			// failed domain leave triggers active on every other domain
			// referencing modules that the live DB does not have (Pi alpha
			// Finding A/B/C root cause).
			fmt.Fprintf(os.Stderr, "FTS5 init error (continuing, search will use LIKE for this domain): %v\nSQL: %s\n", err, sqlQuery)
			slog.Warn("initFTSTables: FTS5 statement failed", "error", err.Error())
			continue
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
		if err := dm.db.QueryRow("SELECT COUNT(*) FROM " + b.destTable).Scan(&count); err != nil {
			slog.Warn("backfillFTSTables: count check failed, treating as empty: %v", "table", b.destTable)
		}
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
	// Join in-flight mirror writes (ChallengeMemoryAsync) with a bounded
	// wait so Close can't hang on a wedged mirror writer, but does not tear
	// the DB down under a live one.
	done := make(chan struct{})
	go func() {
		dm.mirrorWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn("DatabaseManager.Close: mirror writers did not drain within 2s; proceeding")
	}

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
	return dm.MaterializeCascadeIntentsFor(ctx, limit, nil)
}

// MaterializeCascadeIntentsFor is the tool-originated correlator for
// foreground cascade materialize calls. Passing prov=nil is
// equivalent to MaterializeCascadeIntents (background-style audit
// rows with NULL correlation). Passing prov writes system_audit_log
// rows that share invocation_id / mpm_session_id / framework_*_id
// with the originating tool_invocations row.
func (dm *DatabaseManager) MaterializeCascadeIntentsFor(
	ctx context.Context, limit int, prov *InvocationProvenance,
) (MaterializationReport, error) {
	opts := DefaultCascadeMaterializerOptions()
	opts.InvocationProvenance = prov
	return dm.NewCascadeMaterializer(opts).MaterializeBatch(ctx, limit)
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

	// Alpha-4 ledger audit fix: this read path was the lone
	// memory query that lacked both NULL safety on the tags/metadata
	// columns and the deleted_at + expires_at predicates that every
	// other read surface applies. Scanning NULL tags/metadata into
	// concrete Go strings panicked with "converting NULL to string is
	// unsupported" — the exact class of bug that caused the 2026-09-02
	// OpenClaw `theories_pending` discrepancy. The deleted_at + expires_at
	// predicates bring parity with GetMemory, SearchMemories, and
	// HybridSearch so a session-scoped query cannot return rows that
	// any other surface would have hidden.
	rows, err := dm.db.Query(`
		SELECT id, collection, content, tags, metadata, created_at
		FROM memories
		WHERE session_id = ?
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
		ORDER BY created_at DESC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var memID, collection, content, createdAt string
		var tagsJSON, metadataJSON sql.NullString
		if err := rows.Scan(&memID, &collection, &content, &tagsJSON, &metadataJSON, &createdAt); err != nil {
			return nil, err
		}
		// Substrate Defense Triad #2: NULL-safe unwrap. The legacy
		// 42 rows with NULL tags on the live DB would have panicked
		// before this fix; the 0-session rows in the live DB mean the
		// bug is latent today, but the session_id column is general
		// substrate infrastructure and any future writer populating it
		// would trigger the crash.
		tags := []string{}
		if tagsJSON.Valid && tagsJSON.String != "" {
			_ = json.Unmarshal([]byte(tagsJSON.String), &tags)
		}
		metadata := map[string]interface{}{}
		if metadataJSON.Valid && metadataJSON.String != "" {
			_ = json.Unmarshal([]byte(metadataJSON.String), &metadata)
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

func (dm *DatabaseManager) SaveMemory(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, expiresAt ...time.Time) (string, error) {
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
func (dm *DatabaseManager) SaveMemoryNode(node DBNode, collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error) {
	id := GenerateID()
	// Background / non-tool-originated callers pass `ActiveContext{}`,
	// which keeps the existing NULL-correlation audit behaviour
	// (brief §17 background-event contract). Tool-originated callers
	// route through SaveMemoryNodeForInvocation below, which threads
	// ActiveContext into the audit emission so the row carries the
	// typed invocation_id / mpm_session_id / framework_*_id columns.
	return saveMemoryRow(node, dm, id, collection, content, sessionID, tags, metadata, embedding, isLongTerm, weight, referenceID, retrievalPriority, importance, createdAt, ActiveContext{}, expiresAt...)
}

// SaveMemoryNodeForInvocation is the tool-originated sibling of
// SaveMemoryNode. Same INSERT primitive, same scanner guarantees, but
// the audit rows emitted by the sensitive/poison scanners carry the
// four identity columns derived from the supplied ActiveContext. This
// is the bridge that lets `mpm call mpm_memory save` (CLI) and the
// MCP mpm_memory tool share one correlation identity from the
// dispatcher all the way to the system_audit_log row.
//
// Background callers must continue to use SaveMemoryNode so that
// empty-ActiveContext paths keep the NULL-correlation contract.
//
// Note: this sibling routes through the non-transactional path
// (`dm` as the DBNode). The tool-originated save is not currently
// wrapped in a transaction; tx-aware callers that need ActiveContext
// correlation must call SaveMemoryNode directly with their own
// provenance bridge (no such path is reachable today; this sibling
// is the canonical tool path).
func (dm *DatabaseManager) SaveMemoryNodeForInvocation(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, referenceID, retrievalPriority, importance, createdAt string, ac ActiveContext, expiresAt ...time.Time) (string, error) {
	id := GenerateID()
	return saveMemoryRow(dm, dm, id, collection, content, sessionID, tags, metadata, embedding, isLongTerm, weight, referenceID, retrievalPriority, importance, createdAt, ac, expiresAt...)
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
//
// The ac parameter is the tool-originated ActiveContext when available
// (passed by SaveMemoryNodeForInvocation); the background callers
// (SaveMemoryNode, SaveSkill) pass ActiveContext{}. The sensitive /
// poison scanner audit emissions switch on ac.InvocationID: when
// populated they use LogAuditForInvocation (typed correlation
// columns); when empty they fall back to LogAudit (NULL correlation,
// per brief §17 background-event policy).
func saveMemoryRow(node DBNode, dm *DatabaseManager, id, collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, referenceID, retrievalPriority, importance, createdAt string, ac ActiveContext, expiresAt ...time.Time) (string, error) {
	// Alpha remediation (2026-08-27): reject empty / whitespace-only content.
	// The validation run found that `mpm remember ""` and `mpm add "   "`
	// both create memories. A persisted memory must contain meaningful
	// non-whitespace content; this invariant is enforced here, the single
	// canonical INSERT primitive, so every caller is protected
	// structurally rather than by call-site discipline. The audit row is
	// emitted before the sensitive / poison scanners so the validation
	// message is distinct from a security block.
	if reason := validateMemoryContent(content); reason != "" {
		emitMemorySaveAudit(dm, ac, AuditWarn, "validation", "empty or whitespace-only memory content rejected", "memory_save_validation_rejected", AuditContext{
			"reason":    reason,
			"len_chars": len(content),
		})
		return "", fmt.Errorf("memory content is empty or whitespace-only (%s) — provide non-whitespace text", reason)
	}
	if isSensitive, reason := isSensitiveContent(content); isSensitive {
		emitMemorySaveAudit(dm, ac, AuditError, "security", "sensitive content blocked", "memory_save_sensitive_content_blocked", AuditContext{
			"reason":    reason,
			"len_chars": len(content),
		})
		// Wrap with the typed sentinel so ClassifyError can route
		// this deliberate security-policy rejection to
		// OutcomeClassValidation. Without the %w wrap, ClassifyError
		// would fall back to OutcomeClassInternal/"unclassified",
		// incorrectly signalling an MPM substrate failure.
		return "", fmt.Errorf("sensitive content detected and blocked: %s: %w", reason, ErrSensitiveContentBlocked)
	}
	if isPoisoned, reason := isPoisoned(content); isPoisoned {
		emitMemorySaveAudit(dm, ac, AuditError, "security", "poison content blocked", "memory_save_poison_content_blocked", AuditContext{
			"reason":    reason,
			"len_chars": len(content),
		})
		return "", fmt.Errorf("poison content detected and blocked: %s: %w", reason, ErrPoisonContentBlocked)
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

	// F19 idempotency: an identical live save (same collection + content +
	// tags + stable metadata) returns the EXISTING row's id instead of
	// writing a second indistinguishable one. Same content under different
	// provenance/metadata remains a legitimately distinct artifact — that
	// distinction is load-bearing for the contradiction workflows.
	wantIdentity := MemoryIdentityHash(contentHash, tags, metadata)
	if dupID := findLiveDuplicateNode(node, collection, contentHash, wantIdentity); dupID != "" {
		return dupID, nil
	}

	// 2026-08-27: retries=5 (matching the documented ExecTracked convention
	// at db.go:859) so concurrent writers transparently back off and retry
	// on SQLITE_BUSY instead of surfacing the lock error to the caller. The
	// cascade materializer runs materializeTheory concurrently under
	// BEGIN IMMEDIATE; without retry, transient WAL contention on the
	// FTS5-triggered secondary inserts escalated to a hard failure that
	// rolled back the enclosing transaction and silently dropped the
	// theory row. See TestMaterializer_ConcurrentClaim for the regression.
	res, err := node.ExecTracked(`INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, is_long_term, weight, expires_at, confidence, created_at, reference_id, retrieval_priority, importance, content_hash, identity_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		5, id, collection, content, sessionIDVal, string(tagsJSON), string(metadataJSON), embeddingJSON, isLTM, weight, expiresAtStr, initialConf, createdSec, referenceID, retrievalPriority, importance, contentHash, wantIdentity)
	if err != nil {
		if isIdentityConstraintViolation(err) {
			// A concurrent writer inserted the same identity between our
			// probe and this INSERT; the partial unique index serialized
			// us. Return the winner's id — deterministic idempotency.
			if dupID := findLiveDuplicateNode(node, collection, contentHash, wantIdentity); dupID != "" {
				return dupID, nil
			}
		}
		return id, err
	}

	// 2026-08-13 hardening: same silent-promotion class as AddLesson.
	// Exec returned no error, but a sqlite trigger swallowing the INSERT
	// (or a unique-constraint race in WAL) can produce RowsAffected=0
	// while the call returns success. Without these checks, the caller
	// sees a phantom success and the row never lands. A non-tx caller
	// (node == dm) gets a read-back via the canonical GetMemory path;
	// a tx caller defers the read-back to its WithTx Commit (it would
	// race against the uncommitted state otherwise).
	if affected, err := res.RowsAffected(); err != nil {
		return id, fmt.Errorf("save_memory row insert rows-affected: %w", err)
	} else if affected != 1 {
		return id, fmt.Errorf("save_memory row insert affected %d rows, expected 1", affected)
	}
	if nodeUnwrapTx(node) == nil {
		if _, err := dm.GetMemory(id); err != nil {
			return id, fmt.Errorf("save_memory row insert read-back failed (commit did not persist): %w", err)
		}
	}

	// Artifact provenance (best-effort telemetry). RecordArtifactProvenance
	// is SAVEPOINT-isolated so provenance failures never affect the artifact
	// tx. For standalone writes (no existing tx), we open a short-lived tx
	// scoped to just the provenance INSERT. For in-tx writes (via WithTx),
	// we reuse the caller's tx so both the artifact and provenance INSERTs
	// commit atomically together.
	artifactType := artifactTypeFromCollection(collection)
	if prov := dm.getEffectiveProvenance(); prov != nil {
		var tx *sql.Tx
		var err error
		if existing := nodeUnwrapTx(node); existing != nil {
			tx = existing
		} else {
			tx, err = dm.db.Begin()
			if err != nil {
				dm.LogAudit(AuditWarn, "provenance", "begin failed", "", AuditContext{
					"memory_id": id,
				})
			}
		}
		if tx != nil {
			res := dm.RecordArtifactProvenance(
				tx, id, artifactType, prov, false,
			)
			if !res.Recorded {
				dm.LogAudit(AuditWarn, "provenance", "record failed", "", AuditContext{
					"memory_id":     id,
					"artifact_type": artifactType,
					"reason":        res.ValidationReason,
					"sql_error":     res.SQLError,
				})
			}
			if nodeUnwrapTx(node) == nil {
				// Only commit/rollback our own tx; caller tx is managed externally.
				if res.Recorded {
					tx.Commit()
				} else {
					tx.Rollback()
				}
			}
		}
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
func (dm *DatabaseManager) SaveMemoryWithExtras(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error) {
	return dm.SaveMemoryNode(dm, collection, content, sessionID, tags, metadata, embedding, isLongTerm, weight, referenceID, retrievalPriority, importance, createdAt, expiresAt...)
}

// emitMemorySaveAudit is the security/validation audit dispatcher used by
// saveMemoryRow. When the supplied ActiveContext carries an InvocationID
// (tool dispatch path), the audit row is written via LogAuditForInvocation
// with the four typed identity columns; otherwise it falls back to the
// plain LogAudit so background / self-maintenance / skill writes keep the
// brief §17 NULL-correlation contract.
//
// eventCode is the bounded, content-free, source-authored identifier
// (see internal/core/tool_outcome.go). One stable code per event shape:
//   - memory_save_validation_rejected          (whitespace-only)
//   - memory_save_sensitive_content_blocked    (security scanner)
//   - memory_save_poison_content_blocked       (poison scanner)
func emitMemorySaveAudit(dm *DatabaseManager, ac ActiveContext, level AuditLevel, component, message, eventCode string, ctx AuditContext) {
	if dm == nil {
		return
	}
	if ac.InvocationID != "" {
		dm.LogAuditForInvocation(level, component, message,
			ac.InvocationID, ac.MPMSessionID,
			ac.FrameworkName, ac.FrameworkSessionID,
			eventCode, ctx)
		return
	}
	dm.LogAudit(level, component, message, "", ctx)
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
			last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
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
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dotProduct, normA, normB float32
	for i := 0; i < n; i++ {
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
	// Alpha-4 ledger audit T-1 fix: GetTopic by ID lacked the
	// is_active = 1 filter that ListTopics and GetTopicByName apply.
	// After DeleteTopic (which sets is_active = 0), a topic disappears
	// from ListTopics but GetTopic still returns it — a divergent
	// surface where direct ID lookup contradicts the canonical list.
	// Match the canonical filter here too.
	err := dm.db.QueryRow(
		`SELECT name, COALESCE(description,''), created_at, COALESCE(tags,'{}') FROM topics WHERE id = ? AND is_active = 1`, id).Scan(
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
			return nil, fmt.Errorf("scanning topic row: %w", err)
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
			return nil, fmt.Errorf("scanning topic memory row: %w", err)
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

// AddMemoryToTopic adds a memory to a topic.
//
// W-005: validates that topicID exists before inserting the membership
// row. Previously the call did an INSERT OR IGNORE with no FK check,
// silently creating orphan memberships when an agent mistyped a
// topic_id. Now we explicitly verify the topic exists; the cost is a
// single indexed SELECT and the gain is no orphan rows on the public
// write path. Idempotency is preserved: a duplicate (memoryID, topicID)
// still hits INSERT OR IGNORE and returns nil.
//
// The write-path read-back assertion (per the substrate defense triad
// — see CLAUDE.md §Substrate Defense Triad) is implemented via the
// existing TopicsExists query: we don't reload the row, but we do
// fail loudly on the precondition. Callers that need a stronger
// assertion can wrap the call in a transaction and re-read.
func (dm *DatabaseManager) AddMemoryToTopic(memoryID, topicID, role string) error {
	if role == "" {
		role = "manual"
	}

	// W-005: verify the topic exists before creating the membership.
	// Topics are soft-deleted; we honor that and reject links to
	// inactive topics too. Cost: one indexed SELECT.
	exists, err := dm.topicExists(topicID)
	if err != nil {
		return fmt.Errorf("add memory to topic (existence check): %w", err)
	}
	if !exists {
		return fmt.Errorf("topic %q not found", topicID)
	}

	now := time.Now().Unix()
	_, err = dm.db.Exec(`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)`,
		memoryID, topicID, now, role)
	return err
}

// topicExists returns true if a topic with id is present and not
// soft-deleted. Cheap one-row probe — used by AddMemoryToTopic's W-005
// validation and exposed for any future caller that wants the same
// guard without the membership-write overhead.
//
// Schema-tolerance: the production schema gains `deleted_at` via a
// SafeMigration; older test fixtures and downgraded workspaces may
// not have the column. We probe it first and fall back to the bare-id
// check if absent. The probe result is cached for the connection
// lifetime — cheap and matches the topicExists use pattern.
func (dm *DatabaseManager) topicExists(id string) (bool, error) {
	var x int
	query := `SELECT 1 FROM topics WHERE id = ?`
	if dm.hasColumn("topics", "deleted_at") {
		query += ` AND deleted_at IS NULL`
	}
	err := dm.db.QueryRow(query, id).Scan(&x)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// hasColumn reports whether the named table has the named column.
// Uses SQLite's PRAGMA table_info, which is the standard idiom for
// schema introspection at the boundary. The result is cached per call
// because PRAGMA table_info is a metadata round-trip; topicExists
// runs in a write hot path (AddMemoryToTopic).
func (dm *DatabaseManager) hasColumn(table, column string) bool {
	rows, err := dm.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, dfltValue, pk interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

// RemoveMemoryFromTopic removes a memory from a topic. Returns (true, nil)
// when a membership row was actually deleted, (false, nil) when no such
// membership existed (idempotent silent success — matches the soft-delete
// contract in docs/tool-behavioral-contract.md §1; the row is already in
// the desired terminal state).
//
// Part 2A (2026-09-06): widened signature to return a bool so the
// handleUnlinkTopic public surface can distinguish "removed" from
// "already absent" without a separate probe round-trip. The existing
// call sites that only care about errors keep the same behaviour.
func (dm *DatabaseManager) RemoveMemoryFromTopic(memoryID, topicID string) (bool, error) {
	if memoryID == "" {
		return false, fmt.Errorf("memory_id is required")
	}
	if topicID == "" {
		return false, fmt.Errorf("topic_id is required")
	}
	res, err := dm.db.Exec(`DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?`, memoryID, topicID)
	if err != nil {
		return false, fmt.Errorf("remove membership: %w", err)
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// DeleteTopic soft-deletes a topic (memories are NOT deleted).
//
// Both writes run inside a single transaction with the soft-delete UPDATE
// happening first: if a caller lists "active topics" between operations, the
// deactivated topic will not appear even before the membership cleanup runs.
// Previously, the membership DELETE ran first, so a failed UPDATE left an
// active topic with no members — a confusing state for any UI listing.
func (dm *DatabaseManager) DeleteTopic(topicID string) error {
	if topicID == "" {
		return fmt.Errorf("topic_id is required")
	}
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("DeleteTopic: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE topics SET is_active = 0 WHERE id = ?`, topicID)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("DeleteTopic: rows-affected: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("DeleteTopic: no topic with id %s", topicID)
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
	ID                 string         `json:"id"`
	Type               LessonType     `json:"type"`
	Content            string         `json:"content"`
	Tags               []string       `json:"tags"`
	ReinforcementCount int            `json:"reinforcement_count"`
	SourceSessionID    sql.NullString `json:"source_session_id"`
	// ^ 2026-08-13 hardening: was `string` until the silent-promotion archaeology
	// surfaced `Scan error on column index 5, name "source_session_id": converting
	// NULL to string is unsupported` on every list/search/scan path that hit a
	// row with no source_session_id. sql.NullString is the idiomatic Go wrapper
	// for nullable text columns; `.Valid` distinguishes NULL from empty-string.
	// JSON tag drops the `omitempty` so the consumer sees `"source_session_id": null`
	// rather than a missing field — debugging the underlying NULL is easier when
	// the field is visible.
	Created           string  `json:"created"`
	RetrievalPriority float64 `json:"retrieval_priority,omitempty"`
	Importance        float64 `json:"importance,omitempty"`
	Confidence        float64 `json:"confidence,omitempty"`
}

// lessonsIsView reports whether the lessons object is a view (with
// INSTEAD OF triggers routed to lessons_base) or a plain table. Used
// by AddLesson's silent-promotion hardening to decide whether
// RowsAffected is a reliable indicator — on a routed view it
// legitimately reports 0 even when the trigger's INSERT succeeded.
//
// 2026-08-13 hardening: this exists so the read-back check can
// substitute for RowsAffected on the view path without losing the
// loud-fail guarantee on the direct-table path.
func (dm *DatabaseManager) lessonsIsView() bool {
	var kind string
	err := dm.db.QueryRow(
		`SELECT type FROM sqlite_master WHERE name = 'lessons'`,
	).Scan(&kind)
	if err != nil {
		return false // safe default — surface the louder error in the rare ErrNoRows case
	}
	return kind == "view"
}

// sourceSessionIDOrNil converts the empty string to a typed nil so the
// INSERT writes NULL rather than ” to source_session_id. Without this,
// the column holds an empty string and downstream scans treat it as
// Valid=true (empty-string, not NULL) — a subtle silent-promotion class
// where the NULL/empty distinction gets lost at the SQL boundary.
//
// 2026-08-13 hardening: introduced alongside the sql.NullString struct
// change to make sure callers that pass "" actually write NULL.
func sourceSessionIDOrNil(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nullString converts an empty string to a typed nil so the INSERT writes
// NULL rather than ” to a nullable TEXT column. This preserves the
// NULL/empty-string distinction at the SQL boundary.
func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// GitSnapshot captures the observable repository state at event creation.
// Best-effort: if git is not available or not a repo, all fields are empty/false.
type GitSnapshot struct {
	HeadBefore   string
	HeadAfter    string
	DirtyBefore  bool
	DirtyAfter   bool
	ChangedFiles []string
	Committed    bool
}

// CaptureGitSnapshot runs git commands to capture repository state.
// dir is the working directory to run git in; empty means ".".
// It captures HEAD and dirty state before and after (both from a single
// snapshot — committed is false unless the caller performed a commit
// between two snapshots). For simplicity, before==after.
func CaptureGitSnapshot(dir string) GitSnapshot {
	if dir == "" {
		dir = "."
	}
	ws := config.GetMPMDir()
	// Try MPM workspace dir as fallback if dir is "." and not a git repo
	tryDirs := []string{dir}
	if ws != "" && ws != dir {
		tryDirs = append(tryDirs, ws)
	}
	// Also try current working directory
	if cwd, err := os.Getwd(); err == nil && cwd != dir && cwd != ws {
		tryDirs = append(tryDirs, cwd)
	}
	for _, d := range tryDirs {
		if snap, ok := captureGitSnapshotAt(d); ok {
			return snap
		}
	}
	return GitSnapshot{}
}

func captureGitSnapshotAt(dir string) (GitSnapshot, bool) {
	// Check if dir is inside a git work tree
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return GitSnapshot{}, false
	}
	var snap GitSnapshot
	// HEAD
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		// Need to run with Dir set; fallback: run again with Dir
		cmd2 := exec.Command("git", "rev-parse", "HEAD")
		cmd2.Dir = dir
		if b, err := cmd2.Output(); err == nil {
			snap.HeadBefore = strings.TrimSpace(string(b))
			snap.HeadAfter = snap.HeadBefore
		}
		_ = out // unused, we re-ran with Dir correctly
	} else {
		// Try with Dir set correctly
		cmd2 := exec.Command("git", "rev-parse", "HEAD")
		cmd2.Dir = dir
		if b, err := cmd2.Output(); err == nil {
			snap.HeadBefore = strings.TrimSpace(string(b))
			snap.HeadAfter = snap.HeadBefore
		}
	}
	// dirty check
	cmd3 := exec.Command("git", "status", "--porcelain")
	cmd3.Dir = dir
	if out, err := cmd3.Output(); err == nil {
		trimmed := strings.TrimSpace(string(out))
		snap.DirtyBefore = trimmed != ""
		snap.DirtyAfter = snap.DirtyBefore
		// changed files from status
		if trimmed != "" {
			lines := strings.Split(trimmed, "\n")
			for _, line := range lines {
				if len(line) < 3 {
					continue
				}
				f := strings.TrimSpace(line[3:])
				// Handle renames "R  old -> new"
				if idx := strings.Index(f, " -> "); idx != -1 {
					f = f[idx+4:]
				}
				if f != "" {
					snap.ChangedFiles = append(snap.ChangedFiles, f)
				}
			}
		}
	}
	// Also get changed files via diff --name-only if status missed
	if len(snap.ChangedFiles) == 0 {
		cmd4 := exec.Command("git", "diff", "--name-only", "HEAD")
		cmd4.Dir = dir
		if out, err := cmd4.Output(); err == nil {
			s := strings.TrimSpace(string(out))
			if s != "" {
				for _, f := range strings.Split(s, "\n") {
					f = strings.TrimSpace(f)
					if f != "" {
						snap.ChangedFiles = append(snap.ChangedFiles, f)
					}
				}
			}
		}
	}
	// untracked files
	cmd5 := exec.Command("git", "ls-files", "--others", "--exclude-standard")
	cmd5.Dir = dir
	if out, err := cmd5.Output(); err == nil {
		s := strings.TrimSpace(string(out))
		if s != "" {
			for _, f := range strings.Split(s, "\n") {
				f = strings.TrimSpace(f)
				if f != "" && !containsString(snap.ChangedFiles, f) {
					snap.ChangedFiles = append(snap.ChangedFiles, f)
				}
			}
		}
	}
	return snap, true
}

func containsString(arr []string, s string) bool {
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}

// GetActiveDirectiveIDs returns the StableIDs of directives active for the
// given framework. Best-effort: returns empty slice on error.
// Exported so tools/work_handlers can record instruction provenance.
func (dm *DatabaseManager) GetActiveDirectiveIDs(framework string) []string {
	dirs, err := dm.ReadDirectivesForFramework(framework)
	if err != nil || len(dirs) == 0 {
		return nil
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if id, ok := d["id"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
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

	res, err := dm.db.Exec(`
		INSERT INTO lessons (id, type, content, tags, reinforcement_count, source_session_id, created, content_hash, retrieval_priority, importance, confidence)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, 0.5, 0.5, ?)
	`, id, lessonType, content, tagsJSON, sourceSessionIDOrNil(sourceSessionID), now, contentHash, InitialConfidence("lesson"))
	if err != nil {
		return nil, err
	}

	// 2026-08-13 hardening: never let a silent-promotion slip past the
	// write. RowsAffected on a view routed through an INSTEAD OF trigger
	// can legitimately report 0 even when the underlying base-table
	// INSERT succeeded — sqlite counts the row through the trigger's
	// own INSERT, not the outer statement. Trust the read-back instead:
	// if GetLesson returns the row we just wrote, the commit landed;
	// if it returns ErrNoRows, the trigger swallowed it (or some other
	// constraint silently dropped the row) and the caller needs to
	// know. We still surface RowsAffected errors as a loud failure
	// for non-view INSERTs that happen to go through this path in
	// the future.
	if _, err := res.RowsAffected(); err != nil {
		// Only surface this for direct table INSERTs; in the trigger path
		// it's noisy-but-correct to ignore the count.
		if !dm.lessonsIsView() {
			return nil, fmt.Errorf("lesson insert rows-affected: %w", err)
		}
	}
	if _, err := dm.GetLesson(id); err != nil {
		return nil, fmt.Errorf("lesson insert read-back failed (commit did not persist): %w", err)
	}

	// Artifact provenance (best-effort telemetry). RecordArtifactProvenance
	// is SAVEPOINT-isolated so provenance failures never affect the artifact
	// tx. For standalone writes (no existing tx), we open a short-lived tx
	// scoped to just the provenance INSERT. For in-tx writes (via WithTx),
	// we reuse the caller's tx so both the artifact and provenance INSERTs
	// commit atomically together.
	if prov := dm.getEffectiveProvenance(); prov != nil {
		var tx *sql.Tx
		var err error
		if existing := nodeUnwrapTx(dm); existing != nil {
			tx = existing
		} else {
			tx, err = dm.db.Begin()
			if err != nil {
				dm.LogAudit(AuditWarn, "provenance", "begin failed", "", AuditContext{
					"lesson_id": id,
				})
			}
		}
		if tx != nil {
			res := dm.RecordArtifactProvenance(
				tx, id, "lesson", prov, false,
			)
			if !res.Recorded {
				dm.LogAudit(AuditWarn, "provenance", "record failed", "", AuditContext{
					"lesson_id":     id,
					"artifact_type": "lesson",
					"reason":        res.ValidationReason,
					"sql_error":     res.SQLError,
				})
			}
			if nodeUnwrapTx(dm) == nil {
				// Only commit/rollback our own tx; caller tx is managed externally.
				if res.Recorded {
					tx.Commit()
				} else {
					tx.Rollback()
				}
			}
		}
	}

	return &Lesson{
		ID:                 id,
		Type:               lessonType,
		Content:            content,
		Tags:               tags,
		ReinforcementCount: 1,
		// 2026-08-13 hardening: wrap the input string in sql.NullString
		// so the field assignment matches the new struct definition.
		// AddLesson callers pass an empty string when they don't have a
		// session id; we surface this as NULL (not empty-string) so the
		// schema reflects the truth and downstream readers can rely on
		// .Valid distinguishing "absent" from "explicitly empty".
		SourceSessionID:   sql.NullString{String: sourceSessionID, Valid: sourceSessionID != ""},
		Created:           now,
		RetrievalPriority: 0.5,
		Importance:        0.5,
		Confidence:        InitialConfidence("lesson"),
	}, nil
}

// GetLesson retrieves a lesson by ID. Soft-deleted lessons (deleted_at
// set on lessons_base) are excluded — callers asking for a deleted
// lesson by id see "not found", which matches the visible-state
// semantics they expect. Use a separate administrative path (or
// inspect lessons_base directly) to read soft-deleted rows.
//
// Implementation note: queries lessons_base directly (not the
// `lessons` view) because the view doesn't expose deleted_at and
// we want a single column to filter on. The view is reserved for
// write paths (INSERT/UPDATE/DELETE fire the INSTEAD OF triggers).
func (dm *DatabaseManager) GetLesson(id string) (*Lesson, error) {
	var lesson Lesson
	var tagsJSON string
	err := dm.db.QueryRow(`
		SELECT id, type, content, COALESCE(tags,'[]'), reinforcement_count,
		       source_session_id, created
		FROM lessons_base WHERE id = ? AND deleted_at IS NULL
	`, id).Scan(&lesson.ID, &lesson.Type, &lesson.Content, &tagsJSON, &lesson.ReinforcementCount, &lesson.SourceSessionID, &lesson.Created)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("lesson not found: %s", id)
	}
	if err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		UnmarshalJSON(tagsJSON, &lesson.Tags)
	}
	return &lesson, nil
}

// ListLessons returns all lessons, optionally filtered by type.
// Soft-deleted lessons are excluded (same tombstone contract as
// GetLesson / SearchLessons). Reads lessons_base directly for the
// same reason as GetLesson — the `lessons` view doesn't expose
// deleted_at.
func (dm *DatabaseManager) ListLessons(lessonType string) ([]*Lesson, error) {
	// COALESCE at the SQL boundary: legacy lessons rows commonly carry
	// NULL tags / source_session_id; scanning a database NULL into string
	// aborts the whole listing (Stage 8 RC audit, same class as F3).
	query := `SELECT id, type, content, COALESCE(tags,'[]'), reinforcement_count,
	          source_session_id, created FROM lessons_base WHERE deleted_at IS NULL`
	var args []interface{}
	if lessonType != "" {
		query += ` AND type = ?`
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
			return nil, fmt.Errorf("scanning lesson row: %w", err)
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
//
// Ranking: bm25(lessons_fts) first (lower = better FTS5 relevance), so an
// exact content-term match materially outranks a weaker/partial match
// instead of being buried under high-reinforcement unrelated rows. The old
// reinforcement-first ordering could put a loosely related lesson ahead of
// the exact one; reinforcement and age now act only as tiebreakers.
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
		SELECT l.id, l.type, l.content, COALESCE(l.tags,'[]'), l.reinforcement_count,
		       COALESCE(l.source_session_id,''), l.created
		FROM lessons_fts fts
		JOIN lessons_base l ON l.rowid = fts.rowid
		WHERE lessons_fts MATCH ? AND l.deleted_at IS NULL
		ORDER BY bm25(lessons_fts), l.reinforcement_count DESC, l.created DESC
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
			return nil, fmt.Errorf("scanning lesson search row: %w", err)
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

// searchLessonsLike is a fallback when FTS5 is unavailable. The
// FTS5 path uses BM25 over tokenized content; this fallback uses
// LIKE on content and on each tag in the tags JSON array.
//
// alpha-4 ledger audit fix: the previous form was
//
//	WHERE LOWER(content) LIKE ? OR LOWER(tags) LIKE ?
//
// which treats the JSON-encoded tags column as a string. The literal
// `LOWER('["alpha","beta"]') LIKE '%alpha%'` does match, but only as
// an accidental substring — the comparison misses tag queries against
// lessons whose tags column is NULL (it gets coerced to '[]', which
// never matches) and queries against numeric/uppercase tags (the
// JSON-array brackets/quotes/commas complicate substring matching).
// The replacement uses json_each to enumerate tag tokens individually,
// mirroring what BM25 would tokenize.
func (dm *DatabaseManager) searchLessonsLike(query string, limit int) ([]*Lesson, error) {
	q := "%" + strings.ToLower(query) + "%"
	rows, err := dm.db.Query(`
		SELECT DISTINCT l.id, l.type, l.content, COALESCE(l.tags,'[]'),
		       l.reinforcement_count, l.source_session_id, l.created
		FROM lessons_base l
		LEFT JOIN json_each(COALESCE(l.tags,'[]')) je
		WHERE l.deleted_at IS NULL
		  AND (LOWER(l.content) LIKE ? OR LOWER(je.value) LIKE ?)
		ORDER BY l.reinforcement_count DESC
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
			return nil, fmt.Errorf("scanning lesson LIKE row: %w", err)
		}
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &lesson.Tags)
		}
		lessons = append(lessons, &lesson)
	}
	return lessons, rows.Err()
}

// DeleteLesson soft-deletes a lesson by setting its deleted_at
// tombstone on lessons_base. The row remains persisted (history +
// content intact); list/search/get all skip it. RestoreLesson
// clears the tombstone and brings the lesson back. ShredLesson is
// the irreversible hard delete.
//
// 2026-09-10 lifecycle fix: this used to be a hard delete via
// the lessons view (which fires the INSTEAD OF DELETE trigger).
// The behavior change matches the CLI/Tool contract —
// `mpm_lessons delete` is now reversible; `mpm_lessons shred` is
// the destructive path.
//
// Implementation note: writes directly to lessons_base because the
// `lessons` view doesn't expose deleted_at (the view's column list
// is locked by createLessonsViewAndTriggers). INSTEAD OF triggers
// still fire on insert/update/delete through the view from
// callers that use the view; the soft-delete path takes the
// direct base-table route to keep the tombstone semantics
// isolated from the trigger machinery.
func (dm *DatabaseManager) DeleteLesson(id string) error {
	now := time.Now().UTC().Unix()
	res, err := dm.db.Exec(
		`UPDATE lessons_base SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("lesson not found or already deleted: %s", id)
	}
	return nil
}

// RestoreLesson clears the deleted_at tombstone on a soft-deleted
// lesson. Idempotent on an already-visible lesson (returns a clean
// "not deleted" error rather than silently succeeding). A shredded
// lesson (hard-deleted) cannot be restored — its row is gone from
// lessons_base entirely and this function reports "not found".
func (dm *DatabaseManager) RestoreLesson(id string) error {
	res, err := dm.db.Exec(`UPDATE lessons_base SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("lesson not found or not deleted: %s", id)
	}
	return nil
}

// ShredLesson is the irreversible hard delete. Removes the row
// from lessons_base and lessons_fts (bypassing the soft-delete
// filter on the lessons view). A shredded lesson cannot be
// restored.
func (dm *DatabaseManager) ShredLesson(id string) error {
	// Direct base-table delete so soft-deleted rows are also
	// reachable (the view's deleted_at filter would skip them).
	// FTS row removal is best-effort; if the FTS row was already
	// absent (FTS5 out-of-sync repair), the lesson is still
	// shredded correctly.
	_, err := dm.db.Exec(`DELETE FROM lessons_fts WHERE rowid IN (SELECT rowid FROM lessons_base WHERE id = ?)`, id)
	if err != nil {
		return err
	}
	res, err := dm.db.Exec(`DELETE FROM lessons_base WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("lesson not found: %s", id)
	}
	return nil
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
		var weight float64
		var createdAtRaw interface{}
		if err := rows.Scan(&r.ID, &r.MemoryID, &r.Version, &r.Content,
			&weight, &r.Collection, &r.IsLongTerm, &r.IsChallenged,
			&r.ChallengedTheoryID, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("scanning memory revision row: %w", err)
		}
		r.CreatedAt = scanTimestampToUnix(createdAtRaw)
		r.Weight = int(weight)
		revisions = append(revisions, r)
	}
	if revisions == nil {
		return []MemoryRevision{}, nil
	}
	return revisions, rows.Err()
}

// scanTimestampToUnix converts a raw database timestamp value (which may be
// an int64 Unix epoch or a TEXT ISO8601 string) to a Unix epoch int64.
// This handles the mixed-schema state where some memory_revisions rows have
// TEXT created_at values and others have INTEGER.
func scanTimestampToUnix(raw interface{}) int64 {
	switch v := raw.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		if v == "" {
			return 0
		}
		// Try parsing as Unix timestamp first
		if t, err := strconv.ParseInt(v, 10, 64); err == nil {
			return t
		}
		// Fall back to parsing as time string
		if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
			return t.Unix()
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.Unix()
		}
		return 0
	case time.Time:
		return v.Unix()
	}
	return 0
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
	var weight float64
	var createdAtRaw interface{}
	err = row.Scan(&r.ID, &r.MemoryID, &r.Version, &r.Content,
		&weight, &r.Collection, &r.IsLongTerm, &r.IsChallenged,
		&r.ChallengedTheoryID, &createdAtRaw)
	if err != nil {
		return nil, nil // no revision found for that time
	}
	r.CreatedAt = scanTimestampToUnix(createdAtRaw)
	r.Weight = int(weight)
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
	dm.mirrorWG.Add(1)
	go func() {
		defer dm.mirrorWG.Done()
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
		f, err := os.OpenFile(mirrorPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer f.Close()
		f.WriteString(string(line) + "\n")
	}()
}

// ChallengeMemory applies provenance-based contradiction resolution:
// slashes the loser's weight and sets challenged status in the DB.
// The slashAmount is the weight reduction (positive integer; a negative
// value is rejected rather than silently inverted into a weight boost).
//
// The memory's pre-challenge weight is recorded in metadata as
// `challenged_prior_weight` so `mpm challenge restore` can return the
// memory to its exact prior epistemic standing once the challenge
// theory is disproven, instead of leaving the weakened value behind.
//
// All four operations (pre-check, metadata patch with FTS sync, weight update,
// async log) are wrapped in a single transaction. A concurrent ReinforceMemory
// cannot interleave between the metadata patch and the weight decrement, so
// the challenged memory is always observed in a consistent state.
//
// F7.1 (challenge-restoration): A challenge must neutralize the memory's
// evidence and stamp the audit trail. Specifically:
//   - Existing evidence rows are neutralized via expires_at = now (NOT
//     deleted — they remain in the table for the audit trail but cannot
//     contribute to a confidence recompute, since the loadEvidenceForRecompute
//     filter already skips expired rows).
//   - challenged_at (Unix-epoch seconds) is stamped so the cycle is
//     reconstructable. F7.1 invariant: challenge must not silently destroy
//     history.
//   - The pre-challenge confidence is captured for forensic/audit purposes
//     ONLY — restoration does NOT silently re-elevate it.
func (dm *DatabaseManager) ChallengeMemory(memoryID string, slashAmount int, evidence string) error {
	if slashAmount < 0 {
		return fmt.Errorf("ChallengeMemory: slashAmount must be >= 0 (got %d); a negative reduction would silently increase the disputed memory's weight", slashAmount)
	}
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

	nowSec := time.Now().Unix()

	patch := map[string]interface{}{
		"status":               "challenged",
		"challenged_theory_id": evidence,
		"challenged_at":        nowSec,
	}
	patchJSON, _ := json.Marshal(patch)
	if err := updateMemoryMetadataTx(tx, memoryID, string(patchJSON)); err != nil {
		return err
	}

	// Record the pre-challenge weight AND confidence AFTER the status
	// patch so restore sees both atomically. Read through the same tx
	// for a consistent snapshot. The prior_confidence value is forensic
	// only; restoration does NOT silently re-elevate it (F7.1).
	// W-004 (2026-08-31): weight is REAL in the schema; Scan into float64
	// so fractional prior weights (e.g. 7.5) survive the challenge
	// round-trip without silent truncation in the metadata snapshot.
	var priorWeight float64
	var priorConfidence float64
	if err := tx.QueryRow(`SELECT COALESCE(weight, 1), COALESCE(confidence, 0.5) FROM memories WHERE id = ? AND deleted_at IS NULL`, memoryID).Scan(&priorWeight, &priorConfidence); err != nil {
		return fmt.Errorf("ChallengeMemory: read prior state: %w", err)
	}
	priorPatch := map[string]interface{}{
		"challenged_prior_weight":     priorWeight,
		"challenged_prior_confidence": priorConfidence,
	}
	priorJSON, _ := json.Marshal(priorPatch)
	if err := updateMemoryMetadataTx(tx, memoryID, string(priorJSON)); err != nil {
		return err
	}

	if _, err := tx.Exec(`UPDATE memories SET weight = MAX(1, weight - ?), updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL`, slashAmount, memoryID); err != nil {
		return fmt.Errorf("ChallengeMemory: weight update: %w", err)
	}

	// F7.1: neutralize existing evidence rows (set expires_at = now). The
	// rows remain in the table for the audit trail but cannot contribute
	// to a confidence recompute. The loadEvidenceForRecompute filter at
	// evidence_store.go already skips expired rows, so no other code
	// needs to change.
	if _, err := tx.Exec(
		`UPDATE evidence SET expires_at = ? WHERE artifact_id = ? AND artifact_type = 'memory' AND expires_at IS NULL`,
		nowSec, memoryID,
	); err != nil {
		return fmt.Errorf("ChallengeMemory: neutralize evidence: %w", err)
	}

	// F7.1: drop confidence to the challenged floor. The challenged memory
	// cannot retain its pre-challenge high confidence value; fresh evidence
	// is required to re-elevate it after restoration. The confidence_history
	// row that records this recompute is the auditable trail.
	if _, err := tx.Exec(
		`UPDATE memories SET confidence = ? WHERE id = ? AND deleted_at IS NULL`,
		ChallengedMemoryConfidenceFloor, memoryID,
	); err != nil {
		return fmt.Errorf("ChallengeMemory: drop confidence: %w", err)
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
// Returns empty string if not found, allowing graceful default to the "standard"
// provenance tier (absolute > high > standard > ephemeral). Not to be confused
// with the mode fallback name "default" — this is a separate taxonomy.
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

// AddWork inserts a new work item and returns it after read-back assertion.
//
// D-005: title and content are scanned against the secret/poison scanner
// before the INSERT fires. The error format mirrors saveMemoryRow so the
// diagnostic surface is consistent across write paths.
func (dm *DatabaseManager) AddWork(title, content, sessionID string) (*Work, error) {
	if isSensitive, reason := isSensitiveContent(title + " " + content); isSensitive {
		return nil, fmt.Errorf("sensitive content detected and blocked: %s", reason)
	}
	if isPoisoned, reason := isPoisoned(title + " " + content); isPoisoned {
		return nil, fmt.Errorf("poison content detected and blocked: %s", reason)
	}
	id := GenerateID()
	now := time.Now().Unix()

	var sessionIDArg interface{}
	if sessionID != "" {
		sessionIDArg = sessionID
	}

	_, err := dm.db.Exec(`
		INSERT INTO works (id, title, content, status, created_at, updated_at, session_id, migrated_at)
		VALUES (?, ?, ?, 'open', ?, ?, ?, ?)
	`, id, title, content, now, now, sessionIDArg, now)
	if err != nil {
		return nil, fmt.Errorf("insert work: %w", err)
	}

	// Read-back assertion
	work, err := dm.GetWork(id)
	if err != nil {
		return nil, fmt.Errorf("write verification failed for %s: %w", id, err)
	}
	return work, nil
}

// addWorkTx is the transaction-aware companion to AddWork. It performs
// the works INSERT using the supplied DBNode so callers running inside
// a WithTx closure share the transaction boundary (D-001 fix: the prior
// AddWork wrote directly to dm.db, leaving CreateWorkWithContext's
// WithTx with a half-protected boundary — the works row could commit
// while the AppendWorkEvent insert failed, producing ghost works under
// contention).
//
// The scanner check is duplicated here because addWorkTx is also
// reachable directly (e.g. via thin-handler surfaces) and must enforce
// the same invariant regardless of which entry point was used.
//
// Read-back: callers inside a WithTx defer the read-back to after
// commit (mirrors saveMemoryRow's tx-vs-non-tx split). Callers outside
// a transaction should call dm.GetWork after addWorkTx returns.
func (dm *DatabaseManager) addWorkTx(node DBNode, title, content, sessionID string) (string, error) {
	if isSensitive, reason := isSensitiveContent(title + " " + content); isSensitive {
		return "", fmt.Errorf("sensitive content detected and blocked: %s", reason)
	}
	if isPoisoned, reason := isPoisoned(title + " " + content); isPoisoned {
		return "", fmt.Errorf("poison content detected and blocked: %s", reason)
	}
	id := GenerateID()
	now := time.Now().Unix()

	var sessionIDArg interface{}
	if sessionID != "" {
		sessionIDArg = sessionID
	}

	_, err := node.ExecTracked(`
		INSERT INTO works (id, title, content, status, created_at, updated_at, session_id, migrated_at)
		VALUES (?, ?, ?, 'open', ?, ?, ?, ?)
	`, 0, id, title, content, now, now, sessionIDArg, now)
	if err != nil {
		return "", fmt.Errorf("insert work: %w", err)
	}
	return id, nil
}

func (dm *DatabaseManager) GetWork(id string) (*Work, error) {
	var w Work
	var content, sessionID, verification sql.NullString
	var completedAt sql.NullInt64
	err := dm.db.QueryRow(`
		SELECT id, title, content, status, verification, created_at, updated_at, completed_at, session_id
		FROM works WHERE id = ?
	`, id).Scan(&w.ID, &w.Title, &content, &w.Status, &verification, &w.CreatedAt, &w.UpdatedAt, &completedAt, &sessionID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("work not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get work: %w", err)
	}
	if content.Valid {
		w.Content = content.String
	}
	if verification.Valid && verification.String != "" {
		w.Verification = WorkVerification(verification.String)
	} else {
		w.Verification = WorkVerificationUnverified
	}
	if completedAt.Valid {
		w.CompletedAt = &completedAt.Int64
	}
	if sessionID.Valid {
		w.SessionID = sessionID.String
	}
	return &w, nil
}

// MemoryIdentityHash computes the F19 duplicate-identity key over the
// semantically meaningful fields of a memory: the SHA-256 content hash,
// tags, and STABLE metadata. Collection equality is enforced by the SQL
// prefilter. Volatile write-path instrumentation is excluded from the
// hash (_epistemic_snapshot observation telemetry, weight_intent,
// created/timestamp/source/tags save-layer stamps) so an identical
// re-save still matches; but
// meaningful metadata like provenance participates — two saves that
// differ in provenance (human vs model) are legitimately distinct
// artifacts, which is the contract the contradiction-collision workflows
// rely on.
func MemoryIdentityHash(contentHashHex string, tags []string, metadata map[string]interface{}) string {
	metaCopy := make(map[string]interface{}, len(metadata))
	for k, v := range metadata {
		switch k {
		case "_epistemic_snapshot", "weight_intent",
			// Volatile write-path stamps added by the save layer itself:
			// they differ between two calls of the SAME logical save.
			"created", "timestamp", "source",
			// Tag mirror of the tags column (already hashed directly).
			"tags":
			continue
		}
		metaCopy[k] = v
	}
	tj, _ := json.Marshal(tags)
	mj, _ := json.Marshal(metaCopy)
	h := sha256.Sum256([]byte(contentHashHex + "\x00" + string(tj) + "\x00" + string(mj)))
	return hex.EncodeToString(h[:])
}

// isIdentityConstraintViolation reports whether err is the F19 partial
// unique-index rejection (a concurrent identical save won the race).
func isIdentityConstraintViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "idx_memories_identity_live") ||
		strings.Contains(msg, "memories.identity_hash")
}

// identityFromStoredJSON recomputes the full identity hash from a stored
// row's tags/metadata JSON. Rows are prefiltered on content_hash, so the
// stored content itself need not be rehashed — the hash binds identity.
func identityFromStoredJSON(contentHashHex, tagsJSON, metaJSON string) string {
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	var meta map[string]interface{}
	_ = json.Unmarshal([]byte(metaJSON), &meta)
	return MemoryIdentityHash(contentHashHex, tags, meta)
}

// findLiveDuplicateNode is the DBNode variant used inside saveMemoryRow's
// transaction. wantIdentity is the precomputed expected identity hash.
//
// Matching order: the persisted identity_hash column first (frozen at
// insert time — tags/metadata may legitimately mutate afterwards, e.g.
// a challenge patch), falling back to a recomputation from tags/metadata
// for legacy rows written before the column existed.
func findLiveDuplicateNode(node DBNode, collection, contentHashHex, wantIdentity string) string {
	rows, err := node.QueryTracked(`
		SELECT id, COALESCE(identity_hash,''), COALESCE(tags,'[]'), COALESCE(metadata,'{}')
		FROM memories
		WHERE collection = ? AND content_hash = ? AND deleted_at IS NULL
	`, collection, contentHashHex)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var id, ih, tagsJSON, metaJSON string
		if err := rows.Scan(&id, &ih, &tagsJSON, &metaJSON); err != nil {
			slog.Warn("findLiveDuplicateNode: candidate row scan failed; skipping", "error", err.Error())
			continue
		}
		if ih == wantIdentity {
			return id
		}
		if ih == "" && identityFromStoredJSON(contentHashHex, tagsJSON, metaJSON) == wantIdentity {
			return id
		}
	}
	return ""
}

// FindLiveDuplicateMemory returns the id of a live memory with identical
// F19 identity (collection + content + tags + stable metadata), or "".
func (dm *DatabaseManager) FindLiveDuplicateMemory(collection, content string, tags []string, metadata map[string]interface{}) string {
	chBytes := sha256.Sum256([]byte(content))
	ch := hex.EncodeToString(chBytes[:])
	want := MemoryIdentityHash(ch, tags, metadata)
	rows, err := dm.QueryTracked(`
		SELECT id, COALESCE(identity_hash,''), COALESCE(tags,'[]'), COALESCE(metadata,'{}')
		FROM memories
		WHERE collection = ? AND content_hash = ? AND deleted_at IS NULL
	`, collection, ch)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var id, ih, tagsJSON, metaJSON string
		if err := rows.Scan(&id, &ih, &tagsJSON, &metaJSON); err != nil {
			slog.Warn("FindLiveDuplicateMemory: candidate row scan failed; skipping", "error", err.Error())
			continue
		}
		if ih == want {
			return id
		}
		if ih == "" && identityFromStoredJSON(ch, tagsJSON, metaJSON) == want {
			return id
		}
	}
	return ""
}

// WorkFrameworkModel is the user-facing projection of framework/model
// provenance for a work event (audit finding F16). Values stay empty when
// the authoritative records carry none — absence is represented as
// missing, never fabricated.
type WorkFrameworkModel struct {
	FrameworkName string
	Model         string
}

// ResolveFrameworkModelForInvocations batch-loads framework/model metadata
// for work-event invocation IDs. Two authoritative sources, one query each:
//
//  1. tool_invocations — the invocation-time record (framework_name).
//  2. artifact_provenance — the artifact-level record (provider/model),
//     matched via the work's own artifact row so the create event is
//     covered even when tool_invocations rows have aged out.
func (dm *DatabaseManager) ResolveFrameworkModelForInvocations(workID string, invocationIDs []string) map[string]WorkFrameworkModel {
	out := make(map[string]WorkFrameworkModel, len(invocationIDs))
	if len(invocationIDs) == 0 {
		return out
	}
	placeholders := strings.Repeat("?,", len(invocationIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, 0, len(invocationIDs)+1)
	for _, id := range invocationIDs {
		args = append(args, id)
	}

	rows, err := dm.db.Query(`
		SELECT invocation_id, COALESCE(framework_name,''), ''
		FROM tool_invocations WHERE invocation_id IN (`+placeholders+`)
	`, args...)
	if err == nil {
		for rows.Next() {
			var invID, fw, model string
			if err := rows.Scan(&invID, &fw, &model); err == nil {
				out[invID] = WorkFrameworkModel{FrameworkName: fw, Model: model}
			}
		}
		rows.Close()
	} else {
		slog.Warn("ResolveFrameworkModelForInvocations: tool_invocations probe failed", "error", err.Error())
	}

	// Artifact-level fallback: fills model (and framework if absent) from
	// the work's own provenance row. Also seeds entries for invocation ids
	// that had no tool_invocations row (direct DM callers never write that
	// table — it belongs to the CLI/MCP dispatchers).
	var apFW, apModel string
	err = dm.db.QueryRow(`
		SELECT COALESCE(framework_name,''), COALESCE(model_name,'')
		FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'
	`, workID).Scan(&apFW, &apModel)
	if err == nil && (apFW != "" || apModel != "") {
		for _, invID := range invocationIDs {
			fm, ok := out[invID]
			if !ok {
				fm = WorkFrameworkModel{}
			}
			if fm.FrameworkName == "" {
				fm.FrameworkName = apFW
			}
			if fm.Model == "" {
				fm.Model = apModel
			}
			out[invID] = fm
		}
	}
	return out
}

func (dm *DatabaseManager) ListWorks() ([]*Work, error) {
	rows, err := dm.db.Query(`
		SELECT id, title, content, status, verification, created_at, updated_at, completed_at, session_id
		FROM works WHERE status = 'open'
		ORDER BY updated_at DESC, created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list works: %w", err)
	}
	defer rows.Close()

	var works []*Work
	for rows.Next() {
		var w Work
		var content, sessionID, verification sql.NullString
		var completedAt sql.NullInt64
		if err := rows.Scan(&w.ID, &w.Title, &content, &w.Status, &verification, &w.CreatedAt, &w.UpdatedAt, &completedAt, &sessionID); err != nil {
			return nil, fmt.Errorf("scan work row: %w", err)
		}
		if content.Valid {
			w.Content = content.String
		}
		if verification.Valid && verification.String != "" {
			w.Verification = WorkVerification(verification.String)
		} else {
			w.Verification = WorkVerificationUnverified
		}
		if completedAt.Valid {
			w.CompletedAt = &completedAt.Int64
		}
		if sessionID.Valid {
			w.SessionID = sessionID.String
		}
		works = append(works, &w)
	}
	return works, nil
}

func (dm *DatabaseManager) ListAllWorks() ([]*Work, error) {
	rows, err := dm.db.Query(`
		SELECT id, title, content, status, verification, created_at, updated_at, completed_at, session_id
		FROM works
		ORDER BY updated_at DESC, created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all works: %w", err)
	}
	defer rows.Close()

	var works []*Work
	for rows.Next() {
		var w Work
		var content, sessionID, verification sql.NullString
		var completedAt sql.NullInt64
		if err := rows.Scan(&w.ID, &w.Title, &content, &w.Status, &verification, &w.CreatedAt, &w.UpdatedAt, &completedAt, &sessionID); err != nil {
			return nil, fmt.Errorf("scan work row: %w", err)
		}
		if content.Valid {
			w.Content = content.String
		}
		if verification.Valid && verification.String != "" {
			w.Verification = WorkVerification(verification.String)
		} else {
			w.Verification = WorkVerificationUnverified
		}
		if completedAt.Valid {
			w.CompletedAt = &completedAt.Int64
		}
		if sessionID.Valid {
			w.SessionID = sessionID.String
		}
		works = append(works, &w)
	}
	return works, nil
}

func (dm *DatabaseManager) ListWorksByStatus(status string) ([]*Work, error) {
	// Alpha-4 ledger audit W-1 fix: ListWorksByStatus silently returned
	// zero rows for any unknown status string (no validation). The MCP
	// handler validated the enum before invoking this method, but
	// direct DatabaseManager callers (and any future CLI/migration
	// path) would see "0 results" with no signal that the input was
	// invalid. Reject the unknown status at the SQL boundary, matching
	// the validation pattern used by ListDecisions and ListTheories.
	switch WorkStatus(status) {
	case WorkStatusOpen, WorkStatusDone, WorkStatusCancelled:
		// valid
	default:
		return nil, fmt.Errorf("list works by status: unknown status %q (use open|done|cancelled)", status)
	}
	rows, err := dm.db.Query(`
		SELECT id, title, content, status, verification, created_at, updated_at, completed_at, session_id
		FROM works WHERE status = ?
		ORDER BY updated_at DESC, created_at DESC
	`, status)
	if err != nil {
		return nil, fmt.Errorf("list works by status: %w", err)
	}
	defer rows.Close()

	var works []*Work
	for rows.Next() {
		var w Work
		var content, sessionID, verification sql.NullString
		var completedAt sql.NullInt64
		if err := rows.Scan(&w.ID, &w.Title, &content, &w.Status, &verification, &w.CreatedAt, &w.UpdatedAt, &completedAt, &sessionID); err != nil {
			return nil, fmt.Errorf("scan work row: %w", err)
		}
		if content.Valid {
			w.Content = content.String
		}
		if verification.Valid && verification.String != "" {
			w.Verification = WorkVerification(verification.String)
		} else {
			w.Verification = WorkVerificationUnverified
		}
		if completedAt.Valid {
			w.CompletedAt = &completedAt.Int64
		}
		if sessionID.Valid {
			w.SessionID = sessionID.String
		}
		works = append(works, &w)
	}
	return works, nil
}

func (dm *DatabaseManager) CompleteWork(id string) (*Work, error) {
	return dm.updateWorkStatus(id, WorkStatusDone)
}

func (dm *DatabaseManager) CancelWork(id string) (*Work, error) {
	return dm.updateWorkStatus(id, WorkStatusCancelled)
}

func (dm *DatabaseManager) UpdateWork(id string, status WorkStatus) (*Work, error) {
	return dm.updateWorkStatus(id, status)
}

func (dm *DatabaseManager) updateWorkStatus(id string, status WorkStatus) (*Work, error) {
	now := time.Now().Unix()
	var completedAt interface{}
	if status == WorkStatusDone {
		completedAt = now
	}
	// NULL completed_at when moving out of done/cancelled
	if status == WorkStatusOpen {
		completedAt = nil
	}

	// F-B1: enforce the work state machine. The hostile test surfaced
	// that cancelling an already-cancelled work or completing an
	// already-completed work was accepted as a no-op transition; this
	// masks operator error and breaks downstream verification logic.
	// Allowed transitions:
	//   open       → done | cancelled
	//   done       → open (reopen)
	//   cancelled  → open (reopen)
	// Disallowed: any other source/target pair.
	var current WorkStatus
	if err := dm.db.QueryRow(`SELECT status FROM works WHERE id = ?`, id).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("work not found: %s", id)
		}
		return nil, fmt.Errorf("update work status: read current: %w", err)
	}
	if !isValidWorkTransition(current, status) {
		// RUNTIME OUTCOME WIRING (2026-09-21): wrap ErrInvalidWorkTransition
		// so callers + ClassifyError observe the typed sentinel. The
		// underlying human-readable message is preserved (errors.As +
		// errors.Is both still resolve here).
		return nil, fmt.Errorf("%w: %s → %s for work %s",
			ErrInvalidWorkTransition, current, status, id)
	}

	res, err := dm.db.Exec(`
		UPDATE works SET status = ?, updated_at = ?, completed_at = ? WHERE id = ?
	`, status, now, completedAt, id)
	if err != nil {
		return nil, fmt.Errorf("update work status: %w", err)
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, fmt.Errorf("work not found: %s", id)
	}

	// F8.1: status changes must keep verification in lockstep. Direct
	// callers (CompleteWork, CancelWork, UpdateWork) bypass the event-
	// sourced AppendWorkEvent path, so they cannot rely on the latter
	// to call DeriveWorkVerification. Derive here so every status
	// mutation triggers the lifecycle-coupling invariant.
	if _, derr := dm.DeriveWorkVerification(id); derr != nil {
		return nil, fmt.Errorf("update status: derive verification: %w", derr)
	}

	return dm.GetWork(id)
}

// ── Work Event-Sourced API (thin-handler delegation target) ──────────────
// These methods are the sole owners of work state transitions. Handlers in
// work_handlers.go must remain thin: parse payload, fetch ActiveContext,
// delegate here. No UPDATE works queries outside AppendWorkEvent.
//
// Each method is event-sourced: it appends an event via AppendWorkEvent
// (which atomically updates the works projection) and, for actions that
// close work, records Git evidence as a separate evidence table row.

func (dm *DatabaseManager) provenanceFromContext(ac ActiveContext) *EffectiveProvenance {
	base := dm.GetProvenanceResolver().Resolve(ac.SessionID, ac.InvocationID, "", ac.ParentInvocationID)
	if base == nil {
		base = &EffectiveProvenance{ActorKind: "unknown"}
	}
	if ac.Agent != "" {
		base.ActorID = ac.Agent
		if base.ActorKind == "unknown" {
			base.ActorKind = "agent"
		}
	} else if base.ActorKind == "" {
		base.ActorKind = "agent"
	}
	if ac.SessionID != "" {
		base.SessionID = ac.SessionID
	}
	// Framework: prefer provenance env (MPM_PROVENANCE) over ActiveContext default.
	// ActiveContext.FrameworkName defaults to "mpm-cli" when MPM_FRAMEWORK is unset;
	// don't let that overwrite a real framework from provenance.
	if base.FrameworkName == "" && ac.FrameworkName != "" {
		base.FrameworkName = ac.FrameworkName
	} else if ac.FrameworkName != "" && ac.FrameworkName != "mpm-cli" {
		// Explicit framework from ActiveContext (e.g., MPM_FRAMEWORK=claude-code or mcp) wins
		base.FrameworkName = ac.FrameworkName
	}
	if ac.InvocationID != "" {
		base.InvocationID = ac.InvocationID
	} else if base.InvocationID == "" {
		base.InvocationID = GenerateID()
	}
	if ac.ParentInvocationID != "" {
		base.ParentInvocationID = ac.ParentInvocationID
	}
	if base.ModelName == "" && ac.Model != "" {
		base.ModelName = ac.Model
	}
	if base.ActorKind == "" {
		base.ActorKind = "agent"
	}
	return base
}

func (dm *DatabaseManager) recordGitEvidenceForWork(workID string) {
	snap := CaptureGitSnapshot("")
	if snap.HeadBefore == "" && snap.HeadAfter == "" && !snap.DirtyBefore && !snap.DirtyAfter && len(snap.ChangedFiles) == 0 {
		return
	}
	notes := ""
	if len(snap.ChangedFiles) > 0 {
		notes = "git changed_files: " + strings.Join(snap.ChangedFiles, ", ")
		if snap.Committed {
			notes += " (committed)"
		} else if snap.DirtyAfter {
			notes += " (dirty)"
		}
	} else if snap.DirtyAfter {
		notes = "git dirty"
	}
	if snap.HeadBefore != "" {
		notes += " head_before=" + snap.HeadBefore[:7]
	}
	if snap.HeadAfter != "" && snap.HeadAfter != snap.HeadBefore {
		notes += " head_after=" + snap.HeadAfter[:7]
	}
	_, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   workID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "git",
		CreatedBy:    "work_evidence",
		Notes:        notes,
		Strength:     0.6,
	})
	if err != nil {
		slog.Warn("recordGitEvidenceForWork: AddEvidence failed", "work_id", workID, "err", err)
	}
}

// CreateWorkWithContext creates a work and its initial event atomically.
// Thin-handler target for mpm_work create.
func (dm *DatabaseManager) CreateWorkWithContext(title, content, sessionID string, ac ActiveContext) (*Work, error) {
	if title == "" {
		return nil, fmt.Errorf("title is required for create")
	}
	// F16: works.session_id is semantically required — it binds the work
	// to the agent shift that created it. When the caller omits it, the
	// ActiveContext session is authoritative; an empty BOTH means the
	// row legitimately has no session (NULL), not a fabricated one.
	if sessionID == "" {
		sessionID = ac.SessionID
	}
	prov := dm.provenanceFromContext(ac)
	directiveIDs := dm.GetActiveDirectiveIDs(ac.FrameworkName)
	var workID string
	err := dm.WithTx(func(node DBNode) error {
		// D-001: use the tx-aware helper so the works INSERT joins the
		// WithTx transaction. Previously this called AddWork which
		// wrote to dm.db directly — under contention, the works row
		// could commit before AppendWorkEvent hit SQLITE_BUSY, leaving
		// a ghost work with no created event in the ledger.
		id, err := dm.addWorkTx(node, title, content, sessionID)
		if err != nil {
			return err
		}
		workID = id
		_, err = dm.AppendWorkEvent(workID, WorkEvent{
			EventType:    WorkEventTypeCreated,
			Title:        title,
			Content:      content,
			InvocationID: prov.InvocationID,
			DirectiveIDs: directiveIDs,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Record artifact provenance after commit (non-fatal)
	tx, _ := dm.db.Begin()
	if tx != nil {
		res := dm.RecordArtifactProvenance(tx, workID, "work", prov, true)
		if res.Recorded {
			_ = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	return dm.GetWork(workID)
}

// CompleteWorkWithContext marks work done via event-sourced append.
func (dm *DatabaseManager) CompleteWorkWithContext(workID, note string, ac ActiveContext) (*Work, error) {
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for complete")
	}
	prov := dm.provenanceFromContext(ac)
	directiveIDs := dm.GetActiveDirectiveIDs(ac.FrameworkName)
	err := dm.WithTx(func(node DBNode) error {
		_, err := dm.AppendWorkEvent(workID, WorkEvent{
			EventType:    WorkEventTypeClaimedComplete,
			Note:         note,
			InvocationID: prov.InvocationID,
			DirectiveIDs: directiveIDs,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, err
	}
	// F8.1: terminal-lifecycle state transitions must keep verification
	// derived state in lockstep. AppendWorkEvent updated status to 'done';
	// re-derive so the verification column reflects the new lifecycle.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		return nil, fmt.Errorf("complete: derive verification: %w", err)
	}
	return dm.GetWork(workID)
}

// CancelWorkWithContext cancels work via event-sourced append.
func (dm *DatabaseManager) CancelWorkWithContext(workID, note string, ac ActiveContext) (*Work, error) {
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for cancel")
	}
	prov := dm.provenanceFromContext(ac)
	directiveIDs := dm.GetActiveDirectiveIDs(ac.FrameworkName)
	err := dm.WithTx(func(node DBNode) error {
		_, err := dm.AppendWorkEvent(workID, WorkEvent{
			EventType:    WorkEventTypeCancelled,
			Note:         note,
			InvocationID: prov.InvocationID,
			DirectiveIDs: directiveIDs,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, err
	}
	// F8.1: cancellation MUST immediately downgrade verification. Without
	// this call, the works.verification column stays at whatever value it
	// had pre-cancel (often 'verified' from prior outcome evidence), and
	// a follow-up DeriveWorkVerification driven by post-cancel evidence
	// can promote it back to 'verified' — exactly the false-positive the
	// audit flagged. Derive here is structural: the status gate sees the
	// committed 'cancelled' status and locks verification below verified.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		return nil, fmt.Errorf("cancel: derive verification: %w", err)
	}
	return dm.GetWork(workID)
}

// RecordGitEvidenceForWork captures Git state and writes it as an evidence
// row (type: observation, source_group: git). Explicit evidence route for
// work completion — separate from the event ledger.
func (dm *DatabaseManager) RecordGitEvidenceForWork(workID string) {
	dm.recordGitEvidenceForWork(workID)
}

// DeriveWorkVerification computes and persists works.verification from evidence rows
// AND the work item's lifecycle status. It applies conservative decision rules:
//
//   - No evidence → unverified
//   - Only audit evidence (git, ci) → partial
//   - Outcome evidence (filesystem, test, api_response) + audit evidence → verified
//   - Outcome evidence alone (no audit) → verified
//   - Contradictory evidence → contradicted
//
// Git is audit evidence, never the authority. Verification requires outcome evidence
// that the intended result was achieved. Action evidence alone is insufficient.
//
// F8.1 (cancel-implies-verification): Verification is coupled to lifecycle status.
// A cancelled work MUST NOT surface as verified under any evidence pattern —
// cancellation erases the legitimate "work was completed" claim because the
// agent declared it would not produce the intended result. Any verified
// evidence already on the cancelled work becomes the audit trail of a claim
// that was withdrawn, not a success. The lifecycle-status gate below is
// structural: it cannot be bypassed by a follow-up evidence write or a
// re-derivation, because the gate is the first check before evidence is
// even consulted.
//
// Specifically:
//   - status='cancelled' AND contradictory evidence → contradicted
//   - status='cancelled' AND no contradictory evidence → unverified
//     (NOT verified, NOT partial, NOT contradicted-on-withdrawn-claim)
//   - status='open'      → evidence-based derivation (may be verified)
//   - status='done'      → evidence-based derivation (may be verified)
//
// This function never fabricates verification; it assesses what is actually
// observed AND reflects the agent's explicit lifecycle intent.
// evidenceDesignatedForVerification lists the evidence types that
// carry sufficient epistemic weight to verify a work item on their
// own. Mirrors snapshot.go's type-based semantics (any row with
// type='challenge' is contradicted) and prevents BLOCKER 3
// (a single default-strength observation promoting verified): an
// observation's default strength (0.4) is below the corroboration
// threshold, so observations need a corroborating row — either a
// designated type, an explicit high-strength observation, or
// aggregated moderate observations.
//
// The set is intentionally limited to the architectural "strong
// evidence" vocabulary (test, reproduction, decision_outcome). The
// registry at internal/evidence.go is the source of truth for type
// validity; this set is a focused subset used for verification only.
var evidenceDesignatedForVerification = map[string]bool{
	"test":             true,
	"reproduction":     true,
	"decision_outcome": true,
}

// observationVerifyThreshold is the strength an observation must
// individually exceed (or collectively sum past — see corroborationSum)
// to count as verification-grade. Set above the observation registry
// default (0.4) so a single default-strength observation does NOT promote,
// but explicit high-strength observations DO.
const observationVerifyThreshold = 0.7

// corroborationSum is the cumulative observation strength at which
// multiple weak observations corroborate verification. Three default
// observations (0.4 × 3 = 1.2) cross this threshold; two (0.8) do not.
// The threshold intentionally requires at least three independent
// moderate observations rather than two — preventing "two opinions
// against one" promotion patterns while still allowing checkpoint-style
// work to verify.
const corroborationSum = 1.0

// evidenceIsDispute reports whether an evidence row registers a dispute
// against the artifact. A dispute is visible in the audit trail and
// downgrades verified → partial, but is NOT sufficient by itself to
// establish "contradicted" — that requires corroboration.
//
// T20-1 (alpha-final): distinguishing challenge from corroborated
// contradiction is the architectural fix. A type='challenge' row at
// default strength (-0.6) is a "someone questioned this" signal. It must
// not, by itself, permanently convert verified work into established
// contradiction. Substantiation requires EITHER:
//   - two or more challenge rows (corroborating disputes),
//   - a strong negative observation (≤ -0.7), or
//   - cumulative negative evidence weight ≤ negativeCorroborationSum.
//
// A weak negative observation (-0.7 < strength < 0) is a dispute, not a
// contradiction.
func evidenceIsDispute(e Evidence) bool {
	if e.Type == "challenge" {
		return true
	}
	if e.Type == "observation" && e.Strength < 0 && e.Strength > -0.7 {
		return true
	}
	return false
}

// negativeCorroborationSum is the cumulative negative strength at which
// multiple dispute/weak-negative observations corroborate each other into
// established contradiction. Mirrors corroborationSum (1.0) on the
// positive side: three default-strength challenge rows (-0.6 each,
// -1.8 total) cross this threshold; two (-1.2) cross it too. Two weak
// negative observations (-0.5 each, -1.0 total) also cross it. The
// threshold intentionally requires more than one unsubstantiated voice
// to flip the verification state — preventing "one random dispute
// permanently converts verified work to contradicted" patterns.
const negativeCorroborationSum = -1.0

// evidenceSetHasContradiction reports whether the evidence set
// establishes a substantiated contradiction. This is the corroboration
// gate that protects the verified → contradicted transition from a
// single unsubstantiated challenge row.
//
// The earlier per-row evidenceIsContradiction helper conflated "this row
// is a dispute" with "this artifact is contradicted." That asymmetry let
// one challenge evidence row permanently convert verified work into
// established contradiction with no corroboration and no recovery path.
// T20-1 closes that gap: a single challenge row registers a dispute
// (visible in audit, downgrades to partial) but cannot establish
// contradiction alone.
func evidenceSetHasContradiction(evidence []Evidence) bool {
	if len(evidence) == 0 {
		return false
	}
	var (
		negativeSum       float64
		hasStrongNegative bool // single observation ≤ -0.7 — substantiated by its own weight
		hasDesignatedNeg  bool // designated verifier (test/reproduction/decision_outcome) with negative strength
	)
	for _, e := range evidence {
		// Strong negative observation: substantiated single source.
		if e.Type == "observation" && e.Strength <= -0.7 {
			hasStrongNegative = true
		}
		// Designated verifier with negative strength: a failing test is
		// substantiated contradiction on its own.
		if evidenceDesignatedForVerification[e.Type] && e.Strength < 0 {
			hasDesignatedNeg = true
		}
		// Cumulative negative weight — challenge rows (-0.6) and weak
		// negative observations both contribute.
		if e.Type == "challenge" || e.Type == "observation" {
			if e.Strength < 0 {
				negativeSum += e.Strength
			}
		}
	}
	return hasStrongNegative || hasDesignatedNeg || negativeSum <= negativeCorroborationSum
}

func (dm *DatabaseManager) DeriveWorkVerification(workID string) (WorkVerification, error) {
	evidence, err := ListEvidenceForArtifact(dm, workID, "work")
	if err != nil {
		return WorkVerificationUnverified, fmt.Errorf("derive verification: list evidence: %w", err)
	}

	// F8.1: Lifecycle-status gate. Read the work's status FIRST so the
	// decision tree can short-circuit before evidence promotion happens.
	// A missing row (deleted mid-derive) is treated as 'open' for safety —
	// deleted work should not appear in any verification result set.
	var statusStr string
	if err := dm.db.QueryRow(`SELECT COALESCE(status, 'open') FROM works WHERE id = ?`, workID).Scan(&statusStr); err != nil {
		if err == sql.ErrNoRows {
			verification := WorkVerificationUnverified
			return verification, nil
		}
		return WorkVerificationUnverified, fmt.Errorf("derive verification: read status: %w", err)
	}

	verification := WorkVerificationUnverified
	if statusStr == string(WorkStatusCancelled) {
		// F8.1: cancellation locks verification BELOW verified. Evidence
		// attached before or after cancellation is preserved as audit
		// history, but it cannot promote the cancelled work back to a
		// success state. A substantiated contradictory evidence set IS
		// still surfaced as 'contradicted' (it explicitly documents the
		// failure); any other evidence pattern yields 'unverified' — the
		// cancelled work is neither a verified success nor a partial
		// confidence.
		//
		// T20-1: the contradiction gate requires corroboration, matching
		// the open/done paths. A single unsubstantiated challenge row
		// against a cancelled work is recorded as audit history (visible
		// in evidence listing) but does NOT promote verification above
		// unverified — the cancelled work is not "verified success"
		// regardless of disputes.
		if evidenceSetHasContradiction(evidence) {
			verification = WorkVerificationContradicted
		} else {
			verification = WorkVerificationUnverified
		}
	} else if len(evidence) > 0 {
		// Walk the evidence set, classifying each row. We track several
		// orthogonal properties:
		//   hasContradiction — corroborated contradiction (T20-1: requires
		//                     substantiation, see evidenceSetHasContradiction)
		//   hasDispute       — unsubstantiated challenge row or weak
		//                     negative observation (visible in audit, but
		//                     does NOT establish contradiction)
		//   hasDesignated    — any test/reproduction/decision_outcome row
		//   hasHighObs       — any observation row with strength ≥ 0.7
		//   observationSum   — cumulative strength of observation rows
		//   hasOutcome / hasAudit / hasAction — source-group classification
		// The combination of (hasDesignated || hasHighObs || observationSum ≥ 1.0)
		// is the verification condition (BLOCKER 3): an observation alone
		// at default strength cannot verify, but corroboration does.
		var (
			hasDispute     bool
			hasDesignated  bool
			hasHighObs     bool
			observationSum float64
			hasOutcome     bool
			hasAudit       bool
			hasAction      bool
		)
		for _, e := range evidence {
			if evidenceIsDispute(e) {
				hasDispute = true
			}
			if evidenceDesignatedForVerification[e.Type] && e.Strength > 0 {
				hasDesignated = true
			}
			if e.Type == "observation" {
				if e.Strength >= observationVerifyThreshold {
					hasHighObs = true
				}
				if e.Strength > 0 {
					observationSum += e.Strength
				}
			}
			switch classifySourceGroup(e.SourceGroup) {
			case SourceGroupClassOutcome:
				hasOutcome = true
			case SourceGroupClassAudit:
				hasAudit = true
			case SourceGroupClassAction:
				hasAction = true
			}
		}
		hasContradiction := evidenceSetHasContradiction(evidence)

		switch {
		case hasContradiction:
			// T20-1: contradiction only fires when corroborated
			// (≥1.0 cumulative negative, OR designated negative, OR
			// strong negative observation ≤ -0.7). A single default-
			// strength challenge row alone is a dispute, not a
			// contradiction — verified work is downgraded to partial
			// (visible in audit), not permanently flipped to contradicted.
			verification = WorkVerificationContradicted
		case hasDispute && hasOutcome && (hasDesignated || hasHighObs || observationSum >= corroborationSum):
			// T20-1: an unsubstantiated dispute on otherwise-verified
			// work downgrades to partial. The dispute is visible in the
			// audit trail (via ListEvidenceForArtifact) so operators and
			// agents can see "someone questioned this," but the work does
			// not become contradicted until the dispute is corroborated
			// or resolved.
			verification = WorkVerificationPartial
		case hasDispute && (hasOutcome || hasAudit || hasAction):
			// Disputed work that lacks verification-grade corroboration
			// anyway stays partial — the dispute doesn't drag it down
			// further, but it doesn't promote either.
			verification = WorkVerificationPartial
		case hasOutcome && (hasDesignated || hasHighObs || observationSum >= corroborationSum):
			// Outcome evidence + corroboration → verified.
			//
			// BLOCKER 3 fix: a default-strength observation (0.4) alone
			// is NOT sufficient. The verification condition requires:
			//   (a) a designated evidence type (test/reproduction/decision_outcome), OR
			//   (b) an observation carrying explicit high strength (≥ 0.7), OR
			//   (c) aggregated observation strength ≥ corroborationSum (1.0)
			verification = WorkVerificationVerified
		case hasOutcome && !hasDesignated && !hasHighObs && observationSum < corroborationSum:
			// Outcome evidence exists but lacks sufficient corroboration.
			// The audit-trail evidence is preserved (no rows deleted) —
			// it's just insufficient to assert verified. Report partial so
			// the operator sees that observation has occurred but does
			// not yet trust verification.
			verification = WorkVerificationPartial
		case hasAudit && !hasOutcome:
			// Audit evidence exists but no outcome evidence.
			verification = WorkVerificationPartial
		case hasAction && !hasOutcome && !hasAudit:
			// Action occurred but no outcome observed.
			verification = WorkVerificationUnverified
		default:
			// Evidence exists but doesn't fit a recognized category.
			verification = WorkVerificationUnverified
		}
	}

	// Persist the derived value to the works row.
	_, err = dm.db.Exec(`UPDATE works SET verification = ? WHERE id = ?`, string(verification), workID)
	if err != nil {
		return WorkVerificationUnverified, fmt.Errorf("derive verification: persist: %w", err)
	}
	return verification, nil
}

// AddWorkNoteWithContext appends a note event.
func (dm *DatabaseManager) AddWorkNoteWithContext(workID, note string, ac ActiveContext) (*WorkEvent, error) {
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for note")
	}
	if note == "" {
		return nil, fmt.Errorf("note is required for note action")
	}
	// D-005: scan the note against the secret/poison scanner before
	// the underlying AppendWorkEvent fires. The error format mirrors
	// saveMemoryRow so the diagnostic surface is consistent across
	// write paths.
	if isSensitive, reason := isSensitiveContent(note); isSensitive {
		return nil, fmt.Errorf("sensitive content detected and blocked: %s", reason)
	}
	if isPoisoned, reason := isPoisoned(note); isPoisoned {
		return nil, fmt.Errorf("poison content detected and blocked: %s", reason)
	}
	prov := dm.provenanceFromContext(ac)
	directiveIDs := dm.GetActiveDirectiveIDs(ac.FrameworkName)
	var event *WorkEvent
	err := dm.WithTx(func(node DBNode) error {
		var err error
		event, err = dm.AppendWorkEvent(workID, WorkEvent{
			EventType:    WorkEventTypeNoteAppended,
			Note:         note,
			InvocationID: prov.InvocationID,
			DirectiveIDs: directiveIDs,
		}, prov, node)
		return err
	})
	return event, err
}

// ReopenWorkWithContext reopens work via event-sourced append.
func (dm *DatabaseManager) ReopenWorkWithContext(workID string, ac ActiveContext) (*Work, error) {
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for reopen")
	}
	prov := dm.provenanceFromContext(ac)
	directiveIDs := dm.GetActiveDirectiveIDs(ac.FrameworkName)
	err := dm.WithTx(func(node DBNode) error {
		_, err := dm.AppendWorkEvent(workID, WorkEvent{
			EventType:    WorkEventTypeReopened,
			InvocationID: prov.InvocationID,
			DirectiveIDs: directiveIDs,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, err
	}
	// F8.1: reopen puts work back into 'open' status, where the evidence
	// gate is again allowed to promote verification. Re-derive so the
	// fresh evidence picture is reflected immediately. Note: pre-cancel
	// evidence rows are still in the table, so a high-confidence memory
	// reopened from cancelled may now derive 'verified' from outcome
	// evidence — but ONLY because the user explicitly reactivated the
	// work. Cancellation cannot silently demote evidence rows; it only
	// stops them from promoting verification while cancelled.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		return nil, fmt.Errorf("reopen: derive verification: %w", err)
	}
	return dm.GetWork(workID)
}

// ResolveWorkContradiction is the agent-facing recovery path for T20-1.
//
// It neutralizes dispute evidence rows on a work item by setting
// expires_at to now. The rows remain in the `evidence` table (the audit
// trail is intact and reconstructable), but they no longer contribute
// to DeriveWorkVerification because loadEvidenceForRecompute filters
// expired rows.
//
// After neutralization, DeriveWorkVerification re-derives from the
// remaining evidence set, so a previously-verified work that was
// downgraded by an unsubstantiated dispute recovers to verified.
//
// `reason` is recorded in the `notes` field of the most recent
// dispute row that was neutralized, so the resolution event is
// auditable alongside the original dispute.
//
// T20-1 invariant: this is the ONLY recovery path. Resolution is
// always auditable, never silent. Evidence is never deleted.
func (dm *DatabaseManager) ResolveWorkContradiction(workID, reason string) error {
	if workID == "" {
		return fmt.Errorf("work_id is required for resolve-contradiction")
	}
	if reason == "" {
		return fmt.Errorf("reason is required for resolve-contradiction (audit trail)")
	}

	// Verify the work exists.
	var exists int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM works WHERE id = ?`, workID).Scan(&exists); err != nil {
		return fmt.Errorf("resolve-contradiction: read work: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("work not found: %s", workID)
	}

	now := time.Now().Unix()

	// Neutralize dispute rows. We target type='challenge' rows plus
	// weak negative observations (the dispute set, NOT the substantiated
	// contradiction set — a strong negative observation (≤ -0.7) and a
	// designated negative row are preserved so the contradiction remains
	// auditable even after a "resolve" attempt; if the operator wants to
	// withdraw those, they delete the work or annotate, not via this
	// path). T20-1: never delete evidence rows; only neutralize.
	//
	// The reason is stamped into the row's notes via a JSON merge so the
	// audit trail can answer "who resolved this and why."
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return fmt.Errorf("resolve-contradiction: begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE evidence
		SET expires_at = ?,
		    notes = CASE
		      WHEN notes IS NULL OR notes = '' THEN json_object('resolved_reason', ?, 'resolved_at', ?)
		      ELSE json_patch(notes, json_object('resolved_reason', ?, 'resolved_at', ?))
		    END
		WHERE artifact_id = ?
		  AND artifact_type = 'work'
		  AND expires_at IS NULL
		  AND (
		    type = 'challenge'
		    OR (type = 'observation' AND strength < 0 AND strength > -0.7)
		  )
	`, now, reason, now, reason, now, workID)
	if err != nil {
		return fmt.Errorf("resolve-contradiction: neutralize disputes: %w", err)
	}
	affected, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("resolve-contradiction: commit: %w", err)
	}

	// Record the resolution event in the work event ledger (audit trail).
	// Use a note_appended event so the resolution reason is inspectable
	// alongside the original work timeline.
	if _, err := dm.AddWorkNoteWithContext(workID,
		fmt.Sprintf("resolve-contradiction: %s (rows neutralized: %d)", reason, affected),
		ActiveContext{SessionID: "resolve-contradiction"}); err != nil {
		// Resolution already persisted; the note is auxiliary.
		// Log but don't fail.
		dm.LogAudit(AuditWarn, "work", "resolve-contradiction: failed to append audit note",
			"", AuditContext{"work_id": workID, "err": err.Error()})
	}

	// Re-derive verification from the now-cleaned evidence set.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		return fmt.Errorf("resolve-contradiction: rederive: %w", err)
	}
	return nil
}

// UpdateWorkWithContext handles title/content/status updates via events.
// Status is deprecated (maps to completed/cancelled/reopened) but still supported.
func (dm *DatabaseManager) UpdateWorkWithContext(workID, title, content, statusStr string, ac ActiveContext) (*Work, error) {
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for update")
	}
	prov := dm.provenanceFromContext(ac)
	directiveIDs := dm.GetActiveDirectiveIDs(ac.FrameworkName)

	// Defect D (2026-09-13 acceptance): a multi-field update (both
	// title and content) used to drop one of the fields. The pre-fix
	// switch only set eventType for the FIRST non-empty field it saw,
	// so a combined `update <id> --title X --content Y` produced a
	// single TitleUpdated event with both fields stored on the event
	// row, but the projection applied only title — the live `works`
	// row kept the old content. The fix emits one event per supplied
	// field within a single transaction so history + projection
	// agree on every field that was changed.
	var events []WorkEvent
	switch {
	case statusStr != "":
		switch WorkStatus(statusStr) {
		case WorkStatusDone:
			events = append(events, WorkEvent{EventType: WorkEventTypeCompleted, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs})
		case WorkStatusCancelled:
			events = append(events, WorkEvent{EventType: WorkEventTypeCancelled, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs})
		case WorkStatusOpen:
			events = append(events, WorkEvent{EventType: WorkEventTypeReopened, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs})
		default:
			return nil, fmt.Errorf("invalid status %q; must be open, done, or cancelled", statusStr)
		}
	case title != "" && content != "":
		events = append(events,
			WorkEvent{EventType: WorkEventTypeTitleUpdated, Title: title, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs},
			WorkEvent{EventType: WorkEventTypeContentUpdated, Content: content, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs},
		)
	case title != "":
		events = append(events, WorkEvent{EventType: WorkEventTypeTitleUpdated, Title: title, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs})
	case content != "":
		events = append(events, WorkEvent{EventType: WorkEventTypeContentUpdated, Content: content, InvocationID: prov.InvocationID, DirectiveIDs: directiveIDs})
	default:
		return nil, fmt.Errorf("at least one of status, title, or content is required for update")
	}
	if statusStr != "" {
		dm.LogAudit(AuditWarn, "work", "deprecated update status=X path used", "", AuditContext{"work_id": workID, "status": statusStr})
	}
	err := dm.WithTx(func(node DBNode) error {
		for _, ev := range events {
			if _, err := dm.AppendWorkEvent(workID, ev, prov, node); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Alpha-4.1 F-003: deprecated update status=done/cancelled path
	// must NOT auto-record git evidence. The canonical `complete` and
	// `cancel` paths deliberately omit the git auto-inflation (per
	// the P3 fix comment in handleCompleteWork) because it promoted
	// verification to "partial" for false completions. The two paths
	// now reach equivalent verification state.
	//
	// Callers that genuinely want git evidence recorded alongside a
	// status transition should call the explicit observation path
	// (`mpm_evidence action=add source_group=git`) rather than relying
	// on the deprecated update surface.
	//
	// F8.1: every terminal-lifecycle state transition must keep verification
	// derived state in lockstep with status. Without this call, a path that
	// moves status to 'cancelled' through the legacy update surface leaves
	// verification stale at whatever value it had — the exact bug the audit
	// flagged.
	if _, err := dm.DeriveWorkVerification(workID); err != nil {
		return nil, fmt.Errorf("update: derive verification: %w", err)
	}
	return dm.GetWork(workID)
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
		} else {
			dm.LogAudit(AuditWarn, "db", fmt.Sprintf("GetLessonStats: lesson type %q count failed, skipping: %v", t, err), "", AuditContext{})
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
// allowed but `rules` is the canonical home per docs/archive/shared-epistemology.md.
//
// Phase 3 of docs/archive/shared-epistemology.md: operator-only. No agent should be writing
// house rules autonomously. CLI and MCP both gate on confirm=true.
func (dm *DatabaseManager) RecordGlobalRule(content string, tags []string, weight float64, provenance string) (string, error) {
	if !dm.sharedAttached {
		return "", fmt.Errorf("shared DB not attached (set MPM_SHARED_DB)")
	}
	if content == "" {
		return "", fmt.Errorf("content is required")
	}
	if weight < 0 || weight > 100 {
		return "", fmt.Errorf("weight must be 0-100, got %v", weight)
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
		    (?, 'rules', ?, ?, ?, ?, 1, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER), NULL)
	`, id, content, string(tagsJSON), string(metaJSON), weight)
	if err != nil {
		return "", fmt.Errorf("insert shared rule: %w", err)
	}
	return id, nil
}

// PromoteToGlobal copies a local memory to the shared DB. The
// original local row stays in the local DB (per docs/archive/shared-epistemology.md: "Source
// row stays in local DB"). The shared copy is marked is_global=1
// with collection="rules" and a metadata.derived_from_local_id
// field linking back to the original.
//
// Phase 3 of docs/archive/shared-epistemology.md: operator-only. Requires confirm: true at
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
	var weight float64
	var reinforcement int
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
		    (?, ?, ?, ?, ?, ?, ?, 1, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER), NULL)
	`, newID, collection, content, tagsNS.String, string(metaJSON), weight, reinforcement)
	if err != nil {
		return "", fmt.Errorf("insert shared copy: %w", err)
	}
	return newID, nil
}

// RetireGlobalRule soft-retires a global rule by stamping retired_at on the
// shared.memories row. The row stays in the table for forensics; future
// QueryGlobalRules calls without include_retired=true filter it out.
//
// Why a timestamp column and not a status enum / hard delete:
//   - The shared-epistemology.md contract describes rules as "append-mostly";
//     hard delete via deleted_at would erase the audit trail.
//   - A timestamp column needs no CHECK constraint migration (ALTER TABLE ADD
//     COLUMN is idempotent via SafeMigrations; see schema.go:1244).
//   - The project's existing terminal-inactive noun is "retired" (capability
//     state enum, schema.go:530), so the column name matches operator
//     vocabulary without inventing a new word.
//
// Idempotency class (per docs/tool-behavioral-contract.md §1, §4):
//   - Already-retired: silent success with already_retired=true in the
//     response. No second audit row. Matches soft-delete idempotency
//     (mpm_skills.delete, mpm_handoff.shred).
//   - Missing rule_id / missing confirm: error.
//   - Unknown id: error (operator typing a stale id wants a diagnostic).
//   - Shared DB not attached: error.
//
// Args:
//   - ruleID: shared.memories.id (canonical; id-typed as a global rule id).
//   - reason: optional free-form note recorded in the audit row.
//   - confirm: must be exactly true. Strings/numbers/other are rejected.
func (dm *DatabaseManager) RetireGlobalRule(ruleID, reason string, confirm bool) (map[string]interface{}, error) {
	if !dm.sharedAttached {
		return nil, fmt.Errorf("shared DB not attached (set MPM_SHARED_DB)")
	}
	if ruleID == "" {
		return nil, fmt.Errorf("rule_id is required")
	}
	if !confirm {
		return nil, fmt.Errorf("retire_global_rule requires confirm=true; shared-DB state transitions must be operator-gated")
	}

	// Probe + transition in one statement so a stale id and a concurrent
	// retire race to a deterministic outcome. RowsAffected returns 0 when
	// the id is unknown OR when retired_at is already set — we
	// disambiguate below.
	now := time.Now().Unix()
	res, err := dm.db.Exec(`
		UPDATE shared.memories
		SET retired_at = ?
		WHERE id = ? AND is_global = 1 AND deleted_at IS NULL AND retired_at IS NULL
	`, now, ruleID)
	if err != nil {
		return nil, fmt.Errorf("retire global rule: %w", err)
	}
	rows, _ := res.RowsAffected()

	if rows == 0 {
		// Disambiguate: unknown id (error) vs already-retired (silent success).
		var existingRetired sql.NullInt64
		var existingDeleted sql.NullInt64
		err := dm.db.QueryRow(`
			SELECT retired_at, deleted_at FROM shared.memories
			WHERE id = ? AND is_global = 1
		`, ruleID).Scan(&existingRetired, &existingDeleted)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("retire global rule: rule %q not found", ruleID)
		}
		if err != nil {
			return nil, fmt.Errorf("probe rule: %w", err)
		}
		if existingDeleted.Valid && existingDeleted.Int64 != 0 {
			return nil, fmt.Errorf("retire global rule: rule %q is deleted (deleted_at IS NOT NULL); restore it first", ruleID)
		}
		// Already retired — silent success, no audit row.
		return map[string]interface{}{
			"success":         true,
			"rule_id":         ruleID,
			"already_retired": true,
			"retired_at":      existingRetired.Int64,
		}, nil
	}

	// Forensic audit row. The component is shared_db; the audit message
	// names the operator transition so watchdog surfacing reads naturally.
	dm.LogAudit(
		AuditInfo, "shared_db",
		fmt.Sprintf("retire_global_rule %s", ruleID), "",
		AuditContext{
			"rule_id": ruleID,
			"reason":  reason,
		},
	)

	return map[string]interface{}{
		"success":         true,
		"rule_id":         ruleID,
		"retired_at":      now,
		"already_retired": false,
	}, nil
}

// migrateWorkEvents creates the work_events table and seeds initial created events
// for existing works rows that have not yet been migrated. Idempotent: uses
// CREATE TABLE IF NOT EXISTS for the table and WHERE migrated_at IS NULL for the
// seed so re-running on an already-migrated DB is a no-op.
//
// CHECK enum is kept in sync with WorkTables in schema.go (single source).
// If they diverge, fresh DBs and migrated DBs accept different event types
// — see forensic audit §15 #14. The canonical 9-value vocabulary includes
// claimed_complete and evidence_observed for Phase-2 verification.
func (dm *DatabaseManager) migrateWorkEvents() error {
	// Step 1: ensure the work_events table exists (no-op if already created via
	// WorkTables on a fresh DB). Domain-neutral: only invocation linkage,
	// event payload, and directive instruction provenance.
	workEventsDDL := `CREATE TABLE IF NOT EXISTS work_events (
		id                    TEXT PRIMARY KEY,
		work_id               TEXT NOT NULL,
		event_index           INTEGER NOT NULL,
		event_type            TEXT NOT NULL
		                      CHECK (event_type IN (
		                        'created','note_appended','completed',
		                        'cancelled','reopened',
		                        'title_updated','content_updated',
		                        'claimed_complete','evidence_observed'
		                      )),
		created_at            INTEGER NOT NULL
		                      DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		invocation_id         TEXT,
		parent_invocation_id TEXT,
		note                  TEXT,
		title                 TEXT,
		content               TEXT,
		directive_ids         TEXT DEFAULT '[]',
		UNIQUE(work_id, event_index)
	);`
	if _, err := dm.db.Exec(workEventsDDL); err != nil {
		return fmt.Errorf("create work_events table: %w", err)
	}

	// Step 1b: migrate legacy schema (extra provenance/git columns or old CHECK)
	// to domain-neutral shape. See forensic fix.
	if err := dm.migrateWorkEventsCheck(); err != nil {
		return fmt.Errorf("migrate work_events check: %w", err)
	}

	// Step 2: create indexes if they don't exist (no-op if already created).
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_work_events_work_id ON work_events(work_id);`,
		`CREATE INDEX IF NOT EXISTS idx_work_events_invocation ON work_events(invocation_id);`,
	}
	for _, idx := range indexes {
		if _, err := dm.db.Exec(idx); err != nil {
			return fmt.Errorf("create work_events index: %w", err)
		}
	}
	// Drop legacy session index if column no longer exists (ignore error)
	_, _ = dm.db.Exec(`DROP INDEX IF EXISTS idx_work_events_session`)

	// Step 3: seed initial created events for existing works rows that have not
	// yet been migrated. The migrated_at IS NULL predicate makes this idempotent,
	// but also guard against duplicate events if a work was created via the new
	// event-sourced path but migrated_at was not set (pre-fix DBs).
	seedSQL := `
		INSERT OR IGNORE INTO work_events
			(id, work_id, event_index, event_type, created_at,
			 invocation_id, note, title, content, directive_ids)
		SELECT
			lower(hex(randomblob(16))),
			id, 0, 'created', created_at,
			NULL, '', title, content, '[]'
		FROM works
		WHERE migrated_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM work_events WHERE work_events.work_id = works.id AND work_events.event_index = 0);
	`
	if _, err := dm.db.Exec(seedSQL); err != nil {
		return fmt.Errorf("seed work_events: %w", err)
	}

	// Step 4: mark all unmigrated works rows as migrated.
	if _, err := dm.db.Exec(`UPDATE works SET migrated_at = CAST(strftime('%s','now') AS INTEGER) WHERE migrated_at IS NULL`); err != nil {
		return fmt.Errorf("mark works migrated: %w", err)
	}

	return nil
}

// migrateWorkEventsCheck upgrades existing work_events tables to the
// domain-neutral shape (no duplicate provenance, no git columns). Fresh
// tables already have the correct CHECK and shape and are left untouched.
// Handles three legacy cases:
//   - old 7-value CHECK without claimed_complete/evidence_observed
//   - provenance duplication (actor_kind etc.)
//   - git domain-specific columns
func (dm *DatabaseManager) migrateWorkEventsCheck() error {
	var sqlDef string
	err := dm.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='work_events'`).Scan(&sqlDef)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		// Best-effort probe for legacy schema; table may not exist yet
		slog.Warn("migrateWorkEventsCheck: probe failed", "err", err)
		return nil
	}
	if sqlDef == "" {
		return nil
	}
	hasClaimed := strings.Contains(sqlDef, "claimed_complete")
	hasDirective := strings.Contains(sqlDef, "directive_ids")
	hasActor := strings.Contains(sqlDef, "actor_kind")
	hasGit := strings.Contains(sqlDef, "git_head_before")
	// New shape: has claimed, has directive, no actor/git
	if hasClaimed && hasDirective && !hasActor && !hasGit {
		return nil
	}
	// Need to recreate table with domain-neutral schema.
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for check migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	newDDL := `CREATE TABLE work_events_new (
		id                    TEXT PRIMARY KEY,
		work_id               TEXT NOT NULL,
		event_index           INTEGER NOT NULL,
		event_type            TEXT NOT NULL
		                      CHECK (event_type IN (
		                        'created','note_appended','completed',
		                        'cancelled','reopened',
		                        'title_updated','content_updated',
		                        'claimed_complete','evidence_observed'
		                      )),
		created_at            INTEGER NOT NULL
		                      DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		invocation_id         TEXT,
		parent_invocation_id TEXT,
		note                  TEXT,
		title                 TEXT,
		content               TEXT,
		directive_ids         TEXT DEFAULT '[]',
		UNIQUE(work_id, event_index)
	);`
	if _, err := tx.Exec(newDDL); err != nil {
		return fmt.Errorf("create work_events_new: %w", err)
	}
	// Copy existing rows — handle various legacy column sets.
	// Determine which columns exist in old table.
	hasInvocation := strings.Contains(sqlDef, "invocation_id")
	hasDirectiveOld := hasDirective
	if hasInvocation && hasDirectiveOld {
		if _, err := tx.Exec(`INSERT INTO work_events_new
			(id, work_id, event_index, event_type, created_at,
			 invocation_id, parent_invocation_id, note, title, content, directive_ids)
			SELECT id, work_id, event_index, event_type, created_at,
			       invocation_id, parent_invocation_id, note, title, content, COALESCE(directive_ids,'[]')
			FROM work_events`); err != nil {
			return fmt.Errorf("copy work_events data (full): %w", err)
		}
	} else if hasInvocation {
		if _, err := tx.Exec(`INSERT INTO work_events_new
			(id, work_id, event_index, event_type, created_at,
			 invocation_id, parent_invocation_id, note, title, content, directive_ids)
			SELECT id, work_id, event_index, event_type, created_at,
			       invocation_id, parent_invocation_id, note, title, content, '[]'
			FROM work_events`); err != nil {
			return fmt.Errorf("copy work_events data (no directive): %w", err)
		}
	} else {
		if _, err := tx.Exec(`INSERT INTO work_events_new
			(id, work_id, event_index, event_type, created_at,
			 invocation_id, parent_invocation_id, note, title, content, directive_ids)
			SELECT id, work_id, event_index, event_type, created_at,
			       '', '', note, title, content, '[]'
			FROM work_events`); err != nil {
			return fmt.Errorf("copy work_events data (legacy): %w", err)
		}
	}
	if _, err := tx.Exec(`DROP TABLE work_events`); err != nil {
		return fmt.Errorf("drop old work_events: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE work_events_new RENAME TO work_events`); err != nil {
		return fmt.Errorf("rename work_events_new: %w", err)
	}
	// Recreate indexes
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_work_events_work_id ON work_events(work_id)`); err != nil {
		return fmt.Errorf("recreate idx_work_id: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_work_events_invocation ON work_events(invocation_id)`); err != nil {
		return fmt.Errorf("recreate idx_invocation: %w", err)
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_work_events_session`); err != nil {
		return fmt.Errorf("recreate idx_session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit check migration: %w", err)
	}
	return nil
}

// AppendWorkEvent appends an immutable event to the work_events ledger.
// Domain-neutral: only invocation linkage and instruction provenance are
// stored. Full execution telemetry lives in artifact_provenance and is
// reachable via JOIN on invocation_id.
//
// D-005: the event's note/title/content are scanned against the
// secret/poison scanner before the INSERT fires. The error format
// mirrors saveMemoryRow so the diagnostic surface is consistent across
// write paths.
//
// node: DBNode from WithTx callback. Use node.ExecTracked/QueryRowTracked when
// non-nil; fall back to dm.db when nil.
func (dm *DatabaseManager) AppendWorkEvent(workID string, event WorkEvent, ep *EffectiveProvenance, node DBNode) (*WorkEvent, error) {
	// D-005: scan the user-supplied note/title/content against the
	// secret/poison scanner. The check must happen before any DB
	// write so blocked content never reaches work_events (which is
	// FTS-indexed and surfaces in `work history` and
	// `wake_context.open_works`).
	scanned := event.Note
	if event.Title != "" {
		scanned = scanned + " " + event.Title
	}
	if event.Content != "" {
		scanned = scanned + " " + event.Content
	}
	if scanned != "" {
		if isSensitive, reason := isSensitiveContent(scanned); isSensitive {
			return nil, fmt.Errorf("sensitive content detected and blocked: %s", reason)
		}
		if isPoisoned, reason := isPoisoned(scanned); isPoisoned {
			return nil, fmt.Errorf("poison content detected and blocked: %s", reason)
		}
	}

	// 1. Resolve invocation linkage. Full provenance is in artifact_provenance;
	// work_events stores only the join keys.
	invocationID := event.InvocationID
	parentInvocationID := event.ParentInvocationID
	if invocationID == "" && ep != nil && ep.InvocationID != "" {
		invocationID = ep.InvocationID
	}
	if parentInvocationID == "" && ep != nil && ep.ParentInvocationID != "" {
		parentInvocationID = ep.ParentInvocationID
	}
	if invocationID == "" {
		// Fall back to resolver's invocation if available, else generate.
		if base := dm.getEffectiveProvenance(); base != nil && base.InvocationID != "" {
			invocationID = base.InvocationID
		} else {
			invocationID = GenerateID()
		}
	}
	if parentInvocationID == "" && ep != nil {
		parentInvocationID = ep.ParentInvocationID
	}

	// 2. Generate ID if not set.
	id := event.ID
	if id == "" {
		id = GenerateID()
	}

	// 3. Determine event_index. Use node.QueryRowTracked if inside WithTx,
	// otherwise fall back to dm.db.QueryRow.
	var eventIndex int
	var err error
	if node != nil {
		err = node.QueryRowTracked(`
			SELECT COALESCE(MAX(event_index), -1) + 1 FROM work_events WHERE work_id = ?
		`, workID).Scan(&eventIndex)
	} else {
		err = dm.db.QueryRow(`
			SELECT COALESCE(MAX(event_index), -1) + 1 FROM work_events WHERE work_id = ?
		`, workID).Scan(&eventIndex)
	}
	if err != nil {
		return nil, fmt.Errorf("get next event_index: %w", err)
	}

	// F-B1 (gate fix 2026-08-29): the event-sourced path here was the
	// structural bypass — `updateWorkStatus` enforced the state machine,
	// but every caller that goes through AppendWorkEvent (CompleteWorkWithContext,
	// CancelWorkWithContext, UpdateWorkWithContext status=…, ReopenWorkWithContext)
	// landed here and bypassed isValidWorkTransition entirely. The hostile
	// gate re-run surfaced this: `mpm_work action=update status=cancelled` on
	// a `done` work succeeded with `"status":"cancelled"`. Enforce the matrix
	// here for any event whose target status is one of the terminal pair.
	var currentStatus string
	if node != nil {
		if err := node.QueryRowTracked(`SELECT status FROM works WHERE id = ?`, workID).Scan(&currentStatus); err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("work not found: %s", workID)
			}
			return nil, fmt.Errorf("read current work status: %w", err)
		}
	} else {
		if err := dm.db.QueryRow(`SELECT status FROM works WHERE id = ?`, workID).Scan(&currentStatus); err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("work not found: %s", workID)
			}
			return nil, fmt.Errorf("read current work status: %w", err)
		}
	}
	// Defer the transition check until we've computed newStatus below.

	// 4. Derive works-row projection values before the event INSERT.
	now := time.Now().Unix()
	var newStatus string
	var newCompletedAt *int64
	var newTitle *string
	var newContent *string
	switch event.EventType {
	case WorkEventTypeCreated:
		// The works row was just INSERTed with status='open' by AddWork.
		// The Created event is the event-sourced mirror of that initial
		// state — NOT a state transition. newStatus stays empty so the
		// F-B1 state machine check below does not reject 'open → open'
		// for first-time creation.
		newStatus = ""
		if event.Title != "" {
			t := event.Title
			newTitle = &t
		}
		if event.Content != "" {
			c := event.Content
			newContent = &c
		}
	case WorkEventTypeCompleted, WorkEventTypeClaimedComplete:
		newStatus = "done"
		newCompletedAt = &now
	case WorkEventTypeCancelled:
		newStatus = "cancelled"
	case WorkEventTypeReopened:
		newStatus = "open"
		newCompletedAt = nil // clear terminal state
	case WorkEventTypeTitleUpdated:
		newStatus = "" // no status change
		if event.Title != "" {
			t := event.Title
			newTitle = &t
		}
	case WorkEventTypeContentUpdated:
		newStatus = ""
		if event.Content != "" {
			c := event.Content
			newContent = &c
		}
	case WorkEventTypeEvidenceObserved:
		newStatus = ""
	case WorkEventTypeNoteAppended:
		// D-003 (alpha-4.1.1): a note is an annotation, not a state
		// transition. Without this case, the default branch sets
		// newStatus = "open" and the F-B1 state-machine check
		// rejects "open → open" — the very transition we never meant
		// to attempt. Notes must leave status (and verification)
		// unchanged. The handler treats the note as pure event-log
		// content; only title/content updates are routed to the
		// works-row projection at the bottom of this function.
		newStatus = ""
	default:
		newStatus = "open"
	}

	// F-B1 (gate fix 2026-08-29): enforce the state machine for any event
	// whose target status is one of the canonical trio (open/done/cancelled).
	// isValidWorkTransition rejects same-state transitions itself, so we
	// don't gate on newStatus != currentStatus here. Note events, title
	// updates, content updates, and evidence observations leave newStatus
	// empty and pass through without a check. The read of currentStatus
	// above happened inside the WithTx, so the read+write pair is atomic
	// against concurrent AppendWorkEvent callers.
	if newStatus != "" {
		if !isValidWorkTransition(WorkStatus(currentStatus), WorkStatus(newStatus)) {
			// RUNTIME OUTCOME WIRING (2026-09-21): typed sentinel wrap
			// (companion site to the UpdateWorkStatus path above).
			return nil, fmt.Errorf("%w: %s → %s for work %s",
				ErrInvalidWorkTransition, currentStatus, newStatus, workID)
		}
	}

	// 5. Insert the event row. Domain-neutral: only invocation linkage + directive provenance.
	title := event.Title
	content := event.Content
	note := event.Note
	directiveIDsJSON := "[]"
	if len(event.DirectiveIDs) > 0 {
		if b, err := jsonMarshal(event.DirectiveIDs); err == nil {
			directiveIDsJSON = string(b)
		}
	}

	var execErr error
	if node != nil {
		_, execErr = node.ExecTracked(`
			INSERT INTO work_events (
				id, work_id, event_index, event_type, created_at,
				invocation_id, parent_invocation_id,
				note, title, content, directive_ids
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, 5, // D-003 (alpha-4.1.2): match ExecTracked default retry budget
			// so concurrent notes on the same work item tolerate
			// SQLITE_BUSY / SQLITE_LOCKED at the write step. The
			// previous retries=0 meant the INSERT could fail
			// mid-callback after Begin() and the SELECT had
			// succeeded — leaving a half-progressed transaction
			// that the WithTx retry would have rolled back and
			// re-run had it known. Now both read and write sides
			// retry under the same budget.
			id, workID, eventIndex, string(event.EventType), now,
			nullString(invocationID), nullString(parentInvocationID),
			nullString(note), nullString(title), nullString(content),
			directiveIDsJSON,
		)
	} else {
		_, execErr = dm.db.Exec(`
			INSERT INTO work_events (
				id, work_id, event_index, event_type, created_at,
				invocation_id, parent_invocation_id,
				note, title, content, directive_ids
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			id, workID, eventIndex, string(event.EventType), now,
			nullString(invocationID), nullString(parentInvocationID),
			nullString(note), nullString(title), nullString(content),
			directiveIDsJSON,
		)
	}
	if execErr != nil {
		return nil, fmt.Errorf("insert work_event: %w", execErr)
	}

	// 6. Update the works row as a derived projection. Same pattern:
	// use node when inside WithTx, dm.db otherwise.
	// Priority: title/content updates are direct field writes; status updates are status-machine writes.
	// works is a cached projection — UPDATE happens in same atomic transaction as INSERT.
	if newTitle != nil || newContent != nil {
		if newTitle != nil && newContent != nil {
			if node != nil {
				_, err = node.ExecTracked(`UPDATE works SET title = ?, content = ?, updated_at = ? WHERE id = ?`, 0, *newTitle, *newContent, now, workID)
			} else {
				_, err = dm.db.Exec(`UPDATE works SET title = ?, content = ?, updated_at = ? WHERE id = ?`, *newTitle, *newContent, now, workID)
			}
		} else if newTitle != nil {
			if node != nil {
				_, err = node.ExecTracked(`UPDATE works SET title = ?, updated_at = ? WHERE id = ?`, 0, *newTitle, now, workID)
			} else {
				_, err = dm.db.Exec(`UPDATE works SET title = ?, updated_at = ? WHERE id = ?`, *newTitle, now, workID)
			}
		} else {
			if node != nil {
				_, err = node.ExecTracked(`UPDATE works SET content = ?, updated_at = ? WHERE id = ?`, 0, *newContent, now, workID)
			} else {
				_, err = dm.db.Exec(`UPDATE works SET content = ?, updated_at = ? WHERE id = ?`, *newContent, now, workID)
			}
		}
	} else if newStatus != "" {
		if newCompletedAt != nil {
			if node != nil {
				_, err = node.ExecTracked(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = ?
					WHERE id = ?`, 0, newStatus, now, *newCompletedAt, workID)
			} else {
				_, err = dm.db.Exec(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = ?
					WHERE id = ?`, newStatus, now, *newCompletedAt, workID)
			}
		} else if event.EventType == WorkEventTypeReopened {
			if node != nil {
				_, err = node.ExecTracked(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = NULL
					WHERE id = ?`, 0, newStatus, now, workID)
			} else {
				_, err = dm.db.Exec(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = NULL
					WHERE id = ?`, newStatus, now, workID)
			}
		} else {
			if node != nil {
				_, err = node.ExecTracked(`
					UPDATE works SET status = ?, updated_at = ?
					WHERE id = ?`, 0, newStatus, now, workID)
			} else {
				_, err = dm.db.Exec(`
					UPDATE works SET status = ?, updated_at = ?
					WHERE id = ?`, newStatus, now, workID)
			}
		}
	} else if newStatus != "" {
		if newCompletedAt != nil {
			if node != nil {
				_, err = node.ExecTracked(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = ?
					WHERE id = ?`, 0, newStatus, now, *newCompletedAt, workID)
			} else {
				_, err = dm.db.Exec(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = ?
					WHERE id = ?`, newStatus, now, *newCompletedAt, workID)
			}
		} else if event.EventType == WorkEventTypeReopened {
			if node != nil {
				_, err = node.ExecTracked(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = NULL
					WHERE id = ?`, 0, newStatus, now, workID)
			} else {
				_, err = dm.db.Exec(`
					UPDATE works SET status = ?, updated_at = ?, completed_at = NULL
					WHERE id = ?`, newStatus, now, workID)
			}
		} else {
			if node != nil {
				_, err = node.ExecTracked(`
					UPDATE works SET status = ?, updated_at = ?
					WHERE id = ?`, 0, newStatus, now, workID)
			} else {
				_, err = dm.db.Exec(`
					UPDATE works SET status = ?, updated_at = ?
					WHERE id = ?`, newStatus, now, workID)
			}
		}
	} else {
		// Note or other event that doesn't affect works projection — just bump updated_at.
		if node != nil {
			_, err = node.ExecTracked(`UPDATE works SET updated_at = ? WHERE id = ?`, 0, now, workID)
		} else {
			_, err = dm.db.Exec(`UPDATE works SET updated_at = ? WHERE id = ?`, now, workID)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("update works projection: %w", err)
	}

	// 7. Return the WorkEvent with assigned id and event_index.
	event.ID = id
	event.WorkID = workID
	event.EventIndex = eventIndex
	event.CreatedAt = now
	event.InvocationID = invocationID
	event.ParentInvocationID = parentInvocationID

	return &event, nil
}

// RecordWorkArtifactProvenance writes an artifact_provenance row for a newly
// created work item. It opens its own short-lived transaction when called
// outside an active transaction (e.g., from a handler after the main tx
// has committed). Provenance failures are non-fatal — errors are logged
// and the function returns without propagating.
func (dm *DatabaseManager) RecordWorkArtifactProvenance(workID string, actorKind, actorID, frameworkName, sessionID string) {
	prov := &EffectiveProvenance{
		ActorKind:     actorKind,
		ActorID:       actorID,
		FrameworkName: frameworkName,
		SessionID:     sessionID,
	}
	// Begin our own tx. The provenance row is non-fatal, so if the tx
	// cannot start we log and continue without propagating the error.
	tx, err := dm.db.Begin()
	if err != nil {
		dm.LogAudit(AuditWarn, "provenance", "RecordWorkArtifactProvenance tx begin failed", "", AuditContext{
			"work_id": workID,
			"reason":  err.Error(),
		})
		return
	}
	res := dm.RecordArtifactProvenance(tx, workID, "work", prov, true /* skipAudit: provenance row is enough; own error log handles failures */)
	if !res.Recorded {
		tx.Rollback()
		dm.LogAudit(AuditWarn, "provenance", "RecordWorkArtifactProvenance record failed", "", AuditContext{
			"work_id":       workID,
			"reason":        res.ValidationReason,
			"sql_error":     res.SQLError,
			"artifact_type": "work",
		})
		return
	}
	if err := tx.Commit(); err != nil {
		dm.LogAudit(AuditWarn, "provenance", "RecordWorkArtifactProvenance commit failed", "", AuditContext{
			"work_id": workID,
			"reason":  err.Error(),
		})
	}
}

// GetWorkEvents returns all events for a work item ordered by event_index ASC.
// Domain-neutral: only invocation linkage + directive provenance + payload.
func (dm *DatabaseManager) GetWorkEvents(workID string) ([]*WorkEvent, error) {
	rows, err := dm.db.Query(`
		SELECT
			id, work_id, event_index, event_type, created_at,
			COALESCE(invocation_id, '') AS invocation_id,
			COALESCE(parent_invocation_id, '') AS parent_invocation_id,
			COALESCE(note, '') AS note,
			COALESCE(title, '') AS title,
			COALESCE(content, '') AS content,
			COALESCE(directive_ids, '[]') AS directive_ids
		FROM work_events
		WHERE work_id = ?
		ORDER BY event_index ASC
	`, workID)
	if err != nil {
		return nil, fmt.Errorf("get work events: %w", err)
	}
	defer rows.Close()

	events := make([]*WorkEvent, 0)
	for rows.Next() {
		var e WorkEvent
		var eventType string
		var directiveIDsJSON string
		if err := rows.Scan(
			&e.ID, &e.WorkID, &e.EventIndex, &eventType, &e.CreatedAt,
			&e.InvocationID,
			&e.ParentInvocationID,
			&e.Note,
			&e.Title,
			&e.Content,
			&directiveIDsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan work event row: %w", err)
		}
		e.EventType = WorkEventType(eventType)
		if directiveIDsJSON != "" && directiveIDsJSON != "[]" {
			_ = json.Unmarshal([]byte(directiveIDsJSON), &e.DirectiveIDs)
		}
		events = append(events, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return events, nil
}

// GetLatestWorkEvent returns the most recent event for a work item.
func (dm *DatabaseManager) GetLatestWorkEvent(workID string) (*WorkEvent, error) {
	var e WorkEvent
	var eventType string
	var directiveIDsJSON string
	err := dm.db.QueryRow(`
		SELECT
			id, work_id, event_index, event_type, created_at,
			COALESCE(invocation_id, '') AS invocation_id,
			COALESCE(parent_invocation_id, '') AS parent_invocation_id,
			COALESCE(note, '') AS note,
			COALESCE(title, '') AS title,
			COALESCE(content, '') AS content,
			COALESCE(directive_ids, '[]') AS directive_ids
		FROM work_events
		WHERE work_id = ?
		ORDER BY event_index DESC
		LIMIT 1
	`, workID).Scan(
		&e.ID, &e.WorkID, &e.EventIndex, &eventType, &e.CreatedAt,
		&e.InvocationID,
		&e.ParentInvocationID,
		&e.Note,
		&e.Title,
		&e.Content,
		&directiveIDsJSON,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest work event: %w", err)
	}
	e.EventType = WorkEventType(eventType)
	if directiveIDsJSON != "" && directiveIDsJSON != "[]" {
		_ = json.Unmarshal([]byte(directiveIDsJSON), &e.DirectiveIDs)
	}
	return &e, nil
}

// RecomputeWorkProjection recomputes the works row from the event ledger.
// It derives status, title, content, updated_at and completed_at by replaying
// events in order. Verification is NOT derived from events — it is derived
// from evidence rows by DeriveWorkVerification. Used when the works row may
// have drifted from its event source.
func (dm *DatabaseManager) RecomputeWorkProjection(workID string) error {
	rows, err := dm.db.Query(`
		SELECT event_type, created_at, title, content
		FROM work_events
		WHERE work_id = ?
		ORDER BY event_index ASC
	`, workID)
	if err != nil {
		return fmt.Errorf("query work events for projection: %w", err)
	}
	defer rows.Close()

	var (
		title, content string
		status         = "open"
		maxCreatedAt   int64
		completedAt    *int64
		hasEvents      bool
	)
	for rows.Next() {
		var eventType string
		var createdAt int64
		var eTitle, eContent sql.NullString
		if err := rows.Scan(&eventType, &createdAt, &eTitle, &eContent); err != nil {
			return fmt.Errorf("scan event for projection: %w", err)
		}
		hasEvents = true
		if createdAt > maxCreatedAt {
			maxCreatedAt = createdAt
		}
		switch eventType {
		case "created":
			if eTitle.Valid && eTitle.String != "" {
				title = eTitle.String
			}
			if eContent.Valid && eContent.String != "" {
				content = eContent.String
			}
			status = "open"
		case "completed", "claimed_complete":
			status = "done"
			completedAt = &createdAt
		case "cancelled":
			status = "cancelled"
			completedAt = nil
		case "reopened":
			status = "open"
			completedAt = nil
		case "title_updated":
			if eTitle.Valid && eTitle.String != "" {
				title = eTitle.String
			}
		case "content_updated":
			if eContent.Valid && eContent.String != "" {
				content = eContent.String
			}
		case "evidence_observed":
			// Evidence observed does not change status or verification.
			// Verification is derived from evidence rows by DeriveWorkVerification.
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows iteration: %w", err)
	}
	if !hasEvents {
		return nil
	}
	if maxCreatedAt == 0 {
		maxCreatedAt = time.Now().Unix()
	}
	_, err = dm.db.Exec(`
		UPDATE works SET status = ?, title = ?, content = ?, updated_at = ?, completed_at = ?
		WHERE id = ?
	`, status, title, content, maxCreatedAt, completedAt, workID)
	if err != nil {
		return fmt.Errorf("update works projection: %w", err)
	}
	return nil
}
