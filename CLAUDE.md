# CLAUDE.md

Contributor instructions for working safely on MPM.

This file is **not** a second product specification.

## 1. Source of Truth

Use these sources for different purposes:

* **README.md**: canonical product architecture, user-facing behavior, commands, concepts, interfaces, installation, and current semantics.
* **CLAUDE.md**: repository-working rules, validation discipline, safety boundaries, and implementation invariants.
* **Code + tests**: actual implementation truth.

Do not duplicate product documentation here unless a fact is required to prevent an implementation mistake.

If README, CLAUDE.md, tests, and code disagree:

1. stop;
2. identify the disagreement;
3. verify the current implementation and intended contract;
4. reconcile the authoritative documentation and tests as part of the change.

Do not silently treat CLAUDE.md as overriding current product behavior.

### Repository boundary

This repository is the authoritative MPM project.

Do not use sibling repositories, legacy projects, old experiments, or similarly named projects as architectural sources of truth unless explicitly instructed.

In particular, do not infer current MPM behavior from code found outside this repository.

---

## 2. Build and Validation

Canonical commands:

```bash
make build
make test
make test-race
```

`make test-race` is the canonical pre-merge validation gate.

Both test targets use the required SQLite FTS5 build configuration. Bare:

```bash
go test -race ./...
```

is not an equivalent validation command because this repository requires the FTS5 build flags used by the Makefile.

Required build configuration includes:

```bash
CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1
go build -tags fts5
```

Prefer repository Makefile targets over reconstructing build flags manually.

Before considering a substantive change complete:

```bash
make build
make test
make test-race
```

must pass unless the user explicitly accepts a narrower validation scope.

Run `gofmt` on modified Go files.

Run relevant focused tests before the full gate when changing high-risk behavior.

Never bypass repository safety gates with:

```bash
git commit --no-verify
```

Fix the violation instead.

---

## 3. Live-System Safety

Treat the user's installed MPM environment as production state.

Do not, without explicit authorization:

* run deployment scripts;
* restart or modify systemd services;
* replace installed binaries;
* modify live configuration;
* mutate the production MPM database;
* clear persistent state;
* change user-level service enablement;
* push commits or tags.

Commands such as:

```bash
./install.sh
./uninstall.sh
./scripts/deploy.sh
systemctl --user restart ...
```

are operational actions, not ordinary validation steps.

Do not run them merely because the repository builds successfully.

### Test isolation

Automated and manual tests must use isolated state.

Prefer:

* `t.TempDir()`;
* in-memory SQLite fixtures;
* isolated `MPM_WORKSPACE`;
* fake providers via `httptest.Server`;
* temporary config files.

Before running a test helper that performs destructive setup such as `DELETE`, inspect where its database resolves.

Never assume a test fixture is isolated merely because it is named `test`.

If there is any chance a test resolves to the user's production DB, stop and fix the isolation first.

---

## 4. Database and SQLite Invariants

MPM uses SQLite as the durable substrate.

Follow existing `DatabaseManager`, transaction, WAL, timeout, and schema patterns rather than introducing alternative connection ownership.

### Single-connection discipline

Do **not** solve SQLite coordination by calling:

```go
db.SetMaxOpenConns(1)
```

A transaction can hold the sole connection while a nested helper performs a bare-`db` lookup, producing a self-deadlock.

MPM relies on:

* WAL;
* `busy_timeout`;
* explicit transaction discipline;
* threading `*sql.Tx` through helpers where necessary.

Do not perform bare-`db` reads or writes inside a transaction when the operation should participate in that transaction.

Use the repository's existing transaction-aware helper patterns.

### Defensive SQL aggregates

SQLite aggregates such as:

```sql
MAX(...)
MIN(...)
AVG(...)
```

may return `NULL` on empty input.

When scanning into concrete Go values:

* use `COALESCE(...)`, or
* scan through the appropriate `sql.Null*` type.

Do not assume an aggregate always produces a non-NULL primitive.

### Write verification

For durable user-visible/domain writes, `db.Exec(...) == nil` is not sufficient evidence that the intended state persisted.

Where a write can be affected by:

* triggers;
* views;
* constraints;
* transaction behavior;
* derived projections;

verify the resulting authoritative state before returning success.

Follow existing canonical entity write/read-back patterns.

This rule does **not** require every bounded operational write such as a heartbeat, diagnostic cache, or scheduler state update to masquerade as a domain entity. Operational state may use its established persistence contract when that contract is explicitly defined and tested.

### Atomic cross-process files

Files observed by more than one process must use atomic replacement.

Canonical pattern:

1. marshal/write to `<target>.tmp`;
2. flush/close as required;
3. `os.Rename` into place.

Do not expose partially written state.

---

## 5. Projection and Cache Test

Before persisting a derived value, ask:

1. Can it be computed from authoritative state at read time?
2. Is read-time computation cheap enough at expected scale?

If yes to both, prefer computing it on read.

Do not create persistent derived state merely for convenience.

### Allowed derived operational state

Bounded operational or diagnostic caches are allowed when all of the following are true:

