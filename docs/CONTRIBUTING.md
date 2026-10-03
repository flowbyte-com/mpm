# Contributing to MPM

> **Status: alpha.** APIs, CLI surfaces, on-disk formats, and schema may
> change without notice. Do not assume undocumented behaviour is stable.
> Pin a commit SHA if you need it to stay that way. Note: pinning
> captures *instability*, not security — review what the pinned commit
> exposes before relying on it for sensitive use.

MPM is an alpha-stage project with a deliberately strict architecture. Most
of the rules below exist because the architecture is easy to break in ways
that "work" locally and corrupt invariants globally.

## Before you change code

1. Read [`SPEC.md`](SPEC.md). It is the technical specification; most
   architectural intent is summarised there.
2. Read [`CLAUDE.md`](../CLAUDE.md). It contains the
   repository-specific rules the lead developer relies on day-to-day
   (projection test, scanner coverage, threading caps, single-connection
   invariant, foreign-key posture, the stranger test as a release gate).
3. Read [`docs/SPEC.md`](SPEC.md) for the Projection Principle and the
   Projection Test, and for what is and is not part of the current alpha.
4. Read the code you are about to change. The substrate has structural
   tests that enforce invariants — if your change breaks one, it is
   almost certainly breaking something the tests are designed to catch.

## Architecture rules

These are not stylistic preferences. They are the rules the architecture
is built on. Violations will be rejected in review even when the code
"works."

- **Source of truth vs. projections.** If a value can be computed from
  authoritative state at read time, do not persist it. Indexes are not
  persistence; caches that can drift are. The Projection Test in
  [`docs/SPEC.md`](SPEC.md) is the operational form of this rule.
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
- **All artifact writers pass through the scanner boundary.** Memory
  writes go through `SaveMemoryNode` in `internal/core/memory.go`;
  lesson writes go through `AddLesson` in `internal/core/lessons.go`;
  theories, decisions, and skills go through their respective
  writers. Every artifact writer must call the security scanner
  (`isSensitiveContent` + `isPoisoned`) before INSERT. Coverage is
  enforced by `TestScannerCoverage_AllMemoriesWritersScanContent` —
  if you add a new write path, the test will tell you to scan it.
- **Provenance is observational.** Artifact creation must not depend
  on provenance being recorded successfully. Provenance describes
  declared execution metadata and must never be used as a correctness
  or quality signal. A write path that fails because the provenance
  hook errored is wrong.
- **Do not rewrite shared history to hide mistakes.** If credentials
  or sensitive data enter git history, treat it as a security
  incident and follow [`SECURITY.md`](SECURITY.md). Do not assume
  `git rm`, `git commit --amend`, or `git filter-branch` makes
  historical exposure disappear — once a secret is in a commit it is
  in every clone, fork, mirror, and CI cache that has ever fetched
  that SHA. Force-pushing history also breaks any collaborator who
  has already fetched the rewritten SHAs.
- **One shared connection.** All goroutines use the shared
  `DatabaseManager`. Do not `sql.Open` new connections inside hot
  paths. The whitelist is enforced by
  `TestDatabaseManagerIsOnlyOwnerOfSqlOpen` in
  `internal/core/sqlopen_owner_test.go` — if you add a new call site,
  the test will tell you whether it belongs on the whitelist or whether
  the design needs to route through `DatabaseManager` instead.
- **Foreign keys are on.** Always. Both the local DB and the shared
  DB. Foreign keys off is a bug.

## Tool / action surface design

Before adding or modifying a tool/action entry in
`internal/core/tools/registry_list.go` (or any handler in
`internal/core/tools/handlers.go` / `internal/core/tools/work_handlers.go`),
read **[`docs/tool-behavioral-contract.md`](tool-behavioral-contract.md)**.
It codifies the project-wide rules for not-found semantics (per-verb
class, not per-tool), id-parameter naming (canonical `<resource>_id` +
optional D-8.1 `id` alias), silent coercion vs explicit error (no
silent coercion of malformed input), and idempotency (declare the
class explicitly). Adding a tool without consulting it is how
inconsistencies like C.9 / C.15 / C.20 of the 2026-09-05 audit
(
[`docs/full-tool-behavioural-audit-2026-09-05.md`](full-tool-behavioural-audit-2026-09-05.md)
) crept in — one per-tool decision that should have been a project-wide
policy. The contract document is the canonical answer.

If your new action appears to need behaviour that doesn't fit the
documented rules, **update the contract document first** and then
write the action — do not silently diverge from the project-wide
policy. New-tool authors must also add a `assertParityForTool` lock in
`internal/core/tools/registry_dispatcher_parity_test.go` so future
registry/dispatcher drift is caught.

