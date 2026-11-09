# Issue #178 — the bare pool write inventory

**Date:** 2026-11-07  
**Base:** `main` @ `056ce449`  
**Companion:** [`issue-178-bare-write-slice-2-2026-11-07.md`](issue-178-bare-write-slice-2-2026-11-07.md)  
**Previous revision:** 2026-11-06, measured at `main` @ `653e4633` — see §"What changed" below

Every bare pool write on the base commit, with its class. The classes are
the four the issue asks for, and the question each one answers is: **what
happens to the write when the contention outlasts the busy timeout?**

| Class | Occurrences | What the loss means |
|---|---|---|
| `ordinary` | 46 | The user's or the agent's state is not persisted. The write is lost; the caller is told, so the loss is recoverable only by redoing the work. |
| `migration` | 49 | Schema, a one-shot backfill, a seed, or a test-fixture write. Idempotent or detection-gated, so a rerun is safe. |
| `tx-owned` | 0 | A write inside a transaction is owned by the transaction layer. **No bare pool write on the base commit is lexically inside a `WithTx` callback.** |
| `maintenance` | 38 | A ledger, a counter, a retention sweep, a recomputation, or a diagnostic row. Deliberately nonblocking: the failure is logged and costs a re-run, not a state. |

**Total: 133 occurrences across 53 files.**

The `Why` column is the merged ratchet's own reason vocabulary
(`internal/core/bare_pool_write_ratchet_test.go`), so this table and the
guard's allowlist cannot drift apart. The `Class` column is the
reclassification; it is a judgement about the CONSEQUENCE of the loss, not a
second copy of the reason.

The one `reasonConnectionScoped` site
(`internal/core/similarity_candidates_store.go`,
`ensureSharedAttachedOnConn`) is classified `migration` because it is
**schema**, it is **detection-gated** (the `PRAGMA database_list` check
precedes it on the same connection), and a rerun on that connection is
**safe** — which is exactly what the `migration` class means. It is not
`ordinary`, because no agent state is at stake, and it is not
`maintenance`, because its failure is fatal to the operation rather than
deliberately nonblocking. See the first slice's record, §5.1.

## What changed since the previous revision

The previous revision of this document was measured at `653e4633` and
recorded **161** occurrences. Two sets of sites have since left the
inventory, and both are conversions — a conversion DELETES the site, so
the inventory shrinks:

| What | Occurrences removed | Class of the removed rows |
|---|---|---|
| PR #231 — the first remediation slice (the memory write path) | 20 | 20 `ordinary` |
| This slice — the durable user-state path (session, handoff, work, topic link) | 8 | 5 `ordinary`, **3 `migration`** |
| **Total** | **28** | 25 `ordinary`, 3 `migration` |

**161 − 28 = 133.** The per-file headers below and every `Line` in the
tables are re-measured at `056ce449` with this slice's conversions applied.

### The three rows that were `migration` and were not

The three `migration` rows removed by this slice are the reason the second
slice exists, and they are worth naming exactly, because the class was the
thing that hid them:

| Site | Classed `migration` because | What it actually is |
|---|---|---|
| `internal/core/db.go` — `UpdateSessionSummary` | the row said `reasonMigrationOnce` | the ordinary write of a session's summary |
| `internal/core/db.go` — `AddMemoryToTopic` | the row said `reasonMigrationOnce` | the ordinary write that links a memory to a topic |
| `internal/core/db.go` — `RemoveMemoryFromTopic` | the row said `reasonMigrationOnce` | the ordinary write that unlinks a memory from a topic |

Both the document's `Class` column AND the guard's `Why` column had them
as one-shot migrations. They are routine user-path writes that run
whenever a caller asks for them. Nothing mechanical could see that: the
classification is a human judgement, and a wrong judgement is invisible to
every count-based guard.

The second slice therefore made the `migration-once` class **falsifiable**:
every such entry must now name the function the write lives in, and that
function (or its file) must carry a migration verb. See the second slice's
record, §4.

**This revision's line numbers are measured at the base commit named
above** (`056ce449`), with this slice's eight conversions applied. The
previous revision carried the line numbers of `653e4633` under a header
that named it — the same class of error as the false coverage claim the
independent review of 2026-11-06 found in the ratchet's verb list: a
document asserting a base it was not measured against.