* they are not treated as authoritative domain state;
* freshness is explicit;
* invalidation is explicit or self-validating;
* stale data cannot silently become product truth;
* the cache is bounded;
* recomputation remains possible from authoritative state or external verification.

Examples may include diagnostic health state, recent probe results, scheduler metadata, or similar operational observations.

A persisted projection that participates in domain semantics must remain consistent with every authoritative mutation that affects it.

Indexes are not derived product state and may be added when appropriate.

---

## 6. FTS5 and Schema Safety

MPM's SQLite schema depends on FTS5-enabled builds.

Do not validate schema behavior using binaries or tests compiled without the repository's FTS5 configuration.

When modifying:

* FTS tables;
* triggers;
* views;
* migration logic;
* schema initialization;
* shadow-table behavior;

run focused schema tests in addition to the full test gate.

Do not manually manipulate FTS shadow tables.

Do not assume a base table existing implies all FTS-dependent schema objects were created correctly.

Schema changes require explicit compatibility review.

Avoid migrations unless the behavior genuinely requires durable schema evolution.

---

## 7. CLI and Machine-Interface Changes

User-facing interface truth belongs in README/help/tests, not as duplicated command documentation here.

When changing a public surface, check all relevant paths:

* human CLI;
* `--help`;
* machine/JSON output;
* `mpm call`;
* MCP registry/tool behavior where applicable;
* README documentation;
* regression tests.

Do not assume CLI and MCP parity automatically follows from changing one handler.

If a capability is intentionally CLI-only or intentionally not exposed through MCP, make that decision explicit and test it.

Use the existing shared render package for human-facing CLI output.

Do not introduce one-off ANSI formatting or a parallel visual grammar.

Machine-readable output must not depend on terminal rendering.

---

## 8. Provider and Model Integration

When testing provider/model behavior:

* use the production adapter path;
* do not create a second HTTP stack solely for diagnostics;
* do not copy behavior from legacy or sibling projects;
* use fake providers for automated tests;
* never expose secrets in test output, logs, cache state, or rendered errors.

Provider health tests must not call public services during the normal automated test suite.

Use `httptest.Server` or equivalent hermetic fixtures.

Do not modify the user's real credentials merely to manufacture failure cases.

---

## 9. Scheduler and Background Runtime Changes

Do not redesign scheduler cadence, retention, wake semantics, or service behavior in response to a local symptom until the exact lifecycle has been traced.

Keep these concepts separate:

* scheduler tick cadence;
* task due calculation;
* wake materialization;
* wake delivery/consumption;
* retention/cleanup;
* service lifecycle.

A failure in one does not imply the others should change.

When changing scheduler behavior:

* reproduce the defect;
* pin it with a regression test;
* exercise repeated ticks when relevant;
* exercise restart/reopen persistence when relevant;
* preserve bounded cleanup behavior unless the defect is specifically there.

Do not restart the installed scheduler merely to validate source changes without explicit authorization.

---

## 10. Event-Sourced and Cognitive State

Preserve history unless the product contract explicitly calls for destructive deletion.

Where state is event-sourced, distinguish:

* current lifecycle state;
* historical events;
* verification state;
* derived summaries.

Do not infer one axis from another.

Examples:

* lifecycle status and verification may be intentionally orthogonal;
* a cancelled item remaining historically present does not make it active;
* an overdue wake is not necessarily an open work item.

Before changing a filter or projection, identify the authoritative source and the exact query that produced the observed surface.

---

## 11. Scope Discipline

Prefer the smallest change that fixes the reproduced defect or implements the requested capability.

Do not opportunistically redesign adjacent systems.

Do not:

* pull unrelated fixes from stale branches;
* merge historical audit branches just because they contain a similar patch;
* expand public tool surfaces without need;
* add persistence because it is convenient;
* add background services for read-time problems;
* invent compatibility requirements from old projects.

When an old commit contains a useful fix, inspect it and reapply the minimal current semantic change rather than blindly cherry-picking unrelated history.

---

## 12. Documentation Discipline

README.md is the canonical product document.

Keep CLAUDE.md focused on **how to work safely on the repository**.

Do not copy large sections of README architecture or command documentation into this file.

When a product behavior changes:

1. update implementation;
2. update tests;
3. update README/help where appropriate;
4. update CLAUDE.md only if the change affects contributor safety or repository-working rules.

Avoid creating multiple competing descriptions of the same contract.

When product semantics are still moving rapidly, prefer one canonical product document over scattering partially overlapping truth across many files.

---

## 13. Completion Standard

Before reporting a change complete, provide evidence appropriate to its risk.

At minimum:

* implementation complete;
* focused regression tests pass;
* `make build` passes;
* `make test` passes;
* `make test-race` passes;
* working tree state is reported accurately;
* any commit is identified;
* no push is performed unless explicitly requested;
* live install/service state is reported separately from repository/source state.

Do not claim a running installation is fixed merely because source code is fixed.

Distinguish clearly between:

```text
source fixed
binary rebuilt
installed binary updated
service restarted
live behavior verified
```

Those are separate states.

---

## 14. Final Rule

Do not optimize for making the current command succeed.

Optimize for preserving MPM's invariants, reproducibility, auditability, and the ability for another agent or contributor to understand what happened later.

