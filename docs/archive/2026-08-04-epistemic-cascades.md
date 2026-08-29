# Epistemic Cascades Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add deterministic, bounded epistemic cascades that atomically record invalidation intents and asynchronously create one re-evaluation theory for each affected decision or theory.

**Architecture:** Explicit invalidation paths write a durable cascade outbox intent in the same SQLite transaction as the root mutation. A bounded materializer claims intents, creates idempotent pending theories, and schedules delivery wakes; `check_wakes` limits cascade delivery to three per pull. Dependency discovery combines explicit `memories.dependencies` edges with persisted typed provenance citations, while lessons and global rules remain excluded.

**Tech Stack:** Go, SQLite/WAL, `database/sql`, existing MPM `DatabaseManager`, FTS5 triggers, scheduled wakes, audit ledger, testify/require tests.

## Global Constraints

- Use the existing single `DatabaseManager` connection; do not add an unapproved `sql.Open` call.
- Cascades trigger only on explicit invalidation: disproven theory, shred, or defined hard confidence invalidation; ordinary weakening does not cascade.
- Generate exactly one theory per downstream decision/theory; never group targets.
- Lessons and global rules are never automatic cascade targets.
- Default maximum recursive `cascade_depth` is 3; further propagation is suppressed and audited at `CRITICAL`.
- Outbox insertion is atomic with the invalidating mutation; theory materialization is asynchronous.
- Materializer writes must pass the centralized memory scanner and normal FTS/indexing paths.
- Wake delivery defaults to at most 3 cascade theories per `CheckPendingWakes` call.
- Retries are bounded; exhausted intents remain inspectable as dead letters and emit a `CRITICAL` audit record.
- Every implementation task ends with focused tests and a small commit.

---

## File map

- **Create:** `internal/core/cascade_provenance.go` — typed provenance citation persistence and downstream lookup.
- **Create:** `internal/core/cascade_outbox.go` — outbox model, transactional intent insertion, claiming, retries, materialization, and depth/idempotency policy.
- **Create:** `internal/core/cascade_outbox_test.go` — outbox and materializer unit/integration tests.
- **Modify:** `internal/core/schema.go` — base schema for provenance and cascade outbox tables/indexes.
- **Modify:** `internal/core/db.go` and/or migration files — idempotent upgrade path for already-initialized databases and shared schema attachment.
- **Modify:** `internal/core/core.go` — expose provenance, outbox, materializer, and testable configuration methods through `CoreDB` where runtime consumers need them.
- **Modify:** `internal/core/epistemology_tools.go` — record decision/theory citations and make disproven resolution enqueue intents in its transaction.
- **Modify:** `internal/core/memory_tools.go` and `internal/core/web_db.go` — route all supported shred paths through transactional cascade intent capture without regressing lesson-aware behavior.
- **Modify:** `internal/core/evidence_tools.go` — identify the explicit hard confidence invalidation transition and enqueue intents only on that transition.
- **Modify:** `internal/core/wake_tools.go` — classify cascade wakes and apply the default delivery cap without changing notification/cron semantics.
- **Modify:** `internal/core/audit.go` — add focused helpers for dead-letter and depth-suppression payload formatting while continuing to write through the existing audit API.
- **Modify:** `cmd/mpm/main.go` and the existing lifecycle owner in `internal/core/db.go` — start and stop the materializer with the process and ensure shutdown does not leak goroutines.
- **Test:** `internal/core/schema_foundation_test.go`, `internal/core/cascade_provenance_test.go`, `internal/core/cascade_outbox_test.go`, `internal/core/cascade_invalidation_test.go`, `internal/core/cascade_materializer_test.go`, and `internal/core/wake_tools_test.go`.

---

### Task 1: Add durable schema and migration coverage

**Files:**
- Modify: `internal/core/schema.go`
- Modify: `internal/core/db.go` or the repository’s current safe-migration file
- Test: `internal/core/schema_foundation_test.go` and a new focused schema test if needed

**Interfaces:**
- Produces tables `epistemic_provenance` and `epistemic_cascade_outbox`, with indexes needed by invalidation lookup and worker claims.
- Produces columns for `invalidation_event_id`, typed source/downstream IDs, `trigger_evidence_id`, `cascade_depth`, status, materialized theory ID, retry metadata, and timestamps.

- [ ] **Step 1: Write failing schema tests** asserting both tables, required columns, the unique invalidation/downstream key, and indexes exist in a fresh in-memory database and after upgrade initialization.
- [ ] **Step 2: Run the focused schema tests** with `go test -tags fts5 ./internal/core -run 'Test.*Cascade|Test.*Provenance' -v`; confirm failure because the tables/indexes are absent.
- [ ] **Step 3: Add `CREATE TABLE IF NOT EXISTS` and indexes** to the canonical schema. Keep timestamps as INTEGER Unix epoch seconds and use status checks for `pending`, `processing`, `materialized`, and `failed`.
- [ ] **Step 4: Add an idempotent migration/initialization path** so existing databases receive the new tables and shared database attachment receives matching schema.
- [ ] **Step 5: Run the focused schema tests** and then `go test -tags fts5 ./internal/core/...`.
- [ ] **Step 6: Commit** with `git commit -m "feat: add epistemic cascade storage"`.