---


## `cmd/mpm/backfill_embeddings.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 289 | `migration` | `reasonMigrationOnce` | `runReferenceBackfill` | `UPDATE reference_chunks SET embedding = NULL, embedding_source = 'null', embedding_dimension …` |
| 541 | `migration` | `reasonMigrationOnce` | `updateMemoryEmbedding` | `UPDATE memories SET embedding = ?, embedding_source = ?, embedding_dimension = ?, embedding_m…` |

## `cmd/mpm/handlers_backup.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 698 | `maintenance` | `reasonPropagated` | `flushWal` | `PRAGMA wal_checkpoint(TRUNCATE)` |

## `cmd/mpm/handlers_gc.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 174 | `maintenance` | `reasonPropagated` | `handleGC` | `INSERT INTO system_config (key, raw_json, content_hash) VALUES ('last_gc_at', ?, '') ON CONFL…` |
| 221 | `maintenance` | `reasonPropagated` | `handleGC` | `DELETE FROM memories WHERE deleted_at IS NOT NULL AND deleted_at < strftime('%s','now', '-30 …` |

## `cmd/mpm/handlers_restore.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 38 | `ordinary` | `reasonPropagated` | `handleRestore` | `UPDATE memories SET deleted_at = NULL, weight = MAX(?, 1) WHERE id = ?` |

## `cmd/mpm/handlers_topic.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 113 | `ordinary` | `reasonPropagated` | `handleTopicAdd` | `INSERT INTO topics (id, name, description) VALUES (?, ?, ?)` |

## `cmd/mpm/ingest.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 296 | `maintenance` | `reasonPropagated` | `handleIngestCleanup` | `UPDATE raw_memories SET status = 'expired' WHERE status = 'pending' AND expires_at > 0 AND ex…` |
| 376 | `ordinary` | `reasonPropagated` | `handleIngestUndo` | `DELETE FROM raw_memories WHERE import_batch = ? AND status IN ('pending', 'approved', 'review…` |

## `cmd/mpm/main.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 988 | `maintenance` | `reasonPropagated` | `runDeepScanFixSoftDeleteGhosts` | `DELETE FROM memories_fts WHERE rowid IN (SELECT rowid FROM memories WHERE deleted_at IS NOT N…` |

## `cmd/mpm/maint_cmds.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 385 | `maintenance` | `reasonBestEffort` | `handleMaintain` | `PRAGMA incremental_vacuum` |

## `cmd/mpm/self_heal.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 99 | `ordinary` | `reasonPropagated` | `saveSelfHealState` | `INSERT INTO memories (id, collection, content, metadata, created_at, updated_at) VALUES (?, '…` |

## `cmd/mpm/simple_cmds.go` — 4


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 565 | `ordinary` | `reasonPropagated` | `handleRm` | `UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND delet…` |
| 728 | `ordinary` | `reasonPropagated` | `handlePromote` | `UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ? AND deleted_at IS NULL AND col…` |
| 990 | `ordinary` | `reasonPropagated` | `handleSetWeight` | `UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL` |
| 1170 | `ordinary` | `reasonPropagated` | `handleSnooze` | `UPDATE memories SET weight = MIN(weight + 1, 9), last_accessed_at = CAST(strftime('%s','now',…` |

## `internal/blobstore/fs.go` — 4


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 268 | `ordinary` | `reasonPropagated` | `Put` | `INSERT INTO blobs (id, source_tool, source_call_id, session_id, size_bytes, content_type, cre…` |
| 397 | `ordinary` | `reasonPropagated` | `Delete` | `DELETE FROM blobs WHERE id = ?` |
| 577 | `maintenance` | `reasonPropagated` | `GCExpired` | `DELETE FROM blobs WHERE expires_at < ?` |
| 697 | `maintenance` | `reasonPropagated` | `GCSweepOrphans` | `DELETE FROM blobs WHERE id = ?` |

## `internal/core/active_uptime.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 223 | `maintenance` | `reasonPropagated` | `CaptureMissingSettlingBaselines` | `INSERT INTO memory_settling_baselines (memory_id, baseline_active_seconds, captured_at) SELEC…` |