## Testing

- Full suite:
  ```bash
  make test
  ```
  This runs both modules (the standalone `internal/core/` module and the
  main module). It is slow enough that you should run it before
  opening a PR, not on every save.
- Stranger test (the hermetic specification quickstart gate):
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
  `SafeMigrations`. Add new indexes when they are derivable from
  existing state and verify query plans and migration/startup
  behaviour — SQLite/FTS5 indexes in particular can be operationally
  significant and may require rebuild logic (see the
  `references_ai` / `references_ad` / `references_au` triggers, which
  keep the `references_fts` index in step with the `reference_docs`
  base table, for the pattern).
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
4. **Whether the specification or release notes need an update.** A
   behaviour change without a corresponding specification change is a
   documentation bug.
5. **Whether anything in `docs/` needs updating.** Architecture
   and behaviour shifts go in `docs/SPEC.md`. Design records for work
   that has already landed are preserved under `docs/archive/`.

PRs that claim to add functionality should demonstrate the
functionality working. "I added this and the tests pass" is not
enough if the tests do not actually exercise the feature through a
real call path. If the PR description says "users can now do X",
the stranger test or a sibling test should show X happening.

## Git workflow and branch policy

`main` is the canonical release line. Any work claimed to be part of the
current release must eventually be reachable from `main`.

### Branch categories

| Category | Meaning | Action |
|---|---|---|
| `RELEASE` | `main`, `origin/main` | authoritative |
| `MERGED` | fully reachable from `main` | local copy safe to delete |
| `UNMERGED — REVIEW` | has unique commits not on `main` | operator decides; do not auto-delete |

The script [`scripts/git-audit-refs`](../scripts/git-audit-refs) renders
the full ref inventory with merged/unmerged state, unique-commit counts,
and tip-age hints. Run it before any branch-deletion decision.

### Audit / WIP branches

Audit branches (e.g. `audit/*`) may exist when they contain genuinely
independent or in-progress work. They must not be mistaken for release
history. When retaining an audit branch, document its status (in the
branch's last commit subject or a separate `docs/` note) so future
contributors do not assume its content is on `main`.

### Required all-ref inspection before history rewrites

Before any destructive history operation (`git filter-repo`, `git rebase
--onto`, `git push --force`, `git push --force-with-lease`, or branch
deletion of an unmerged branch), inspect **every ref** — local and
remote-tracking. A blob can be reachable from a remote-tracking branch
even after it has been removed from `main`'s history. The
2026-09-10 `cmd/mpm/mpm_config.json` scrub demonstrated this: a
test-fixture blob survived main's filter-repo pass because the
remote-tracking `origin/audit/...` ref still pointed to a pre-filter
commit. The audit caught it before force-push.

[`scripts/git-safety-gate`](../scripts/git-safety-gate) is the canonical
guardrail. It enumerates refs, highlights substantive unmerged history,
runs a path-reachability check, and requires explicit confirmation
before destructive operations proceed. Use it; do not bypass it.

```bash
# Standard pre-destructive workflow:
scripts/git-safety-gate --op=filter-repo --path=cmd/mpm/mpm_config.json
```

For path-specific checks (e.g. "is this sensitive file present in any
ref?"), use [`scripts/git-history-check`](../scripts/git-history-check):

```bash
scripts/git-history-check cmd/mpm/mpm_config.json
scripts/git-history-check mpm_config.json --content="sk-cp-"
```

### Force-push expectations

- `main` may be force-pushed only when a security or correctness
  operation requires it (e.g. scrubbing leaked secrets). Use
  `git-safety-gate` first; document the rationale in the commit message.
- Audit branches should never be force-pushed unilaterally. They may
  carry unmerged WIP; rewriting their history orphans that work.
- Remote branch deletion requires explicit authorisation — never
  scripted without a human-reviewed list.

### How to determine whether work is actually on `main`

```bash
git log --oneline main..<branch>          # commits on <branch> not on main
git log --oneline <branch>..main          # commits on main not on <branch>
git merge-base --is-ancestor <branch> main && echo "merged" || echo "not merged"
```

If the first command produces output, the branch has unique work not on
`main`. Decide explicitly whether to merge, archive, or delete.

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

AGPL-3.0. Contributions are accepted under the project's AGPL-3.0
license. If your situation requires a formal Contributor License
Agreement (CLA) or Developer Certificate of Origin (DCO), contact
the maintainer before sending a patch. See [`LICENSE`](../LICENSE) for
the full text.

---

_Last updated: 2026-09-05, post-2026-09-05 audit + tool-behavioral-contract map._