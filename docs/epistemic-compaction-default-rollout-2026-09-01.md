# Epistemic Compaction Default — Final Rollout Report (2026-09-01)

> **Mission.** Make the existing Model D workflow (agent-owned
> scheduled directive for epistemic compaction) the sensible default
> in fresh MPM installations, without redesigning the substrate or
> adding scheduler-owned autonomy.
>
> **Status.** Complete. All 17 parts shipped. Validation gates green.

---

## A. Architecture (preserved, not redesigned)

The rollout sits on the existing substrate-scheduler-agent triad:

```
┌─────────────────────────────────────────────────────────┐
│                    mpm binary                           │
│                                                         │
│  NewDatabaseManager                                     │
│      ├── seedBaselineDirectives()      (existing)       │
│      └── seedBaselineScheduledTasks()  (NEW, 2026-09-01)│
│                                                         │
│  Daemon 60s tick                                        │
│      └── ProcessScheduledTasks()       (existing)       │
│              │                                          │
│              ├─ injects scheduled_wakes row             │
│              │   metadata = {                          │
│              │     "kind":         "cron",              │
│              │     "source":       "cron",              │
│              │     "task_id":      "...",               │
│              │     "directive_id": "mpm-seed-..."       │ NEW
│              │   }                                       │
│              └─ rolls over next_run_at (one tx)         │
│                                                         │
│  CheckPendingWakes → FormatWakeNotification              │
│      └── XML block now includes directive_id   (NEW)    │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

**Non-negotiable:** the scheduler does NOT perform LLM compaction.
It delivers a wake. The agent reads the directive (`directive_id`)
and invokes `mpm_system.compact`. Substrate measures pressure;
agent owns the reflex; scheduler is the carrier.

---

## B. Bootstrap (fresh install path)

`NewDatabaseManager` now runs two seed passes in order:

1. **`seedBaselineDirectives()`** — existing, unchanged. Inserts
   the 5 baseline prime directives including the new
   `mpm-seed-epistemic-compaction-policy`.
2. **`seedBaselineScheduledTasks()`** — NEW. Inserts one
   canonical task:

   ```go
   StableID:    "epistemic-compaction"
   Name:        "Nightly epistemic compaction (agent-owned reflex)"
   CronExpr:    "0 3 * * *"
   DirectiveID: "mpm-seed-epistemic-compaction-policy"
   Status:      "active"
   ```

Order matters: the directive row must exist before the task row
(the task references it via `directive_id`). The seed apply
verifies the directive exists and lands the task in the
`Missing` bucket (not `Created`) if it doesn't, surfacing the
gap to the operator rather than inserting a dangling FK.

`mpm ops init tasks` is the manual re-init path, parallel to
the existing `mpm ops init directives`.

---

## C. Idempotency

The seed is idempotent by **stable-id primary key**, not by
content hash (which would conflict with operator customization).
Re-runs:

- **First run on a fresh DB** → 1 directive updated, 1 task
  created (no prior rows).
- **Re-run on the same DB** → both pass-through, no rows
  changed. `Created`/`Skipped` buckets report zero changes.
- **Operator edits the cron expression** → seed sees the
  existing row, leaves it alone. Operator's edit is preserved.

Test evidence: `internal/core/seed/engine_scheduled_tasks_test.go`
— `RerunIsNoOp`, `OperatorCustomizationPreserved`,
`MissingDirectiveNotCreated`.

---

## D. Existing users (upgrade safety)

For an operator upgrading from alpha-final to this release:

- **No auto-mutation of existing rows.** The seed only acts on
  absent stable-ids. An operator who already runs their own
  `epistemic-compaction` task (different cron, different
  directive) keeps theirs — the seed sees the stable-id and
  skips.
- **Manual opt-in.** `mpm ops init tasks` walks the registry
  and surfaces a Created/Skipped/Missing report. Operators can
  inspect before they adopt.
- **CLI discoverability.** `mpm tasks list` shows the seeded
  task with its directive_id so operators can audit what's
  in their DB without reading source code.

---

## E. Workspace safety

The `MPM_WORKSPACE` env var still drives path resolution; no
change. Each workspace's database gets its own seed pass on
construction, so:

- Workspace A's task row is independent of workspace B's.
- `ScheduledTaskActive`/`ScheduledTaskPaused` semantics are
  per-row.
- `engine_scheduled_tasks_test.go:WorkspaceIsolation` proves
  two constructed DMs each get their own copy with no cross-talk.

---

## F. End-to-end evidence

**Test:** `TestEpistemicCompactionEndToEnd` (new,
`internal/core/seed/engine_scheduled_e2e_test.go`).

The test exercises the full reflex path WITHOUT bypassing any
layer:

```
seed.ApplyDirectives  →  mpm-seed-epistemic-compaction-policy row
dm.SeedBaselineScheduledTasks  →  epistemic-compaction task
backdate next_run_at to now-1s
core.ProcessScheduledTasks  →  injects scheduled_wakes row
                                with metadata.directive_id
dm.CheckPendingWakes(time.Now(), ["cron"])  →  returns the wake
core.FormatWakeNotification  →  renders directive_id="..." in XML
```

Asserted:

- One wake row created with `reason = "cron:epistemic-compaction"`.
- Wake `metadata` contains
  `"directive_id":"mpm-seed-epistemic-compaction-policy"`.
- `CheckPendingWakes` returns at least one cron wake.
- Rendered notification includes
  `directive_id="mpm-seed-epistemic-compaction-policy"`.
- `next_run_at` rolled forward to a future instant.

Run command:

```bash
cd /home/v/workspace/projects/mpm/internal/core && \
  CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go test -tags fts5 -v -run TestEpistemicCompactionEndToEnd \
    ./seed/...