## `internal/core/admission_db.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 106 | `ordinary` | `reasonPropagated` | `RecordAdmissionOutcome` | `INSERT INTO admission_log ( id, doc_id, chunk_id, admit, content, confidence, reason, justifi…` |

## `internal/core/arbitration.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 464 | `ordinary` | `reasonPropagated` | `ReinforceSharedMemory` | `UPDATE shared.memories SET reinforcement_count = COALESCE(reinforcement_count, 0) + ?, weight…` |

## `internal/core/audit.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 242 | `maintenance` | `reasonPropagated` | `LogSkillWorkshopAudit` | `INSERT INTO system_audit_log (id, level, component, message, stack_trace, context, created_at…` |
| 378 | `maintenance` | `reasonBestEffort` | `PruneAuditLog` | `DELETE FROM system_audit_log WHERE created_at < CAST(strftime('%s','now', ?) AS INTEGER)` |
| 543 | `maintenance` | `reasonPropagated` | `upsertClusterCounter` | `INSERT INTO audit_cluster_proposals (cluster_key, component, message_hash, count, first_seen,…` |

## `internal/core/cascade_outbox.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 773 | `migration` | `reasonMigrationOnce` | `PruneCascadeOutbox` | `DELETE FROM epistemic_cascade_outbox WHERE status IN ('materialized', 'failed') AND updated_a…` |

## `internal/core/cascade_wake_reconcile.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 108 | `maintenance` | `reasonPropagated` | `ReconcileUnscheduledCascadeWakes` | `UPDATE epistemic_cascade_outbox SET wake_scheduled = 1, updated_at = ? WHERE id = ? AND wake_…` |

## `internal/core/cluster_proposals.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 238 | `ordinary` | `reasonPropagated` | `SetClusterStatus` | `UPDATE audit_cluster_proposals SET status = ?, snooze_until = ? WHERE cluster_key = ?` |
| 245 | `ordinary` | `reasonPropagated` | `SetClusterStatus` | `UPDATE audit_cluster_proposals SET status = ?, snooze_until = NULL WHERE cluster_key = ?` |
| 255 | `ordinary` | `reasonPropagated` | `SetClusterStatus` | `UPDATE audit_cluster_proposals SET status = ?, snooze_until = NULL WHERE cluster_key = ?` |

## `internal/core/compact.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 1019 | `maintenance` | `reasonPropagated` | `recordCompactionEvent` | `INSERT INTO system_config (key, raw_json, content_hash) VALUES ('compaction.last_run', ?, '')…` |

## `internal/core/contradiction_log.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 311 | `ordinary` | `reasonPropagated` | `EnqueueContradictionReport` | `INSERT OR IGNORE INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarit…` |

## `internal/core/contradiction_verification.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 348 | `ordinary` | `reasonPropagated` | `AdjudicateContradiction` | `UPDATE shared.contradiction_log SET detected_by = ? WHERE id = ? AND resolved_at IS NULL` |