---

### Task 2: Persist typed provenance citations

**Files:**
- Create: `internal/core/cascade_provenance.go`
- Modify: `internal/core/core.go`
- Modify: `internal/core/epistemology_tools.go`
- Modify: `internal/core/tools/handlers.go` and tool schemas for `record_decision`/`propose_theory` if source IDs are exposed there
- Test: `internal/core/cascade_provenance_test.go`

**Interfaces:**
- `RecordProvenance(sourceID, sourceType, downstreamID, downstreamType, eventID string) error`
- `ListDownstreamCitations(sourceID string, allowedTypes []string) ([]ProvenanceCitation, error)`
- `ProvenanceCitation` carries source ID/type, downstream ID/type, and event ID.

- [ ] **Step 1: Write failing tests** for recording typed citations, querying only decisions/theories, preserving multiple citations, and ignoring lesson/global-rule targets.
- [ ] **Step 2: Run the focused provenance tests** and confirm failure because no citation API/table exists.
- [ ] **Step 3: Implement `RecordProvenance`** with validation, idempotent uniqueness, and one shared/local DB behavior matching existing writes.
- [ ] **Step 4: Wire source IDs into decision/theory creation** so citations are persisted, not merely counted in `retrieval_metadata`. Preserve current lesson `IncrementSuccess` behavior while treating lesson source IDs as telemetry only.
- [ ] **Step 5: Add compatibility behavior** for legacy untyped IDs by resolving the target collection before inserting or querying a citation.
- [ ] **Step 6: Run provenance and existing epistemology tests**, then commit with `feat: persist reasoning provenance edges`.

---

### Task 3: Implement transactional outbox intent capture

**Files:**
- Create: `internal/core/cascade_outbox.go`
- Modify: `internal/core/core.go`
- Test: `internal/core/cascade_outbox_test.go`

**Interfaces:**
- `type CascadeIntent struct { ... }`
- `CreateInvalidationEvent(tx *sql.Tx, deadArtifactID, deadArtifactType, triggerEvidenceID, reason string, depth int) (string, error)`
- `EnqueueCascadeIntents(tx *sql.Tx, event CascadeInvalidation, targets []ProvenanceTarget) (int, error)`
- `ListPendingCascadeIntents(limit int) ([]CascadeIntent, error)`

- [ ] **Step 1: Write failing transaction tests** showing one intent per eligible target, stable event IDs, duplicate suppression, and rollback when intent insertion fails.
- [ ] **Step 2: Run the focused outbox tests** and confirm failure.
- [ ] **Step 3: Implement event ID generation and target normalization** using crypto-safe IDs and typed artifact validation.
- [ ] **Step 4: Implement insertion with the composite unique constraint** and `ON CONFLICT DO NOTHING`; preserve `trigger_evidence_id` and cascade depth.
- [ ] **Step 5: Implement downstream discovery combining `dependencies` JSON edges and provenance rows**, deduplicating a target found through both paths.
- [ ] **Step 6: Run focused tests plus `go test -tags fts5 ./internal/core/...` and commit** with `feat: enqueue epistemic cascade intents`.

---

### Task 4: Integrate explicit invalidation paths

**Files:**
- Modify: `internal/core/epistemology_tools.go`
- Modify: `internal/core/memory_tools.go`
- Modify: `internal/core/web_db.go`
- Modify: `internal/core/evidence_tools.go`
- Test: `internal/core/cascade_invalidation_test.go`, existing `shred_cascade_test.go`, `cascading_decay_test.go`, and evidence tests

**Interfaces:**
- Existing public methods retain their signatures.
- Internal transaction helpers accept `*sql.Tx` and enqueue intents before commit.

- [ ] **Step 1: Write failing tests** for disproven theory resolution, memory shred, and hard confidence invalidation; assert root mutation and outbox intents commit together.
- [ ] **Step 2: Add a transaction-aware invalidation helper** that captures the current evidence snapshot and calls outbox discovery without opening a second connection.
- [ ] **Step 3: Refactor `ResolveTheory`** so only a transition to `disproven` creates an invalidation event; proven and repeated status updates do not.
- [ ] **Step 4: Refactor lesson-aware and memory shred wrappers** so both supported shred paths enqueue intents while retaining challenged-theory cleanup, topic cleanup, idempotency, and existing return maps.
- [ ] **Step 5: Define and implement the hard confidence threshold transition** in the existing confidence recomputation path; do not enqueue on ordinary weakening or non-crossing updates.
- [ ] **Step 6: Run all affected focused tests and commit** with `feat: hook cascades into invalidation paths`.

---

### Task 5: Build bounded asynchronous materializer

**Files:**
- Modify: `internal/core/cascade_outbox.go`
- Create or modify: the existing daemon worker owner identified from startup inspection
- Modify: `internal/core/core.go` if lifecycle methods are exposed
- Test: `internal/core/cascade_materializer_test.go`