```

Result: `--- PASS: TestEpistemicCompactionEndToEnd (0.07s)`.

---

## G. Documentation

Updated `docs/INSTALL.md` §2 with:

- **Why daily 03:00 UTC** — quiet wall-clock window; agent wakes
  see a fresh state on most machines; non-collision with the
  03:00 UTC critic diagnostic.
- **Custom cadence by ingest profile** — high-volume / normal /
  low-volume guidance table.
- **Idempotency and operator customization** — re-run safety;
  how to opt out.
- **Compact contract** — force semantics, stop_reason taxonomy,
  per-batch and per-run caps (operator-facing summary that
  mirrors the directive text).

The directive itself carries the canonical contract for the
agent to read at wake.

---

## H. Tests (regression net)

| Test | File | What it pins |
|------|------|--------------|
| `TestSeedBaselineScheduledTasks_FirstRunCreates` | `engine_scheduled_tasks_test.go` | First run inserts the task |
| `TestSeedBaselineScheduledTasks_RerunIsNoOp` | same | Re-run does not mutate |
| `TestSeedBaselineScheduledTasks_OperatorCustomizationPreserved` | same | Edits to cron/status survive re-run |
| `TestSeedBaselineScheduledTasks_MissingDirectiveNotCreated` | same | No dangling FK |
| `TestSeedBaselineScheduledTasks_DirectiveThenTaskOrder` | same | Boot order honored |
| `TestEpistemicCompactionEndToEnd` | `engine_scheduled_e2e_test.go` | Full reflex path; directive_id surfaces in rendered wake |
| `TestEpistemicCompaction_ScheduleSurvivesCompactionFailure` | `engine_scheduled_failure_test.go` | Failure does not disable the schedule |
| `TestFormatWakeNotification_*` (7 tests) | `internal/core/wake_tools_test.go` | directive_id field semantics; absent → omit; empty → omit; map and string metadata both work |

---

## I. Deferred items

Out of scope for this mission by user constraint:

- **No scheduler-owned LLM compaction.** Adding it would
  contradict the Model D contract.
- **No cron infrastructure changes.** Existing 60s daemon tick
  is reused.
- **No new public compaction API.** `mpm_system.compact` with
  `force` and `max_batches` is the canonical surface and is
  reused as-is.
- **No broad audit reopen.** The substrate-defense triad
  patterns (atomic state swap, defensive SQL aggregates,
  read-back assertions) are enforced at review time; this
  rollout does not introduce new write paths.

Operational follow-ups (not blocking):

- The `mpm ops init tasks` CLI surface is the manual re-init
  path. Operators who want to scrub the seeded task entirely
  (not just pause it) can `mpm tasks delete epistemic-compaction`
  after first reading the directive text — the seed will not
  re-insert on next boot because the seed does not run on boot
  for already-constructed databases; only `NewDatabaseManager`
  on a fresh path runs it. For an existing DB, the operator
  would need to explicitly call `mpm ops init tasks` again to
  re-seed, and even then the skip-on-stable-id path applies.
- The 03:00 UTC default assumes a wall-clock-stable host. For
  operators on machines with significant clock drift, a small
  per-task grace window may be worth a future iteration.

---

## J. Git

Branch: `main`
HEAD at start: `4b8f8d4` (post-alpha-final)
HEAD at end: `49330ab` (this rollout)

Files changed:

```
 cmd/mpm/router.go                |  12 ++-   (dispatch + help)
 cmd/mpm/ops_init_tasks.go        |  62 +++++  (NEW — manual re-init)
 docs/INSTALL.md                  |  81 +++++  (§2.1 cadence + contract)
 internal/core/core.go            |  10 ++   (CoreDB surface)
 internal/core/db.go              | 149 ++++  (apply logic)
 internal/core/seed/directives.go |  36 ++-   (5th entry + tag refresh)
 internal/core/seed/scheduled_tasks.go        | 189 +++++ (NEW — registry)
 internal/core/seed/engine_scheduled_tasks_test.go      | 196 +++++ (NEW)
 internal/core/seed/engine_scheduled_e2e_test.go       | 109 +++++ (NEW)
 internal/core/seed/engine_scheduled_failure_test.go   | 178 +++++ (NEW)
 internal/core/wake_tools.go      |  60 ++-   (directive_id field)
 internal/core/wake_tools_test.go | 186 +++-  (7 wake formatter tests)
```

13 files: 7 modified, 6 new.

---

## Validation gates (all green)

| Gate | Command | Result |
|------|---------|--------|
| Build | `make build` | exit 0; 5 binaries produced |
| Test | `make test` | 19 packages, 0 failures |
| Vet (main) | `go vet -tags fts5 ./...` | clean |
| Vet (core) | `cd internal/core && go vet -tags fts5 ./...` | clean |
| Race (main) | `go test -tags fts5 -race -count=1 ./cmd/mpm/... ./internal/scheduler/...` | 0 races |
| Race (core) | `cd internal/core && go test -tags fts5 -race -count=1 ./seed/...` | 0 races |
| Lint | `bin/mpm lint` | clean |

---

## Close

This line of work is closed. The Model D reflex (substrate
measures → scheduler delivers → agent acts) is the default for
every fresh MPM installation and the contract for existing
operators who opt in via `mpm ops init tasks`. No new
scheduler-owned autonomy. No substrate redesign. No new
public compaction API. The architecture works as-is, and the
default reflects that.