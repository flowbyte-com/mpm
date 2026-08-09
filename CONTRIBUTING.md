# Contributing to MPM

> **Status: alpha.** APIs, CLI surfaces, on-disk formats, and schema may
> change without notice. Do not assume undocumented behaviour is stable.
> Pin a commit SHA if you need it to stay that way.

This is a small project run by a small group of people. Most of the rules
below exist because the architecture is easy to break in ways that "work"
locally and corrupt invariants globally.

## Before you change code

1. Read [`README.md`](README.md). It is the user-facing spec; most
   architectural intent is summarised there.
2. Read [`CLAUDE.md`](CLAUDE.md). It contains the
   repository-specific rules the lead developer relies on day-to-day
   (projection test, scanner coverage, threading caps, single-connection
   invariant, foreign-key posture, the stranger test as a release gate).
3. Read [`docs/architecture.md`](docs/architecture.md) for the
   Projection Principle and the Projection Test. Both are short.
4. Read [`docs/RELEASE-NOTES-mpm-alpha.md`](docs/RELEASE-NOTES-mpm-alpha.md)
   for what is and is not part of the current alpha.
5. Read the code you are about to change. The substrate has structural
   tests that enforce invariants — if your change breaks one, it is
   almost certainly breaking something the tests are designed to catch.

## Architecture rules

These are not stylistic preferences. They are the rules the architecture
is built on. Violations will be rejected in review even when the code
"works."

- **Source of truth vs. projections.** If a value can be computed from
  authoritative state at read time, do not persist it. Indexes are not
  persistence; caches that can drift are. The Projection Test in
  `docs/architecture.md` is the operational form of this rule.
- **One persistence for one concern.** Do not write the same fact to two
  tables. Do not write the same value to `metadata.X` *and* a dedicated
  column.
- **No persistence for presentation concerns.** Views, summaries,
  formatted strings, sorted lists — compute them on read. If a UI
  needs a sorted list, sort at the query layer, not by writing sorted
  blobs to the database.
- **Observability stays optional.** Telemetry, provenance, confidence
  scores, reinforcement counts — none of these may become correctness
  dependencies. A write path that requires a confidence value to be set
  is wrong. A read path that fails because a confidence value is
  missing is wrong.
- **All writes go through `SaveMemoryNode`.** The 19-pattern
  secret/poison scanner is structurally downstream of this single
  chokepoint in `internal/core/memory.go`. New write paths route
  through it. The `TestScannerCoverage_AllMemoriesWritersScanContent`
  test fails if you forget.
- **One shared connection.** All goroutines use the shared
  `DatabaseManager`. Do not `sql.Open` new connections inside hot
  paths. A static-analysis test enforces the whitelist.
- **Foreign keys are on.** Always. Both the local DB and the shared
  DB. Foreign keys off is a bug.

## Testing

- Full suite:
  ```bash
  make test
  ```
  This runs both modules (the standalone `internal/core/` module and the
  main module). It is slow enough that you should run it before
  opening a PR, not on every save.
- Stranger test (the hermetic README quickstart gate):
  ```bash
  bash scripts/stranger-test.sh
  ```
  This is what catches "the code compiles and the unit tests pass but
  the user-facing quickstart is broken." Run it before opening a PR
  if your change affects user-visible behaviour.
- Add tests for behaviour you add or change. Tests are table-driven;
  match the patterns in the package you are touching.
- A single test in isolation:
  ```bash
  cd internal/core && go test -tags fts5 -v ./... -run TestName
  ```
  or
  ```bash
  go test -tags fts5 -v ./cmd/mpm/... -run TestName
  ```
- CI must export `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5` or tests will panic.
  If you add a test that needs an environment variable, document it
  in the test and in the PR description.

## Database / schema

- Schema lives in `internal/core/schema.go` as three pieces:
  `BaseTables` (DDL), `CommonIndexes` (indexes), and `SafeMigrations`
  (column additions for upgrade-in-place). Add new tables to
  `BaseTables`. Add new columns to existing tables via
  `SafeMigrations`. Add new indexes freely — indexes are not state.
- FTS5 virtual tables are created in `db.go` (not `schema.go`); that
  separation is intentional and is how tests get a minimal in-memory
  DB without the FTS surface.
- Do not commit `src/db/mpm.db`. Do not commit any SQLite file,
  including fixtures that contain real data. Use `t.TempDir()` and
  seed in-test; the recent hermetic-fixture work in the alpha
  release is the model.
- Do not casually modify the semantics of historical data. If a
  schema change requires reinterpreting existing rows, write a
  migration. If you cannot write a migration, ask first.

## Security

- See [`SECURITY.md`](SECURITY.md) for how to report a vulnerability
  and what to expect.
- Do not commit credentials. API keys, bot tokens, OAuth refresh
  tokens, database dumps, session files. The `.gitignore` catches the
  common cases. If you find a gap, open an issue — do not work
  around it by deleting the file you were about to commit.
- Do not commit runtime state. The SQLite database, backup SQL
  dumps, WAL/SHM files, populated `mpm_config.json`, scratchpads,
  session captures, personal workspace files.
- A secret that has been in a single commit is in every clone, fork,
  mirror, and CI cache. If you discover that a secret has been
  committed, report it as a security incident, not as cleanup.
  Removal from the working tree is not removal from history.

## Pull requests

PRs that change behaviour should explain:

1. **What changed and why.** A paragraph. "Fix bug" is not enough.
2. **What the new behaviour is** for users on the previous version,
   including how to migrate if migration is needed.
3. **Which tests demonstrate the new behaviour** and which tests
   demonstrate that nothing else broke.
4. **Whether the README or release notes need an update.** A
   behaviour change without a corresponding README change is a
   documentation bug.
5. **Whether anything in `docs/` needs updating.** Architecture
   shifts go in `docs/architecture.md` (or a sibling). Behaviour
   shifts go in `README.md`. Cross-cutting shifts go in
   `docs/RELEASE-NOTES-*.md`.

PRs that claim to add functionality should demonstrate the
functionality working. "I added this and the tests pass" is not
enough if the tests do not actually exercise the feature through a
real call path. If the PR description says "users can now do X",
the stranger test or a sibling test should show X happening.

## Issues

- Use the issue templates under `.github/ISSUE_TEMPLATE/` if one
  matches. If none matches, write a clear title and a complete body.
- Bug reports: include the version (commit SHA or tag), OS, the
  exact command that failed, and the observed vs. expected output.
  The stranger test runs against a hermetic install — most user
  reports need a more specific repro to be actionable.
- Feature requests: describe the use case, not just the proposed
  solution. MPM has strong opinions about how features fit into the
  substrate; "it would be nice if…" often turns into "actually no"
  once the architectural cost is named.

## License

AGPL-3.0. By submitting a contribution, you agree it will be
licensed under AGPL-3.0. See [`LICENSE`](LICENSE) for the full text.

---

_Last updated: 2026-08-09, for the `mpm-alpha` release._