## `internal/core/db.go` — 39


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 214 | `migration` | `reasonStartupDDL` | `NewSQLiteConnection` | `PRAGMA foreign_keys = ON` |
| 215 | `migration` | `reasonStartupDDL` | `NewSQLiteConnection` | `PRAGMA journal_mode = WAL` |
| 218 | `migration` | `reasonStartupDDL` | `NewSQLiteConnection` | `PRAGMA busy_timeout = 5000` |
| 248 | `maintenance` | `reasonPropagated` | `WipeRecord` | `VACUUM` |
| 671 | `maintenance` | `reasonBestEffort` | `HealthCheck` | `PRAGMA wal_checkpoint(TRUNCATE)` |
| 1692 | `migration` | `reasonStartupDDL` | `NewDatabaseManager` | `PRAGMA foreign_keys = ON` |
| 1693 | `migration` | `reasonStartupDDL` | `NewDatabaseManager` | `PRAGMA journal_mode = WAL` |
| 1717 | `migration` | `reasonStartupDDL` | `NewDatabaseManager` | `PRAGMA busy_timeout = 5000` |
| 1718 | `migration` | `reasonStartupDDL` | `NewDatabaseManager` | `PRAGMA synchronous = NORMAL` |
| 1719 | `migration` | `reasonStartupDDL` | `NewDatabaseManager` | `PRAGMA cache_size = -64000` |
| 2060 | `migration` | `reasonStartupDDL` | `NewSession` | `PRAGMA foreign_keys = ON` |
| 2061 | `migration` | `reasonStartupDDL` | `NewSession` | `PRAGMA journal_mode = WAL` |
| 2062 | `migration` | `reasonStartupDDL` | `NewSession` | `PRAGMA synchronous = NORMAL` |
| 2063 | `migration` | `reasonStartupDDL` | `NewSession` | `PRAGMA cache_size = -64000` |
| 2674 | `migration` | `reasonMigrationOnce` | `backfillSharedFTSIfEmpty` | `INSERT OR IGNORE INTO shared.memories_fts(rowid, content, collection, session_id, tags) SELEC…` |
| 2933 | `migration` | `reasonStartupDDL` | `initUnifiedSchema` | `PRAGMA journal_mode = WAL` |
| 2934 | `migration` | `reasonStartupDDL` | `initUnifiedSchema` | `PRAGMA busy_timeout = 5000` |
| 2935 | `migration` | `reasonStartupDDL` | `initUnifiedSchema` | `PRAGMA synchronous = NORMAL` |
| 3766 | `migration` | `reasonStartupDDL` | `createLessonsViewAndTriggers` | `CREATE VIEW IF NOT EXISTS lessons AS SELECT rowid, id, type, content, tags, reinforcement_cou…` |
| 3780 | `migration` | `reasonStartupDDL` | `createLessonsViewAndTriggers` | `CREATE TRIGGER lessons_instead_of_insert INSTEAD OF INSERT ON lessons BEGIN INSERT INTO lesso…` |
| 3802 | `migration` | `reasonStartupDDL` | `createLessonsViewAndTriggers` | `CREATE TRIGGER lessons_instead_of_update INSTEAD OF UPDATE ON lessons BEGIN UPDATE lessons_ba…` |
| 3823 | `migration` | `reasonStartupDDL` | `createLessonsViewAndTriggers` | `CREATE TRIGGER lessons_instead_of_delete INSTEAD OF DELETE ON lessons BEGIN DELETE FROM lesso…` |
| 3965 | `migration` | `reasonStartupDDL` | `ensureLessonsFTS` | `CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(content, tags, tokenize='porter uni…` |
| 3985 | `migration` | `reasonStartupDDL` | `initFTSTables` | `CREATE VIRTUAL TABLE test_fts USING fts5(content, tokenize='porter unicode61');` |
| 4903 | `ordinary` | `reasonPropagated` | `SaveSystemConfig` | `INSERT OR REPLACE INTO system_config (key, raw_json, content_hash, updated_at, config_snapsho…` |
| 4944 | `ordinary` | `reasonPropagated` | `DeleteSystemConfig` | `DELETE FROM system_config WHERE key = ?` |
| 5398 | `ordinary` | `reasonPropagated` | `CreateTopic` | `INSERT INTO topics (id, name, description, created_at, tags, is_active, updated_at) VALUES (?…` |
| 6256 | `ordinary` | `reasonPropagated` | `DeleteLesson` | `UPDATE lessons_base SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL` |
| 6276 | `ordinary` | `reasonPropagated` | `RestoreLesson` | `UPDATE lessons_base SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL` |
| 6297 | `ordinary` | `reasonPropagated` | `ShredLesson` | `DELETE FROM lessons_fts WHERE rowid IN (SELECT rowid FROM lessons_base WHERE id = ?)` |
| 6301 | `ordinary` | `reasonPropagated` | `ShredLesson` | `DELETE FROM lessons_base WHERE id = ?` |
| 6659 | `maintenance` | `reasonPropagated` | `DecayWeights` | `UPDATE memories SET weight = MAX(weight - MAX(1, CAST(weight * ? * COALESCE( CASE json_extrac…` |
| 8180 | `ordinary` | `reasonPropagated` | `RecordGlobalRule` | `INSERT INTO shared.memories (id, collection, content, tags, metadata, weight, is_global, crea…` |
| 8236 | `ordinary` | `reasonPropagated` | `PromoteToGlobal` | `INSERT INTO shared.memories (id, collection, content, tags, metadata, weight, reinforcement_c…` |
| 8290 | `ordinary` | `reasonPropagated` | `RetireGlobalRule` | `UPDATE shared.memories SET retired_at = ? WHERE id = ? AND is_global = 1 AND deleted_at IS NU…` |
| 8380 | `migration` | `reasonStartupDDL` | `migrateWorkEvents` | `CREATE TABLE IF NOT EXISTS work_events ( id TEXT PRIMARY KEY, work_id TEXT NOT NULL, event_in…` |
| 8419 | `migration` | `reasonMigrationOnce` | `migrateWorkEvents` | `INSERT OR IGNORE INTO work_events (id, work_id, event_index, event_type, created_at, invocati…` |
| 8424 | `migration` | `reasonMigrationOnce` | `migrateWorkEvents` | `UPDATE works SET migrated_at = CAST(strftime('%s','now') AS INTEGER) WHERE migrated_at IS NULL` |
| 9118 | `maintenance` | `reasonPropagated` | `RecomputeWorkProjection` | `UPDATE works SET status = ?, title = ?, content = ?, updated_at = ?, completed_at = ?, archiv…` |

