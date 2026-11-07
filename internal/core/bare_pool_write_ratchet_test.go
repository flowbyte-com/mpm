package internal

// bare_pool_write_ratchet_test.go — issue #178, permanent prevention.
//
// WHAT THIS FILE IS
// -----------------
// Issue #178's audit found bare pool writes (`dm.db.Exec(...)`,
// `dm.SQLDB().Exec(...)`, `s.DB.Exec(...)`) that bypass ExecTracked's
// statement-level retry budget: under contention that outlasts the
// busy timeout the write is simply lost. The corrections route the
// dangerous ones through a budget; this file keeps the class
// discharged.
//
// WHY AN ALLOWLIST AND NOT A COUNT
// --------------------------------
// The first version of this guard was a per-file count ceiling: a file
// whose bare-write count grew failed the build. That stops growth, but
// it does not stop SUBSTITUTION — fixing a dangerous write and adding
// a new bare write in the same file nets to zero and sails through.
// The audit's finding was never "there are N of them"; it was a list
// of specific call sites, each with a verdict. The guard keeps that
// shape: every remaining bare pool write is named, by the file it
// lives in and the normalized shape of its SQL, with a documented
// reason it is allowed to stay bare. Removing a site (routing it
// through a budget, or moving it inside a transaction) deletes its
// entry; adding a site fails loudly until it is recorded with a
// reason. A corrected call that is reintroduced is caught, because the
// fixed shape is gone from the list and the reintroduced spelling is
// not on it.
//
// MULTIPLICITY
// ------------
// Two identical bare writes in the same file share one identity —
// (file, normalized SQL) alone treats them as one call site, so
// inserting a second copy of an allowlisted statement would sail
// through. Every entry therefore records its expected occurrence
// Count, and the comparison is multiset-aware: the live count per
// identity must equal the recorded count. A duplicated write is
// reported as surplus occurrences with their line numbers; a fixed
// write is reported as a stale entry with its expected count. This
// closes the known identity flaw without making line numbers part of
// the identity (moving a statement within its file still does not
// invalidate its entry).
//
// WHAT COUNTS
// -----------
// A bare pool write is a direct `Exec` OR `ExecContext` call whose
// receiver is a pool handle (`*.db`, `*.DB`, `*.SQLDB()`, a local
// `db`/`q`/`ex` handle) with a write-verb SQL (DML and DDL alike — a
// growing DDL count must also trip review, since DDL wants a
// transaction, not a budget), that is NOT lexically inside a `WithTx`
// callback and NOT on a `tx` handle. `node.Tx().Exec(...)` is
// transaction-owned and is not counted.
//
// THE ExecContext SHAPE
// ---------------------
// `ExecContext(ctx, query, args...)` puts the query in the SECOND
// argument — `Args[0]` is the context. A scanner that reads `Args[0]`
// sees the context, not the SQL, and every `ExecContext` write is
// invisible to it.
//
// That is not hypothetical: the phase-0 audit scanner
// (`internal/audit/barewritescan`, from the unmerged audit branch)
// matched the method name `ExecContext` but still read `Args[0]`, so it
// classified all 17 of those sites as non-writes and left them out of
// the inventory entirely. The merged ratchet matched only the method
// name `Exec`, and had the same blind spot for the same reason.
//
// A detector that cannot see a write shape cannot keep that shape
// honest, and the shape is one line away from every future caller. So
// this scanner resolves the SQL from the argument position that
// matches the method:
//
//	Exec(query, args...)         → Args[0]
//	ExecContext(ctx, query, ...) → Args[1]
//
// The 12 bare-pool `ExecContext` sites that exist today are recorded in
// the allowlist below with a reason each, exactly like the `Exec` sites.
// The other 5 are transaction-owned (`tx.ExecContext` inside an explicit
// `BeginTx`) and are correctly not counted.
//
// The classification is syntactic and therefore an approximation —
// see the audit document for the shapes that defeat it
// (`docs/dev/bare-write-risk-assessment-2026-10-29.md`, "three bugs in
// my first scanner"). The approximation is load-bearing in exactly one
// direction: it may OVER-count (a site that is actually safe still
// trips the ratchet until it is recorded), it must never UNDER-count
// a genuinely bare write. Every shape below was checked against the
// audit scanner's inventory before the baseline was recorded.
//
// THE REASONS
// -----------
// Every entry carries one reason from this closed vocabulary. A new
// entry must pick one, which forces the author to classify the write
// rather than merely count it:
//
// - reasonPropagated: the error reaches the caller (returned error,
//   CLI exit code, tool error, agent-visible failure), which can retry
//   or report. The write can still be lost to contention — the caller
//   is told, so the loss is recoverable rather than silent.
// - reasonStartupDDL: schema or migration DDL that runs once per boot
//   under the schema-init lock (or is otherwise idempotent:
//   IF NOT EXISTS, duplicate-tolerant, detection-gated). Needs a
//   transaction for multi-statement safety, not a statement budget.
// - reasonMigrationOnce: a one-shot data migration, backfill, repair,
//   seeding or sentinel insert. Idempotent or detection-gated, so a
//   retry or a rerun is safe.
// - reasonBestEffort: deliberately non-critical — diagnostics,
//   telemetry, retention sweeps, capability probes. Carries no
//   statement budget by design; persistent failure is logged, but it
//   never blocks the caller.
// - reasonTestFixture: test helper database. Not production traffic.
// - reasonSharedPrimitive: the write runs through a SHARED primitive —
//   one function whose executor is a pool on one call site and a
//   transaction on another — so that the semantics cannot drift apart.
//   The pool call site passes a BUDGETED executor (poolMemExecer), so
//   the write is not bare there; the transactional call site passes the
//   transaction, whose retry policy belongs to the transaction. The
//   scanner is syntactic and cannot see which executor a call site
//   passes, so it keeps flagging the shared `ex.Exec` site. The site
//   therefore STAYS in this list rather than being deleted: a NEW bare
//   write through the same primitive must still trip the surplus check.
//   The budget on the pool path is pinned by
//   TestSharedPrimitivePoolPathsCarryAStatementLevelBudget, which is a
//   stronger guarantee than this list can give.
// - reasonConnectionScoped: the write is scoped to ONE CONNECTION, not
//   to the database. SQLite's ATTACH is per-connection: the schema it
//   attaches is visible only to the connection that attached it, and
//   only that connection. The site
//   (internal/core/similarity_candidates_store.go:972) therefore runs
//   on a PINNED `*sql.Conn` — one connection drawn from the pool and
//   held for the length of the operation.
//
//   This reason is NOT "startup-ddl" and it is NOT "propagated",
//   because neither describes what the site is:
//
//   - It is not startup DDL. The attachment is not made once per boot
//     on one connection and left there; it is made per CONNECTION, on
//     whichever connection the caller pinned, and the pool is
//     deliberately multi-connection.
//   - A retry budget on the pool would be WRONG, not merely redundant.
//     The substrate's budget is spent by `execTracked`, which writes
//     through the `*sql.DB` — so a retried ATTACH can land on a
//     DIFFERENT pooled connection than the one that holds the
//     attachment the caller is about to use. A retried ATTACH on the
//     SAME connection, meanwhile, fails with "database shared is
//     already in use" once the attachment is in place. Neither is the
//     behaviour the site needs.
//   - The site's own guard is the `PRAGMA database_list` check that
//     precedes it on the same connection: the attachment is made only
//     if it is missing, so the operation is idempotent per connection
//     WITHOUT a retry.
//
//   The failure is reported to the caller as ErrSharedUnavailable, and
//   the caller treats a shared source that cannot be attached as a
//   broken installation rather than silently scanning half the corpus.
//
//   The remedy for a future connection-scoped write is a per-connection
//   retry that keeps the SAME connection, not a pool budget. No such
//   helper exists today, and the site does not need one: the ATTACH is
//   preceded by a lock-free existence check.
//
// NON-VACUITY
// -----------
// TestBarePoolScannerFlagsRepresentativeShapes feeds the classifier
// synthetic sources and asserts each bare shape is caught — including
// a deliberately reintroduced defect — and that the guarded and
// transaction-owned shapes are not. TestBarePoolAllowlistDetectsReintroducedWrite
// goes one step further: it takes the exact pre-fix shape of the
// handoff UPSERT (the F2 defect this issue closed) and asserts the
// allowlist does NOT contain it, so reintroducing that call fails the
// guard. TestBarePoolMultisetIdentitiesDetected pins the multiplicity
// rule on synthetic multisets: a second identical bare Exec where the
// allowlist records Count 1 reports exactly one surplus occurrence at
// its line, and a removed write reports as stale with its recorded
// count. The controls are synthetic on purpose: a mutation of the real
// tree proves the guard fires once (verified live in both directions
// during development), whereas the synthetic controls prove it fires
// on the SHAPE, and keep proving it as the tree changes.
//
// TestBarePoolScannerFlagsRepresentativeShapes carries the
// ATTACH/DETACH/REINDEX shapes added for review finding M1, and
// TestBarePoolAllowlistDetectsUnapprovedAttach pins the M1 site
// itself: the exact shape of the connection-scoped ATTACH must be
// detected by the classifier AND must not be allowlisted under any
// OTHER file, so a NEW unapproved ATTACH anywhere in the tree fails
// the completeness guard by shape rather than by luck.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// bareWriteVerb matches a statement that writes, DML or DDL. DDL is
// included deliberately: a multi-statement DDL Exec may have applied
// part of itself, so a retry can compound the damage and DDL wants a
// transaction instead — but a NEW bare DDL site must still trip review
// rather than land silently.
//
// PRAGMA and VACUUM were added on 2026-11-06 (issue #178, first
// remediation slice). They were a DETECTOR GAP: a bare
// `db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")` or `db.Exec("VACUUM INTO ?")`
// was invisible to this guard, because the verb list stopped at the DML
// and DDL verbs. A detector that cannot see a write shape cannot keep
// that shape honest, and the shape is one line away from every future
// caller.
//
// The two verbs are here for the same reason the DDL verbs are: a bare
// pool PRAGMA/VACUUM has no statement-level retry budget, so under
// contention it is lost the same way a bare INSERT is. Most of them are
// startup connection configuration or best-effort maintenance, and they
// are recorded in the allowlist with those reasons — the point is that
// they are REVIEWED, not that they are all converted.
//
// ATTACH, DETACH and REINDEX were added on 2026-11-06 (issue #178,
// independent review finding M1). The comment that preceded them
// claimed the tree contains no bare pool write of any of them. THAT
// CLAIM WAS FALSE. `internal/core/similarity_candidates_store.go:972`
// issues `ATTACH DATABASE '%s' AS shared` through a pinned `*sql.Conn`
// drawn from the substrate's pool, and this scanner's receiver rule
// counts every identifier other than `tx` as a pool receiver — so the
// site was invisible ONLY because the verb was missing.
//
// Both halves of the old rationale were wrong in opposite directions.
// "A verb in this list that no site can ever match is a claim the
// guard cannot back" is true of a verb nothing can match — but the fix
// is to make the list COMPLETE, not to leave a hole and then assert
// the hole is empty. A verb that a site CAN match and the list omits
// is a hole in the guard, and the hole is invisible precisely because
// the detector cannot see it.
//
// So this list is about DETECTION CAPABILITY, not about inventory: it
// names every write shape the detector can see, and the allowlist
// below is what says which of those shapes exist today.
//
// What exists today, exactly: ONE bare ATTACH
// (internal/core/similarity_candidates_store.go:972), and NO bare
// DETACH and NO bare REINDEX. The ATTACH is recorded below with the
// reason `connection-scoped`, because its semantics are per-connection
// and a pool retry budget would MISPLACE it — see THE REASONS above.
var bareWriteVerb = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|REPLACE|CREATE|DROP|ALTER|PRAGMA|VACUUM|ATTACH|DETACH|REINDEX)\b`)

// barePoolReason is the closed vocabulary of justifications for a
// bare pool write. See THE REASONS above.
type barePoolReason string

const (
	reasonPropagated    barePoolReason = "propagated"
	reasonStartupDDL    barePoolReason = "startup-ddl"
	reasonMigrationOnce barePoolReason = "migration-once"
	reasonBestEffort    barePoolReason = "best-effort"
	reasonTestFixture   barePoolReason = "test-fixture"
	// reasonSharedPrimitive is documented in THE REASONS above.
	reasonSharedPrimitive barePoolReason = "shared-primitive"
	// reasonConnectionScoped is documented in THE REASONS above.
	reasonConnectionScoped barePoolReason = "connection-scoped"
)

// barePoolEntry is one bare pool write the guard knows about: the file
// it lives in, the normalized shape of its SQL, and why it is allowed
// to stay bare.
//
// The SQL is compared in normalizeSQL form (whitespace collapsed), so
// reindentation does not invalidate the entry but any change to what
// the statement DOES does.
type barePoolEntry struct {
	File string
	SQL  string
	Why  barePoolReason
	// Count is the expected number of live call sites with this exact
	// identity. Most identities occur once; a few statements are
	// legitimately duplicated (identical SQL in the same file), and
	// their Count records that. Inserting another copy is surplus;
	// removing one (by routing it through ExecTracked or into a
	// transaction) makes the entry stale until it is decremented.
	Count int
	// Func is the name of the function the call sits in.
	//
	// It is REQUIRED for the `migration-once` class and optional
	// elsewhere. The reason is in THE REASONS above: `migration-once`
	// is a claim about WHEN a write runs — once, on a migration path,
	// never on a user path — and that claim is only checkable if it
	// names the place the write lives. Without the name, the claim is
	// unfalsifiable, and a reviewer filing an ordinary user-state
	// write under this class is invisible to every mechanical guard,
	// because classification is human judgement.
	//
	// The second remediation slice (issue #178) found exactly that: three
	// ordinary contended writes — a session summary, a topic membership
	// insert and a topic membership delete — were filed as
	// `migration-once`, and the misfiling was what hid them from the
	// ordinary-write accounting. Naming the function, and checking the
	// name against a closed migration vocabulary, is what makes the
	// class falsifiable.
	Func string
}

// barePoolAllowlist is the complete inventory of bare pool writes on
// the integrated tree, recorded after the phase-2 corrections landed.
// Removing a site (budget, transaction, deletion) deletes its entry.
// Adding a site fails the guard until it is recorded here with a
// reason — which is the review.
var barePoolAllowlist = []barePoolEntry{
	{File: "cmd/mpm/backfill_embeddings.go",
		SQL:   "UPDATE memories SET embedding = ?, embedding_source = ?, embedding_dimension = ?, embedding_model = ? WHERE id = ?",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "updateMemoryEmbedding"},
	{File: "cmd/mpm/backfill_embeddings.go",
		SQL:   "UPDATE reference_chunks SET embedding = NULL, embedding_source = 'null', embedding_dimension = NULL, embedding_model = NULL",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "runReferenceBackfill"},
	{File: "cmd/mpm/handlers_backup.go",
		SQL:   "PRAGMA wal_checkpoint(TRUNCATE)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/handlers_gc.go",
		SQL:   "DELETE FROM memories WHERE deleted_at IS NOT NULL AND deleted_at < strftime('%s','now', '-30 days') AND",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/handlers_gc.go",
		SQL:   "INSERT INTO system_config (key, raw_json, content_hash) VALUES ('last_gc_at', ?, '') ON CONFLICT(key) DO UPDATE SET raw_json = excluded.raw_json, updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE ( system_config.raw_json IS NULL OR json_extract(system_config.raw_json, '$.updated_at') IS NULL OR datetime(json_extract(system_config.raw_json, '$.updated_at')) < datetime('now', '-' || ? || ' hours') )",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/handlers_restore.go",
		SQL:   "UPDATE memories SET deleted_at = NULL, weight = MAX(?, 1) WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/handlers_topic.go",
		SQL:   "INSERT INTO topics (id, name, description) VALUES (?, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/ingest.go",
		SQL:   "DELETE FROM raw_memories WHERE import_batch = ? AND status IN ('pending', 'approved', 'reviewing')",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/ingest.go",
		SQL:   "UPDATE raw_memories SET status = 'expired' WHERE status = 'pending' AND expires_at > 0 AND expires_at < ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/main.go",
		SQL:   "DELETE FROM memories_fts WHERE rowid IN (SELECT rowid FROM memories WHERE deleted_at IS NOT NULL)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/maint_cmds.go",
		SQL:   "PRAGMA incremental_vacuum",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "cmd/mpm/self_heal.go",
		SQL:   "INSERT INTO memories (id, collection, content, metadata, created_at, updated_at) VALUES (?, 'projects', ?, ?, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER)) ON CONFLICT(id) DO UPDATE SET metadata = excluded.metadata, updated_at = CAST(strftime('%s','now') AS INTEGER)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/simple_cmds.go",
		SQL:   "UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/simple_cmds.go",
		SQL:   "UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ? AND deleted_at IS NULL AND collection != 'lessons'",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/simple_cmds.go",
		SQL:   "UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "cmd/mpm/simple_cmds.go",
		SQL:   "UPDATE memories SET weight = MIN(weight + 1, 9), last_accessed_at = CAST(strftime('%s','now', '+' || ? || ' seconds') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/blobstore/fs.go",
		SQL:   "DELETE FROM blobs WHERE expires_at < ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/blobstore/fs.go",
		SQL:   "DELETE FROM blobs WHERE id = ?",
		Why:   reasonPropagated,
		Count: 2},
	{File: "internal/blobstore/fs.go",
		SQL:   "INSERT INTO blobs (id, source_tool, source_call_id, session_id, size_bytes, content_type, created_at, expires_at, checksum) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/active_uptime.go",
		SQL:   "INSERT INTO memory_settling_baselines (memory_id, baseline_active_seconds, captured_at) SELECT m.id, ?, CAST(strftime('%s','now') AS INTEGER) FROM memories m WHERE m.deleted_at IS NULL AND NOT EXISTS ( SELECT 1 FROM memory_settling_baselines b WHERE b.memory_id = m.id ) ORDER BY m.id LIMIT ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/admission_db.go",
		SQL:   "INSERT INTO admission_log ( id, doc_id, chunk_id, admit, content, confidence, reason, justification, admission_model, created_at ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/arbitration.go",
		SQL:   "UPDATE shared.memories SET reinforcement_count = COALESCE(reinforcement_count, 0) + ?, weight = COALESCE(weight, 1) + ?, last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/audit.go",
		SQL:   "DELETE FROM system_audit_log WHERE created_at < CAST(strftime('%s','now', ?) AS INTEGER)",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/audit.go",
		SQL:   "INSERT INTO audit_cluster_proposals (cluster_key, component, message_hash, count, first_seen, last_seen) VALUES (?, ?, ?, 1, ?, ?) ON CONFLICT(cluster_key) DO UPDATE SET count = CASE WHEN audit_cluster_proposals.last_seen < CAST(strftime('%s','now', ?) AS INTEGER) THEN 1 ELSE audit_cluster_proposals.count + 1 END, first_seen = CASE WHEN audit_cluster_proposals.last_seen < CAST(strftime('%s','now', ?) AS INTEGER) THEN excluded.last_seen ELSE audit_cluster_proposals.first_seen END, last_seen = excluded.last_seen, updated_at = CAST(strftime('%s','now') AS INTEGER), status = CASE WHEN audit_cluster_proposals.status = 'snoozed' AND audit_cluster_proposals.snooze_until < CAST(strftime('%s','now') AS INTEGER) THEN 'active' ELSE audit_cluster_proposals.status END",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/audit.go",
		SQL:   "INSERT INTO system_audit_log (id, level, component, message, stack_trace, context, created_at) VALUES (?, 'info', 'skill_workshop', ?, ?, ?, CAST(strftime('%s','now') AS INTEGER))",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/cascade_outbox.go",
		SQL:   "DELETE FROM epistemic_cascade_outbox WHERE status IN ('materialized', 'failed') AND updated_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "PruneCascadeOutbox"},
	{File: "internal/core/cascade_wake_reconcile.go",
		SQL:   "UPDATE epistemic_cascade_outbox SET wake_scheduled = 1, updated_at = ? WHERE id = ? AND wake_scheduled = 0",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/cluster_proposals.go",
		SQL:   "UPDATE audit_cluster_proposals SET status = ?, snooze_until = ? WHERE cluster_key = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/cluster_proposals.go",
		SQL:   "UPDATE audit_cluster_proposals SET status = ?, snooze_until = NULL WHERE cluster_key = ?",
		Why:   reasonPropagated,
		Count: 2},
	{File: "internal/core/compact.go",
		SQL:   "INSERT INTO system_config (key, raw_json, content_hash) VALUES ('compaction.last_run', ?, '') ON CONFLICT(key) DO UPDATE SET raw_json = excluded.raw_json, updated_at = CAST(strftime('%s','now') AS INTEGER)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/contradiction_log.go",
		SQL:   "INSERT OR IGNORE INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/contradiction_verification.go",
		SQL:   "UPDATE shared.contradiction_log SET detected_by = ? WHERE id = ? AND resolved_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE TABLE IF NOT EXISTS work_events ( id TEXT PRIMARY KEY, work_id TEXT NOT NULL, event_index INTEGER NOT NULL, event_type TEXT NOT NULL CHECK (event_type IN ( 'created','note_appended','completed', 'cancelled','reopened', 'title_updated','content_updated', 'claimed_complete','evidence_observed', 'archived','unarchived' )), created_at INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)), invocation_id TEXT, parent_invocation_id TEXT, note TEXT, title TEXT, content TEXT, directive_ids TEXT DEFAULT '[]', UNIQUE(work_id, event_index) );",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE TRIGGER lessons_instead_of_delete INSTEAD OF DELETE ON lessons BEGIN DELETE FROM lessons_base WHERE rowid=OLD.rowid; DELETE FROM lessons_fts WHERE rowid=OLD.rowid; END",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE TRIGGER lessons_instead_of_insert INSTEAD OF INSERT ON lessons BEGIN INSERT INTO lessons_base(rowid, id, type, content, tags, reinforcement_count, source_session_id, created, content_hash, retrieval_priority, importance, confidence) VALUES (NEW.rowid, NEW.id, NEW.type, NEW.content, NEW.tags, COALESCE(NEW.reinforcement_count, 1), NEW.source_session_id, NEW.created, NEW.content_hash, COALESCE(NEW.retrieval_priority, 0.5), COALESCE(NEW.importance, 0.5), COALESCE(NEW.confidence, 0.7)); INSERT INTO lessons_fts(rowid, content, tags) VALUES (NEW.rowid, NEW.content, NEW.tags); END",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE TRIGGER lessons_instead_of_update INSTEAD OF UPDATE ON lessons BEGIN UPDATE lessons_base SET rowid=NEW.rowid, id=NEW.id, type=NEW.type, content=NEW.content, tags=NEW.tags, reinforcement_count=COALESCE(NEW.reinforcement_count, reinforcement_count), source_session_id=NEW.source_session_id, created=NEW.created, content_hash=NEW.content_hash, retrieval_priority=COALESCE(NEW.retrieval_priority, retrieval_priority), importance=COALESCE(NEW.importance, importance), confidence=COALESCE(NEW.confidence, confidence) WHERE rowid=OLD.rowid; DELETE FROM lessons_fts WHERE rowid=OLD.rowid; INSERT INTO lessons_fts(rowid, content, tags) VALUES (NEW.rowid, NEW.content, NEW.tags); END",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE VIEW IF NOT EXISTS lessons AS SELECT rowid, id, type, content, tags, reinforcement_count, source_session_id, created, content_hash, retrieval_priority, importance, confidence FROM lessons_base",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter unicode61');",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "CREATE VIRTUAL TABLE test_fts USING fts5(content, tokenize='porter unicode61');",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "DELETE FROM lessons_base WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "DELETE FROM lessons_fts WHERE rowid IN (SELECT rowid FROM lessons_base WHERE id = ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "DELETE FROM system_config WHERE key = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "INSERT INTO shared.memories (id, collection, content, tags, metadata, weight, is_global, created_at, updated_at, deleted_at) VALUES (?, 'rules', ?, ?, ?, ?, 1, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER), NULL)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "INSERT INTO shared.memories (id, collection, content, tags, metadata, weight, reinforcement_count, is_global, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, 1, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER), NULL)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "INSERT INTO topics (id, name, description, created_at, tags, is_active, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "INSERT OR IGNORE INTO shared.memories_fts(rowid, content, collection, session_id, tags) SELECT rowid, content, COALESCE(collection, ''), COALESCE(session_id, ''), COALESCE(tags, '') FROM shared.memories WHERE is_global = 1 AND deleted_at IS NULL",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "backfillSharedFTSIfEmpty"},
	{File: "internal/core/db.go",
		SQL:   "INSERT OR IGNORE INTO work_events (id, work_id, event_index, event_type, created_at, invocation_id, note, title, content, directive_ids) SELECT lower(hex(randomblob(16))), id, 0, 'created', created_at, NULL, '', title, content, '[]' FROM works WHERE migrated_at IS NULL AND NOT EXISTS (SELECT 1 FROM work_events WHERE work_events.work_id = works.id AND work_events.event_index = 0);",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "migrateWorkEvents"},
	{File: "internal/core/db.go",
		SQL:   "INSERT OR REPLACE INTO system_config (key, raw_json, content_hash, updated_at, config_snapshot) VALUES (?, ?, ?, CAST(strftime('%s','now') AS INTEGER), ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "PRAGMA busy_timeout = 5000",
		Why:   reasonStartupDDL,
		Count: 3},
	{File: "internal/core/db.go",
		SQL:   "PRAGMA cache_size = -64000",
		Why:   reasonStartupDDL,
		Count: 2},
	{File: "internal/core/db.go",
		SQL:   "PRAGMA foreign_keys = ON",
		Why:   reasonStartupDDL,
		Count: 3},
	{File: "internal/core/db.go",
		SQL:   "PRAGMA journal_mode = WAL",
		Why:   reasonStartupDDL,
		Count: 4},
	{File: "internal/core/db.go",
		SQL:   "PRAGMA synchronous = NORMAL",
		Why:   reasonStartupDDL,
		Count: 3},
	{File: "internal/core/db.go",
		SQL:   "PRAGMA wal_checkpoint(TRUNCATE)",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "UPDATE lessons_base SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "UPDATE lessons_base SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "UPDATE memories SET weight = MAX(weight - MAX(1, CAST(weight * ? * COALESCE( CASE json_extract(metadata, '$.provenance.compute') WHEN 'absolute' THEN 0.0 WHEN 'high' THEN 0.5 WHEN 'ephemeral' THEN 2.0 ELSE 1.0 END, 1.0) AS INTEGER)), 1), updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE is_long_term = 1 AND deleted_at IS NULL AND collection = ? AND updated_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER) AND weight > 1",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "UPDATE shared.memories SET retired_at = ? WHERE id = ? AND is_global = 1 AND deleted_at IS NULL AND retired_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "UPDATE works SET migrated_at = CAST(strftime('%s','now') AS INTEGER) WHERE migrated_at IS NULL",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "migrateWorkEvents"},
	{File: "internal/core/db.go",
		SQL:   "UPDATE works SET status = ?, title = ?, content = ?, updated_at = ?, completed_at = ?, archived_at = ? WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/db.go",
		SQL:   "VACUUM",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/embedding_migration.go",
		SQL:   "INSERT INTO embedding_migration_log (memory_id, old_weight, new_weight, reason, migrated_at) VALUES ('sentinel', 0, 0, 'migration_applied', CAST(strftime('%s','now') AS INTEGER))",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "recordSentinel"},
	{File: "internal/core/embedding_migration.go",
		SQL:   "UPDATE memories SET synthetic = 1 WHERE synthetic = 0 AND collection = 'theories' AND deleted_at IS NULL AND json_extract(metadata, '$.challenged_memory_id') IN ( SELECT id FROM memories WHERE embedding_source = 'hash' AND deleted_at IS NULL )",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "markSyntheticTheories"},
	{File: "internal/core/embedding_migration.go",
		SQL:   "VACUUM INTO ?",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "takeBackup"},
	{File: "internal/core/epistemology_tools.go",
		SQL:   "UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/fts_recovery.go",
		SQL:   "CREATE VIRTUAL TABLE %s USING fts5(%s, tokenize='porter unicode61')",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/fts_recovery.go",
		SQL:   "DROP TABLE IF EXISTS",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/fts_recovery.go",
		SQL:   "DROP TRIGGER IF EXISTS",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/gc_tools.go",
		SQL:   "INSERT INTO system_config (key, raw_json, content_hash) VALUES ('last_gc_at', ?, '') ON CONFLICT(key) DO UPDATE SET raw_json = excluded.raw_json, updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE ( system_config.raw_json IS NULL OR json_extract(system_config.raw_json, '$.updated_at') IS NULL OR datetime(json_extract(system_config.raw_json, '$.updated_at')) < datetime('now', '-' || ? || ' hours') )",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/gc_tools.go",
		SQL:   "UPDATE memories SET runtime_seconds_since_access = CASE WHEN runtime_last_accrued_at IS NULL OR runtime_last_accrued_at <= 0 THEN CASE WHEN last_accessed_at IS NOT NULL AND last_accessed_at > 0 THEN MAX(0, CAST(strftime('%s','now') AS INTEGER) - last_accessed_at) WHEN created_at IS NOT NULL AND created_at > 0 THEN MAX(0, CAST(strftime('%s','now') AS INTEGER) - created_at) ELSE 0 END ELSE runtime_seconds_since_access + MIN( CAST(strftime('%s','now') AS INTEGER) - runtime_last_accrued_at, CAST(? AS INTEGER) ) END, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER) WHERE deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/handoff.go",
		SQL:   "DELETE FROM session_handoffs WHERE created_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/ingest.go",
		SQL:   "INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at, status, import_batch, updated_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/ingest.go",
		SQL:   "INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at, status, llm_verdict, llm_notes, import_batch, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'rejected', 'toxic', ?, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/ingest.go",
		SQL:   "UPDATE raw_memories SET status = 'pending', updated_at = ? WHERE status = 'reviewing' AND updated_at < ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/lesson_proposals.go",
		SQL:   "INSERT INTO lesson_proposals (id, type, content, tags, created, status, source_session_id, framework_name, model_name, source_ids) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/lesson_proposals.go",
		SQL:   "UPDATE lesson_proposals SET status = ?, promoted_lesson_id = ?, reviewed_at = ? WHERE id = ? AND status = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/lesson_proposals.go",
		SQL:   "UPDATE lesson_proposals SET status = ?, reviewed_at = ? WHERE id = ? AND status = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/lessons_fts_sync.go",
		SQL:   "INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, CAST(strftime('%s','now') AS INTEGER))",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "recordLessonsFTSSentinel"},
	{File: "internal/core/memory.go",
		SQL:   "ALTER TABLE %s ADD COLUMN %s %s",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/core/memory.go",
		SQL:   "UPDATE memories SET collection = collection || '_inactive', metadata = JSON_SET(COALESCE(metadata, '{}'), '$.is_deleted', true)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/memory.go",
		SQL:   "UPDATE memories SET collection = collection || '_inactive', metadata = JSON_SET(COALESCE(metadata, '{}'), '$.is_deleted', true) WHERE collection = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/memory.go",
		SQL:   "UPDATE memories SET content_hash = lower(hex(sha256(content))) WHERE content_hash IS NULL OR content_hash = ''",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "BackfillContentHash"},
	{File: "internal/core/memory.go",
		SQL:   "UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE expires_at IS NOT NULL AND expires_at < strftime('%s','now') AND",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/memory.go",
		SQL:   "UPDATE memories SET is_long_term = 1, weight = 10 WHERE weight = 1 AND is_long_term = 0 AND json_extract(metadata, '$.is_long_term') = 'true'",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "BackfillLTMFlags"},
	{File: "internal/core/memory.go",
		SQL:   "UPDATE topics SET is_active = 0, updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/memory_tools.go",
		SQL:   "DELETE FROM lessons WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/migrate_md.go",
		SQL:   "UPDATE raw_memories SET status='approved', memory_id=?, updated_at=? WHERE id=?",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "PromoteRawMemoryBatch"},
	{File: "internal/core/migration_memories_affinity_rebuild.go",
		SQL:   "PRAGMA foreign_keys = OFF",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "RebuildMemoriesColumnAffinity"},
	{File: "internal/core/migration_memories_affinity_rebuild.go",
		SQL:   "PRAGMA foreign_keys = ON",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "RebuildMemoriesColumnAffinity"},
	{File: "internal/core/migration_memories_created_at_not_null.go",
		SQL:   "PRAGMA foreign_keys = OFF",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "EnforceMemoriesCreatedAtNotNull"},
	{File: "internal/core/migration_memories_created_at_not_null.go",
		SQL:   "PRAGMA foreign_keys = ON",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "EnforceMemoriesCreatedAtNotNull"},
	{File: "internal/core/reference_db.go",
		SQL:   "UPDATE reference_chunks SET embedding = ?, embedding_source = 'provider', embedding_dimension = ?, embedding_model = ? WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/reference_db.go",
		SQL:   "UPDATE reference_chunks SET embedding = ?, embedding_source = ?, embedding_dimension = ?, embedding_model = ? WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/reference_ingest.go",
		SQL:   "UPDATE reference_docs SET title = ?, tags = ?, import_reason = ?, file_path = ?, source_kind = COALESCE(NULLIF(?, ''), source_kind), source_url = COALESCE(NULLIF(?, ''), source_url), requested_url = COALESCE(NULLIF(?, ''), requested_url), fetched_at = COALESCE(NULLIF(?, 0), fetched_at), http_etag = COALESCE(NULLIF(?, ''), http_etag), http_last_modified = COALESCE(NULLIF(?, ''), http_last_modified), last_indexed = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/retrieval_metadata.go",
		SQL:   "INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_count) VALUES (?, ?, 0, NULL, 1) ON CONFLICT(node_id) DO UPDATE SET success_count = retrieval_metadata.success_count + 1, updated_at = CAST(strftime('%s','now') AS INTEGER)",
		Why:   reasonPropagated,
		Count: 2},
	{File: "internal/core/retrieval_metadata.go",
		SQL:   "INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_count) VALUES (?, ?, 1, CAST(strftime('%s','now') AS INTEGER), 0) ON CONFLICT(node_id) DO UPDATE SET reuse_count = retrieval_metadata.reuse_count + 1, last_retrieved_at = CAST(strftime('%s','now') AS INTEGER), updated_at = CAST(strftime('%s','now') AS INTEGER)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/review.go",
		SQL:   "INSERT INTO memory_reviews (memory_id, reviewed_at, decision, note) VALUES (?, ?, ?, ?) ON CONFLICT(memory_id) DO UPDATE SET reviewed_at = excluded.reviewed_at, decision = excluded.decision, note = excluded.note",
		Why:   reasonSharedPrimitive,
		Count: 1},
	{File: "internal/core/scheduled_tasks.go",
		SQL:   "DELETE FROM scheduled_tasks WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/scheduled_tasks.go",
		SQL:   "INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, cron_expr = excluded.cron_expr, directive_id = excluded.directive_id, status = excluded.status, next_run_at = excluded.next_run_at, updated_at = excluded.updated_at",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/seed/engine.go",
		SQL:   "INSERT OR IGNORE INTO memories (id, collection, content, tags, metadata, is_prime_directive, weight, confidence, retrieval_priority, importance, created_at, updated_at) VALUES (?, 'directives', ?, ?, ?, 1, 10, 1.0, 1.0, 1.0, ?, ?)",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "insertSeedRow"},
	{File: "internal/core/seed/engine.go",
		SQL:   "UPDATE memories SET deleted_at = NULL, content = ?, tags = ?, metadata = ?, is_prime_directive = 1, collection = 'directives' WHERE id = ? AND deleted_at IS NOT NULL AND deleted_at > 0",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "reviveSeedRow"},
	{File: "internal/core/similarity_candidates_store.go",
		SQL:   "ATTACH DATABASE '%s' AS shared",
		Why:   reasonConnectionScoped,
		Count: 1},
	{File: "internal/core/similarity_candidates_store.go",
		SQL:   "INSERT OR IGNORE INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/skill_db.go",
		SQL:   "UPDATE memories SET content = ?, tags = ?, metadata = ?, is_long_term = 1, deleted_at = NULL, updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/skill_db.go",
		SQL:   "UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND collection = 'skills' AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/skill_db.go",
		SQL:   "UPDATE memories SET is_global = 1, metadata = json_set(COALESCE(metadata, '{}'), '$.derived_from_skill_id', ?, '$.promoted_at', strftime('%s','now')) WHERE id = ? AND collection = 'skills' AND deleted_at IS NULL",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/testhelpers.go",
		SQL:   "CREATE TABLE memories ( id TEXT PRIMARY KEY, collection TEXT, content TEXT, session_id TEXT, tags TEXT, metadata TEXT, embedding BLOB, weight REAL, confidence REAL, retrieval_priority REAL, importance REAL, created_at INTEGER, updated_at INTEGER, deleted_at INTEGER, reference_id TEXT, content_hash TEXT, is_prime_directive INTEGER, toxicity_score REAL, is_long_term INTEGER, reinforcement_count INTEGER, last_accessed_at INTEGER, expires_at INTEGER, source_db TEXT, source_id TEXT, promoted_at REAL, is_global INTEGER ); CREATE TABLE schema_migrations ( id TEXT PRIMARY KEY, applied_at INTEGER NOT NULL );",
		Why:   reasonTestFixture,
		Count: 1},
	{File: "internal/core/tools/handlers.go",
		SQL:   "UPDATE raw_memories SET status='rejected', llm_notes='undo by mcp tool', updated_at=? WHERE import_batch=? AND status IN ('pending','approved')",
		Why:   reasonMigrationOnce,
		Count: 1,
		Func:  "handleMigrate"},
	{File: "internal/core/vector_index.go",
		SQL:   "DELETE FROM WHERE memory_id = ?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/vector_index.go",
		SQL:   "INSERT OR REPLACE INTO (memory_id, cluster_id, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/wake_tools.go",
		SQL:   "INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, fired, created_by, metadata) VALUES (?, ?, ?, NULL, 0, ?, ?)",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/core/web_db.go",
		SQL:   "DELETE FROM memories WHERE AND last_accessed_at IS NULL AND reinforcement_count = 0 AND weight = 1 AND is_long_term = 0 AND deleted_at IS NULL",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/web_db.go",
		SQL:   "DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/web_db.go",
		SQL:   "DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < strftime('%s','now') AND",
		Why:   reasonBestEffort,
		Count: 1},
	{File: "internal/core/web_db.go",
		SQL:   "UPDATE memories SET reinforcement_count = MAX(reinforcement_count - ?, 0), weight = MAX(weight - ?, 1), last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL",
		Why:   reasonSharedPrimitive,
		Count: 1},
	{File: "internal/scheduler/drill_handler.go",
		SQL:   "UPDATE drill_runs SET status='error', error_message=?, completed_at=? WHERE id=?",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/telemetry/store.go",
		SQL:   "CREATE TABLE IF NOT EXISTS telemetry_invocation ( invocation_id TEXT PRIMARY KEY, parent_invocation_id TEXT, session_id TEXT, framework TEXT NOT NULL, framework_version TEXT, provider TEXT NOT NULL, model TEXT NOT NULL, model_revision TEXT, started_at INTEGER NOT NULL, completed_at INTEGER NOT NULL, received_at INTEGER NOT NULL, status TEXT NOT NULL, stop_reason TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER, duration_ms INTEGER, provider_metadata TEXT NOT NULL DEFAULT '{}', schema_version TEXT NOT NULL ); CREATE INDEX IF NOT EXISTS idx_tel_session ON telemetry_invocation(session_id); CREATE INDEX IF NOT EXISTS idx_tel_parent_inv ON telemetry_invocation(parent_invocation_id); CREATE INDEX IF NOT EXISTS idx_tel_started_at ON telemetry_invocation(started_at); CREATE INDEX IF NOT EXISTS idx_tel_framework ON telemetry_invocation(framework);",
		Why:   reasonStartupDDL,
		Count: 1},
	{File: "internal/update/backup.go",
		SQL:   "VACUUM INTO '%s'",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/update/health.go",
		SQL:   "VACUUM INTO '%s'",
		Why:   reasonPropagated,
		Count: 1},
	{File: "internal/update/preflight.go",
		SQL:   "VACUUM INTO '%s'",
		Why:   reasonPropagated,
		Count: 1},
}

// barePoolSite is one direct Exec call the ratchet tracks.
type barePoolSite struct {
	File string
	Line int
	SQL  string
	// Func is the name of the function the call sits in ("" when the
	// call is not inside a named function declaration — a package-level
	// var initializer, say).
	//
	// It is what makes the `migration-once` classification checkable:
	// that class claims the write runs ONCE, on a migration path, and
	// never on a user path. The enclosing function's name is the
	// observable proxy for that claim, so the guard can compare the
	// claim against the code.
	Func string
}

// funcDeclRange is one function declaration's name and source range.
type funcDeclRange struct {
	name  string
	start token.Pos
	end   token.Pos
}

// funcDeclRanges returns every function declaration in file, with its
// name and source range.
//
// The name is the bare function or method name ("migrateWorkEvents",
// "handleMigrate"). The receiver type is deliberately NOT part of it:
// the vocabulary the migration-once guard checks is a vocabulary of
// VERBS, and a verb reads the same on a method as on a function.
func funcDeclRanges(file *ast.File) []funcDeclRange {
	var out []funcDeclRange
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		out = append(out, funcDeclRange{
			name:  fn.Name.Name,
			start: fn.Pos(),
			end:   fn.End(),
		})
	}
	return out
}

// enclosingFunc returns the name of the function declaration that
// contains pos, or "" when pos is not inside one.
func enclosingFunc(ranges []funcDeclRange, pos token.Pos) string {
	for _, r := range ranges {
		if r.start <= pos && pos <= r.end {
			return r.name
		}
	}
	return ""
}

// bareWriteMethod returns the argument index at which the SQL lives for
// a write method, and whether the method is a write method at all.
//
// The index is the whole point of this function: `Exec(query, args...)`
// puts the query first, `ExecContext(ctx, query, args...)` puts the
// CONTEXT first. A scanner that assumes one position for both misses
// every site of the other shape.
//
// A method that takes a context but whose SQL is not resolvable from a
// fixed position is not silently ignored: the caller resolves the SQL
// and, if it cannot, the site is simply not counted — the same
// fail-open the `Exec` path has always had for a variable query.
func bareWriteMethod(name string) (sqlArgIndex int, ok bool) {
	switch name {
	case "Exec":
		return 0, true
	case "ExecContext":
		return 1, true
	}
	return 0, false
}

// isPoolReceiver reports whether the receiver of an Exec call is a
// pool handle rather than a transaction handle.
func isPoolReceiver(x ast.Expr) bool {
	switch r := x.(type) {
	case *ast.SelectorExpr:
		// dm.db, cm.dm.db, s.DB, sc.DB — the pool behind the manager.
		return r.Sel.Name == "db" || r.Sel.Name == "DB"
	case *ast.CallExpr:
		// dm.SQLDB() — the raw pool. node.Tx() is transaction-owned.
		if sel, ok := r.Fun.(*ast.SelectorExpr); ok {
			return sel.Sel.Name == "SQLDB"
		}
		return true
	case *ast.Ident:
		// tx is the transaction handle by universal convention in
		// this tree; everything else holding an Exec method and a
		// write-verb SQL is treated as a pool write. This direction
		// is deliberate: an unknown receiver fails closed (counted)
		// rather than open (missed).
		return r.Name != "tx"
	}
	return true
}

// withTxRanges returns the source ranges of every function literal
// passed directly to a WithTx call: statements inside them are
// transaction-owned, not bare pool writes.
func withTxRanges(fset *token.FileSet, file *ast.File) [][2]token.Pos {
	var out [][2]token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithTx" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				out = append(out, [2]token.Pos{lit.Pos(), lit.End()})
			}
		}
		return true
	})
	_ = fset
	return out
}

// scanBarePoolSource records every bare pool write in one file.
func scanBarePoolSource(rel string, src []byte) ([]barePoolSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	scope := literalScope(file.Decls)
	txRanges := withTxRanges(fset, file)
	funcRanges := funcDeclRanges(file)

	inTx := func(pos token.Pos) bool {
		for _, r := range txRanges {
			if r[0] <= pos && pos <= r[1] {
				return true
			}
		}
		return false
	}

	var sites []barePoolSite
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		sqlIdx, isWriteMethod := bareWriteMethod(sel.Sel.Name)
		if !isWriteMethod {
			return true
		}
		if len(call.Args) <= sqlIdx {
			return true
		}
		recv, ok := receiverOf(call)
		if !ok || !isPoolReceiver(recv) {
			return true
		}
		if inTx(call.Pos()) {
			return true
		}
		sql := resolveSQL(call.Args[sqlIdx], scope)
		if !bareWriteVerb.MatchString(sql) {
			return true
		}
		sites = append(sites, barePoolSite{
			File: rel,
			Line: fset.Position(call.Pos()).Line,
			SQL:  sql,
			Func: enclosingFunc(funcRanges, call.Pos()),
		})
		return true
	})
	return sites, nil
}

// receiverOf returns the receiver expression of a selector call.
func receiverOf(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	return sel.X, true
}

// scanBarePoolRepository walks the repository and classifies every
// bare pool write, mirroring scanRepository's walk.
func scanBarePoolRepository(root string) (map[string][]barePoolSite, []string, error) {
	byFile := map[string][]barePoolSite{}
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" || base == ".build" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sites, perr := scanBarePoolSource(rel, src)
		if perr != nil {
			return perr
		}
		if len(sites) > 0 {
			byFile[rel] = sites
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return byFile, files, nil
}

// barePoolIdentity is the stable identity of a bare pool site: the
// file it lives in and the normalized shape of its SQL. Line numbers
// are deliberately NOT part of it — moving a statement within its
// file must not invalidate its entry, and two spellings of different
// statements must not collapse into one.
func barePoolIdentity(file, sql string) string {
	return file + "\x00" + normalizeSQL(sql)
}

// allowlistIndex keys the allowlist by identity for lookup. Two
// entries must never share an identity — that would make the expected
// count ambiguous — and TestBarePoolAllowlistIdentitiesAreUnique pins
// it.
func allowlistIndex() map[string]barePoolEntry {
	out := map[string]barePoolEntry{}
	for _, e := range barePoolAllowlist {
		out[barePoolIdentity(e.File, e.SQL)] = e
	}
	return out
}

// diffBarePool compares the live sites against the allowlist as
// multisets keyed by identity. It returns the surplus live sites —
// identities the allowlist does not know, plus occurrences beyond the
// recorded Count — and the stale entries, whose live count fell short
// of the recorded Count (a fix that was not followed by deleting or
// decrementing its entry).
//
// The function is pure (no repo walk, no test context) so the
// duplicate-insertion negative control pins it directly: a second
// identical bare Exec where Count is 1 reports exactly one surplus
// occurrence at its line.
func diffBarePool(live []barePoolSite, allow []barePoolEntry) (surplus []barePoolSite, stale []barePoolEntry) {
	liveCount := map[string]int{}
	liveByIdentity := map[string][]barePoolSite{}
	for _, s := range live {
		id := barePoolIdentity(s.File, s.SQL)
		liveCount[id]++
		liveByIdentity[id] = append(liveByIdentity[id], s)
	}
	want := map[string]barePoolEntry{}
	for _, e := range allow {
		want[barePoolIdentity(e.File, e.SQL)] = e
	}
	for id, sites := range liveByIdentity {
		e, ok := want[id]
		if !ok {
			surplus = append(surplus, sites...)
			continue
		}
		if len(sites) > e.Count {
			surplus = append(surplus, sites[e.Count:]...)
		}
	}
	for id, e := range want {
		if liveCount[id] < e.Count {
			stale = append(stale, e)
		}
	}
	return surplus, stale
}

// TestBarePoolWriteAllowlistIsComplete is the guard: every bare pool
// write in the tree must be a recorded entry. A new bare Exec fails
// here — route it through ExecTracked (budget 5), move it inside a
// transaction, or record it with a reason from the closed vocabulary
// above, which is the review.
func TestBarePoolWriteAllowlistIsComplete(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	byFile, _, err := scanBarePoolRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}
	var live []barePoolSite
	for _, sites := range byFile {
		live = append(live, sites...)
	}
	surplus, _ := diffBarePool(live, barePoolAllowlist)
	if len(surplus) > 0 {
		var unlisted []string
		for _, st := range surplus {
			unlisted = append(unlisted, fmt.Sprintf("%s:%d: %s", st.File, st.Line, firstLine(st.SQL)))
		}
		sort.Strings(unlisted)
		t.Fatalf("bare pool writes without an allowlist entry — record each one with a reason, or route it through ExecTracked (budget 5), or move it inside a transaction. A second copy of an allowlisted statement also lands here, as surplus occurrences:\n  %s",
			strings.Join(unlisted, "\n  "))
	}
	if len(barePoolAllowlist) == 0 {
		t.Fatal("the allowlist is empty, so this test proves nothing")
	}
}

// TestBarePoolAllowlistHasNoStaleEntries is the other half: the
// live count per identity must meet the recorded Count. A fix that
// routes a write through a budget (or moves it) without deleting or
// decrementing its entry fails here — a stale entry is a hole waiting
// for a future write with the same shape to hide in.
func TestBarePoolAllowlistHasNoStaleEntries(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	byFile, _, err := scanBarePoolRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}
	var live []barePoolSite
	for _, sites := range byFile {
		live = append(live, sites...)
	}
	_, stale := diffBarePool(live, barePoolAllowlist)
	if len(stale) > 0 {
		var names []string
		for _, e := range stale {
			names = append(names, fmt.Sprintf("%s (recorded %d): %s", e.File, e.Count, firstLine(e.SQL)))
		}
		sort.Strings(names)
		t.Fatalf("allowlist entries with too few live sites — the write was fixed or moved without updating its entry (delete it, or decrement its Count):\n  %s",
			strings.Join(names, "\n  "))
	}
}

// TestBarePoolAllowlistIdentitiesAreUnique pins the invariant the
// multiset comparison relies on: one identity, one entry. Two entries
// with the same (file, SQL) would make the expected count ambiguous.
func TestBarePoolAllowlistIdentitiesAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, e := range barePoolAllowlist {
		id := barePoolIdentity(e.File, e.SQL)
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("duplicate allowlist identity (merge the entries and sum their Counts): %s", e.File)
		}
		if e.Count < 1 {
			t.Fatalf("allowlist entry with non-positive Count: %+v", e)
		}
		switch e.Why {
		case reasonPropagated, reasonStartupDDL, reasonMigrationOnce, reasonBestEffort, reasonTestFixture, reasonSharedPrimitive, reasonConnectionScoped:
		default:
			t.Fatalf("allowlist entry with undocumented reason: %+v", e)
		}
	}
}

// TestBarePoolMultisetIdentitiesDetected is the negative control for
// the known identity flaw: inserting a SECOND identical bare Exec
// where the allowlist records Count 1 must fail as surplus, and it
// must do so by line number, not by count alone.
func TestBarePoolMultisetIdentitiesDetected(t *testing.T) {
	shape := "INSERT INTO probe_target (id) VALUES (?)"
	mkSite := func(line int) barePoolSite {
		return barePoolSite{File: "internal/core/probe.go", Line: line, SQL: shape}
	}
	allow := []barePoolEntry{{File: "internal/core/probe.go", SQL: shape, Why: reasonPropagated, Count: 1}}

	// One live site: clean.
	if surplus, stale := diffBarePool([]barePoolSite{mkSite(10)}, allow); len(surplus) != 0 || len(stale) != 0 {
		t.Fatalf("exact match should be clean, got surplus=%v stale=%v", surplus, stale)
	}
	// A second identical site: exactly one surplus occurrence, at its line.
	surplus, stale := diffBarePool([]barePoolSite{mkSite(10), mkSite(42)}, allow)
	if len(stale) != 0 {
		t.Fatalf("a duplicated write is not a stale entry: %v", stale)
	}
	if len(surplus) != 1 || surplus[0].Line != 42 {
		t.Fatalf("expected exactly the second occurrence as surplus, got %+v", surplus)
	}
	// Removing the site: stale, with its recorded count for the fix-up.
	_, stale = diffBarePool(nil, allow)
	if len(stale) != 1 {
		t.Fatalf("a removed write must surface as stale, got %+v", stale)
	}
	// Count 2 honoured both ways.
	allow2 := []barePoolEntry{{File: "internal/core/probe.go", SQL: shape, Why: reasonBestEffort, Count: 2}}
	if surplus, stale := diffBarePool([]barePoolSite{mkSite(10), mkSite(42)}, allow2); len(surplus) != 0 || len(stale) != 0 {
		t.Fatalf("two live sites against Count 2 should be clean, got surplus=%v stale=%v", surplus, stale)
	}
	if surplus, _ := diffBarePool([]barePoolSite{mkSite(10), mkSite(42), mkSite(77)}, allow2); len(surplus) != 1 {
		t.Fatalf("a third identical write must surface exactly one surplus occurrence, got %+v", surplus)
	}
}

// TestBarePoolAllowlistDetectsReintroducedSlice2Writes is the negative
// control for the SECOND remediation slice's eight conversions.
//
// The convention this guard has always kept is that a converted site
// DELETES its allowlist entry, and the deletion is the record. But a
// deletion alone only proves the entry is gone: it does not prove the
// shape would be CAUGHT if it came back. This test closes that gap for
// the eight sites the second slice converted, one by one:
//
//  1. the exact converted SQL shape must NOT be on the allowlist, under
//     ANY reason — so a reintroduction cannot pass by inheriting a
//     neighbour's entry; and
//  2. the classifier must still SEE the shape as a bare pool write — so
//     a live reintroduction is caught by shape, not by luck.
//
// The shapes are the ones the slice removed:
//
//   - internal/core/db.go:      UPDATE sessions SET summary = ...
//   - internal/core/db.go:      INSERT OR IGNORE INTO topic_memberships ...
//   - internal/core/db.go:      DELETE FROM topic_memberships ...
//   - internal/core/db.go:      INSERT INTO works ...
//   - internal/core/db.go:      UPDATE works SET status = ...
//   - internal/core/db.go:      UPDATE works SET verification = ...
//   - internal/core/handoff.go: UPDATE session_handoffs SET read_at = ...
//   - internal/core/handoff.go: DELETE FROM session_handoffs WHERE id = ?
//
// Two of the eight were filed under `migration-once` — a MISFILE. The
// misfiling is what hid them from the ordinary-write accounting, and
// TestBarePoolMigrationOnceEntriesNameAMigrationFunction is the guard
// that makes the class falsifiable in future.
func TestBarePoolAllowlistDetectsReintroducedSlice2Writes(t *testing.T) {
	converted := []struct {
		file string
		sql  string
	}{
		{"internal/core/db.go", "UPDATE sessions SET summary = ? WHERE session_id = ?"},
		{"internal/core/db.go", "INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)"},
		{"internal/core/db.go", "DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?"},
		{"internal/core/db.go", "INSERT INTO works (id, title, content, status, created_at, updated_at, session_id, migrated_at) VALUES (?, ?, ?, 'open', ?, ?, ?, ?)"},
		{"internal/core/db.go", "UPDATE works SET status = ?, updated_at = ?, completed_at = ? WHERE id = ?"},
		{"internal/core/db.go", "UPDATE works SET verification = ? WHERE id = ?"},
		{"internal/core/handoff.go", "UPDATE session_handoffs SET read_at = ?, read_by = ? WHERE id = ? AND read_at IS NULL"},
		{"internal/core/handoff.go", "DELETE FROM session_handoffs WHERE id = ?"},
	}

	allowed := allowlistIndex()
	for _, c := range converted {
		if _, ok := allowed[barePoolIdentity(c.file, c.sql)]; ok {
			t.Fatalf("%s: the converted shape is allowlisted again: %s", c.file, firstLine(c.sql))
		}
		// The classifier must still see the shape as bare, so a live
		// reintroduction fails the completeness guard.
		src := "package internal\n" +
			"func probe(dm *DatabaseManager) { _, _ = dm.db.Exec(`" + c.sql + "`, 1, \"x\") }\n"
		sites, err := scanBarePoolSource(c.file, []byte(src))
		if err != nil {
			t.Fatalf("%s: scan: %v", c.file, err)
		}
		if len(sites) != 1 {
			t.Fatalf("%s: the converted shape is not detected as a bare pool write (got %d sites): %s",
				c.file, len(sites), firstLine(c.sql))
		}
		if _, ok := allowed[barePoolIdentity(c.file, sites[0].SQL)]; ok {
			t.Fatalf("%s: the converted shape resolves to an allowlisted identity: %s",
				c.file, firstLine(c.sql))
		}
	}
}

// TestBarePoolMigrationOnceEntriesNameAMigrationFunction is the guard
// against the MISFILING hole the second remediation slice found.
//
// The `migration-once` class is a claim about WHEN a write runs: once,
// on a migration path, never on a user path. Three ordinary contended
// writes — a session summary, a topic-membership insert and a
// topic-membership delete — were filed under it, and because
// classification is human judgement, no mechanical guard could see them.
// The claim was unfalsifiable: nothing tied the class to the place the
// write actually lives.
//
// This test ties it. Every `migration-once` entry must:
//
//  1. NAME the function the write sits in, and the live site must still
//     be in that function (so a relocation is caught); and
//  2. name a function or a file whose name carries a MIGRATION VERB
//     (so an ordinary user-path function cannot be filed under this
//     class without tripping the guard).
//
// The vocabulary is CLOSED. A new one-shot migration whose name carries
// none of these verbs fails here, and adding a verb is a reviewable act
// — which is the point: the class stays falsifiable instead of becoming
// a place where ordinary writes go to hide.
//
// The approximation is load-bearing in one direction only: a migration
// whose verb is not in the vocabulary is over-reported (it must be
// reviewed), never under-reported. A user-path function that DOES carry
// a migration verb ("PromoteMemory", "SeedRow") can still be misfiled,
// and that hole is closed by the recorded Func — the misfiling would
// have to name a function the guard then checks against the live code.
func TestBarePoolMigrationOnceEntriesNameAMigrationFunction(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	byFile, _, err := scanBarePoolRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}
	live := map[string][]barePoolSite{}
	for _, sites := range byFile {
		for _, s := range sites {
			id := barePoolIdentity(s.File, s.SQL)
			live[id] = append(live[id], s)
		}
	}

	var problems []string
	for _, e := range barePoolAllowlist {
		if e.Why != reasonMigrationOnce {
			continue
		}
		if e.Func == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: a migration-once entry must name the function it lives in: %s",
				e.File, firstLine(e.SQL)))
			continue
		}
		id := barePoolIdentity(e.File, e.SQL)
		sites := live[id]
		if len(sites) == 0 {
			// The stale-entry guard reports this separately; nothing to
			// check against here.
			continue
		}
		for _, s := range sites {
			if s.Func != e.Func {
				problems = append(problems, fmt.Sprintf(
					"%s: the migration-once entry names function %q but the live write is in %q — "+
						"a one-shot migration that MOVED is a review, not a reclassification: %s",
					e.File, e.Func, s.Func, firstLine(e.SQL)))
			}
		}
		if !hasMigrationVerb(e.File) && !hasMigrationVerb(e.Func) {
			problems = append(problems, fmt.Sprintf(
				"%s: function %q carries no migration verb — a migration-once write must live on a "+
					"migration path (the file %q carries none either): %s",
				e.File, e.Func, e.File, firstLine(e.SQL)))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("migration-once entries whose classification is not checkable:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// migrationVerbs is the CLOSED vocabulary of migration verbs. A
// migration-once write must live in a file or a function whose name
// carries one of them.
//
// The list is deliberately short and deliberately excludes the verbs a
// USER path would use — "mark", "promote", "update" — because those
// names are shared by ordinary surfaces (MarkHandoffRead,
// PromoteMemory, UpdateMemory) and would let a user-path write pass as
// a migration.
var migrationVerbs = []string{
	"migrat", // migrate, migration
	"backfill",
	"seed",
	"rebuild",
	"enforce",
	"prune",
	"sentinel",
}

// hasMigrationVerb reports whether name carries a migration verb.
func hasMigrationVerb(name string) bool {
	lower := strings.ToLower(name)
	for _, verb := range migrationVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// TestBarePoolAllowlistDetectsReintroducedWrite is the regression the
// operator asked for: the exact pre-fix shape of the handoff UPSERT
// (the F2 defect — a bare pool UPSERT that lost the session-continuity
// record under contention) must NOT be allowlisted. Reintroducing that
// call fails this test by name, not just by count.
func TestBarePoolAllowlistDetectsReintroducedWrite(t *testing.T) {
	// The pre-fix spelling, verbatim: a bare dm.db.Exec of the
	// session_handoffs UPSERT.
	oldShape := `INSERT INTO session_handoffs (id, session_id, mpm_session_id, framework_session_id, ended_at, ended_state, summary, commitments, open_questions, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET id = session_handoffs.id`
	allowed := allowlistIndex()
	if _, ok := allowed[barePoolIdentity("internal/core/handoff.go", oldShape)]; ok {
		t.Fatalf("the pre-fix handoff UPSERT is allowlisted: reintroducing the F2 defect would pass the guard")
	}
	// And the classifier still sees the shape as bare, so a live
	// reintroduction would be caught by the completeness test above.
	sites, err := scanBarePoolSource("internal/core/handoff.go", []byte(
		"package internal\nfunc f(dm *M) { _, _ = dm.db.Exec(`"+oldShape+"`, 1) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("the reintroduced F2 shape was not detected as a bare pool write (got %d sites)", len(sites))
	}
	if _, ok := allowed[barePoolIdentity("internal/core/handoff.go", sites[0].SQL)]; ok {
		t.Fatalf("the reintroduced F2 shape resolves to an allowlisted identity")
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}

// TestBarePoolAllowlistDetectsReintroducedExecContextWrite is the
// non-vacuity control for the ExecContext shape.
//
// The phase-0 audit scanner matched the METHOD NAME `ExecContext` and
// still read the SQL from `Args[0]` — the context — so every
// `ExecContext` write was classified as a non-write and left out of the
// inventory. The first version of this ratchet did not match the method
// name at all. Both blind spots hid the same 17 sites.
//
// This test pins the fix from both ends:
//   - the classifier must SEE a bare `ExecContext` write (a synthetic
//     source, so it keeps proving the SHAPE as the tree changes); and
//   - the allowlist must not contain the synthetic shape, so a live
//     reintroduction fails the completeness guard rather than passing
//     because a detector could not see it.
func TestBarePoolAllowlistDetectsReintroducedExecContextWrite(t *testing.T) {
	const shape = "UPDATE probe_target SET last_seen_at = ? WHERE id = ?"
	const file = "internal/core/probe.go"

	// The synthetic source uses the ExecContext shape: the query is the
	// SECOND argument, because the first is the context.
	src := "package internal\n" +
		"func probe(ctx context.Context, dm *DatabaseManager) { _, _ = dm.db.ExecContext(ctx, `" + shape + "`, 1, \"x\") }\n"
	sites, err := scanBarePoolSource(file, []byte(src))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("a bare ExecContext write must be detected as a bare pool write (got %d sites)", len(sites))
	}
	if !strings.Contains(sites[0].SQL, "probe_target") {
		t.Fatalf("the detected site must carry the RESOLVED SQL, not the context: %q", sites[0].SQL)
	}

	// And the shape must not be allowlisted: a live reintroduction has
	// to fail the completeness guard.
	if _, ok := allowlistIndex()[barePoolIdentity(file, sites[0].SQL)]; ok {
		t.Fatalf("the synthetic ExecContext shape resolves to an allowlisted identity: " +
			"a live bare ExecContext write would pass the guard")
	}

	// The same shape expressed through Exec is detected too, so the two
	// spellings cannot hide behind each other.
	srcExec := "package internal\n" +
		"func probe(dm *DatabaseManager) { _, _ = dm.db.Exec(`" + shape + "`, 1, \"x\") }\n"
	sitesExec, err := scanBarePoolSource(file, []byte(srcExec))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sitesExec) != 1 {
		t.Fatalf("the Exec spelling of the same shape must be detected (got %d sites)", len(sitesExec))
	}
}

// TestBarePoolAllowlistDetectsUnapprovedAttach is the M1 control.
//
// The independent review of 2026-11-06 found the ratchet's comment
// claiming the tree contains no bare pool ATTACH — while
// internal/core/similarity_candidates_store.go:972 carries exactly
// one, on a pinned pooled connection. The verb was missing from the
// detector's list, so the site was invisible.
//
// This test pins the fix from both ends:
//
//   - the CLASSIFIER must SEE the connection-scoped ATTACH shape (a
//     synthetic source, so it keeps proving the SHAPE as the tree
//     changes); and
//   - the shape must be allowlisted ONLY under the one file that owns
//     the reviewed site. A NEW bare ATTACH in any other file — the
//     "newly introduced unapproved statement" the review asked to be
//     demonstrated — therefore resolves to an identity that is NOT on
//     the allowlist, and the completeness guard fails by shape rather
//     than by luck.
func TestBarePoolAllowlistDetectsUnapprovedAttach(t *testing.T) {
	// The exact statement the M1 site runs, resolved by the scanner's
	// own SQL resolution (fmt.Sprintf by its format string).
	const attachShape = "ATTACH DATABASE '%s' AS shared"

	// 1. The classifier sees it. The receiver is `conn` — a pinned
	//    connection, not a transaction — and the scanner's receiver
	//    rule counts every identifier other than `tx` as a pool
	//    receiver, deliberately failing closed.
	src := "package internal\n" +
		"func probe(ctx context.Context, conn *sql.Conn) { _, _ = conn.ExecContext(ctx, " +
		"fmt.Sprintf(\"" + attachShape + "\", path)) }\n"
	sites, err := scanBarePoolSource("internal/core/probe.go", []byte(src))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("a bare ATTACH on a pinned connection must be detected as a bare pool write (got %d sites)", len(sites))
	}
	if !strings.Contains(sites[0].SQL, "ATTACH DATABASE") {
		t.Fatalf("the detected site must carry the RESOLVED SQL, not the context: %q", sites[0].SQL)
	}

	// 2. The reviewed site is allowlisted, exactly once, under its own
	//    file, with the connection-scoped reason.
	allowed := allowlistIndex()
	entry, ok := allowed[barePoolIdentity("internal/core/similarity_candidates_store.go", attachShape)]
	if !ok {
		t.Fatalf("the reviewed connection-scoped ATTACH is not allowlisted: " +
			"the completeness guard would fail on the tree's own site")
	}
	if entry.Why != reasonConnectionScoped {
		t.Fatalf("the reviewed ATTACH is recorded with reason %q, want %q — "+
			"a pool ATTACH is not startup DDL and not a propagated write: "+
			"its scope is one connection", entry.Why, reasonConnectionScoped)
	}
	if entry.Count != 1 {
		t.Fatalf("the reviewed ATTACH is recorded with Count %d, want 1: "+
			"the tree carries exactly one such site", entry.Count)
	}

	// 3. The SAME shape anywhere else is unapproved: a newly
	//    introduced ATTACH fails the completeness guard, because its
	//    identity carries a different file.
	probeIdentity := barePoolIdentity("internal/core/probe.go", sites[0].SQL)
	if _, ok := allowed[probeIdentity]; ok {
		t.Fatalf("the synthetic ATTACH shape resolves to an allowlisted identity: " +
			"a live bare ATTACH in a new file would pass the guard")
	}
	if _, ok := allowed[barePoolIdentity("internal/core/other.go", attachShape)]; ok {
		t.Fatalf("the ATTACH shape is allowlisted outside its reviewed file: " +
			"a new bare ATTACH would inherit the reviewed site's entry")
	}
}

// TestBarePoolScannerFlagsRepresentativeShapes is the negative control:
// the classifier must catch every bare shape — including a deliberately
// reintroduced defect — and must not flag the guarded or
// transaction-owned shapes.
func TestBarePoolScannerFlagsRepresentativeShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			"bare dm.db.Exec is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`INSERT INTO t VALUES (?)`, 1) }\ntype M struct { db *D }\ntype D struct {}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare dm.SQLDB().Exec is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.SQLDB().Exec(`UPDATE t SET a = 1`) }\ntype M struct{}\nfunc (m *M) SQLDB() *D { return nil }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare s.DB.Exec is caught",
			"package p\nfunc f(s *M) { _, _ = s.DB.Exec(`DELETE FROM t`) }\ntype M struct { DB *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare local db.Exec is caught",
			"package p\nfunc f(db *D) { _, _ = db.Exec(`CREATE TABLE t (a)`) }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"reintroduced defect: a new bare write is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`INSERT INTO audit_log VALUES (?)`, 1) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			// The shape the phase-0 scanner and the first version of
			// this ratchet both MISSED: the query is the SECOND
			// argument, because the first is the context.
			"bare dm.db.ExecContext is caught",
			"package p\nfunc f(ctx context.Context, dm *M) { _, _ = dm.db.ExecContext(ctx, `INSERT INTO t VALUES (?)`, 1) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare dm.SQLDB().ExecContext is caught",
			"package p\nfunc f(ctx context.Context, dm *M) { _, _ = dm.SQLDB().ExecContext(ctx, `UPDATE t SET a = 1`) }\ntype M struct{}\nfunc (m *M) SQLDB() *D { return nil }\ntype D struct{}\nfunc (d *D) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare local db.ExecContext is caught",
			"package p\nfunc f(ctx context.Context, db *D) { _, _ = db.ExecContext(ctx, `CREATE TABLE t (a)`) }\ntype D struct{}\nfunc (d *D) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			// The PRAGMA/VACUUM gap. Before 2026-11-06 the verb list
			// stopped at the DML and DDL verbs, so a bare PRAGMA or
			// VACUUM on the pool was invisible to this guard. These
			// two cases are the negative control for that gap: they
			// fail against the old verb list and pass against the
			// current one.
			"bare PRAGMA write is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare VACUUM write is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`VACUUM INTO ?`, \"/tmp/x.db\") }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare PRAGMA write through ExecContext is caught",
			"package p\nfunc f(ctx context.Context, dm *M) { _, _ = dm.db.ExecContext(ctx, `PRAGMA journal_mode = WAL`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			// The M1 gap. Before the independent review of 2026-11-06
			// the verb list stopped at PRAGMA/VACUUM, so a bare
			// ATTACH/DETACH/REINDEX was invisible to this guard — and
			// the tree already carried one bare ATTACH
			// (internal/core/similarity_candidates_store.go:972),
			// which the comment claimed did not exist. These three
			// cases are the negative control for that gap: they fail
			// against the old verb list and pass against the current
			// one.
			"bare ATTACH is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`ATTACH DATABASE '/tmp/x.db' AS shared`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare DETACH is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`DETACH DATABASE shared`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			"bare REINDEX is caught",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Exec(`REINDEX idx_memories`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			// The M1 site itself: the ATTACH runs on a PINNED
			// `*sql.Conn`, and the scanner's receiver rule counts
			// every identifier other than `tx` as a pool receiver.
			// That is deliberate — the rule fails CLOSED, because a
			// connection-scoped write that is invisible to the
			// guard is exactly the hole this guard exists to close.
			"a bare ATTACH on a pinned connection is caught",
			"package p\nfunc f(ctx context.Context, conn *C) { _, _ = conn.ExecContext(ctx, `ATTACH DATABASE '/tmp/x.db' AS shared`) }\ntype C struct{}\nfunc (c *C) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			1,
		},
		{
			// A shared primitive: the executor is a parameter, so the
			// scanner cannot see which concrete type it carries. It
			// fails CLOSED and counts the site — the pool call site of
			// such a primitive is the one that loses the write.
			"a shared primitive's Exec is caught",
			"package p\nfunc f(ex E) { _, _ = ex.Exec(`INSERT INTO t VALUES (?)`, 1) }\ntype E interface { Exec(q string, a ...interface{}) (int, error) }\n",
			1,
		},
		{
			"guarded ExecTracked is not flagged",
			"package p\nfunc f(dm *M) { _, _ = dm.ExecTracked(`INSERT INTO t VALUES (?)`, 5, 1) }\ntype M struct{}\nfunc (m *M) ExecTracked(q string, n int, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"tx.Exec inside a WithTx callback is not flagged",
			"package p\nfunc f(dm *M) { _ = dm.WithTx(func(tx *T) error { _, _ = tx.Exec(`UPDATE t SET a = 1`); return nil }) }\ntype M struct{}\ntype T struct{}\nfunc (m *M) WithTx(fn func(*T) error) error { return fn(nil) }\nfunc (t *T) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			// A transaction-owned ExecContext is a transaction-owned
			// ExecContext: the SQL is still the second argument, but the
			// receiver is the transaction.
			"tx.ExecContext inside a WithTx callback is not flagged",
			"package p\nfunc f(ctx context.Context, dm *M) { _ = dm.WithTx(func(tx *T) error { _, _ = tx.ExecContext(ctx, `UPDATE t SET a = 1`); return nil }) }\ntype M struct{}\ntype T struct{}\nfunc (m *M) WithTx(fn func(*T) error) error { return fn(nil) }\nfunc (t *T) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"node.Tx().Exec is not flagged",
			"package p\nfunc f(n *N) { _, _ = n.Tx().Exec(`DELETE FROM t`) }\ntype N struct{}\ntype T struct{}\nfunc (n *N) Tx() *T { return nil }\nfunc (t *T) Exec(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"a read is not flagged",
			"package p\nfunc f(dm *M) { _, _ = dm.db.Query(`SELECT * FROM t`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) Query(q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
		{
			"a read via ExecContext is not flagged",
			"package p\nfunc f(ctx context.Context, dm *M) { _, _ = dm.db.ExecContext(ctx, `SELECT * FROM t`) }\ntype M struct { db *D }\ntype D struct{}\nfunc (d *D) ExecContext(ctx context.Context, q string, a ...interface{}) (int, error) { return 0, nil }\n",
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sites, err := scanBarePoolSource("synthetic.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(sites) != tc.want {
				t.Fatalf("got %d bare pool sites, want %d", len(sites), tc.want)
			}
		})
	}
}

// TestBarePoolScanReachesEveryRuntimePackage mirrors the budget
// guard's reach test: a scan that cannot see a package cannot ratchet
// it.
func TestBarePoolScanReachesEveryRuntimePackage(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	byFile, _, err := scanBarePoolRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}
	countUnder := func(prefix string) int {
		n := 0
		for f, sites := range byFile {
			if f == prefix || strings.HasPrefix(f, prefix+"/") {
				n += len(sites)
			}
		}
		return n
	}
	for _, prefix := range []string{"cmd/mpm", "internal/scheduler", "internal/core"} {
		if countUnder(prefix) == 0 {
			t.Errorf("the bare-pool scan reached no bare write under %q", prefix)
		}
	}
	total := 0
	for _, sites := range byFile {
		total += len(sites)
	}
	if total == 0 {
		t.Fatal("no bare pool write was examined: the scan found nothing, so this test proves nothing")
	}
}