**Interfaces:**
- `NewCascadeMaterializer(dm *DatabaseManager, opts CascadeMaterializerOptions) *CascadeMaterializer`
- `Start(ctx context.Context)` and `Stop()`
- `MaterializeBatch(ctx context.Context, limit int) (MaterializationReport, error)`
- Options include batch size, poll interval, max retries, max depth, and wake delay.

- [ ] **Step 1: Write failing tests** for one-to-one theory creation, metadata/dependency/validation payloads, retryable failure, restart recovery, and idempotent reprocessing.
- [ ] **Step 2: Implement atomic claim/reclaim logic** for pending and abandoned processing rows using the existing single DB connection and timestamps.
- [ ] **Step 3: Materialize through the normal scanner/FTS theory write path**, then update the outbox row with the theory ID and schedule a `cascade` wake.
- [ ] **Step 4: Implement bounded retry/backoff and terminal dead-letter state** with `CRITICAL` audit context.
- [ ] **Step 5: Enforce `cascade_depth <= 3`** and emit a suppression audit record instead of creating deeper intents.
- [ ] **Step 6: Start/stop the worker with the daemon lifecycle**, respecting context cancellation and avoiding leaked goroutines.
- [ ] **Step 7: Run focused materializer tests and commit** with `feat: materialize epistemic cascades asynchronously`.

---

### Task 6: Add bounded cascade wake delivery

**Files:**
- Modify: `internal/core/wake_tools.go`
- Modify: `internal/core/tools/handlers.go` only if response folding needs a new field
- Test: `internal/core/wake_tools_test.go` and `internal/core/cascade_materializer_test.go`

**Interfaces:**
- Preserve `CheckPendingWakes(now time.Time, kinds []string) ([]map[string]interface{}, error)`.
- Add an internal/default constant or configuration key for the cascade limit, default `3`.

- [ ] **Step 1: Write failing tests** with five due cascade wakes and assert one check returns three, leaves two pending, and a later check returns the remainder.
- [ ] **Step 2: Implement classification from `metadata.kind == "cascade"`** and apply the cap only to cascade rows; notification and cron behavior remains unchanged.
- [ ] **Step 3: Verify transactional mark-fired behavior under concurrent checks** so no cascade wake is delivered twice.
- [ ] **Step 4: Run wake tests and commit** with `feat: throttle cascade wake delivery`.

---

### Task 7: Complete adversarial integration and documentation

**Files:**
- Modify: `internal/core/cascade_outbox_test.go` or create `internal/core/epistemic_cascade_integration_test.go`
- Modify: `docs/` operator documentation and tool descriptions where cascade inspection/recovery is exposed
- Modify: `internal/core/tools/registry_list.go` if a dead-letter inspection tool or configuration description is added

- [ ] **Step 1: Add an end-to-end test** that creates one foundation, multiple decision/theory targets via explicit and provenance edges, invalidates the foundation, materializes intents, and verifies three-at-a-time wake delivery.
- [ ] **Step 2: Add exclusion tests** proving lessons/global rules remain untouched and ordinary confidence decreases produce no outbox rows.
- [ ] **Step 3: Add recursive-chain tests** proving depth 1–3 materialize and depth 4 is audited/suppressed.
- [ ] **Step 4: Add failure-injection tests** for scanner/FTS failure, SQLite busy/retry, process restart, and dead-letter visibility.
- [ ] **Step 5: Run the complete required suite:** `make test` and the focused `go test -tags fts5 ./internal/core/...` commands; record any failures rather than claiming success.
- [ ] **Step 6: Update operator documentation** with outbox status, retry/dead-letter semantics, depth configuration, and wake pagination.
- [ ] **Step 7: Commit** with `test: cover epistemic cascade blast radius`.

---

## Self-review

- **Spec coverage:** Trigger policy is covered by Task 4; outbox atomicity and schema by Tasks 1 and 3; provenance by Task 2; one-to-one payload and async retries by Task 5; pagination by Task 6; recursion and exclusions by Tasks 5 and 7; dead-letter auditing by Tasks 5 and 7.
- **Placeholder scan:** No `TBD`, `TODO`, or unspecified “appropriate handling” steps are present.
- **Type consistency:** `CascadeIntent`, `CascadeInvalidation`, `ProvenanceCitation`, `CascadeMaterializerOptions`, and `MaterializationReport` are defined at their producing tasks before downstream use. Existing `CoreDB` signatures remain stable except for explicitly added lifecycle/query methods.
- **Repository reconciliation:** The current implementation increments retrieval success for lesson `source_ids` but does not yet persist decision/theory citation edges. Task 2 therefore adds the missing durable provenance edge table rather than pretending `retrieval_metadata` alone is a dependency graph.
- **Clean baseline:** The design commit is isolated, but the repository still has unrelated untracked plugin/persona files. Do not tag or push a release until an operator explicitly cleans or separates those files.

## Execution handoff

Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Create the feature branch/worktree only at execution time after preserving the unrelated untracked files.