## `internal/core/embedding_migration.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 380 | `migration` | `reasonMigrationOnce` | `takeBackup` | `VACUUM INTO ?` |
| 401 | `migration` | `reasonMigrationOnce` | `markSyntheticTheories` | `UPDATE memories SET synthetic = 1 WHERE synthetic = 0 AND collection = 'theories' AND deleted…` |
| 505 | `migration` | `reasonMigrationOnce` | `recordSentinel` | `INSERT INTO embedding_migration_log (memory_id, old_weight, new_weight, reason, migrated_at) …` |

## `internal/core/epistemology_tools.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 79 | `ordinary` | `reasonPropagated` | `ChallengeMemoryWithTheory` | `UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND delete…` |

## `internal/core/fts_recovery.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 340 | `migration` | `reasonStartupDDL` | `dropStaleFTSyncTriggers` | `DROP TRIGGER IF EXISTS` |
| 437 | `migration` | `reasonStartupDDL` | `repairOrphanDomain` | `DROP TABLE IF EXISTS` |
| 447 | `migration` | `reasonStartupDDL` | `repairOrphanDomain` | `CREATE VIRTUAL TABLE %s USING fts5(%s, tokenize='porter unicode61')` |

## `internal/core/gc_tools.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 81 | `maintenance` | `reasonPropagated` | `RunGC` | `INSERT INTO system_config (key, raw_json, content_hash) VALUES ('last_gc_at', ?, '') ON CONFL…` |
| 142 | `maintenance` | `reasonPropagated` | `RunGC` | `UPDATE memories SET runtime_seconds_since_access = CASE WHEN runtime_last_accrued_at IS NULL …` |

## `internal/core/handoff.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 597 | `maintenance` | `reasonBestEffort` | `PruneHandoffs` | `DELETE FROM session_handoffs WHERE created_at < CAST(strftime('%s','now', '-' \` |

## `internal/core/ingest.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 210 | `ordinary` | `reasonPropagated` | `insertRawMemoryRejectedFromAdapter` | `INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at…` |
| 229 | `ordinary` | `reasonPropagated` | `insertRawMemory` | `INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, ingested_at…` |
| 290 | `maintenance` | `reasonPropagated` | `ResetStaleReviewing` | `UPDATE raw_memories SET status = 'pending', updated_at = ? WHERE status = 'reviewing' AND upd…` |

## `internal/core/lesson_proposals.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 159 | `ordinary` | `reasonPropagated` | `AddLessonProposal` | `INSERT INTO lesson_proposals (id, type, content, tags, created, status, source_session_id, fr…` |
| 325 | `ordinary` | `reasonPropagated` | `PromoteLessonProposal` | `UPDATE lesson_proposals SET status = ?, promoted_lesson_id = ?, reviewed_at = ? WHERE id = ? …` |
| 395 | `ordinary` | `reasonPropagated` | `RejectLessonProposal` | `UPDATE lesson_proposals SET status = ?, reviewed_at = ? WHERE id = ? AND status = ?` |

## `internal/core/lessons_fts_sync.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 131 | `migration` | `reasonMigrationOnce` | `recordLessonsFTSSentinel` | `INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, CAST(strftime('%s','now')…` |

## `internal/core/memory.go` — 7


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 286 | `migration` | `reasonStartupDDL` | `addColumnIfNotExists` | `ALTER TABLE %s ADD COLUMN %s %s` |
| 2632 | `ordinary` | `reasonPropagated` | `PromoteTopicToMemory` | `UPDATE topics SET is_active = 0, updated_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?` |
| 2822 | `ordinary` | `reasonPropagated` | `DeleteAllByCollection` | `UPDATE memories SET collection = collection \` |
| 2849 | `ordinary` | `reasonPropagated` | `DeleteAllMemories` | `UPDATE memories SET collection = collection \` |
| 3346 | `migration` | `reasonMigrationOnce` | `BackfillContentHash` | `UPDATE memories SET content_hash = lower(hex(sha256(content))) WHERE content_hash IS NULL OR …` |
| 3368 | `migration` | `reasonMigrationOnce` | `BackfillLTMFlags` | `UPDATE memories SET is_long_term = 1, weight = 10 WHERE weight = 1 AND is_long_term = 0 AND j…` |
| 3512 | `maintenance` | `reasonBestEffort` | `PruneExpired` | `UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE expires_at IS NO…` |

## `internal/core/memory_tools.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 677 | `ordinary` | `reasonPropagated` | `ShredMemoryWithCascade` | `DELETE FROM lessons WHERE id = ?` |

## `internal/core/migrate_md.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 441 | `migration` | `reasonMigrationOnce` | `PromoteRawMemoryBatch` | `UPDATE raw_memories SET status='approved', memory_id=?, updated_at=? WHERE id=?` |

## `internal/core/migration_memories_affinity_rebuild.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 169 | `migration` | `reasonMigrationOnce` | `RebuildMemoriesColumnAffinity` | `PRAGMA foreign_keys = OFF` |
| 176 | `migration` | `reasonMigrationOnce` | `RebuildMemoriesColumnAffinity` | `PRAGMA foreign_keys = ON` |

## `internal/core/migration_memories_created_at_not_null.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 160 | `migration` | `reasonMigrationOnce` | `EnforceMemoriesCreatedAtNotNull` | `PRAGMA foreign_keys = OFF` |
| 164 | `migration` | `reasonMigrationOnce` | `EnforceMemoriesCreatedAtNotNull` | `PRAGMA foreign_keys = ON` |

## `internal/core/reference_db.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 349 | `maintenance` | `reasonPropagated` | `EmbedReferenceChunks` | `UPDATE reference_chunks SET embedding = ?, embedding_source = ?, embedding_dimension = ?, emb…` |
| 702 | `maintenance` | `reasonPropagated` | `RefreshStaleReferenceEmbeddings` | `UPDATE reference_chunks SET embedding = ?, embedding_source = 'provider', embedding_dimension…` |

## `internal/core/reference_ingest.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 592 | `ordinary` | `reasonPropagated` | `updateReferenceMetadata` | `UPDATE reference_docs SET title = ?, tags = ?, import_reason = ?, file_path = ?, source_kind …` |

## `internal/core/retrieval_metadata.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 37 | `maintenance` | `reasonPropagated` | `RecordRetrieval` | `INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_c…` |
| 65 | `maintenance` | `reasonPropagated` | `RecordRetrievalSuccess` | `INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_c…` |
| 102 | `maintenance` | `reasonPropagated` | `IncrementSuccess` | `INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_c…` |

## `internal/core/review.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 379 | `ordinary` | `reasonSharedPrimitive` | `recordMemoryReviewOn` | `INSERT INTO memory_reviews (memory_id, reviewed_at, decision, note) VALUES (?, ?, ?, ?) ON CO…` |

## `internal/core/scheduled_tasks.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 152 | `ordinary` | `reasonPropagated` | `UpsertScheduledTask` | `INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at, created_…` |
| 339 | `ordinary` | `reasonPropagated` | `DeleteScheduledTask` | `DELETE FROM scheduled_tasks WHERE id = ?` |

## `internal/core/seed/engine.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 179 | `migration` | `reasonMigrationOnce` | `reviveSeedRow` | `UPDATE memories SET deleted_at = NULL, content = ?, tags = ?, metadata = ?, is_prime_directiv…` |
| 235 | `migration` | `reasonMigrationOnce` | `insertSeedRow` | `INSERT OR IGNORE INTO memories (id, collection, content, tags, metadata, is_prime_directive, …` |

## `internal/core/similarity_candidates_store.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 972 | `migration` | `reasonConnectionScoped` | `ensureSharedAttachedOnConn` | `ATTACH DATABASE '%s' AS shared` |
| 1258 | `ordinary` | `reasonPropagated` | `EnqueueSimilarityCandidatesTransactional` | `INSERT OR IGNORE INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarit…` |

## `internal/core/skill_db.go` — 3


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 624 | `ordinary` | `reasonPropagated` | `SaveSkillWithInvocation` | `UPDATE memories SET content = ?, tags = ?, metadata = ?, is_long_term = 1, deleted_at = NULL,…` |
| 810 | `ordinary` | `reasonPropagated` | `PromoteSkillToGlobal` | `UPDATE memories SET is_global = 1, metadata = json_set(COALESCE(metadata, '{}'), '$.derived_f…` |
| 857 | `ordinary` | `reasonPropagated` | `ShredSkill` | `UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND colle…` |

## `internal/core/testhelpers.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 234 | `migration` | `reasonTestFixture` | `OpenLegacyMemoriesDB` | `CREATE TABLE memories ( id TEXT PRIMARY KEY, collection TEXT, content TEXT, session_id TEXT, …` |

## `internal/core/tools/handlers.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 1736 | `migration` | `reasonMigrationOnce` | `handleMigrate` | `UPDATE raw_memories SET status='rejected', llm_notes='undo by mcp tool', updated_at=? WHERE i…` |

## `internal/core/vector_index.go` — 2


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 320 | `maintenance` | `reasonPropagated` | `AssignToCluster` | `INSERT OR REPLACE INTO (memory_id, cluster_id, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)` |
| 383 | `maintenance` | `reasonPropagated` | `UnassignFromCluster` | `DELETE FROM WHERE memory_id = ?` |

## `internal/core/wake_tools.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 118 | `maintenance` | `reasonPropagated` | `ScheduleCascadeSummaryWake` | `INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, fired, created_by, metadata)…` |

## `internal/core/web_db.go` — 4


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 724 | `ordinary` | `reasonSharedPrimitive` | `weakenMemoryOn` | `UPDATE memories SET reinforcement_count = MAX(reinforcement_count - ?, 0), weight = MAX(weigh…` |
| 797 | `maintenance` | `reasonBestEffort` | `PruneExpired` | `DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < strftime('%s','now') AND` |
| 1782 | `maintenance` | `reasonBestEffort` | `PruneOlderThan` | `DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND` |
| 1799 | `maintenance` | `reasonBestEffort` | `PruneNeverAccessed` | `DELETE FROM memories WHERE AND last_accessed_at IS NULL AND reinforcement_count = 0 AND weigh…` |

## `internal/scheduler/drill_handler.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 249 | `maintenance` | `reasonPropagated` | `updateDrillRunError` | `UPDATE drill_runs SET status='error', error_message=?, completed_at=? WHERE id=?` |

## `internal/telemetry/store.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 68 | `migration` | `reasonStartupDDL` | `Open` | `CREATE TABLE IF NOT EXISTS telemetry_invocation ( invocation_id TEXT PRIMARY KEY, parent_invo…` |

## `internal/update/backup.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 88 | `maintenance` | `reasonPropagated` | `vacuumInto` | `VACUUM INTO '%s'` |

## `internal/update/health.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 184 | `maintenance` | `reasonPropagated` | `stageScratchDB` | `VACUUM INTO '%s'` |

## `internal/update/preflight.go` — 1


| Line | Class | Why | Function | Statement |
|---|---|---|---|---|
| 32 | `maintenance` | `reasonPropagated` | `snapshotDB` | `VACUUM INTO '%s'` |
